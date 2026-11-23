// Package core turns WhatsApp messages into sales the rep confirms in one tap,
// and confirmed sales into Conversions API events.
//
// The flow, per conversation:
//
//	webhook message ─► detector ─► proposal card for the chat's owner
//	                                   │ confirm (one tap, amount editable)
//	                                   ▼
//	                 pre-flight gate ─► outbox (dedup, retry) ─► Meta
//
// Nothing reaches Meta without a person confirming it: a wrong Purchase
// teaches Meta's ad delivery to find the wrong buyers.
package core

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/khushisinghal21/salessignal/internal/audit"
	"github.com/khushisinghal21/salessignal/internal/capi"
	"github.com/khushisinghal21/salessignal/internal/detect"
	"github.com/khushisinghal21/salessignal/internal/outbox"
	"github.com/khushisinghal21/salessignal/internal/whatsapp"
)

const Admin = "admin"

var (
	ErrNotFound  = errors.New("not found")
	ErrNotOpen   = errors.New("proposal is no longer open")
	ErrForbidden = errors.New("this chat belongs to another rep")
	ErrNeedValue = errors.New("enter the sale amount before confirming")
	ErrGate      = errors.New("event failed the pre-flight check")
	ErrDuplicate = errors.New("this sale was already sent to Meta")
)

type Config struct {
	WABAID          string
	PageID          string
	DefaultCurrency string
	ValueCap        float64
	Reps            []string
	StaleAfter      time.Duration // nudge the rep when a sale waits this long
	DuplicateWindow time.Duration // same amount, same chat, within this window = the same sale
}

type Conversation struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Phone       string           `json:"phone"` // masked
	Owner       string           `json:"owner"`
	FromAd      bool             `json:"from_ad"`
	AdID        string           `json:"ad_id,omitempty"`
	AdHeadline  string           `json:"ad_headline,omitempty"`
	FirstSeen   time.Time        `json:"first_seen"`
	Messages    []detect.Message `json:"messages"`
	WindowStart int              `json:"window_start"` // messages before this are already accounted for

	waID     string
	ctwaClid string
	seen     map[string]bool
	lastSale *sale
}

type sale struct {
	value    float64
	currency string
	orderRef string
	at       time.Time
}

type Status string

const (
	Open      Status = "open"
	Confirmed Status = "confirmed"
	Rejected  Status = "rejected"
)

