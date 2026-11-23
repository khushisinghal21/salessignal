// Package eval scores the sale detector against a labelled set of chats.
package eval

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/khushisinghal21/salessignal/internal/detect"
)

type Label struct {
	Purchase bool    `json:"purchase"`
	Value    float64 `json:"value"`
	Currency string  `json:"currency"`
}

type Chat struct {
	ID       string   `json:"id"`
	Currency string   `json:"currency"`
	Label    Label    `json:"label"`
	Msgs     []string `json:"msgs"`
}

type Set struct {
	Note  string `json:"note"`
	Chats []Chat `json:"chats"`
}

func Load(path string) (Set, error) {
	var s Set
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// Messages turns the "c:" / "b:" / "c[img]:" shorthand into detector input.
func (c Chat) Messages() ([]detect.Message, error) {
	base := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	out := make([]detect.Message, 0, len(c.Msgs))
	for i, raw := range c.Msgs {
		prefix, text, ok := strings.Cut(raw, ":")
		if !ok {
			return nil, fmt.Errorf("%s: message %d has no speaker prefix", c.ID, i)
		}
		m := detect.Message{ID: fmt.Sprintf("%s-%d", c.ID, i), Kind: "text", Text: strings.TrimSpace(text), At: base.Add(time.Duration(i) * time.Minute)}
		switch strings.TrimSpace(prefix) {
		case "c":
			m.Dir = detect.Inbound
		case "b":
			m.Dir = detect.Outbound
		case "c[img]":
			m.Dir, m.Kind = detect.Inbound, "image"
		default:
			return nil, fmt.Errorf("%s: unknown speaker %q", c.ID, prefix)
		}
		out = append(out, m)
	}
	return out, nil
}

type Row struct {
	ID        string
	Want      bool
	Got       bool
	Score     float64
	WantValue float64
	GotValue  float64
	GotCur    string
	Rule      string
}

type Report struct {
	Rows                   []Row
	TP, FP, FN, TN         int
	AmountRight, AmountAll int
}

func (r Report) Precision() float64 { return ratio(r.TP, r.TP+r.FP) }
func (r Report) Recall() float64    { return ratio(r.TP, r.TP+r.FN) }
func (r Report) AmountAccuracy() float64 {
	return ratio(r.AmountRight, r.AmountAll)
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func Run(s Set) (Report, error) {
	var rep Report
	for _, c := range s.Chats {
		msgs, err := c.Messages()
		if err != nil {
			return rep, err
		}
		sig := detect.Detect(msgs, c.Currency)
		row := Row{ID: c.ID, Want: c.Label.Purchase, Got: sig.Proposable(), Score: sig.Score, WantValue: c.Label.Value, GotValue: sig.Value, GotCur: sig.Currency}
		if len(sig.Evidence) > 0 {
			row.Rule = sig.Evidence[len(sig.Evidence)-1].Rule
		}
		switch {
		case row.Want && row.Got:
			rep.TP++
			rep.AmountAll++
			if math.Abs(sig.Value-c.Label.Value) < 0.5 && sig.Currency == c.Label.Currency {
				rep.AmountRight++
			}
		case !row.Want && row.Got:
			rep.FP++
		case row.Want && !row.Got:
			rep.FN++
		default:
			rep.TN++
		}
		rep.Rows = append(rep.Rows, row)
	}
	return rep, nil
}

func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-32s %-6s %-6s %-6s %-14s %s\n", "chat", "want", "got", "score", "amount", "last rule")
	for _, row := range r.Rows {
		mark := " "
		if row.Want != row.Got {
			mark = "x"
		} else if row.Want && math.Abs(row.GotValue-row.WantValue) >= 0.5 {
			mark = "$"
		}
		amt := "-"
		if row.Got {
			amt = fmt.Sprintf("%.0f %s", row.GotValue, row.GotCur)
		}
		fmt.Fprintf(&b, "%s %-30s %-6v %-6v %-6.3f %-14s %s\n", mark, row.ID, row.Want, row.Got, row.Score, amt, row.Rule)
	}
	fmt.Fprintf(&b, "\nchats=%d  TP=%d FP=%d FN=%d TN=%d\n", len(r.Rows), r.TP, r.FP, r.FN, r.TN)
	fmt.Fprintf(&b, "precision=%.3f  recall=%.3f  amount+currency exact on true positives=%d/%d (%.3f)\n",
		r.Precision(), r.Recall(), r.AmountRight, r.AmountAll, r.AmountAccuracy())
	return b.String()
}
