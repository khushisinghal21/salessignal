package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/khushisinghal21/salessignal/internal/capi"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

type scriptedSender struct {
	errs  []error // returned in order; nil = success
	calls [][]capi.Event
	// reject rejects any batch containing an event with this value (non-retryable).
	reject float64
}

func (s *scriptedSender) Send(_ context.Context, req capi.Request) (capi.Response, error) {
	s.calls = append(s.calls, req.Data)
	for _, e := range req.Data {
		if s.reject != 0 && e.CustomData != nil && e.CustomData.Value == s.reject {
			return capi.Response{}, &capi.SendError{Status: 400, Message: "invalid parameter"}
		}
	}
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]
		if err != nil {
			return capi.Response{}, err
		}
	}
	return capi.Response{EventsReceived: len(req.Data)}, nil
}

func ev(v float64) capi.Event {
	return capi.Build("Purchase", time.Now(), capi.Identity{WABAID: "w", CtwaClid: "c"}, &capi.CustomData{Currency: "INR", Value: v})
}

func newBox(s capi.Sender, c *clock) *Outbox {
	return New(s, Options{Now: c.now, Jitter: func(time.Duration) time.Duration { return 0 }, BaseBackoff: time.Second, MaxAttempts: 3})
}

func TestEnqueueDeduplicates(t *testing.T) {
	c := &clock{time.Unix(1_800_000_000, 0)}
	b := newBox(&scriptedSender{}, c)
	if !b.Enqueue("k1", ev(1), nil) {
		t.Fatal("first enqueue refused")
	}
	if b.Enqueue("k1", ev(1), nil) {
		t.Fatal("duplicate key accepted")
	}
	b.ProcessDue(context.Background())
	if b.Enqueue("k1", ev(1), nil) {
		t.Fatal("key accepted again after it was sent")
	}
}

func TestRetryThenSuccess(t *testing.T) {
	c := &clock{time.Unix(1_800_000_000, 0)}
	s := &scriptedSender{errs: []error{&capi.SendError{Status: 503, Retryable: true}, nil}}
	b := newBox(s, c)
	b.Enqueue("k", ev(1), nil)

	b.ProcessDue(context.Background())
	if it := b.Items()[0]; it.State != Retrying || it.Attempts != 1 {
		t.Fatalf("after 503: %+v", it)
	}
	if n := b.ProcessDue(context.Background()); n != 0 {
		t.Fatal("retried before the backoff elapsed")
	}
	c.advance(time.Second)
	b.ProcessDue(context.Background())
	if it := b.Items()[0]; it.State != Sent || it.Attempts != 2 {
		t.Fatalf("after retry: %+v", it)
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	c := &clock{time.Unix(1_800_000_000, 0)}
	e := &capi.SendError{Status: 503, Retryable: true}
	b := newBox(&scriptedSender{errs: []error{e, e, e}}, c)
	b.Enqueue("k", ev(1), nil)
	for i := 0; i < 3; i++ {
		b.ProcessDue(context.Background())
		c.advance(time.Minute)
	}
	if it := b.Items()[0]; it.State != Dead || it.Attempts != 3 {
		t.Fatalf("want dead after 3 attempts: %+v", it)
	}
}

func TestBadEventIsIsolatedFromItsBatch(t *testing.T) {
	c := &clock{time.Unix(1_800_000_000, 0)}
	s := &scriptedSender{reject: 2}
	b := newBox(s, c)
	b.Enqueue("good-1", ev(1), nil)
	b.Enqueue("bad", ev(2), nil)
	b.Enqueue("good-3", ev(3), nil)
	for i := 0; i < 5; i++ {
		b.ProcessDue(context.Background())
	}
	states := map[string]State{}
	for _, it := range b.Items() {
		states[it.Key] = it.State
	}
	if states["good-1"] != Sent || states["good-3"] != Sent || states["bad"] != Dead {
		t.Fatalf("want only the bad event dead, got %v (calls %d)", states, len(s.calls))
	}
}

func TestTransitionsAreReported(t *testing.T) {
	c := &clock{time.Unix(1_800_000_000, 0)}
	var seen []State
	b := New(&scriptedSender{}, Options{Now: c.now, OnTransition: func(it Item, _ State) { seen = append(seen, it.State) }})
	b.Enqueue("k", ev(1), nil)
	b.ProcessDue(context.Background())
	if len(seen) != 2 || seen[0] != Queued || seen[1] != Sent {
		t.Fatalf("transitions = %v", seen)
	}
}