type Proposal struct {
	ID             string            `json:"id"`
	ConversationID string            `json:"conversation_id"`
	Customer       string            `json:"customer"`
	Owner          string            `json:"owner"`
	Event          string            `json:"event"`
	Score          float64           `json:"score"`
	Value          float64           `json:"value"`
	Currency       string            `json:"currency"`
	ValueSource    string            `json:"value_source"`
	DetectedValue  float64           `json:"detected_value"`
	OrderRef       string            `json:"order_ref,omitempty"`
	Evidence       []detect.Evidence `json:"evidence"`
	Path           string            `json:"path"` // ctwa_clid | hashed_phone
	AdHeadline     string            `json:"ad_headline,omitempty"`
	Status         Status            `json:"status"`
	Contradicted   bool              `json:"contradicted"`
	PossibleRepeat bool              `json:"possible_repeat"`
	SaleAt         time.Time         `json:"sale_at"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	DecidedBy      string            `json:"decided_by,omitempty"`
	DecidedAt      *time.Time        `json:"decided_at,omitempty"`
	RejectReason   string            `json:"reject_reason,omitempty"`
	EventKey       string            `json:"event_key,omitempty"`
	Warnings       []string          `json:"warnings,omitempty"`

	firstEvidenceID string
}

type Service struct {
	mu         sync.Mutex
	cfg        Config
	now        func() time.Time
	log        *audit.Log
	box        *outbox.Outbox
	convs      map[string]*Conversation
	convOrder  []string
	props      map[string]*Proposal
	propOrder  []string
	openByConv map[string]string
	nextRep    int
	seq        int
	outSeq     int

	duplicatesSuppressed int
	gateRejections       int
	ownershipDenials     int
}

func New(cfg Config, now func() time.Time, log *audit.Log, box *outbox.Outbox) *Service {
	if now == nil {
		now = time.Now
	}
	if cfg.DefaultCurrency == "" {
		cfg.DefaultCurrency = "INR"
	}
	if len(cfg.Reps) == 0 {
		cfg.Reps = []string{"rep:alex"}
	}
	if cfg.StaleAfter == 0 {
		cfg.StaleAfter = time.Hour
	}
	if cfg.DuplicateWindow == 0 {
		cfg.DuplicateWindow = 72 * time.Hour
	}
	return &Service{cfg: cfg, now: now, log: log, box: box,
		convs: map[string]*Conversation{}, props: map[string]*Proposal{}, openByConv: map[string]string{}}
}

func (s *Service) Config() Config { return s.cfg }

// ConversationID derives a stable chat ID that does not reveal the phone
// number, so IDs can appear in logs, URLs and the audit trail.
func ConversationID(waID string) string {
	sum := sha256.Sum256([]byte("chat|" + capi.NormalizePhone(waID)))
	return "chat-" + hex.EncodeToString(sum[:5])
}

// MaskPhone shows enough of a number to recognise it, never the whole thing.
func MaskPhone(waID string) string {
	d := capi.NormalizePhone(waID)
	if len(d) < 8 {
		return "•••"
	}
	return "+" + d[:4] + strings.Repeat("•", len(d)-7) + d[len(d)-3:]
}

func (s *Service) conversation(waID, name string, at time.Time) *Conversation {
	id := ConversationID(waID)
	if c, ok := s.convs[id]; ok {
		if c.Name == "" && name != "" {
			c.Name = name
		}
		return c
	}
	owner := s.cfg.Reps[s.nextRep%len(s.cfg.Reps)]
	s.nextRep++
	c := &Conversation{ID: id, Name: name, Phone: MaskPhone(waID), Owner: owner, FirstSeen: at, waID: waID, seen: map[string]bool{}}
	s.convs[id] = c
	s.convOrder = append(s.convOrder, id)
	s.log.Append("system", "conversation_assigned", id, map[string]any{"owner": owner})
	return c
}

// Ingest handles a Cloud API webhook. Meta redelivers webhooks, so a message
// ID that was already processed is ignored.
func (s *Service) Ingest(p whatsapp.Payload) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range p.Entry {
		for _, ch := range e.Changes {
			if ch.Field != "messages" {
				continue
			}
			names := map[string]string{}
			for _, c := range ch.Value.Contacts {
				names[c.WaID] = c.Profile.Name
			}
			for _, m := range ch.Value.Messages {
				at := m.Time()
				if at.IsZero() {
					at = s.now()
				}
				c := s.conversation(m.From, names[m.From], at)
				if c.seen[m.ID] {
					continue
				}
				c.seen[m.ID] = true
				if r := m.Referral; r != nil && r.CtwaClid != "" && c.ctwaClid == "" {
					c.ctwaClid, c.FromAd, c.AdID, c.AdHeadline = r.CtwaClid, true, r.SourceID, r.Headline
					s.log.Append("system", "ad_click_captured", c.ID, map[string]any{"ad_id": r.SourceID, "headline": r.Headline})
				}
				kind := m.Type
				if kind == "" {
					kind = "text"
				}
				c.Messages = append(c.Messages, detect.Message{ID: m.ID, Dir: detect.Inbound, Kind: kind, Text: m.Content(), At: at})
				s.evaluate(c)
				n++
			}
		}
	}
	return n
}

// AddOutbound records a message the rep sent from the app.
func (s *Service) AddOutbound(convID, text string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.convs[convID]
	if !ok {
		return ErrNotFound
	}
	if at.IsZero() {
		at = s.now()
	}
	s.outSeq++
	c.Messages = append(c.Messages, detect.Message{ID: fmt.Sprintf("out-%d", s.outSeq), Dir: detect.Outbound, Kind: "text", Text: text, At: at})
	s.evaluate(c)
	return nil
}

func sameAmount(a, b float64) bool {
	return math.Abs(a-b) <= math.Max(1, 0.01*math.Max(a, b))
}

// isDuplicate decides whether a fresh signal is the sale we already confirmed
// (the rep re-sending a receipt, the customer re-sharing the screenshot).
func (s *Service) isDuplicate(last *sale, sig detect.Signal) bool {
	if last == nil {
		return false
	}
	if sig.OrderRef != "" && last.orderRef != "" {
		return sig.OrderRef == last.orderRef
	}
	return (sig.Value == 0 || sameAmount(sig.Value, last.value)) && sig.SaleAt.Sub(last.at) <= s.cfg.DuplicateWindow
}

func (s *Service) evaluate(c *Conversation) {
	sig := detect.Detect(c.Messages[c.WindowStart:], s.cfg.DefaultCurrency)
	pid, hasOpen := s.openByConv[c.ID]
	now := s.now()

	if !sig.Proposable() {
		if hasOpen && sig.Contradicted {
			p := s.props[pid]
			if !p.Contradicted {
				p.Contradicted, p.UpdatedAt = true, now
				s.log.Append("detector", "proposal_contradicted", p.ID, map[string]any{"conversation": c.ID})
			}
		}
		return
	}

	if !hasOpen && s.isDuplicate(c.lastSale, sig) {
		s.duplicatesSuppressed++
		s.log.Append("detector", "duplicate_suppressed", c.ID, map[string]any{"value": sig.Value, "order_ref": sig.OrderRef, "reason": "matches the sale already sent for this chat"})
		c.WindowStart = len(c.Messages)
		return
	}

	path := "hashed_phone"
	if c.ctwaClid != "" {
		path = "ctwa_clid"
	}
	if hasOpen {
		p := s.props[pid]
		changed := p.Value != sig.Value || p.Score != sig.Score
		p.Score, p.Evidence, p.SaleAt, p.Contradicted, p.UpdatedAt = sig.Score, sig.Evidence, sig.SaleAt, false, now
		p.Value, p.DetectedValue, p.Currency, p.ValueSource = sig.Value, sig.Value, sig.Currency, sig.ValueSource
		if sig.OrderRef != "" {
			p.OrderRef = sig.OrderRef
		}
		if changed {
			s.log.Append("detector", "proposal_updated", p.ID, map[string]any{"score": sig.Score, "value": sig.Value, "currency": sig.Currency})
		}
		return
	}

	s.seq++
	p := &Proposal{
		ID: fmt.Sprintf("sale-%04d", s.seq), ConversationID: c.ID, Customer: c.Name, Owner: c.Owner,
		Event: sig.Event, Score: sig.Score, Value: sig.Value, DetectedValue: sig.Value, Currency: sig.Currency,
		ValueSource: sig.ValueSource, OrderRef: sig.OrderRef, Evidence: sig.Evidence, Path: path,
		AdHeadline: c.AdHeadline, Status: Open, PossibleRepeat: c.lastSale != nil,
		SaleAt: sig.SaleAt, CreatedAt: now, UpdatedAt: now, firstEvidenceID: sig.Evidence[0].MessageID,
	}
	s.props[p.ID] = p
	s.propOrder = append(s.propOrder, p.ID)
	s.openByConv[c.ID] = p.ID
	s.log.Append("detector", "proposal_created", p.ID, map[string]any{
		"conversation": c.ID, "score": sig.Score, "value": sig.Value, "currency": sig.Currency, "path": path,
		"rules": rulesOf(sig.Evidence),
	})
}

func rulesOf(ev []detect.Evidence) []string {
	out := make([]string, len(ev))
	for i, e := range ev {
		out[i] = e.Rule
	}
	return out
}

func (s *Service) authorize(actor string, c *Conversation, p *Proposal, action string) error {
	if actor == Admin || actor == c.Owner {
		return nil
	}
	s.ownershipDenials++
	s.log.Append(actor, action+"_denied", p.ID, map[string]any{"owner": c.Owner})
	return ErrForbidden
}

type ConfirmInput struct {
	Value    float64 `json:"value"`
	Currency string  `json:"currency"`
	Force    bool    `json:"force"` // re-confirm a value above the sanity cap
}

// Confirm turns a proposal into a Purchase event and queues it for Meta.
func (s *Service) Confirm(id, actor string, in ConfirmInput) (Proposal, capi.Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.props[id]
	if !ok {
		return Proposal{}, capi.Report{}, ErrNotFound
	}
	if p.Status != Open {
		return *p, capi.Report{}, ErrNotOpen
	}
	c := s.convs[p.ConversationID]
	if err := s.authorize(actor, c, p, "confirm"); err != nil {
		return *p, capi.Report{}, err
	}

	value, currency := p.Value, p.Currency
	if in.Value > 0 {
		value = in.Value
	}
	if in.Currency != "" {
		currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	}
	if value <= 0 {
		return *p, capi.Report{}, ErrNeedValue
	}

	now := s.now()
	ev := capi.Build("Purchase", p.SaleAt, capi.Identity{WABAID: s.cfg.WABAID, PageID: s.cfg.PageID, CtwaClid: c.ctwaClid, Phone: c.waID},
		&capi.CustomData{Currency: currency, Value: value, OrderID: p.OrderRef})
	rep := capi.Validate(ev, now, capi.Limits{ValueCap: s.cfg.ValueCap, Force: in.Force})
	if !rep.OK() {
		s.gateRejections++
		s.log.Append(actor, "gate_rejected", p.ID, map[string]any{"errors": rep.Errors, "value": value, "currency": currency})
		return *p, rep, ErrGate
	}

	ref := p.OrderRef
	if ref == "" {
		ref = p.firstEvidenceID
	}
	key := capi.EventKey(s.cfg.WABAID, c.ID, "Purchase", ref)
	if !s.box.Enqueue(key, ev, map[string]string{"proposal": p.ID, "conversation": c.ID, "customer": c.Name}) {
		s.duplicatesSuppressed++
		s.log.Append(actor, "duplicate_suppressed", p.ID, map[string]any{"event_key": key})
		return *p, rep, ErrDuplicate
	}

	edited := !sameAmount(value, p.DetectedValue) || currency != p.Currency
	p.Status, p.Value, p.Currency, p.DecidedBy, p.DecidedAt = Confirmed, value, currency, actor, &now
	p.EventKey, p.Warnings, p.UpdatedAt = key, rep.Warnings, now
	delete(s.openByConv, c.ID)
	c.WindowStart = len(c.Messages)
	c.lastSale = &sale{value: value, currency: currency, orderRef: p.OrderRef, at: p.SaleAt}
	s.log.Append(actor, "sale_confirmed", p.ID, map[string]any{
		"value": value, "currency": currency, "edited_by_rep": edited, "path": ev.Path(), "event_key": key, "warnings": len(rep.Warnings),
	})
	return *p, rep, nil
}

// Reject records "not a sale". Rejections are the labels a better detector
// would be trained on.
func (s *Service) Reject(id, actor, reason string) (Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.props[id]
	if !ok {
		return Proposal{}, ErrNotFound
	}
	if p.Status != Open {
		return *p, ErrNotOpen
	}
	c := s.convs[p.ConversationID]
	if err := s.authorize(actor, c, p, "reject"); err != nil {
		return *p, err
	}
	now := s.now()
	p.Status, p.DecidedBy, p.DecidedAt, p.RejectReason, p.UpdatedAt = Rejected, actor, &now, reason, now
	delete(s.openByConv, c.ID)
	c.WindowStart = len(c.Messages)
	s.log.Append(actor, "sale_rejected", p.ID, map[string]any{"reason": reason, "rules": rulesOf(p.Evidence)})
	return *p, nil
}

// Nudge is an open sale that needs attention.
type Nudge struct {
	ProposalID string  `json:"proposal_id"`
	Customer   string  `json:"customer"`
	Owner      string  `json:"owner"`
	Value      float64 `json:"value"`
	Currency   string  `json:"currency"`
	Reason     string  `json:"reason"`
	AgeHours   float64 `json:"age_hours"`
}

// Sweep lists open sales the owner has sat on, and hashed-path sales close to
// the end of Meta's 7-day attribution window.
func (s *Service) Sweep() []Nudge {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var out []Nudge
	for _, id := range s.propOrder {
		p := s.props[id]
		if p.Status != Open {
			continue
		}
		age := now.Sub(p.SaleAt)
		n := Nudge{ProposalID: p.ID, Customer: p.Customer, Owner: p.Owner, Value: p.Value, Currency: p.Currency, AgeHours: math.Round(age.Hours()*10) / 10}
		switch {
		case p.Path == "hashed_phone" && age > 5*24*time.Hour:
			n.Reason = fmt.Sprintf("hashed-phone match: %.1f days left before Meta stops attributing this sale", math.Max(0, 7-age.Hours()/24))
		case age > s.cfg.StaleAfter:
			n.Reason = "waiting for the rep to confirm"
		default:
			continue
		}
		out = append(out, n)
	}
	return out
}

func (s *Service) Proposals(owner string, status Status) []Proposal {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Proposal
	for i := len(s.propOrder) - 1; i >= 0; i-- {
		p := s.props[s.propOrder[i]]
		if (owner == "" || owner == Admin || p.Owner == owner) && (status == "" || p.Status == status) {
			out = append(out, *p)
		}
	}
	return out
}

func (s *Service) Conversations() []Conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Conversation, 0, len(s.convOrder))
	for _, id := range s.convOrder {
		c := *s.convs[id]
		c.Messages = append([]detect.Message(nil), c.Messages...)
		out = append(out, c)
	}
	return out
}

// ConversationByName finds a chat by customer name (used by the demo).
func (s *Service) ConversationByName(name string) (Conversation, bool) {
	for _, c := range s.Conversations() {
		if c.Name == name {
			return c, true
		}
	}
	return Conversation{}, false
}

type Stats struct {
	Conversations        int                  `json:"conversations"`
	FromAds              int                  `json:"from_ads"`
	OpenProposals        int                  `json:"open_proposals"`
	PendingValue         map[string]float64   `json:"pending_value"`
	Confirmed            int                  `json:"confirmed"`
	ConfirmedValue       map[string]float64   `json:"confirmed_value"`
	EditedByRep          int                  `json:"edited_by_rep"`
	Rejected             int                  `json:"rejected"`
	DuplicatesSuppressed int                  `json:"duplicates_suppressed"`
	GateRejections       int                  `json:"gate_rejections"`
	OwnershipDenials     int                  `json:"ownership_denials"`
	Outbox               map[outbox.State]int `json:"outbox"`
	AuditEntries         int                  `json:"audit_entries"`
	AuditBrokenAt        int                  `json:"audit_broken_at"` // 0 = chain intact
}

func (s *Service) Stats() Stats {
	s.mu.Lock()
	st := Stats{Conversations: len(s.convs), PendingValue: map[string]float64{}, ConfirmedValue: map[string]float64{},
		DuplicatesSuppressed: s.duplicatesSuppressed, GateRejections: s.gateRejections, OwnershipDenials: s.ownershipDenials}
	for _, c := range s.convs {
		if c.FromAd {
			st.FromAds++
		}
	}
	for _, p := range s.props {
		switch p.Status {
		case Open:
			st.OpenProposals++
			st.PendingValue[p.Currency] += p.Value
		case Confirmed:
			st.Confirmed++
			st.ConfirmedValue[p.Currency] += p.Value
			if !sameAmount(p.Value, p.DetectedValue) {
				st.EditedByRep++
			}
		case Rejected:
			st.Rejected++
		}
	}
	s.mu.Unlock()
	st.Outbox = s.box.Counts()
	st.AuditEntries = len(s.log.Entries())
	st.AuditBrokenAt = s.log.Verify()
	return st
}

func (s *Service) Audit(limit int) []audit.Entry {
	all := s.log.Entries()
	sort.SliceStable(all, func(i, j int) bool { return all[i].Seq > all[j].Seq })
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all
}

func (s *Service) Outbox() []outbox.Item { return s.box.Items() }
