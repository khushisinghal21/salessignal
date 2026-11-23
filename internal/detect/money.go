package detect

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Money is an amount found in a chat message.
type Money struct {
	Value    float64 `json:"value"`
	Currency string  `json:"currency"` // ISO 4217; empty when the text named none
	Raw      string  `json:"raw"`
	start    int
	end      int
	explicit bool // a currency marker sat next to the number
}

// num matches 1499, 1,499, 1,49,999 (Indian grouping) and 1499.50.
const num = `(\d{1,3}(?:,\d{2,3})+(?:\.\d+)?|\d+(?:\.\d+)?)`

var (
	// ₹1,499  Rs. 1499  QAR 90  $64  AED 350
	rePrefix = regexp.MustCompile(`(₹|\$|\brs\.?|\binr|\bqar|\bqr|\baed|\bsar|\busd)\s?` + num + `(\s?k\b)?`)
	// 1499 rs  350 AED  120 riyals  1499/-
	reSuffix = regexp.MustCompile(num + `(\s?k\b)?\s?(rupees?\b|rs\b\.?|inr\b|qar\b|qr\b|riyals?\b|aed\b|dirhams?\b|sar\b|usd\b|dollars?\b|/-)`)
	// "total 2098", "paid 2098", "sent 1.5k": a bare number right after a money word.
	reKeyword = regexp.MustCompile(`\b(grand total|total|amount|price|cost|paid|pay|payable|bill|mrp|sent|send|transferred|transfer)\b[^\d\n]{0,12}?` + num + `(\s?k\b)?`)

	// A bare number followed by one of these is a count or a time, not money.
	reNotMoneyAfter = regexp.MustCompile(`^\s*(days?|hrs?|hours?|mins?|minutes?|weeks?|months?|items?|bottles?|packs?|jars?|bags?|pcs|pieces|units|kg|g\b|ml|x\b|%|am\b|pm\b)`)

	reTotalBefore = regexp.MustCompile(`(grand total|total|=|payable|final amount)\s*(is|of|:|-)?\s*$`)
	rePayBefore   = regexp.MustCompile(`(payment|pay|paid|amount|price|cost|send|sent|transfer(red)?|bill|mrp|advance|fee)\s*(is|of|:|-|me|us)?\s*$`)
	reUnitAfter   = regexp.MustCompile(`^\s*(each|per\b|/\s?(unit|pc|piece|bottle|pack|jar|month)|a (pack|bottle|piece|jar|month)|x\b|\*)`)
)

func currencyOf(tok string) string {
	t := strings.TrimSuffix(strings.TrimSpace(strings.ToLower(tok)), ".")
	switch t {
	case "₹", "rs", "inr", "rupee", "rupees", "/-":
		return "INR"
	case "qar", "qr":
		return "QAR"
	case "aed", "dirham", "dirhams":
		return "AED"
	case "sar":
		return "SAR"
	case "$", "usd", "dollar", "dollars":
		return "USD"
	}
	// "riyal" is ambiguous (QAR or SAR): leave it to the workspace default.
	return ""
}

func parseNum(s string, thousands bool) (float64, bool) {
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	if thousands {
		v *= 1000
	}
	return v, true
}

func digitCount(s string) int {
	n := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			n++
		}
	}
	return n
}

// FindAmounts returns every amount in text, left to right. A number counts as
// money only when a currency marker or a money word sits next to it, so phone
// numbers, UTRs, sizes and order numbers are ignored.
func FindAmounts(text string) []Money {
	lower := strings.ToLower(text)
	type cand struct {
		m        Money
		priority int
	}
	var cands []cand

	for _, ix := range rePrefix.FindAllStringSubmatchIndex(lower, -1) {
		v, ok := parseNum(lower[ix[4]:ix[5]], ix[6] >= 0)
		if !ok {
			continue
		}
		cands = append(cands, cand{Money{Value: v, Currency: currencyOf(lower[ix[2]:ix[3]]), Raw: lower[ix[0]:ix[1]], start: ix[0], end: ix[1], explicit: true}, 2})
	}
	for _, ix := range reSuffix.FindAllStringSubmatchIndex(lower, -1) {
		v, ok := parseNum(lower[ix[2]:ix[3]], ix[4] >= 0)
		if !ok {
			continue
		}
		cands = append(cands, cand{Money{Value: v, Currency: currencyOf(lower[ix[6]:ix[7]]), Raw: lower[ix[0]:ix[1]], start: ix[0], end: ix[1], explicit: true}, 2})
	}
	for _, ix := range reKeyword.FindAllStringSubmatchIndex(lower, -1) {
		numStr := lower[ix[4]:ix[5]]
		if digitCount(numStr) >= 9 { // phone numbers, UTRs, account numbers
			continue
		}
		end := ix[5]
		if ix[6] >= 0 {
			end = ix[7]
		}
		if reNotMoneyAfter.MatchString(lower[end:]) {
			continue
		}
		v, ok := parseNum(numStr, ix[6] >= 0)
		if !ok {
			continue
		}
		cands = append(cands, cand{Money{Value: v, Raw: lower[ix[4]:end], start: ix[4], end: end}, 1})
	}

	// Keep the strongest reading of each span.
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].priority != cands[j].priority {
			return cands[i].priority > cands[j].priority
		}
		return cands[i].m.start < cands[j].m.start
	})
	var out []Money
	for _, c := range cands {
		overlap := false
		for _, o := range out {
			if c.m.start < o.end && o.start < c.m.end {
				overlap = true
				break
			}
		}
		if !overlap {
			out = append(out, c.m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// PickAmount chooses the amount a message is most likely about: the one after
// "total" or "=", then the one after "pay"/"paid", never a per-unit price
// when a total is also present; later amounts win ties.
func PickAmount(text string) (Money, bool) {
	ms := FindAmounts(text)
	if len(ms) == 0 {
		return Money{}, false
	}
	lower := strings.ToLower(text)
	best, bestScore := -1, -1e9
	for i, m := range ms {
		score := 0.0
		before := lower[max(0, m.start-18):m.start]
		after := lower[m.end:min(len(lower), m.end+12)]
		switch {
		case reTotalBefore.MatchString(before):
			score += 3
		case rePayBefore.MatchString(before):
			score += 2
		}
		if reUnitAfter.MatchString(after) || strings.HasSuffix(strings.TrimSpace(before), "(") {
			score -= 2
		}
		if m.explicit {
			score += 0.5
		}
		if i == len(ms)-1 {
			score += 1
		}
		if score >= bestScore {
			best, bestScore = i, score
		}
	}
	return ms[best], true
}
