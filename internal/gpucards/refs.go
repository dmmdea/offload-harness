package gpucards

import (
	"fmt"
	"strings"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// The text of a gpuprobe.RefReport, one definition for the surfaces that print it (doctor, `gpu status`, `gpu cards`).
// The JSON surfaces (`gpu status --json`, the offload_status brief) carry the report itself under "card_refs".

// maxSeatsShown is how many seat names a stale reference lists before it says how many more there are.
const maxSeatsShown = 6

// StaleText is the sentence for one stale reference: the setting, the UUID, and what the setting does meanwhile.
func StaleText(r gpuprobe.Ref) string {
	subject := r.Setting
	if len(r.Seats) > 0 {
		shown := r.Seats
		more := ""
		if len(shown) > maxSeatsShown {
			more = fmt.Sprintf(", and %d more", len(shown)-maxSeatsShown)
			shown = shown[:maxSeatsShown]
		}
		subject = fmt.Sprintf("%s (%d seats: %s%s)", r.Setting, len(r.Seats), strings.Join(shown, ", "), more)
	}
	return fmt.Sprintf("%s names %s, which no live card has: %s", subject, r.Key, r.Effect)
}

// CandidatesText names the live cards the settings that hold a stale UUID do not name, the candidates for the card that
// replaced it. Empty when those settings name every live card.
func CandidatesText(r gpuprobe.RefReport) string {
	if len(r.Unnamed) == 0 {
		return ""
	}
	parts := make([]string, len(r.Unnamed))
	for i, c := range r.Unnamed {
		parts[i] = fmt.Sprintf("idx %d %s %s", c.Index, c.Name, c.UUID)
	}
	return "live cards the stale settings do not name (replacement candidates): " + strings.Join(parts, "; ")
}

// UnknownText says a report is unknown: references exist and the card table could not be read, so nothing was
// judged (an unread table is never read as "the card is gone").
func UnknownText(r gpuprobe.RefReport) string {
	return fmt.Sprintf("the settings that name a card by UUID could not be checked against the live cards (%s); none is called stale", r.Why)
}

// RefLines is the report as terminal lines for `gpu status` and `gpu cards`; empty when there is nothing to say
// (nothing names a card, or every reference resolves).
func RefLines(r gpuprobe.RefReport) []string {
	var out []string
	switch r.State {
	case gpuprobe.RefsStale:
		out = append(out, fmt.Sprintf("DEFECT: %d configured card UUID(s) match no live card (the card was replaced or is gone)", len(r.Stale)))
		for _, s := range r.Stale {
			out = append(out, "  "+StaleText(s))
		}
		if c := CandidatesText(r); c != "" {
			out = append(out, "  "+c)
		}
		out = append(out, "  fix: "+r.Fix)
	case gpuprobe.RefsUnknown:
		out = append(out, "card references: UNKNOWN, "+UnknownText(r))
	}
	for _, u := range r.Unchecked {
		out = append(out, "card references: UNKNOWN, not checked: "+u)
	}
	return out
}
