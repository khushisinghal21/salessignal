package detect

import (
	"fmt"
	"testing"
	"time"
)

func TestPickAmount(t *testing.T) {
	cases := []struct {
		text string
		want float64
		cur  string
		ok   bool
	}{
		{"The pack is ₹1,499 with free shipping", 1499, "INR", true},
		{"Total ₹1,499 + ₹99 shipping = ₹1,598", 1598, "INR", true},
		{"Please pay ₹2,598 for 2 packs (₹1,299 each)", 2598, "INR", true},
		{"₹749 each, ₹1,349 for two", 1349, "INR", true},
		{"Booking amount is ₹1,50,000", 150000, "INR", true},
		{"QAR 45 per bottle", 45, "QAR", true},
		{"transferred 350 AED just now", 350, "AED", true},
		{"I sent you $64 on PayPal", 64, "USD", true},
		{"Consultation is 1500 rs", 1500, "INR", true},
		{"sent 1.5k on phonepe", 1500, "", true},
		{"paid 2098", 2098, "", true},
		{"1499/- only", 1499, "INR", true},
		{"call me on 9876543210", 0, "", false},
		{"UTR 412398765432", 0, "", false},
		{"order #VC-2231 for size 42", 0, "", false},
		{"paid 2 days ago", 0, "", false},
		{"I'll take 2 bottles", 0, "", false},
	}
	for _, c := range cases {
		got, ok := PickAmount(c.text)
		if ok != c.ok || (ok && (got.Value != c.want || got.Currency != c.cur)) {
			t.Errorf("PickAmount(%q) = %v %q %v, want %v %q %v", c.text, got.Value, got.Currency, ok, c.want, c.cur, c.ok)
		}
	}
}

func chat(lines ...string) []Message {
	base := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	var out []Message
	for i, l := range lines {
		m := Message{ID: fmt.Sprint(i), Kind: "text", At: base.Add(time.Duration(i) * time.Minute)}
		switch l[:2] {
		case "c:":
			m.Dir = Inbound
		case "b:":
			m.Dir = Outbound
		case "i:":
			m.Dir, m.Kind = Inbound, "image"
		}
		m.Text = l[2:]
		out = append(out, m)
	}
	return out
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name  string
		msgs  []Message
		want  bool
		value float64
		ref   string
	}{
		{"customer paid with amount", chat("b:It's ₹1,499", "c:done, paid ₹1,499. UTR 412398765432"), true, 1499, "412398765432"},
		{"value falls back to quote", chat("b:Total ₹2,247 hai. UPI: x@okhdfc", "c:payment kar diya bhai"), true, 2247, ""},
		{"screenshot right after payment request", chat("b:Please pay ₹650 on the QR", "i:"), true, 650, ""},
		{"image without a payment request is not enough", chat("b:₹650 for the pack", "i:"), false, 0, ""},
		{"image with a question caption", chat("b:Pay ₹650 and share screenshot", "i:which shade is this?"), false, 0, ""},
		{"failure wipes the earlier claim", chat("c:paid", "c:oh it shows failed"), false, 0, ""},
		{"retry after failure counts", chat("c:paid", "c:it says failed", "c:done now, paid ₹899"), true, 899, ""},
		{"refund wipes the sale", chat("c:paid ₹1,499", "b:received, thanks", "c:please cancel and refund"), false, 0, ""},
		{"future payment is not payment", chat("b:₹899", "c:I'll pay tomorrow"), false, 0, ""},
		{"cash on delivery is not a purchase yet", chat("b:Order confirmed ✅ ₹999, pay cash on delivery"), false, 0, ""},
		{"paid ad is a noun", chat("c:I saw your paid ad on Instagram"), false, 0, ""},
		{"earlier order", chat("c:I paid last time via UPI"), false, 0, ""},
		{"rep confirmation with order number", chat("b:Received your payment of ₹1,499, order confirmed! Order #VC-2231"), true, 1499, "VC-2231"},
		{"once received is conditional", chat("b:Once we receive the payment, we will ship"), false, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sig := Detect(c.msgs, "INR")
			if sig.Proposable() != c.want {
				t.Fatalf("proposable = %v (score %.3f, evidence %+v), want %v", sig.Proposable(), sig.Score, sig.Evidence, c.want)
			}
			if c.want && sig.Value != c.value {
				t.Errorf("value = %v, want %v", sig.Value, c.value)
			}
			if c.want && sig.OrderRef != c.ref {
				t.Errorf("order ref = %q, want %q", sig.OrderRef, c.ref)
			}
		})
	}
}

func TestContradictedFlag(t *testing.T) {
	sig := Detect(chat("c:paid", "b:we haven't received the payment yet"), "INR")
	if sig.Proposable() || !sig.Contradicted {
		t.Fatalf("want contradicted and not proposable, got %+v", sig)
	}
}

func TestSaleTimeIsStrongestEvidence(t *testing.T) {
	msgs := chat("c:paid ₹500", "b:Received your payment of ₹500, thanks!")
	sig := Detect(msgs, "INR")
	if !sig.SaleAt.Equal(msgs[1].At) {
		t.Fatalf("sale time = %v, want the rep's confirmation at %v", sig.SaleAt, msgs[1].At)
	}
}

func BenchmarkDetect(b *testing.B) {
	msgs := chat(
		"c:Hi! Saw your ad, how much is the 30-day pack?",
		"b:Hi! The Glow Collagen 30-day pack is ₹1,499 with free shipping.",
		"c:ok great, can I pay on gpay?",
		"b:Yes, UPI: verdant@okhdfc, please share a screenshot",
		"c:done, paid ₹1,499. UTR 412398765432",
		"b:Received your payment of ₹1,499, order confirmed! Order #VC-2231",
		"c:thanks!",
		"b:Dispatching tomorrow",
	)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Detect(msgs, "INR")
	}
}
