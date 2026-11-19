package gpuprobe

// Card references: the settings that name a card by UUID, held against the cards the driver lists.
//
// A card is named by UUID in several places (the ComfyUI order, the headline card, a layer's pins, a llama-swap
// seat's CUDA_VISIBLE_DEVICES). When the card is replaced or falls off the bus the UUID stays in the file and
// matches nothing, and every reader degrades on its own and says little: the ComfyUI order is rejected with one
// note line, and a seat whose only pin is the missing UUID sees no CUDA device and loads on the CPU, with nothing
// to say so. CheckRefs is the one answer the surfaces share (doctor, `gpu status`, the offload_status brief):
// which references name no live card, and which live cards nothing names.
//
// Unknown is never absent: a card table that could not be read judges nothing, and the report says so.

import (
	"fmt"
	"strings"
)

// The states of a RefReport.
const (
	// RefsNone: nothing in the configuration names a card by UUID, so there is nothing to compare.
	RefsNone = "none"
	// RefsOK: every card named by UUID is a live card.
	RefsOK = "ok"
	// RefsStale: at least one reference names no live card (Stale says which).
	RefsStale = "stale"
	// RefsUnknown: references exist and the card table could not be read, so none was judged.
	RefsUnknown = "unknown"
)

// Ref is one place the configuration names a card by UUID.
type Ref struct {
	// Setting is the setting that holds Key, as the operator writes it (`gpu_comfy_order`,
	// `layers[display].display_device`, the serving config's seat pins).
	Setting string `json:"setting"`
	// Key is the UUID, or UUID prefix, as written.
	Key string `json:"key"`
	// Seats are the llama-swap models whose CUDA_VISIBLE_DEVICES holds Key (serving config references only).
	Seats []string `json:"seats,omitempty"`
	// Effect is what the setting does while Key names no live card.
	Effect string `json:"effect"`
	// Exact: the reader compares the whole UUID (primary_gpu_uuid) and a prefix does not name a card. Every other
	// reader (CUDA, ResolveCards) takes a prefix.
	Exact bool `json:"-"`
}

// names reports whether the reference names this card.
func (r Ref) names(c Card) bool {
	u, k := strings.ToLower(strings.TrimSpace(c.UUID)), strings.ToLower(strings.TrimSpace(r.Key))
	if r.Exact {
		return u == k
	}
	return strings.HasPrefix(u, "gpu-"+strings.TrimPrefix(k, "gpu-")) || strings.HasPrefix(u, k)
}

// NamedCard is a live card in a report: the fields an operator needs to pick it out of `nvidia-smi -L`.
type NamedCard struct {
	UUID  string `json:"uuid"`
	Index int    `json:"index"`
	Name  string `json:"name"`
}

// RefReport is the verdict of CheckRefs. The JSON keys are the stable surface an agent branches on: state first.
type RefReport struct {
	State string `json:"state"`
	// Why says why the state is unknown (the card table could not be read).
	Why string `json:"why,omitempty"`
	// Stale are the references that name no live card.
	Stale []Ref `json:"stale,omitempty"`
	// Unnamed are the live cards the settings that hold a stale UUID do not name: the candidates for the card that
	// replaced it (a card another setting still pins is a candidate for a setting that does not). Set only when
	// something is stale.
	Unnamed []NamedCard `json:"unnamed_cards,omitempty"`
	// Fix is the lever: what to change and what to measure before trusting it (Remedy). Set only when something is stale.
	Fix string `json:"fix,omitempty"`
	// Unchecked are the places that could have named a card and could not be read (a serving config that is
	// set and unreadable): not judged, and not called clean.
	Unchecked []string `json:"unchecked,omitempty"`
}

// CheckRefs holds the references against the card table. cardsErr is the card table's read error (nil when it was
// read); a table that is empty counts as unread. Nothing is called stale without a table.
func CheckRefs(cards []Card, cardsErr error, refs []Ref, unchecked []string) RefReport {
	rep := RefReport{State: RefsNone, Unchecked: unchecked}
	if len(refs) == 0 {
		return rep
	}
	if cardsErr != nil || len(cards) == 0 {
		rep.State, rep.Why = RefsUnknown, "no card table was read"
		if cardsErr != nil {
			rep.Why = cardsErr.Error()
		}
		return rep
	}
	hits := make([][]int, len(refs)) // the cards each reference names, by nvidia-smi index
	affected := map[string]bool{}    // the settings that hold a stale UUID
	for i, r := range refs {
		for _, c := range cards {
			if r.names(c) {
				hits[i] = append(hits[i], c.NvidiaIndex)
			}
		}
		if len(hits[i]) == 0 {
			rep.Stale = append(rep.Stale, r)
			affected[r.Setting] = true
		}
	}
	if len(rep.Stale) == 0 {
		rep.State = RefsOK
		return rep
	}
	rep.State = RefsStale
	named := map[int]bool{}
	for i, r := range refs {
		if affected[r.Setting] {
			for _, ix := range hits[i] {
				named[ix] = true
			}
		}
	}
	for _, c := range cards {
		if !named[c.NvidiaIndex] {
			rep.Unnamed = append(rep.Unnamed, NamedCard{UUID: c.UUID, Index: c.NvidiaIndex, Name: c.Name})
		}
	}
	rep.Fix = rep.Remedy()
	return rep
}

// StaleKeys is the distinct stale UUIDs, in the order they were found.
func (r RefReport) StaleKeys() []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range r.Stale {
		if k := strings.ToLower(strings.TrimSpace(s.Key)); !seen[k] {
			seen[k] = true
			out = append(out, s.Key)
		}
	}
	return out
}

// Remedy is the lever, in one sentence an operator or an agent can act on. It names the replacement candidates
// (and, when one stale UUID meets one unnamed card, the likely pair) and says what to measure before a new
// gpu_comfy_order is trusted. It is empty unless something is stale.
func (r RefReport) Remedy() string {
	if r.State != RefsStale {
		return ""
	}
	var b strings.Builder
	b.WriteString("replace each stale UUID, in the setting on its row, with the UUID of the card that took its place (`nvidia-smi -L` lists the live ones) and run `local-offload doctor` again; a card that fell off the bus is restored, not re-pinned")
	if keys := r.StaleKeys(); len(keys) == 1 && len(r.Unnamed) == 1 {
		fmt.Fprintf(&b, "; one stale UUID and one unnamed card: most likely %s replaced %s", r.Unnamed[0].UUID, keys[0])
	}
	for _, s := range r.Stale {
		if s.Setting == "gpu_comfy_order" {
			b.WriteString("; gpu_comfy_order is ComfyUI's device order, fastest card first, and nvidia-smi cannot order two cards of one model, so measure the order of the new list with a CUDA_VISIBLE_DEVICES=<uuid> probe before trusting it")
			break
		}
	}
	return b.String()
}
