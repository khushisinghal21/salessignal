// Package outbox queues confirmed events for Meta and delivers each one
// exactly once from our side: duplicate keys are refused at the door,
// transient failures are retried with backoff, and a batch Meta rejects is
// split so one bad event cannot block the good ones.
package outbox

import (
	"context"
	"errors"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/khushisinghal21/salessignal/internal/capi"
)

type State string

const (
	Queued   State = "queued"
	Retrying State = "retrying"
	Sent     State = "sent"
	Dead     State = "dead"
)

type Item struct {
	Key         string            `json:"key"`
	Event       capi.Event        `json:"event"`
	State       State             `json:"state"`
	Attempts    int               `json:"attempts"`
	NextAttempt time.Time         `json:"next_attempt"`
	LastError   string            `json:"last_error,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	SentAt      *time.Time        `json:"sent_at,omitempty"`
	Meta        map[string]string `json:"meta,omitempty"`

	inflight bool
	solo     bool // send alone: it was in a batch Meta rejected
}

type Options struct {
	BatchSize    int
	MaxAttempts  int
	BaseBackoff  time.Duration
	MaxBackoff   time.Duration
	PartnerAgent string
	Now          func() time.Time
	Jitter       func(time.Duration) time.Duration
	// OnTransition is called (outside the outbox lock) whenever an item
	// changes state, so the caller can write it to the audit log.
	OnTransition func(it Item, from State)
}

type Outbox struct {
	mu     sync.Mutex
	items  map[string]*Item
	order  []string
	sender capi.Sender
	opt    Options
}

func New(sender capi.Sender, opt Options) *Outbox {
	if opt.BatchSize <= 0 {
		opt.BatchSize = 50
	}
	if opt.MaxAttempts <= 0 {
		opt.MaxAttempts = 8
	}
	if opt.BaseBackoff <= 0 {
		opt.BaseBackoff = time.Second
	}
	if opt.MaxBackoff <= 0 {
		opt.MaxBackoff = 5 * time.Minute
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Jitter == nil {
		opt.Jitter = func(d time.Duration) time.Duration { return time.Duration(rand.Int63n(int64(d)/5 + 1)) }
	}
	return &Outbox{items: map[string]*Item{}, sender: sender, opt: opt}
}

// Enqueue adds an event. It returns false if the key was already queued or
// sent, which is how a sale confirmed twice is reported only once.
func (o *Outbox) Enqueue(key string, ev capi.Event, meta map[string]string) bool {
	o.mu.Lock()
	if _, exists := o.items[key]; exists {
		o.mu.Unlock()
		return false
	}
	now := o.opt.Now()
	it := &Item{Key: key, Event: ev, State: Queued, NextAttempt: now, CreatedAt: now, Meta: meta}
	o.items[key] = it
	o.order = append(o.order, key)
	snap := *it
	o.mu.Unlock()
	o.notify(snap, "")
	return true
}

func (o *Outbox) notify(it Item, from State) {
	if o.opt.OnTransition != nil {
		o.opt.OnTransition(it, from)
	}
}

func (o *Outbox) backoff(attempts int) time.Duration {
	d := o.opt.BaseBackoff << (attempts - 1)
	if d <= 0 || d > o.opt.MaxBackoff {
		d = o.opt.MaxBackoff
	}
	return d + o.opt.Jitter(d)
}

// due picks the next batch to send and marks it in flight.
func (o *Outbox) due(now time.Time) []*Item {
	var batch []*Item
	for _, k := range o.order {
		it := o.items[k]
		if it.inflight || (it.State != Queued && it.State != Retrying) || it.NextAttempt.After(now) {
			continue
		}
		if it.solo {
			if len(batch) == 0 {
				batch = append(batch, it)
			}
			break
		}
		batch = append(batch, it)
		if len(batch) == o.opt.BatchSize {
			break
		}
	}
	for _, it := range batch {
		it.inflight = true
	}
	return batch
}

// ProcessDue sends one batch of due events and returns how many it tried.
func (o *Outbox) ProcessDue(ctx context.Context) int {
	o.mu.Lock()
	batch := o.due(o.opt.Now())
	o.mu.Unlock()
	if len(batch) == 0 {
		return 0
	}

	req := capi.Request{PartnerAgent: o.opt.PartnerAgent}
	for _, it := range batch {
		req.Data = append(req.Data, it.Event)
	}
	_, err := o.sender.Send(ctx, req)

	var sendErr *capi.SendError
	retryable := err != nil && (!errors.As(err, &sendErr) || sendErr.Retryable)

	type change struct {
		it   Item
		from State
	}
	var changes []change

	o.mu.Lock()
	now := o.opt.Now()
	for _, it := range batch {
		it.inflight = false
		from := it.State
		switch {
		case err == nil:
			it.Attempts++
			it.State, it.LastError = Sent, ""
			it.SentAt = &now
		case !retryable && len(batch) > 1:
			// Meta rejected the whole batch for something in it. Send each
			// event on its own so only the bad one ends up dead.
			it.solo = true
			it.LastError = "batch rejected, isolating: " + err.Error()
		case !retryable:
			it.Attempts++
			it.State, it.LastError = Dead, err.Error()
		default:
			it.Attempts++
			it.LastError = err.Error()
			if it.Attempts >= o.opt.MaxAttempts {
				it.State = Dead
			} else {
				it.State = Retrying
				it.NextAttempt = now.Add(o.backoff(it.Attempts))
			}
		}
		if it.State != from {
			changes = append(changes, change{*it, from})
		}
	}
	o.mu.Unlock()

	for _, c := range changes {
		o.notify(c.it, c.from)
	}
	return len(batch)
}

// Run delivers due events until ctx is cancelled.
func (o *Outbox) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for o.ProcessDue(ctx) > 0 {
			}
		}
	}
}

// Items returns a snapshot, newest first.
func (o *Outbox) Items() []Item {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Item, 0, len(o.order))
	for _, k := range o.order {
		out = append(out, *o.items[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (o *Outbox) Counts() map[State]int {
	o.mu.Lock()
	defer o.mu.Unlock()
	c := map[State]int{Queued: 0, Retrying: 0, Sent: 0, Dead: 0}
	for _, it := range o.items {
		c[it.State]++
	}
	return c
}
