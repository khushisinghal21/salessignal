// Package audit keeps an append-only, hash-chained record of every decision:
// what the detector proposed, who confirmed or rejected it, what the gate
// blocked and what was sent to Meta. Editing any past entry breaks the chain.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type Entry struct {
	Seq     int            `json:"seq"`
	At      time.Time      `json:"at"`
	Actor   string         `json:"actor"`
	Action  string         `json:"action"`
	Subject string         `json:"subject"`
	Detail  map[string]any `json:"detail,omitempty"`
	Prev    string         `json:"prev"`
	Hash    string         `json:"hash"`
}

type Log struct {
	mu      sync.Mutex
	entries []Entry
	now     func() time.Time
}

func New(now func() time.Time) *Log {
	if now == nil {
		now = time.Now
	}
	return &Log{now: now}
}

func digest(e Entry) string {
	detail, _ := json.Marshal(e.Detail) // map keys are sorted, so this is stable
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%s|%s|%s|%s",
		e.Prev, e.Seq, e.At.UTC().Format(time.RFC3339Nano), e.Actor, e.Action, e.Subject, detail)))
	return hex.EncodeToString(sum[:])
}

func (l *Log) Append(actor, action, subject string, detail map[string]any) Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := Entry{Seq: len(l.entries) + 1, At: l.now(), Actor: actor, Action: action, Subject: subject, Detail: detail}
	if n := len(l.entries); n > 0 {
		e.Prev = l.entries[n-1].Hash
	}
	e.Hash = digest(e)
	l.entries = append(l.entries, e)
	return e
}

func (l *Log) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Entry(nil), l.entries...)
}

// Verify recomputes the chain. It returns the first broken sequence number,
// or 0 if the whole log is intact.
func (l *Log) Verify() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	prev := ""
	for _, e := range l.entries {
		if e.Prev != prev || digest(e) != e.Hash {
			return e.Seq
		}
		prev = e.Hash
	}
	return 0
}
