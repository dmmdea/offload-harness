package gpuprobe

// The card table: one row per GPU, keyed by the card's UUID (plan invariant I2).
//
// Three index spaces disagree on a mixed box: nvidia-smi enumerates by PCI bus,
// CUDA's default (CUDA_DEVICE_ORDER=FASTEST_FIRST) enumerates fastest first and so does
// ComfyUI's `--cuda-device`, and CUDA_VISIBLE_DEVICES follows whichever the process was
// started with. A lease therefore names a card by its UUID, and an index appears only at
// the process boundary where something hands us one (a flag, an env var).
//
// nvidia-smi reports no FASTEST_FIRST position, and two same-model cards cannot be
// ordered by anything it does report, so Card.ComfyOrder is KNOWN only when the operator
// declares the order (config gpu_comfy_order) or the box has a single card. Everything
// that translates a ComfyUI index to a card refuses on an unknown order and names the fix.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Card is one GPU as the card table sees it.
type Card struct {
	// UUID is the card's nvidia-smi UUID as reported ("GPU-..."). LeaseID is the form a
	// lease records.
	UUID         string  `json:"uuid"`
	NvidiaIndex  int     `json:"index"`
	Name         string  `json:"name"`
	Display      bool    `json:"display"`
	VRAMTotalGiB float64 `json:"vram_total_gb"`
	VRAMFreeGiB  float64 `json:"vram_free_gb"`
	UtilPct      int     `json:"util_pct"`
	UtilKnown    bool    `json:"util_known"`
	// ComfyOrder is the card's position in CUDA's FASTEST_FIRST order (what ComfyUI's
	// --cuda-device counts), or -1 when it is not known. See the file comment.
	ComfyOrder int `json:"comfy_order"`
	// DisplayUnknown marks a card of a reading that could not say which card the monitor is
	// on (Device.AttachedUnknown). The allocator does not auto-assign such a card: "unknown" is
	// never "not the operator's screen".
	DisplayUnknown bool `json:"display_unknown,omitempty"`
}

// LeaseID is the form a lease records for this card: the UUID lower-cased (a claim is
// one file per card, and the file system may be case-insensitive).
func (c Card) LeaseID() string { return strings.ToLower(strings.TrimSpace(c.UUID)) }

// BuildCards turns parsed nvidia-smi devices into the card table. Display is the placement
// rule, ScreenCardUUIDs (a box whose only card is its display card excludes nothing).
// comfyOrder is the operator's declaration of ComfyUI's order, a comma list of nvidia
// indices or UUID prefixes with the fastest card first ("" = not declared). A spec that
// does not name every card exactly once is rejected as a whole: warn says why and every
// ComfyOrder stays unknown, because half an order answers no question.
func BuildCards(devs []Device, comfyOrder string) (cards []Card, warn string) {
	display := ScreenCardUUIDs(devs)
	cards = make([]Card, 0, len(devs))
	for _, d := range devs {
		cards = append(cards, Card{
			UUID: d.UUID, NvidiaIndex: d.Index, Name: d.Name,
			Display:        display[d.UUID],
			DisplayUnknown: d.AttachedUnknown,
			VRAMTotalGiB:   d.TotalGiB, VRAMFreeGiB: d.FreeGiB,
			UtilPct: d.UtilPct, UtilKnown: d.UtilKnown,
			ComfyOrder: -1,
		})
	}
	if len(cards) == 1 {
		cards[0].ComfyOrder = 0 // one card has one possible position
	}
	spec := strings.TrimSpace(comfyOrder)
	if spec == "" || len(cards) == 1 {
		return cards, ""
	}
	var keys []string
	for _, k := range strings.Split(spec, ",") {
		keys = append(keys, strings.TrimSpace(k))
	}
	ordered, err := ResolveCards(cards, keys)
	if err != nil {
		return cards, fmt.Sprintf("gpu_comfy_order %q ignored: %v", spec, err)
	}
	if len(ordered) != len(cards) {
		return cards, fmt.Sprintf("gpu_comfy_order %q ignored: it names %d of the %d cards (it must name every card once, fastest first)", spec, len(ordered), len(cards))
	}
	pos := map[int]int{}
	for i, c := range ordered {
		pos[c.NvidiaIndex] = i
	}
	for i := range cards {
		cards[i].ComfyOrder = pos[cards[i].NvidiaIndex]
	}
	return cards, ""
}

// ReadCards runs the nvidia-smi per-device query and builds the card table.
func ReadCards(ctx context.Context, comfyOrder string) ([]Card, string, error) {
	devs, err := Read(ctx)
	if err != nil {
		return nil, "", err
	}
	cards, warn := BuildCards(devs, comfyOrder)
	return cards, warn, nil
}

// ResolveCards maps keys to cards, in key order. A key is a nvidia-smi index (all
// digits) or a UUID prefix (at least four characters after an optional "GPU-"), compared
// case-insensitively. A key that matches no card, matches several, or names a card a
// second time is an error: a lease that silently landed on the wrong card is the defect
// this table exists to prevent.
func ResolveCards(cards []Card, keys []string) ([]Card, error) {
	var out []Card
	seen := map[int]string{}
	for _, raw := range keys {
		key := strings.TrimSpace(raw)
		if key == "" {
			return nil, fmt.Errorf("a card key is blank")
		}
		c, err := resolveOne(cards, key)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[c.NvidiaIndex]; dup {
			return nil, fmt.Errorf("card %d is named twice (%q and %q)", c.NvidiaIndex, prev, key)
		}
		seen[c.NvidiaIndex] = key
		out = append(out, c)
	}
	return out, nil
}

func resolveOne(cards []Card, key string) (Card, error) {
	if idx, err := strconv.Atoi(key); err == nil {
		for _, c := range cards {
			if c.NvidiaIndex == idx {
				return c, nil
			}
		}
		return Card{}, fmt.Errorf("no card has nvidia-smi index %d", idx)
	}
	lk := strings.ToLower(key)
	bare := strings.TrimPrefix(lk, "gpu-")
	if len(bare) < 4 {
		return Card{}, fmt.Errorf("card key %q is too short to be a UUID prefix (give an nvidia-smi index or at least four characters of the UUID)", key)
	}
	var hits []Card
	for _, c := range cards {
		u := strings.ToLower(c.UUID)
		if strings.HasPrefix(u, "gpu-"+bare) || strings.HasPrefix(u, lk) {
			hits = append(hits, c)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return Card{}, fmt.Errorf("no card matches %q", key)
	default:
		return Card{}, fmt.Errorf("card key %q is ambiguous: it matches %d cards", key, len(hits))
	}
}

// CardByComfyOrder finds the card at a ComfyUI/FASTEST_FIRST position. ok is false when
// no card carries that position, which includes every card when the order is unknown.
func CardByComfyOrder(cards []Card, n int) (Card, bool) {
	if n < 0 {
		return Card{}, false
	}
	for _, c := range cards {
		if c.ComfyOrder == n {
			return c, true
		}
	}
	return Card{}, false
}
