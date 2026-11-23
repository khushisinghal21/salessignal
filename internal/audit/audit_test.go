package audit

import "testing"

func TestChainDetectsTampering(t *testing.T) {
	l := New(nil)
	l.Append("rep:alex", "sale_confirmed", "sale-1", map[string]any{"value": 1499})
	l.Append("outbox", "event_sent", "sale-1", nil)
	l.Append("rep:alex", "sale_rejected", "sale-2", map[string]any{"reason": "not a payment"})
	if bad := l.Verify(); bad != 0 {
		t.Fatalf("fresh chain broken at %d", bad)
	}

	l.entries[0].Detail["value"] = 14990 // someone edits the amount after the fact
	if bad := l.Verify(); bad != 1 {
		t.Fatalf("edit not detected: broken at %d", bad)
	}
}

func TestChainDetectsDeletion(t *testing.T) {
	l := New(nil)
	for i := 0; i < 3; i++ {
		l.Append("system", "x", "y", nil)
	}
	l.entries = append(l.entries[:1], l.entries[2:]...)
	if l.Verify() == 0 {
		t.Fatal("deleted entry not detected")
	}
}
