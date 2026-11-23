package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/khushisinghal21/salessignal/internal/audit"
	"github.com/khushisinghal21/salessignal/internal/capi"
	"github.com/khushisinghal21/salessignal/internal/outbox"
	"github.com/khushisinghal21/salessignal/internal/whatsapp"
)

type env struct {
	svc  *Service
	box  *outbox.Outbox
	dry  *capi.DryRunSender
	now  time.Time
	seqs map[string]int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC), dry: &capi.DryRunSender{}, seqs: map[string]int{}}
	clock := func() time.Time { return e.now }
	log := audit.New(clock)
	e.box = outbox.New(e.dry, outbox.Options{Now: clock})
	e.svc = New(Config{WABAID: "waba", PageID: "page", DefaultCurrency: "INR", ValueCap: 100000, Reps: []string{"rep:alex", "rep:priya"}}, clock, log, e.box)
	return e
}

func (e *env) customer(waID, text string, ref *whatsapp.Referral) whatsapp.Payload {
	e.seqs[waID]++
	id := fmt.Sprintf("wamid.%s.%d", waID, e.seqs[waID])
	msg := whatsapp.Message{From: waID, ID: id, Timestamp: fmt.Sprint(e.now.Unix()), Type: "text", Text: &whatsapp.Text{Body: text}, Referral: ref}
	v := whatsapp.Value{Messages: []whatsapp.Message{msg}, Contacts: []whatsapp.Contact{{WaID: waID}}}
	v.Contacts[0].Profile.Name = "Customer " + waID[len(waID)-2:]
	return whatsapp.Payload{Entry: []whatsapp.Entry{{ID: "waba", Changes: []whatsapp.Change{{Field: "messages", Value: v}}}}}
}

func (e *env) openFor(t *testing.T, waID string) Proposal {
	t.Helper()
	for _, p := range e.svc.Proposals(Admin, Open) {
		if p.ConversationID == ConversationID(waID) {
			return p
		}
	}
	t.Fatalf("no open proposal for %s", waID)
	return Proposal{}
}

var adRef = &whatsapp.Referral{SourceType: "ad", SourceID: "ad-1", Headline: "Glow pack", CtwaClid: "ARAkClick1"}

func TestEndToEndAdSale(t *testing.T) {
	e := newEnv(t)
	e.svc.Ingest(e.customer("919845012345", "how much?", adRef))
	_ = e.svc.AddOutbound(ConversationID("919845012345"), "₹1,499 for the pack", time.Time{})
	e.svc.Ingest(e.customer("919845012345", "paid via gpay", nil))

	p := e.openFor(t, "919845012345")
	if p.Value != 1499 || p.Path != "ctwa_clid" || p.ValueSource != "quote" {
		t.Fatalf("proposal = %+v", p)
	}
	if _, _, err := e.svc.Confirm(p.ID, p.Owner, ConfirmInput{}); err != nil {
		t.Fatal(err)
	}
	e.box.ProcessDue(context.Background())
	req, ok := e.dry.Last()
	if !ok || len(req.Data) != 1 {
		t.Fatal("nothing sent")
	}
	got := req.Data[0]
	if got.UserData.CtwaClid != "ARAkClick1" || got.CustomData.Value != 1499 || got.ActionSource != "business_messaging" {
		t.Fatalf("sent %+v", got)
	}
}

func TestWebhookRedeliveryIsIgnored(t *testing.T) {
	e := newEnv(t)
	p := e.customer("919800000001", "paid ₹500", adRef)
	e.svc.Ingest(p)
	e.svc.Ingest(p) // Meta retries the same webhook
	if n := len(e.svc.Conversations()[0].Messages); n != 1 {
		t.Fatalf("message stored %d times", n)
	}
}

