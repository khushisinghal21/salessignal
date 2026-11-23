// Package demo loads a few realistic chats for a fictional D2C brand through
// the real webhook path, so the UI has something to show.
package demo

import (
	"fmt"
	"time"

	"github.com/khushisinghal21/salessignal/internal/core"
	"github.com/khushisinghal21/salessignal/internal/whatsapp"
)

type step struct {
	ago  time.Duration
	in   bool // customer message (webhook) vs rep message (app)
	kind string
	text string
}

type script struct {
	waID, name string
	ad         *whatsapp.Referral
	steps      []step
}

func ad(id, headline, clid string) *whatsapp.Referral {
	return &whatsapp.Referral{SourceType: "ad", SourceID: id, Headline: headline, SourceURL: "https://fb.me/demo", CtwaClid: clid}
}

func c(ago time.Duration, text string) step {
	return step{ago: ago, in: true, kind: "text", text: text}
}
func img(ago time.Duration, cap string) step {
	return step{ago: ago, in: true, kind: "image", text: cap}
}
func b(ago time.Duration, text string) step { return step{ago: ago, text: text} }

const (
	h = time.Hour
	m = time.Minute
	d = 24 * time.Hour
)

var scripts = []script{
	{"919845012345", "Ananya Rao", ad("120210000000001", "Glow Collagen · 30-day pack", "ARAkDemoClickIdAnanya01"), []step{
		c(3*h, "Hi! Saw your ad, how much is the 30-day pack?"),
		b(3*h-5*m, "Hi Ananya! The Glow Collagen 30-day pack is ₹1,499 with free shipping."),
		c(2*h+50*m, "ok great, sending on gpay"),
		c(2*h+45*m, "done, paid ₹1,499. UTR 412398765432"),
	}},
	{"919900112233", "Rahul Mehta", ad("120210000000002", "Glow Collagen · Duo offer", "ARAkDemoClickIdRahul02"), []step{
		c(45*m, "Need 2 packs of the collagen"),
		b(40*m, "Sure! Please pay ₹2,598 for 2 packs (₹1,299 each). UPI: verdant@okhdfc, and share a screenshot"),
		img(30*m, ""),
	}},
	{"919812300077", "Fatima Sheikh", nil, []step{
		c(6*d+h, "Hi, is the vitamin C serum in stock?"),
		b(6*d+55*m, "Yes! It's ₹899. UPI: verdant@okhdfc"),
		c(6*d+50*m, "paid"),
		b(6*d+40*m, "Hmm, we haven't received the payment yet"),
		c(6*d+30*m, "it says failed, trying again"),
		c(6*d+20*m, "done now, paid ₹899"),
	}},
	{"919811122233", "Vikram Nair", ad("120210000000001", "Glow Collagen · 30-day pack", "ARAkDemoClickIdVikram04"), []step{
		c(h+10*m, "₹1,499 is too expensive"),
		b(h, "We can do ₹1,349 this week"),
		c(50*m, "will think and pay tomorrow"),
	}},
	{"919876500011", "Meera Iyer", ad("120210000000003", "Starter kit · Reels", "ARAkDemoClickIdMeera05"), []step{
		c(5*h, "I'd like the starter kit"),
		b(5*h-5*m, "It's ₹1,499. UPI: verdant@okhdfc"),
		c(4*h+50*m, "Paid ₹1,499 on PhonePe"),
		b(4*h+45*m, "Received your payment of ₹1,499, order confirmed! Order #VC-2231"),
	}},
	{"919830099887", "Arjun Das", ad("120210000000002", "Glow Collagen · Duo offer", "ARAkDemoClickIdArjun06"), []step{
		c(35*m, "3 pack chahiye"),
		b(30*m, "Total ₹2,247 hai. UPI: verdant@okhdfc"),
		c(20*m, "payment kar diya bhai"),
	}},
	{"919845098765", "Sana Khan", ad("120210000000004", "Glow kit · Stories", "ARAkDemoClickIdSana07"), []step{
		c(2*h, "I saw your paid ad on Instagram, what's in the kit?"),
		b(2*h-5*m, "Collagen + Vitamin C, ₹2,098"),
		c(h+50*m, "COD possible?"),
		b(h+45*m, "Order confirmed ✅ ₹2,098, pay cash on delivery"),
	}},
}

func payload(waba, waID, name, id string, at time.Time, kind, text string, ref *whatsapp.Referral) whatsapp.Payload {
	msg := whatsapp.Message{From: waID, ID: id, Timestamp: fmt.Sprint(at.Unix()), Type: kind, Referral: ref}
	if kind == "image" {
		msg.Image = &whatsapp.Media{ID: "media-" + id, Caption: text, MimeType: "image/jpeg"}
	} else {
		msg.Text = &whatsapp.Text{Body: text}
	}
	v := whatsapp.Value{MessagingProduct: "whatsapp", Messages: []whatsapp.Message{msg}}
	v.Contacts = []whatsapp.Contact{{WaID: waID}}
	v.Contacts[0].Profile.Name = name
	return whatsapp.Payload{Object: "whatsapp_business_account", Entry: []whatsapp.Entry{{ID: waba, Changes: []whatsapp.Change{{Field: "messages", Value: v}}}}}
}

// Seed plays the scripts into svc. Meera's sale is confirmed as her rep would,
// then the rep re-sends the receipt, which shows duplicate suppression.
func Seed(svc *core.Service, now time.Time) {
	waba := svc.Config().WABAID
	for _, s := range scripts {
		first := true
		for i, st := range s.steps {
			at := now.Add(-st.ago)
			if st.in {
				var ref *whatsapp.Referral
				if first {
					ref = s.ad
				}
				svc.Ingest(payload(waba, s.waID, s.name, fmt.Sprintf("wamid.%s.%d", s.waID, i), at, st.kind, st.text, ref))
				first = false
			} else {
				_ = svc.AddOutbound(core.ConversationID(s.waID), st.text, at)
			}
		}
	}

	conv, ok := svc.ConversationByName("Meera Iyer")
	if !ok {
		return
	}
	for _, p := range svc.Proposals(core.Admin, core.Open) {
		if p.ConversationID == conv.ID {
			_, _, _ = svc.Confirm(p.ID, conv.Owner, core.ConfirmInput{})
		}
	}
	_ = svc.AddOutbound(conv.ID, "Payment of ₹1,499 received, dispatching tomorrow. Order #VC-2231", now.Add(-time.Hour))
}
