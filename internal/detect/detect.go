// Package detect reads a WhatsApp conversation and decides whether a sale
// was closed in it, how much it was for, and which messages prove it.
//
// It is deliberately rule-based: it is cheap enough to run on every message,
// every rule that fires is shown to the rep as evidence, and nothing it finds
// reaches Meta until a person confirms it.
package detect

import (
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type Direction string

const (
	Inbound  Direction = "inbound"  // customer -> business
	Outbound Direction = "outbound" // business (rep) -> customer
)

type Message struct {
	ID   string    `json:"id"`
	Dir  Direction `json:"dir"`
	Kind string    `json:"kind"` // text | image | document
	Text string    `json:"text"` // body, or caption for media
	At   time.Time `json:"at"`
}

// Evidence is one message that pushed the score up, and why.
type Evidence struct {
	MessageID string    `json:"message_id"`
	Rule      string    `json:"rule"`
	Weight    float64   `json:"weight"`
	Snippet   string    `json:"snippet"`
	At        time.Time `json:"at"`

	text   string
	amount *Money // amount named in this message, if any
	quote  *Money // last amount the business quoted before this message
}

// Signal is the detector's verdict on a conversation window.
type Signal struct {
	Event        string     `json:"event"`
	Score        float64    `json:"score"`
	Value        float64    `json:"value"`
	Currency     string     `json:"currency"`
	ValueSource  string     `json:"value_source"` // message | quote | none
	OrderRef     string     `json:"order_ref,omitempty"`
	Evidence     []Evidence `json:"evidence"`
	SaleAt       time.Time  `json:"sale_at"`      // when the strongest evidence was sent
	Contradicted bool       `json:"contradicted"` // a later message said the payment failed or was refunded
}

// Threshold is the score at which a signal becomes a card the rep must confirm.
const Threshold = 0.6

// Proposable reports whether the signal should be shown to a rep.
func (s Signal) Proposable() bool { return s.Score >= Threshold }

type rule struct {
	name   string
	dir    Direction
	weight float64
	re     *regexp.Regexp
}

// Input is lowercased before matching, so patterns need no (?i).
func rx(s string) *regexp.Regexp { return regexp.MustCompile(s) }

var purchaseRules = []rule{
	// The rep saying the money arrived is the strongest signal there is.
	{"rep_confirms_payment_received", Outbound, 0.9, rx(`\b(received|got|recieved)\b.{0,20}(\b(payment|amount|money|transfer)|₹|\brs\.?|\bqar|\baed|\$)`)},
	{"rep_confirms_payment", Outbound, 0.9, rx(`\bpayment\b.{0,15}\b(received|confirmed|successful|verified|mil (gaya|gayi|gya)|aa (gaya|gayi|gya))`)},
	{"rep_thanks_for_payment", Outbound, 0.8, rx(`\bthanks?( you)? for (your |the )?(payment|purchase|order)\b`)},
	{"rep_confirms_order", Outbound, 0.75, rx(`\border\b.{0,20}\b(confirmed|placed|booked)\b`)},

	// Customer claims.
	{"customer_says_paid", Inbound, 0.7, rx(`\b(paid|payed)\b`)},
	{"customer_says_payment_done", Inbound, 0.7, rx(`\bpayment\b.{0,12}\b(done|completed?|successful|made|sent|ho gay[ai]|ho gya)\b`)},
	{"customer_says_transferred", Inbound, 0.7, rx(`\b(sent|transferred|transfered|bhej (diya|diye|di))\b.{0,15}(\b(money|amount|payment|paise|paisa)\b|₹|\brs\.?|\$|\b\d)`)},
	{"customer_says_paid_hinglish", Inbound, 0.7, rx(`\b(payment|paise|paisa|pay|upi|gpay|phonepe|paytm)\b.{0,10}\b(kar|kr)\s?(diya|diye|di|dia)\b`)},
	{"customer_says_upi_done", Inbound, 0.7, rx(`\b(gpay|google pay|phonepe|paytm|upi|neft|imps|rtgs)\b.{0,10}\b(done|sent|kiya|successful)\b`)},
	{"customer_shares_txn_ref", Inbound, 0.6, rx(`\b(utr|txn id|transaction id|transaction no|ref no|reference no|reference number)\b`)},
	{"customer_asks_if_received", Inbound, 0.55, rx(`\b(did|have) you (get|got|receive|received)\b.{0,15}\b(payment|money|amount|transfer)`)},
}

var (
	// A later message that contradicts an earlier payment claim wipes it.
	reReset = []*regexp.Regexp{
		rx(`\b(payment|transaction|txn|transfer|upi|gpay|phonepe|paytm|amount)\b[^.!?\n]{0,25}\b(failed|fail|declined|pending|not (received|reflect\w*|credited|gone through|go through|went through)|didn'?t go|did not go|nahi (hua|hui|aaya|aayi|mila)|nahin (hua|aaya|mila))`),
		rx(`\b(not|haven'?t|have not|didn'?t|did not)\b[^.!?\n]{0,12}\b(receive|received|get|got)\b[^.!?\n]{0,15}\b(payment|amount|money|transfer)`),
		rx(`\b(failed|declined)\b`),
		rx(`\b(refund|refunded|cancel|cancelled|canceled)\b`),
	}
	// Talking about paying later is not paying.
	reFuture = rx(`\b(will|i'?ll|we'?ll|going to|gonna|shall|want to|wanna|need to|ready to|can i|could i|should i|how (do|can|to|should) (i|we)|before (i|we)|once|as soon as|when (i|you|we)|if (i|you|we)|kal|tomorrow|later|tonight)\b[^.!?\n]{0,30}\b(pay|paid|payment|transfer|send|receive|received)`)
	// A payment for some earlier order says nothing about this one.
	rePastOrder = rx(`\b(last|previous|earlier|pichl[ei])\s+(order|time|month|purchase|week)\b`)
	// Cash on delivery is an OrderCreated, not a Purchase: no money has moved.
	reCOD = rx(`\b(cod|cash on delivery|pay on delivery)\b`)
	// "paid ad", "paid partnership": the word, not the act.
	rePaidNoun = rx(`\bpaid (ad|ads|promotion|partnership|collab|post|campaign|version|plan)\b`)

	reAskPay    = rx(`\b(pay|payment|upi|qr|gpay|phonepe|paytm|bank details|account (no|number|details)|screenshot|scan)\b`)
	reShortDone = rx(`^\W*(ok(ay)?|haan|ha|yes)?\W*(done|sent|ho gaya|ho gya|kar diya|kr diya|bhej diya|bhej diye|transferred|paid)\W*$`)
	reOrderRef  = rx(`\b(utr|txn id|transaction id|ref(?:erence)? (?:no|number|id)|order)\s*(?:no\.?|number|id|#)?\s*[:#-]?\s*([a-z0-9][a-z0-9-]{3,})\b`)
)

func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= 120 {
		return s
	}
	r := []rune(s)
	return string(r[:117]) + "..."
}

