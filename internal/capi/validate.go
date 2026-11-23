package capi

import (
	"fmt"
	"regexp"
	"time"
)

// Report is the result of the pre-flight gate. Any error blocks the event;
// warnings are shown to the rep but do not block.
type Report struct {
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}

func (r Report) OK() bool { return len(r.Errors) == 0 }

func (r *Report) errf(f string, a ...any)  { r.Errors = append(r.Errors, fmt.Sprintf(f, a...)) }
func (r *Report) warnf(f string, a ...any) { r.Warnings = append(r.Warnings, fmt.Sprintf(f, a...)) }

type Limits struct {
	ValueCap float64 // above this a Purchase is held as a likely typo (paise typed as rupees, an extra zero)
	Force    bool    // the rep re-confirmed a value above the cap
}

var (
	reCurrency       = regexp.MustCompile(`^[A-Z]{3}$`)
	commonCurrencies = map[string]bool{"INR": true, "QAR": true, "AED": true, "SAR": true, "KWD": true, "OMR": true, "BHD": true, "USD": true, "EUR": true, "GBP": true, "SGD": true, "MYR": true, "KRW": true}
)

const maxClockSkew = 5 * time.Minute

// Validate checks an event against Meta's business-messaging rules before it
// is queued. It fails closed: anything it cannot vouch for is an error.
func Validate(e Event, now time.Time, lim Limits) Report {
	var r Report

	if !SupportedEvents[e.EventName] {
		r.errf("event_name %q is not supported for business messaging", e.EventName)
	}
	if e.ActionSource != ActionSource {
		r.errf("action_source must be %q, got %q", ActionSource, e.ActionSource)
	}
	if e.MessagingChannel != MessagingChannel {
		r.errf("messaging_channel must be %q, got %q", MessagingChannel, e.MessagingChannel)
	}

	t := time.Unix(e.EventTime, 0)
	switch {
	case e.EventTime <= 0:
		r.errf("event_time is missing")
	case t.After(now.Add(maxClockSkew)):
		r.errf("event_time %s is in the future", t.UTC().Format(time.RFC3339))
	}

	ud := e.UserData
	switch {
	case ud.CtwaClid != "":
		if ud.WhatsAppBusinessAccountID == "" {
			r.errf("whatsapp_business_account_id is required with ctwa_clid")
		}
		if IsHashed(ud.CtwaClid) {
			r.errf("ctwa_clid looks hashed: Meta expects the raw click ID from the webhook")
		}
	default:
		if ud.PageID == "" {
			r.errf("no ctwa_clid and no page_id: Meta cannot tell which Page the chat belongs to")
		}
		if len(ud.Ph) == 0 {
			r.errf("no ctwa_clid and no hashed customer information: nothing to match the sale to an ad")
		}
		r.warnf("chat did not start from a click-to-WhatsApp ad: Meta will try to match by hashed phone, which is not guaranteed")
		if age := now.Sub(t); e.EventTime > 0 && age > 7*24*time.Hour {
			r.warnf("sale happened %.0f days ago: on the hashed-phone path Meta only attributes to ad interactions from the past 7 days, so this will be accepted but not attributed", age.Hours()/24)
		}
	}
	for i, h := range ud.Ph {
		if !IsHashed(h) {
			r.errf("ph[%d] is not a normalised SHA-256 digest: raw phone numbers must never be sent", i)
		}
	}

	if e.EventName == "Purchase" {
		cd := e.CustomData
		switch {
		case cd == nil || cd.Value <= 0:
			r.errf("Purchase needs a value greater than zero")
		case !reCurrency.MatchString(cd.Currency):
			r.errf("currency %q is not a 3-letter ISO 4217 code", cd.Currency)
		default:
			if !commonCurrencies[cd.Currency] {
				r.warnf("currency %s is unusual for this workspace: double-check it", cd.Currency)
			}
			if lim.ValueCap > 0 && cd.Value > lim.ValueCap && !lim.Force {
				r.errf("value %.2f %s is above this workspace's sanity cap of %.0f: re-confirm if it is real", cd.Value, cd.Currency, lim.ValueCap)
			}
		}
	}
	return r
}
