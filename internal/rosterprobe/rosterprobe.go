// Package rosterprobe is how the single-shot fleet lanes — vision, text, stt upload,
// compose and the accelerator forwarder — read the fleet roster (config
// delegate_remotes) before they pick a node for ONE call.
//
// Rules that live here so that five lanes cannot drift apart on them (ADR 0074):
//
//   - ADMISSION. A roster entry is judged by the same tailnet shape check the agent lane
//     applies (netguard.TailnetURL) before it is dialled. The lanes used to rely on the
//     dial gate alone, so one entry was refused by one lane and used by five, and a bad
//     entry got no message naming the key. A refused entry is a named miss, never a
//     failed call: the other entries still serve.
//   - READING. The roster is probed CONCURRENTLY, in the order it is configured, with a
//     short process-wide memo of good answers and a negative cache of transport failures,
//     so k dead nodes cost one probe bound, once per cache window, instead of k bounds in
//     series on the critical path of every call (see Probe and its timings).
//
// The agent lane keeps its own admission and reading inside internal/delegate (per Run, at
// its own bound); this package is the single-shot lanes' and the doctor's.
package rosterprobe

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dmmdea/offload-harness/internal/netguard"
)

// Member is one delegate_remotes entry as a lane sees it: normalized, and judged.
type Member struct {
	// Index is the entry's position in the configured list, blanks included, so a lane that
	// breaks ties by "config order" and a message that names an entry both mean the same slot.
	Index int
	// Base is the entry trimmed of whitespace and trailing slashes.
	Base string
	// Refused is why the tailnet guard will not admit the entry (a *Refusal); nil when it
	// admits it. A refused member is never dialled.
	Refused error
}

// Refusal is the tailnet guard's verdict on a roster entry, as an error a caller can tell from a
// failed dial: errors.As(err, new(*Refusal)). Reason is the guard's own message, which names the
// entry only in its redacted form (netguard.TailnetURLIn), so a Refusal is safe to print.
type Refusal struct{ Reason error }

func (r *Refusal) Error() string { return fmt.Sprintf("refused by the tailnet guard (%v)", r.Reason) }

// Unwrap exposes the guard's message.
func (r *Refusal) Unwrap() error { return r.Reason }

// IsRefusal reports whether err is a Refusal: the entry was judged and never dialled.
func IsRefusal(err error) bool {
	var r *Refusal
	return errors.As(err, &r)
}

// Members normalizes and vets the roster. A blank entry is an unset slot (the key's own
// loader exempts it) and is left out; every other entry comes back in configured order, the
// refused ones flagged rather than dropped, so a lane can name each in its "probed ..." line.
func Members(remotes []string) []Member {
	var out []Member
	for i, raw := range remotes {
		base := strings.TrimRight(strings.TrimSpace(raw), "/")
		if base == "" {
			continue
		}
		m := Member{Index: i, Base: base}
		if err := netguard.TailnetURL(base); err != nil {
			m.Refused = &Refusal{Reason: err}
		}
		out = append(out, m)
	}
	return out
}

// Miss words a refused member the way a lane words every other reason it passed a node over:
// the base first, then the cause. It names what was NOT done, so "no node is eligible" never
// reads as though the entry had been tried. The base is printed redacted (Shown): an entry may
// have been pasted with a token in it, and these lines reach chats.
func (m Member) Miss() string {
	return fmt.Sprintf("%s: not dialled, %s", m.Shown(), Scrub(m.Base, m.Refused))
}
