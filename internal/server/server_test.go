package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/khushisinghal21/salessignal/internal/audit"
	"github.com/khushisinghal21/salessignal/internal/capi"
	"github.com/khushisinghal21/salessignal/internal/core"
	"github.com/khushisinghal21/salessignal/internal/outbox"
	"github.com/khushisinghal21/salessignal/internal/whatsapp"
)

func newTestServer() *Server {
	return New(Options{
		VerifyToken: "verify-me",
		AppSecret:   []byte("app-secret"),
		Build: func() *State {
			dry := &capi.DryRunSender{}
			log := audit.New(nil)
			box := outbox.New(dry, outbox.Options{})
			svc := core.New(core.Config{WABAID: "waba", PageID: "page", Reps: []string{"rep:alex"}}, nil, log, box)
			return &State{Svc: svc, Box: box, DryRun: dry}
		},
	})
}

func payload() []byte {
	msg := whatsapp.Message{From: "919845012345", ID: "wamid.1", Timestamp: fmt.Sprint(time.Now().Add(-time.Hour).Unix()), Type: "text", Text: &whatsapp.Text{Body: "paid ₹1,499"},
		Referral: &whatsapp.Referral{SourceType: "ad", SourceID: "ad-1", CtwaClid: "ARAkClick"}}
	p := whatsapp.Payload{Entry: []whatsapp.Entry{{ID: "waba", Changes: []whatsapp.Change{{Field: "messages", Value: whatsapp.Value{Messages: []whatsapp.Message{msg}}}}}}}
	b, _ := json.Marshal(p)
	return b
}

func TestWebhookHandshake(t *testing.T) {
	h := newTestServer().Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/webhook?hub.mode=subscribe&hub.verify_token=verify-me&hub.challenge=42", nil))
	if rec.Code != 200 || rec.Body.String() != "42" {
		t.Fatalf("handshake: %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/webhook?hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=42", nil))
	if rec.Code != 403 {
		t.Fatalf("wrong token accepted: %d", rec.Code)
	}
}

func TestWebhookRequiresSignature(t *testing.T) {
	s := newTestServer()
	h := s.Handler()
	body := payload()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/webhook", bytes.NewReader(body)))
	if rec.Code != 401 {
		t.Fatalf("unsigned webhook accepted: %d", rec.Code)
	}

	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", whatsapp.Sign([]byte("app-secret"), body))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("signed webhook rejected: %d %s", rec.Code, rec.Body)
	}
	if open := s.Current().Svc.Proposals(core.Admin, core.Open); len(open) != 1 {
		t.Fatalf("webhook did not create a proposal: %+v", open)
	}
}

func TestConfirmOverHTTP(t *testing.T) {
	s := newTestServer()
	h := s.Handler()
	s.Current().Svc.Ingest(func() whatsapp.Payload { var p whatsapp.Payload; _ = json.Unmarshal(payload(), &p); return p }())
	id := s.Current().Svc.Proposals(core.Admin, core.Open)[0].ID

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/proposals/"+id+"/confirm", nil))
	if rec.Code != 401 {
		t.Fatalf("confirm without an actor: %d", rec.Code)
	}

	req := httptest.NewRequest("POST", "/api/proposals/"+id+"/confirm", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-Actor", "rep:alex")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body)
	}

	req = httptest.NewRequest("POST", "/api/proposals/"+id+"/confirm", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-Actor", "rep:alex")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 409 {
		t.Fatalf("second confirm: %d", rec.Code)
	}
}
