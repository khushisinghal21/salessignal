// Package whatsapp parses WhatsApp Cloud API "messages" webhooks.
package whatsapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

type Payload struct {
	Object string  `json:"object"`
	Entry  []Entry `json:"entry"`
}

type Entry struct {
	ID      string   `json:"id"` // WhatsApp Business Account ID
	Changes []Change `json:"changes"`
}

type Change struct {
	Field string `json:"field"`
	Value Value  `json:"value"`
}

type Value struct {
	MessagingProduct string    `json:"messaging_product"`
	Metadata         Metadata  `json:"metadata"`
	Contacts         []Contact `json:"contacts"`
	Messages         []Message `json:"messages"`
}

type Metadata struct {
	DisplayPhoneNumber string `json:"display_phone_number"`
	PhoneNumberID      string `json:"phone_number_id"`
}

type Contact struct {
	Profile struct {
		Name string `json:"name"`
	} `json:"profile"`
	WaID string `json:"wa_id"`
}

type Message struct {
	From      string    `json:"from"`
	ID        string    `json:"id"`
	Timestamp string    `json:"timestamp"`
	Type      string    `json:"type"`
	Text      *Text     `json:"text,omitempty"`
	Image     *Media    `json:"image,omitempty"`
	Document  *Media    `json:"document,omitempty"`
	Referral  *Referral `json:"referral,omitempty"`
}

type Text struct {
	Body string `json:"body"`
}

type Media struct {
	ID       string `json:"id,omitempty"`
	Caption  string `json:"caption,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

// Referral is present on the first message of a chat that started from a
// click-to-WhatsApp ad. CtwaClid is what Meta needs back with the sale.
type Referral struct {
	SourceURL  string `json:"source_url,omitempty"`
	SourceID   string `json:"source_id,omitempty"` // the ad ID
	SourceType string `json:"source_type,omitempty"`
	Headline   string `json:"headline,omitempty"`
	Body       string `json:"body,omitempty"`
	CtwaClid   string `json:"ctwa_clid,omitempty"`
}

// Content returns the text the detector should read: the body for text
// messages, the caption for media.
func (m Message) Content() string {
	switch {
	case m.Text != nil:
		return m.Text.Body
	case m.Image != nil:
		return m.Image.Caption
	case m.Document != nil:
		return m.Document.Caption
	}
	return ""
}

func (m Message) Time() time.Time {
	sec, err := strconv.ParseInt(m.Timestamp, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

// VerifySignature checks Meta's X-Hub-Signature-256 header against the raw
// request body using the app secret.
func VerifySignature(appSecret, body []byte, header string) bool {
	got, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	want, err := hex.DecodeString(got)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, appSecret)
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

// Sign produces the header Meta would send; used by tests and the demo.
func Sign(appSecret, body []byte) string {
	mac := hmac.New(sha256.New, appSecret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
