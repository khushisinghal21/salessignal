// Package capi builds, checks and sends Meta Conversions API events for
// sales closed in WhatsApp chats (action_source "business_messaging").
//
// Spec: https://developers.facebook.com/documentation/ads-commerce/conversions-api/business-messaging
package capi

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
)

const (
	ActionSource     = "business_messaging"
	MessagingChannel = "whatsapp"
	GraphVersion     = "v27.0"
)

// SupportedEvents are the event names Meta accepts for business messaging.
var SupportedEvents = map[string]bool{
	"Purchase": true, "LeadSubmitted": true, "InitiateCheckout": true, "AddToCart": true,
	"ViewContent": true, "OrderCreated": true, "OrderShipped": true, "OrderDelivered": true,
	"OrderCanceled": true, "OrderReturned": true, "CartAbandoned": true, "QualifiedLead": true,
	"RatingProvided": true, "ReviewProvided": true,
}

type UserData struct {
	WhatsAppBusinessAccountID string   `json:"whatsapp_business_account_id,omitempty"`
	CtwaClid                  string   `json:"ctwa_clid,omitempty"` // sent raw, never hashed
	PageID                    string   `json:"page_id,omitempty"`
	Ph                        []string `json:"ph,omitempty"` // SHA-256 of the normalised phone
}

type CustomData struct {
	Currency string  `json:"currency,omitempty"`
	Value    float64 `json:"value,omitempty"`
	OrderID  string  `json:"order_id,omitempty"`
}

type Event struct {
	EventName        string      `json:"event_name"`
	EventTime        int64       `json:"event_time"`
	ActionSource     string      `json:"action_source"`
	MessagingChannel string      `json:"messaging_channel"`
	UserData         UserData    `json:"user_data"`
	CustomData       *CustomData `json:"custom_data,omitempty"`
}

// Path says how Meta will match the event to an ad: by the click ID from the
// ad, or (for chats that did not start from an ad click) by hashed phone.
func (e Event) Path() string {
	if e.UserData.CtwaClid != "" {
		return "ctwa_clid"
	}
	return "hashed_phone"
}

type Request struct {
	Data         []Event `json:"data"`
	PartnerAgent string  `json:"partner_agent,omitempty"`
}

type Response struct {
	EventsReceived int    `json:"events_received"`
	FBTraceID      string `json:"fbtrace_id,omitempty"`
}

// Identity is what we know about who is behind a conversation.
type Identity struct {
	WABAID   string // WhatsApp Business Account ID
	PageID   string // Facebook Page linked to the dataset
	CtwaClid string // from the referral object of the first ad-originated message
	Phone    string // the customer's wa_id
}

// Build makes an event. With a ctwa_clid it uses the click-ID path exactly as
// Meta's sample does; without one it falls back to page_id + hashed phone.
func Build(name string, at time.Time, id Identity, cd *CustomData) Event {
	ud := UserData{WhatsAppBusinessAccountID: id.WABAID}
	if id.CtwaClid != "" {
		ud.CtwaClid = id.CtwaClid
	} else {
		ud.PageID = id.PageID
		if h := HashPhone(id.Phone); h != "" {
			ud.Ph = []string{h}
		}
	}
	return Event{
		EventName:        name,
		EventTime:        at.Unix(),
		ActionSource:     ActionSource,
		MessagingChannel: MessagingChannel,
		UserData:         ud,
		CustomData:       cd,
	}
}

var nonDigits = regexp.MustCompile(`\D`)

// NormalizePhone keeps digits only and drops leading zeros, per Meta's
// customer-information rules. A wa_id already includes the country code.
func NormalizePhone(raw string) string {
	return strings.TrimLeft(nonDigits.ReplaceAllString(raw, ""), "0")
}

// HashPhone returns the lowercase hex SHA-256 of the normalised phone.
func HashPhone(raw string) string {
	n := NormalizePhone(raw)
	if n == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(n))
	return hex.EncodeToString(sum[:])
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// IsHashed reports whether s looks like a normalised SHA-256 digest.
func IsHashed(s string) bool { return hex64.MatchString(s) }

// EventKey is the idempotency key for an event. Meta does not deduplicate
// business-messaging events, so this is what stops a sale being reported twice.
func EventKey(wabaID, conversationID, eventName, orderOrMessageRef string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{wabaID, conversationID, eventName, orderOrMessageRef}, "|")))
	return hex.EncodeToString(sum[:16])
}