func TestOnlyOwnerOrAdminCanConfirm(t *testing.T) {
	e := newEnv(t)
	e.svc.Ingest(e.customer("919800000002", "paid ₹500", adRef))
	p := e.openFor(t, "919800000002")
	other := "rep:priya"
	if p.Owner == other {
		other = "rep:alex"
	}
	if _, _, err := e.svc.Confirm(p.ID, other, ConfirmInput{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other rep confirmed: %v", err)
	}
	if _, _, err := e.svc.Confirm(p.ID, Admin, ConfirmInput{}); err != nil {
		t.Fatalf("admin blocked: %v", err)
	}
}

func TestNeedsValueAndRespectsCap(t *testing.T) {
	e := newEnv(t)
	e.svc.Ingest(e.customer("919800000003", "paid", adRef))
	p := e.openFor(t, "919800000003")
	if _, _, err := e.svc.Confirm(p.ID, p.Owner, ConfirmInput{}); !errors.Is(err, ErrNeedValue) {
		t.Fatalf("confirmed without an amount: %v", err)
	}
	if _, rep, err := e.svc.Confirm(p.ID, p.Owner, ConfirmInput{Value: 149900}); !errors.Is(err, ErrGate) || rep.OK() {
		t.Fatalf("typo above cap went through: %v", err)
	}
	if _, _, err := e.svc.Confirm(p.ID, p.Owner, ConfirmInput{Value: 1499}); err != nil {
		t.Fatal(err)
	}
	if st := e.svc.Stats(); st.GateRejections != 1 || st.Confirmed != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestReSentReceiptIsSuppressed(t *testing.T) {
	e := newEnv(t)
	conv := "919800000004"
	e.svc.Ingest(e.customer(conv, "paid ₹1,499", adRef))
	_ = e.svc.AddOutbound(ConversationID(conv), "Received your payment of ₹1,499, order confirmed! Order #VC-2231", time.Time{})
	p := e.openFor(t, conv)
	if _, _, err := e.svc.Confirm(p.ID, p.Owner, ConfirmInput{}); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(3 * time.Hour)
	_ = e.svc.AddOutbound(ConversationID(conv), "Payment of ₹1,499 received, dispatching tomorrow. Order #VC-2231", time.Time{})
	if open := e.svc.Proposals(Admin, Open); len(open) != 0 {
		t.Fatalf("receipt re-send created a second sale: %+v", open)
	}
	if st := e.svc.Stats(); st.DuplicatesSuppressed != 1 {
		t.Fatalf("duplicates = %d", st.DuplicatesSuppressed)
	}

	// A genuinely new order in the same chat is still caught.
	e.now = e.now.Add(48 * time.Hour)
	e.svc.Ingest(e.customer(conv, "paid ₹2,598 for two more", nil))
	if p := e.openFor(t, conv); p.Value != 2598 || !p.PossibleRepeat {
		t.Fatalf("repeat purchase = %+v", p)
	}
}

func TestOrganicChatUsesHashedPhone(t *testing.T) {
	e := newEnv(t)
	conv := "919812300077"
	e.svc.Ingest(e.customer(conv, "paid ₹899", nil))
	p := e.openFor(t, conv)
	if p.Path != "hashed_phone" {
		t.Fatalf("path = %s", p.Path)
	}
	_, rep, err := e.svc.Confirm(p.ID, p.Owner, ConfirmInput{})
	if err != nil || len(rep.Warnings) == 0 {
		t.Fatalf("err=%v warnings=%v", err, rep.Warnings)
	}
	e.box.ProcessDue(context.Background())
	req, _ := e.dry.Last()
	ud := req.Data[0].UserData
	if ud.PageID != "page" || len(ud.Ph) != 1 || ud.Ph[0] != capi.HashPhone(conv) || ud.CtwaClid != "" {
		t.Fatalf("user_data = %+v", ud)
	}
}

func TestContradictionFlagsOpenProposal(t *testing.T) {
	e := newEnv(t)
	conv := "919800000005"
	e.svc.Ingest(e.customer(conv, "paid ₹700", adRef))
	_ = e.svc.AddOutbound(ConversationID(conv), "We haven't received the payment yet", time.Time{})
	if p := e.openFor(t, conv); !p.Contradicted {
		t.Fatal("open proposal not flagged after 'not received'")
	}
}

func TestSweepNudges(t *testing.T) {
	e := newEnv(t)
	e.svc.Ingest(e.customer("919800000006", "paid ₹500", adRef))
	if len(e.svc.Sweep()) != 0 {
		t.Fatal("fresh sale nudged")
	}
	e.now = e.now.Add(2 * time.Hour)
	if n := e.svc.Sweep(); len(n) != 1 || n[0].Reason == "" {
		t.Fatalf("nudges = %+v", n)
	}
}

func TestRejectIsAudited(t *testing.T) {
	e := newEnv(t)
	e.svc.Ingest(e.customer("919800000007", "paid my electricity bill", adRef))
	p := e.openFor(t, "919800000007")
	if _, err := e.svc.Reject(p.ID, p.Owner, "not about our product"); err != nil {
		t.Fatal(err)
	}
	if st := e.svc.Stats(); st.Rejected != 1 || st.AuditBrokenAt != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestConversationIDHidesPhone(t *testing.T) {
	id := ConversationID("919845012345")
	if strings.Contains(id, "9845") || id != ConversationID("+91 98450 12345") {
		t.Fatalf("ConversationID = %q", id)
	}
}

func TestMaskPhone(t *testing.T) {
	if got := MaskPhone("919845012345"); got != "+9198•••••345" {
		t.Fatalf("MaskPhone = %q", got)
	}
}
