package eval

import "testing"

// TestDetectorRegression fails if a rule change makes the detector worse on
// the labelled chats. Precision is held higher than recall on purpose: a
// missed sale can still be marked by hand, but a false Purchase trains Meta
// to look for the wrong buyers.
func TestDetectorRegression(t *testing.T) {
	set, err := Load("chats.json")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(set)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + rep.String())
	if p := rep.Precision(); p < 0.95 {
		t.Errorf("precision %.3f fell below 0.95", p)
	}
	if r := rep.Recall(); r < 0.85 {
		t.Errorf("recall %.3f fell below 0.85", r)
	}
	if a := rep.AmountAccuracy(); a < 0.95 {
		t.Errorf("amount accuracy %.3f fell below 0.95", a)
	}
}
