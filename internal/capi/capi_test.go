package capi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func TestNormalizeAndHashPhone(t *testing.T) {
	if got := NormalizePhone("+91 98450-12345"); got != "919845012345" {
		t.Fatalf("NormalizePhone = %q", got)
	}
	if got := NormalizePhone("0091 9845012345"); got != "919845012345" {
		t.Fatalf("leading zeros not stripped: %q", got)
	}
	a, b := HashPhone("+91 98450 12345"), HashPhone("919845012345")
	if a != b || !IsHashed(a) {
		t.Fatalf("same number must hash the same: %q vs %q", a, b)
	}
	if HashPhone("") != "" {
		t.Fatal("empty phone must not hash")
	}
}

func TestBuildPaths(t *testing.T) {
	cd := &CustomData{Currency: "INR", Value: 1499}
	ad := Build("Purchase", now, Identity{WABAID: "waba", PageID: "page", CtwaClid: "ARAkClick", Phone: "919845012345"}, cd)
	if ad.Path() != "ctwa_clid" || ad.UserData.PageID != "" || len(ad.UserData.Ph) != 0 {
		t.Fatalf("ad path should carry only waba + ctwa_clid: %+v", ad.UserData)
	}
	org := Build("Purchase", now, Identity{WABAID: "waba", PageID: "page", Phone: "919845012345"}, cd)
	if org.Path() != "hashed_phone" || org.UserData.PageID != "page" || len(org.UserData.Ph) != 1 {
		t.Fatalf("organic path should carry page_id + hashed phone: %+v", org.UserData)
	}
	raw, _ := json.Marshal(ad)
	for _, want := range []string{`"action_source":"business_messaging"`, `"messaging_channel":"whatsapp"`, `"ctwa_clid":"ARAkClick"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("payload missing %s: %s", want, raw)
		}
	}
}

func TestValidate(t *testing.T) {
	good := func() Event {
		return Build("Purchase", now.Add(-time.Hour), Identity{WABAID: "waba", CtwaClid: "ARAkClick"}, &CustomData{Currency: "INR", Value: 1499})
	}
	if r := Validate(good(), now, Limits{ValueCap: 100000}); !r.OK() {
		t.Fatalf("good event rejected: %v", r.Errors)
	}

	cases := map[string]func(e *Event){
		"unsupported event":   func(e *Event) { e.EventName = "Subscribe" },
		"wrong action source": func(e *Event) { e.ActionSource = "website" },
		"future time":         func(e *Event) { e.EventTime = now.Add(time.Hour).Unix() },
		"zero value":          func(e *Event) { e.CustomData.Value = 0 },
		"bad currency":        func(e *Event) { e.CustomData.Currency = "rupees" },
		"above cap":           func(e *Event) { e.CustomData.Value = 149900 },
		"hashed click id":     func(e *Event) { e.UserData.CtwaClid = HashPhone("919845012345") },
		"no identity":         func(e *Event) { e.UserData = UserData{WhatsAppBusinessAccountID: "waba"} },
		"raw phone":           func(e *Event) { e.UserData = UserData{PageID: "p", Ph: []string{"919845012345"}} },
	}
	for name, mutate := range cases {
		e := good()
		mutate(&e)
		if r := Validate(e, now, Limits{ValueCap: 100000}); r.OK() {
			t.Errorf("%s: expected the gate to block it", name)
		}
	}

	e := good()
	e.CustomData.Value = 149900
	if r := Validate(e, now, Limits{ValueCap: 100000, Force: true}); !r.OK() {
		t.Errorf("force should let a re-confirmed large value through: %v", r.Errors)
	}

	old := Build("Purchase", now.Add(-8*24*time.Hour), Identity{WABAID: "w", PageID: "p", Phone: "919845012345"}, &CustomData{Currency: "INR", Value: 10})
	r := Validate(old, now, Limits{})
	if !r.OK() || len(r.Warnings) < 2 {
		t.Errorf("old hashed-path sale should pass with warnings, got %+v", r)
	}
}

func TestEventKeyStable(t *testing.T) {
	if EventKey("w", "c", "Purchase", "VC-1") != EventKey("w", "c", "Purchase", "VC-1") {
		t.Fatal("key must be deterministic")
	}
	if EventKey("w", "c", "Purchase", "VC-1") == EventKey("w", "c", "Purchase", "VC-2") {
		t.Fatal("different orders must get different keys")
	}
}

func TestHTTPSender(t *testing.T) {
	var gotPath, gotToken string
	var gotBody Request
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotToken = r.URL.Path, r.URL.Query().Get("access_token")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(status)
		if status == 200 {
			w.Write([]byte(`{"events_received":1,"fbtrace_id":"abc"}`))
		} else {
			w.Write([]byte(`{"error":{"message":"bad"}}`))
		}
	}))
	defer srv.Close()

	s := &HTTPSender{DatasetID: "123", AccessToken: "tok", BaseURL: srv.URL}
	req := Request{Data: []Event{Build("Purchase", now, Identity{WABAID: "w", CtwaClid: "c"}, &CustomData{Currency: "INR", Value: 1})}, PartnerAgent: "salesignal-poc"}
	resp, err := s.Send(context.Background(), req)
	if err != nil || resp.EventsReceived != 1 {
		t.Fatalf("send: %v %+v", err, resp)
	}
	if gotPath != "/"+GraphVersion+"/123/events" || gotToken != "tok" || gotBody.PartnerAgent != "salesignal-poc" {
		t.Fatalf("wrong request: path=%s token=%s body=%+v", gotPath, gotToken, gotBody)
	}

	for code, retry := range map[int]bool{400: false, 429: true, 503: true} {
		status = code
		_, err := s.Send(context.Background(), req)
		se, ok := err.(*SendError)
		if !ok || se.Retryable != retry {
			t.Errorf("status %d: retryable = %v, want %v", code, se, retry)
		}
	}
}
