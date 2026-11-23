package whatsapp

import (
	"encoding/json"
	"testing"
)

// Shape taken from Meta's Conversions API for Business Messaging guide.
const sample = `{
  "object": "whatsapp_business_account",
  "entry": [{
    "id": "104729351122",
    "changes": [{
      "field": "messages",
      "value": {
        "messaging_product": "whatsapp",
        "metadata": {"display_phone_number": "15550001111", "phone_number_id": "1234"},
        "contacts": [{"profile": {"name": "Kerry Fisher"}, "wa_id": "16315551234"}],
        "messages": [{
          "from": "16315551234",
          "id": "ABGGFlA5FpafAgo6tHcNmNjXmuSf",
          "referral": {
            "body": "This is a great product",
            "ctwa_clid": "ARAkLkA8rmlFeiCktEJQ-QTwRiyYHAFDLMNDBH0CD3qpjd0HR4irJ6LEkR7JwFF4XvnO2E4Nx0-eM-GABDLOPaOdRMv-_zfUQ2a",
            "headline": "Our new product",
            "source_id": "1234567890",
            "source_type": "ad",
            "source_url": "https://fb.me/AAAAA"
          },
          "text": {"body": "Can I learn more about your business?"},
          "timestamp": "1678189586",
          "type": "text"
        }]
      }
    }]
  }]
}`

func TestParseReferral(t *testing.T) {
	var p Payload
	if err := json.Unmarshal([]byte(sample), &p); err != nil {
		t.Fatal(err)
	}
	m := p.Entry[0].Changes[0].Value.Messages[0]
	if m.Referral == nil || m.Referral.CtwaClid == "" || m.Referral.SourceID != "1234567890" {
		t.Fatalf("referral not parsed: %+v", m.Referral)
	}
	if m.Content() != "Can I learn more about your business?" || m.Time().Unix() != 1678189586 {
		t.Fatalf("content/time wrong: %q %v", m.Content(), m.Time())
	}
}

func TestImageCaptionIsContent(t *testing.T) {
	m := Message{Type: "image", Image: &Media{Caption: "paid ✅"}}
	if m.Content() != "paid ✅" {
		t.Fatalf("caption not used: %q", m.Content())
	}
}

func TestSignature(t *testing.T) {
	secret, body := []byte("app-secret"), []byte(sample)
	h := Sign(secret, body)
	if !VerifySignature(secret, body, h) {
		t.Fatal("valid signature rejected")
	}
	if VerifySignature(secret, append(body, ' '), h) {
		t.Fatal("tampered body accepted")
	}
	if VerifySignature([]byte("other"), body, h) || VerifySignature(secret, body, "") {
		t.Fatal("wrong secret or missing header accepted")
	}
}
