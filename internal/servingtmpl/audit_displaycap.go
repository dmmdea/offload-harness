// audit_displaycap.go - the display-cap-unseen note.
//
// gpu_display_cap_pct bounds how much of the display card a job may take, and the card allocator enforces it for
// the jobs it places. A model llama-swap loads onto that card is not one of them, and the yaml that pins it states no
// VRAM footprint (a llama.cpp seat's weights, KV and compute buffers are nowhere in the file; a `proxy:` seat's cards
// are set in the engine's own launcher, outside the file), so this audit CANNOT judge an entry against the cap. What
// it can do is stop passing in silence: it names, in ONE aggregated note, the entries that may sit on the card the
// driver flags as the display card, and says plainly that nothing is judged. The live verdict is `gpu status` (the
// display cap row of the card table), which needs no knowledge of how a seat is pinned.
//
// It is a note and nothing else: never a violation, never an exit code, and no config is read (the cap it states is
// the default, said to be the default). Footprint-based violations are deferred until a placement-neutral footprint
// source exists (docs/systems/setup-installer.md).
package servingtmpl

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// unseenRule is the rule id of the note.
const unseenRule = "display-cap-unseen"

// maxUnseenNames is how many entries of one group the note names before it counts the rest.
const maxUnseenNames = 6

// HasProxyEntry reports whether any model entry of a llama-swap config is a `proxy:` seat, so a caller reads the
// box's card table (an nvidia-smi run) for a file whose only claim on a card is a seat running elsewhere. A
// document that does not parse has none.
func HasProxyEntry(text string) bool {
	var doc struct {
		Models map[string]map[string]any `yaml:"models"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return false
	}
	for _, m := range doc.Models {
		if isProxyEntry(m) { // any proxy: seat
			return true
		}
	}
	return false
}

// isProxyEntry: the entry hands its requests to an engine running elsewhere (`proxy:` names it), so the cards it
// uses are pinned in that engine's own launcher and not in this file.
func isProxyEntry(m map[string]any) bool {
	p, _ := m["proxy"].(string)
	return strings.TrimSpace(p) != ""
}

// unseenCard is a display card of the reading.
type unseenCard struct {
	Index    int
	UUID     string
	Name     string
	TotalGiB float64
}

// unseenPin is an entry pinned onto a display card.
type unseenPin struct {
	Name string
	Card int // the card's nvidia-smi index
}

// unseenReport is what the audit found, before it is worded.
type unseenReport struct {
	Cards    []unseenCard // the cards the driver flags as the display card, by index
	Pinned   []unseenPin  // entries pinned (by UUID, or by index under CUDA_DEVICE_ORDER=PCI_BUS_ID) onto one of them
	Proxy    []string     // `proxy:` seats
	Unpinned []string     // entries with no CUDA_VISIBLE_DEVICES pin: every visible card, the display card included
	Unmapped []string     // entries pinned by an index that follows CUDA's own order, which names no card here
	Unknown  bool         // the reading could not say which card the monitor is on: nothing was classified
}

// unseenEntries classifies the entries of a file against a card table. ok is false when the file does not parse
// (Audit owns that rule). The display card is the driver's reading by UUID (Card.Display), never a remembered
// index, so a monitor moved to another card is followed on the next read; a box with no flagged display card
// (a single card, a headless box) has none to look at. What another rule owns is left to it: an empty value
// (cvd-empty), -1, a MIG slice, a token that is not a UUID or an index, and a UUID that names no card (A-87).
func unseenEntries(text string, cards []gpuprobe.Card) (unseenReport, bool) {
	var doc struct {
		Models map[string]map[string]any `yaml:"models"`
		Macros any                       `yaml:"macros"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return unseenReport{}, false
	}
	var r unseenReport
	if len(doc.Models) == 0 {
		return r, true
	}
	for _, c := range cards {
		if c.DisplayUnknown {
			r.Unknown = true
			return r, true
		}
	}
	for _, c := range cards {
		if c.Display {
			r.Cards = append(r.Cards, unseenCard{Index: c.NvidiaIndex, UUID: c.UUID, Name: strings.TrimSpace(c.Name), TotalGiB: c.VRAMTotalGiB})
		}
	}
	if len(r.Cards) == 0 {
		return r, true
	}
	sort.Slice(r.Cards, func(i, j int) bool { return r.Cards[i].Index < r.Cards[j].Index })

	macros := macroTable(doc.Macros)
	names := make([]string, 0, len(doc.Models))
	for name := range doc.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		m := doc.Models[name]
		if isProxyEntry(m) { // a proxy: seat, whatever its own env says
			r.Proxy = append(r.Proxy, name)
			continue
		}
		pin, found := pinOf(m, macros)
		if !found {
			r.Unpinned = append(r.Unpinned, name)
			continue
		}
		indices, uuids, _ := pinTokens(pin.value)
		var onCards []int
		unmapped := false
		for _, u := range uuids {
			if got, err := gpuprobe.ResolveCards(cards, []string{u}); err == nil && got[0].Display {
				onCards = append(onCards, got[0].NvidiaIndex)
			}
		}
		for _, ix := range indices {
			if !pin.pci {
				unmapped = true
				continue
			}
			for _, c := range cards {
				if c.NvidiaIndex == ix && c.Display {
					onCards = append(onCards, ix)
				}
			}
		}
		sort.Ints(onCards)
		for i, card := range onCards {
			if i == 0 || card != onCards[i-1] {
				r.Pinned = append(r.Pinned, unseenPin{Name: name, Card: card})
			}
		}
		if len(onCards) == 0 && unmapped {
			r.Unmapped = append(r.Unmapped, name)
		}
	}
	return r, true
}

