package gpulease

// What a lease holds, as a consumer reads it (plan P4, register C-86).
//
// WHY. A card-scoped lease names its cards (record v2), but every consumer that gates a
// SEAT (the model-affinity load gate, the delegator's busy formula, the placement table)
// asked only "is a lease held" and so fenced the whole box for a render on one card. This
// file is the one place that turns a lease and a seat's cards into the answer, and every
// consumer calls it, so no site can restate the rule differently.
//
// THE RULE. A lease touches a card set when it is whole-node, or its EFFECTIVE devices
// intersect the set. Effective devices are, in order: the record's declared devices; for a
// legacy whole-node record the set the evidence rule inferred (infer.go), if it found one;
// otherwise none, which means every card. An UNKNOWN card set (a seat nobody declared a pin
// for, a pin the card table cannot resolve, no card table at all) is every card, so the
// answer for it is today's: any live lease touches it. Nothing here ever narrows on a
// guess; the direction of every doubt is "fence".

import (
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// ScopeSource says where a lease's effective card set came from.
type ScopeSource string

const (
	// ScopeDeclared: the record names its cards (record v2).
	ScopeDeclared ScopeSource = "declared"
	// ScopeInferred: a legacy whole-node record scoped by the evidence rule.
	ScopeInferred ScopeSource = "inferred"
	// ScopeWholeNode: no declared cards and no evidence: every card.
	ScopeWholeNode ScopeSource = "whole-node"
)

// EffectiveDevices is the card set the lease sits on, as lease ids: the declared devices,
// else the inferred ones, else nil (the whole node).
func (i Info) EffectiveDevices() []string {
	if len(i.Devices) > 0 {
		return i.Devices
	}
	return i.Inferred
}

// ScopeKind names where EffectiveDevices came from, spelling out the zero value.
func (i Info) ScopeKind() ScopeSource {
	switch {
	case len(i.Devices) > 0:
		return ScopeDeclared
	case len(i.Inferred) > 0:
		return ScopeInferred
	default:
		return ScopeWholeNode
	}
}

// Touches reports whether this lease sits on any card in ids (lease ids, lower-cased
// UUIDs). An empty ids is an unknown card set, which is every card; a lease that holds
// nothing touches nothing.
func (i Info) Touches(ids []string) bool {
	if !i.Held {
		return false
	}
	if len(ids) == 0 {
		return true
	}
	eff := i.EffectiveDevices()
	if len(eff) == 0 {
		return true
	}
	return devicesIntersect(eff, ids)
}

// devicesIntersect reports whether the two id lists share a card. Neither list is empty
// here: an empty list is the whole node and is handled by the callers.
func devicesIntersect(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if strings.EqualFold(x, y) {
				return true
			}
		}
	}
	return false
}

// For narrows an inspection to the live leases that touch ids: a consumer that gates one
// seat reads the leases that matter to that seat and nothing else, then asks the same
// predicates (Held, Class, Exclusive, Draining, Reserved, BlocksNewRun, Fenced) it always
// asked. An empty ids is an unknown card set and returns i unchanged. The result keeps the
// whole-node-era shape: one lease is the Info itself, several are the lowest epoch with
// Leases carrying each.
func (i Info) For(ids []string) Info {
	if len(ids) == 0 || !i.Held {
		return i
	}
	var keep []Info
	for _, l := range i.Each() {
		if l.Touches(ids) {
			keep = append(keep, l)
		}
	}
	switch len(keep) {
	case 0:
		return Info{}
	case 1:
		out := keep[0]
		out.Leases = nil
		out.Epochs = []uint64{out.Epoch}
		return out
	}
	sort.SliceStable(keep, func(a, b int) bool { return keep[a].Epoch < keep[b].Epoch })
	out := keep[0]
	out.Leases = keep
	out.Epochs = make([]uint64, len(keep))
	for n, l := range keep {
		out.Epochs[n] = l.Epoch
	}
	return out
}

// ResolvePins turns a seat's device pins into lease ids. A seat is launched with
// CUDA_VISIBLE_DEVICES under CUDA_DEVICE_ORDER=PCI_BUS_ID (the tier's gpu_env), so a bare
// pin is an nvidia-smi index; a pin that is a UUID prefix (a display device pinned across
// board reorders) resolves the same way. ok is false for no pins, no card table, or any pin
// the table cannot place (unknown index, ambiguous prefix, a card named twice, a blank
// entry): the caller reads that as UNKNOWN, which is every card.
func ResolvePins(pins []string, cards []gpuprobe.Card) ([]string, bool) {
	if len(pins) == 0 || len(cards) == 0 {
		return nil, false
	}
	got, err := gpuprobe.ResolveCards(cards, pins)
	if err != nil || len(got) != len(pins) {
		return nil, false
	}
	ids := make([]string, 0, len(got))
	for _, c := range got {
		ids = append(ids, c.LeaseID())
	}
	return ids, true
}

// WithLegacyNote reads every held legacy whole-node lease in i as the whole node and says why in
// ScopeWhy: the evidence rule was not run (it is off on this host), so nothing was inferred. A
// lease that declares its cards, or one a binary that could name cards wrote, is left as it is.
func (i Info) WithLegacyNote(why string) Info {
	if len(i.Leases) == 0 {
		if i.Held && i.Legacy && len(i.Devices) == 0 {
			i.Scope, i.ScopeWhy = ScopeWholeNode, why
		}
		return i
	}
	leases := make([]Info, len(i.Leases))
	for n, l := range i.Leases {
		leases[n] = l.WithLegacyNote(why)
	}
	out := leases[0]
	out.Epochs = i.Epochs
	out.Leases = leases
	return out
}
