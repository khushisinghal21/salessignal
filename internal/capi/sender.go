package capi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Sender delivers a batch of events to Meta.
type Sender interface {
	Send(ctx context.Context, req Request) (Response, error)
}

// SendError says whether a failed send is worth retrying.
type SendError struct {
	Status    int
	Retryable bool
	Message   string
}

func (e *SendError) Error() string {
	return fmt.Sprintf("meta capi: status %d (retryable=%v): %s", e.Status, e.Retryable, e.Message)
}

// HTTPSender posts to graph.facebook.com/{version}/{dataset_id}/events.
type HTTPSender struct {
	DatasetID   string
	AccessToken string
	BaseURL     string // defaults to https://graph.facebook.com
	Version     string // defaults to GraphVersion
	Client      *http.Client
}

func (h *HTTPSender) Send(ctx context.Context, req Request) (Response, error) {
	base, ver, client := h.BaseURL, h.Version, h.Client
	if base == "" {
		base = "https://graph.facebook.com"
	}
	if ver == "" {
		ver = GraphVersion
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, &SendError{Message: err.Error()}
	}
	u := fmt.Sprintf("%s/%s/%s/events?%s", base, ver, url.PathEscape(h.DatasetID), url.Values{"access_token": {h.AccessToken}}.Encode())
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return Response{}, &SendError{Message: err.Error()}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil { // network trouble: try again later
		return Response{}, &SendError{Retryable: true, Message: "request failed"}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 == 2 {
		var r Response
		if err := json.Unmarshal(b, &r); err != nil {
			return Response{}, &SendError{Status: resp.StatusCode, Message: "unreadable response"}
		}
		return r, nil
	}
	msg := string(b)
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return Response{}, &SendError{
		Status:    resp.StatusCode,
		Retryable: resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
		Message:   msg,
	}
}

// DryRunSender records what would have been sent. It can pretend Meta is
// down, so the retry path can be watched in the demo.
type DryRunSender struct {
	mu      sync.Mutex
	failing bool
	sent    []Request
	calls   int
}

func (d *DryRunSender) Send(_ context.Context, req Request) (Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.failing {
		return Response{}, &SendError{Status: 503, Retryable: true, Message: "simulated Meta outage"}
	}
	d.sent = append(d.sent, req)
	if len(d.sent) > 50 {
		d.sent = d.sent[len(d.sent)-50:]
	}
	return Response{EventsReceived: len(req.Data), FBTraceID: fmt.Sprintf("dryrun-%d", d.calls)}, nil
}

func (d *DryRunSender) SetFailing(on bool) {
	d.mu.Lock()
	d.failing = on
	d.mu.Unlock()
}

func (d *DryRunSender) Failing() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.failing
}

// Last returns the most recent request that "reached" Meta.
func (d *DryRunSender) Last() (Request, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.sent) == 0 {
		return Request{}, false
	}
	return d.sent[len(d.sent)-1], true
}