// AuditDisplayCapUnseen is the display-cap-unseen note over a file and this box's card table: at most one
// Violation (Where names the display card of this reading), to be printed as a note and never counted. It is
// silent when the file does not parse, has no entries, no card is flagged as the display card, or no entry is
// one the audit cannot place off that card.
func AuditDisplayCapUnseen(text string, cards []gpuprobe.Card) []Violation {
	r, ok := unseenEntries(text, cards)
	if !ok {
		return nil
	}
	if r.Unknown {
		return []Violation{{Rule: unseenRule, Where: "no display card known", Text: "the card table could not say which card the monitor is on " +
			"(this reading carries no display_attached), so which entries sit on the display card was not looked at; " +
			"the live reading is the display cap row of `gpu status`"}}
	}
	if len(r.Pinned)+len(r.Proxy)+len(r.Unpinned)+len(r.Unmapped) == 0 {
		return nil
	}
	var groups []string
	if len(r.Pinned) > 0 {
		labelled := make([]string, len(r.Pinned))
		for i, p := range r.Pinned {
			labelled[i] = fmt.Sprintf("%s (card %d)", p.Name, p.Card)
		}
		groups = append(groups, "pinned onto it: "+nameList(labelled))
	}
	if len(r.Proxy) > 0 {
		groups = append(groups, "proxy: seats, whose cards are set in the engine's own launcher and not in this yaml: "+nameList(r.Proxy))
	}
	if len(r.Unpinned) > 0 {
		groups = append(groups, "no CUDA_VISIBLE_DEVICES pin, so every card including it: "+nameList(r.Unpinned))
	}
	if len(r.Unmapped) > 0 {
		groups = append(groups, "a plain index with no CUDA_DEVICE_ORDER=PCI_BUS_ID, which cannot be mapped to a card here: "+nameList(r.Unmapped))
	}
	idx := make([]string, len(r.Cards))
	desc := make([]string, len(r.Cards))
	budgets := make([]string, len(r.Cards))
	for i, c := range r.Cards {
		idx[i] = fmt.Sprint(c.Index)
		desc[i] = describeUnseenCard(c)
		budgets[i] = unseenBudget(c, len(r.Cards) > 1)
	}
	where := "card " + idx[0]
	lead := desc[0] + " is the display card on this reading"
	if len(r.Cards) > 1 {
		where = "cards " + strings.Join(idx, ", ")
		lead = strings.Join(desc, " and ") + " are display cards on this reading"
	}
	text = fmt.Sprintf("%s. The default gpu_display_cap_pct is %d (audit-yaml reads no config, so the live key may differ): %s. "+
		"The yaml states no VRAM footprint, so nothing is judged here, and entries that can run together (matrix:) are not summed. "+
		"Entries this audit cannot place off that card: %s. The live reading is the display cap row of `gpu status`",
		lead, config.DefaultGPUDisplayCapPct, strings.Join(budgets, ", "), strings.Join(groups, "; "))
	return []Violation{{Rule: unseenRule, Where: where, Text: text}}
}

// nameList names at most maxUnseenNames entries and counts the rest.
func nameList(names []string) string {
	if len(names) <= maxUnseenNames {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(names[:maxUnseenNames], ", "), len(names)-maxUnseenNames)
}

// describeUnseenCard is "card 1 (name, GPU-...tail, 15.9 GiB)": the index and the UUID's tail of THIS reading.
func describeUnseenCard(c unseenCard) string {
	parts := []string{}
	if c.Name != "" {
		parts = append(parts, c.Name)
	}
	if tail := uuidTail(c.UUID); tail != "" {
		parts = append(parts, "GPU-..."+tail)
	}
	if c.TotalGiB > 0 {
		parts = append(parts, fmt.Sprintf("%.1f GiB", c.TotalGiB))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("card %d", c.Index)
	}
	return fmt.Sprintf("card %d (%s)", c.Index, strings.Join(parts, ", "))
}

func uuidTail(uuid string) string {
	uuid = strings.TrimSpace(uuid)
	if len(uuid) <= 8 {
		return ""
	}
	return uuid[len(uuid)-8:]
}

// unseenBudget words what the default cap leaves of a card, by the allocator's own arithmetic
// (gpulease.DisplayBudgetGiB), or says the total was not read and no budget is stated.
func unseenBudget(c unseenCard, many bool) string {
	prefix := ""
	if many {
		prefix = fmt.Sprintf("card %d: ", c.Index)
	}
	budget, ok := gpulease.DisplayBudgetGiB(c.TotalGiB, config.DefaultGPUDisplayCapPct, 0)
	if !ok {
		return prefix + "its total VRAM was not read, so no budget is stated"
	}
	return prefix + fmt.Sprintf("a job may fill %.1f GiB of its %.1f GiB", budget, c.TotalGiB)
}