func isReset(lower string) bool {
	if strings.Contains(lower, "deliver") { // "delivery failed" is a shipping problem, not a payment one
		return false
	}
	for _, re := range reReset {
		if re.MatchString(lower) {
			return true
		}
	}
	return false
}

func orderRef(text string) string {
	for _, m := range reOrderRef.FindAllStringSubmatch(text, -1) {
		ref := m[2]
		if strings.ContainsAny(ref, "0123456789") {
			return strings.ToUpper(ref)
		}
	}
	return ""
}

// Detect scans a conversation window in order and returns the purchase signal
// it ends on. defaultCurrency fills in amounts written without a currency.
func Detect(msgs []Message, defaultCurrency string) Signal {
	sig := Signal{Event: "Purchase", ValueSource: "none"}
	var ev []Evidence
	var quote *Money
	sinceAsk := 99
	wiped := false

	for _, m := range msgs {
		text := strings.TrimSpace(m.Text)
		lower := strings.ToLower(text)
		sinceAsk++

		var amt *Money
		if a, ok := PickAmount(text); ok {
			amt = &a
		}
		if m.Dir == Outbound {
			if amt != nil {
				quote = amt
			}
			if reAskPay.MatchString(lower) {
				sinceAsk = 0
			}
		}

		if isReset(lower) {
			if len(ev) > 0 {
				wiped = true
			}
			ev = nil
			continue
		}
		if rePastOrder.MatchString(lower) || reFuture.MatchString(lower) || reCOD.MatchString(lower) || rePaidNoun.MatchString(lower) {
			continue
		}

		best := Evidence{}
		for _, r := range purchaseRules {
			if r.dir == m.Dir && r.weight > best.Weight && r.re.MatchString(lower) {
				best = Evidence{Rule: r.name, Weight: r.weight}
			}
		}
		if m.Dir == Inbound {
			if sinceAsk <= 3 && best.Weight < 0.5 && reShortDone.MatchString(lower) {
				best = Evidence{Rule: "customer_says_done_after_payment_request", Weight: 0.5}
			}
			if m.Kind == "image" && best.Weight == 0 && !strings.Contains(text, "?") {
				switch {
				case sinceAsk <= 3:
					best = Evidence{Rule: "screenshot_after_payment_request", Weight: 0.6}
				case quote != nil:
					best = Evidence{Rule: "image_after_quote", Weight: 0.3}
				}
			}
		}
		if best.Weight == 0 {
			continue
		}
		best.MessageID, best.At = m.ID, m.At
		best.Snippet = snippet(text)
		if best.Snippet == "" {
			best.Snippet = "[" + m.Kind + "]"
		}
		best.text, best.amount, best.quote = lower, amt, quote
		ev = append(ev, best)
		wiped = false
	}

	if len(ev) == 0 {
		sig.Contradicted = wiped
		return sig
	}

	// Noisy-OR: independent pieces of evidence reinforce each other.
	miss := 1.0
	strongest := 0
	for i, e := range ev {
		miss *= 1 - e.Weight
		if e.Weight >= ev[strongest].Weight {
			strongest = i
		}
	}
	sig.Score = math.Round((1-miss)*1000) / 1000
	sig.Evidence = ev
	sig.SaleAt = ev[strongest].At

	// Amount: from the strongest message that names one, then any evidence
	// message, then the last price the business quoted.
	var chosen *Money
	if ev[strongest].amount != nil {
		chosen, sig.ValueSource = ev[strongest].amount, "message"
	}
	for i := len(ev) - 1; chosen == nil && i >= 0; i-- {
		if ev[i].amount != nil {
			chosen, sig.ValueSource = ev[i].amount, "message"
		}
	}
	for i := len(ev) - 1; chosen == nil && i >= 0; i-- {
		if ev[i].quote != nil {
			chosen, sig.ValueSource = ev[i].quote, "quote"
		}
	}
	if chosen != nil {
		sig.Value = chosen.Value
		sig.Currency = chosen.Currency
	}
	if sig.Currency == "" {
		sig.Currency = defaultCurrency
	}

	for i := len(ev) - 1; i >= 0 && sig.OrderRef == ""; i-- {
		sig.OrderRef = orderRef(ev[i].text)
	}
	if sig.OrderRef == "" {
		for i := len(msgs) - 1; i >= 0 && sig.OrderRef == ""; i-- {
			if msgs[i].Dir == Outbound {
				sig.OrderRef = orderRef(strings.ToLower(msgs[i].Text))
			}
		}
	}
	return sig
}
