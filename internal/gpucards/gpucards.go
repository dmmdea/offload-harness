// Package gpucards is the per-card view of the GPU lease: the card table joined with the
// live leases, one row per card, the per-lease device sets and the queue. `gpu status`,
// `gpu cards` and offload_status render the SAME rows, so the surfaces a session reads
// cannot disagree about which card is held by whom (the same rule as gpulease.QueueHint).
//
// Cards are keyed by UUID (plan invariant I2). A whole-node lease holds every card and
// the row says so. JSON keys are only ever added to.
package gpucards

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// Holder is the lease that holds a card.
type Holder struct {
	Epoch          uint64 `json:"epoch"`
	Class          string `json:"class"`
	PID            int    `json:"pid"`
	Scope          string `json:"scope"` // "card" or "whole-node"
	Group          string `json:"group,omitempty"`
	Command        string `json:"command,omitempty"`
	WrapperVersion string `json:"wrapper_version,omitempty"`
}

// Row is one line of the card table.
type Row struct {
	UUID         string  `json:"uuid"`
	Index        int     `json:"index"`
	Name         string  `json:"name"`
	Display      bool    `json:"display"`
	VRAMTotalGiB float64 `json:"vram_total_gb"`
	VRAMFreeGiB  float64 `json:"vram_free_gb"`
	UtilPct      int     `json:"util_pct"`
	UtilKnown    bool    `json:"util_known"`
	ComfyOrder   int     `json:"comfy_order"` // -1 = not declared
	State        string  `json:"state"`       // "free" or "held"
	Holder       *Holder `json:"holder"`
	// DisplayUnknown: the reading could not say which card the monitor is on (gpuprobe.Card.DisplayUnknown).
	DisplayUnknown bool `json:"display_unknown,omitempty"`
	// DisplayCap is the live verdict of gpu_display_cap_pct on this card; see DisplayCap.
	DisplayCap *DisplayCap `json:"display_cap,omitempty"`
}

// DisplayCap is the live reading of the display card against gpu_display_cap_pct: the VRAM in use now (the
// desktop and every process counted, which is how the allocator counts it) against the share of the card the cap
// allows. It is the verdict the allocator would reach on an empty request, taken from the card as it is, so it
// covers memory the allocator never placed (a seat llama-swap loaded onto the card). It cannot say whose memory it
// is. The two states that are not a verdict say why: Closed (the cap is 0, the operator's "I need the card"
// switch) and Unknown (a reading that could not say which card the monitor is on, or a card with no total).
// OverByGiB is present exactly when the card is over, to four decimals of a GiB and never rounded to nothing.
type DisplayCap struct {
	CapPct    int     `json:"cap_pct"`
	BudgetGiB float64 `json:"budget_gb"`
	UsedGiB   float64 `json:"used_gb"`
	UsedPct   float64 `json:"used_pct"`
	OverCap   bool    `json:"over_cap"`
	OverByGiB float64 `json:"over_by_gb,omitempty"`
	Closed    bool    `json:"closed,omitempty"`
	Unknown   string  `json:"unknown,omitempty"`
}

// RowsWithCap joins the card table with the live leases and the display card's cap. A card-scoped lease holds
// the cards it names; a whole-node lease holds every card, and says so. The display card's row carries its cap
// verdict (DisplayCap), computed here and nowhere else: capPct and floorGiB are the allocator's own inputs
// (gpu_display_cap_pct as the config file says now, and the display layer's declared floor), and this is the only
// constructor of rows, so no surface can print the table without them.
func RowsWithCap(cards []gpuprobe.Card, leases []gpulease.Info, capPct int, floorGiB float64) []Row {
	rows := make([]Row, 0, len(cards))
	for _, c := range cards {
		r := Row{
			UUID: c.UUID, Index: c.NvidiaIndex, Name: c.Name, Display: c.Display, DisplayUnknown: c.DisplayUnknown,
			VRAMTotalGiB: c.VRAMTotalGiB, VRAMFreeGiB: c.VRAMFreeGiB,
			UtilPct: c.UtilPct, UtilKnown: c.UtilKnown, ComfyOrder: c.ComfyOrder, State: "free",
		}
		if l, scope, ok := holderOf(c, leases); ok {
			r.State = "held"
			r.Holder = &Holder{Epoch: l.Epoch, Class: string(l.Class), PID: l.PID, Scope: scope,
				Group: l.Group, Command: l.Command, WrapperVersion: l.WrapperVersion}
		}
		r.DisplayCap = displayCapOf(r, capPct, floorGiB)
		rows = append(rows, r)
	}
	return rows
}

// The two reasons a display card is not judged, in the words every surface prints.
const (
	unknownTotal   = "total VRAM not given"
	unknownDisplay = "the reading could not say which card the monitor is on"
)

// capEpsilonGiB absorbs the float error of a share of a total, as the allocator's own comparison does
// (gpulease's capEpsilonGiB): a card that sits exactly on its budget is within it.
const capEpsilonGiB = 1e-6

// minOverByGiB is the resolution of over_by_gb (four decimals of a GiB). The verdict is made on the unrounded values,
// as the allocator makes it, and nvidia-smi reads whole MiB, so a card a few MiB past its budget is a real reading;
// the amount is never rounded down to nothing for a card that is over.
const minOverByGiB = 1e-4

// smallOverGiB is where the text line stops printing the excess in tenths of a GiB: "OVER by 0.0 GiB" is never printed.
const smallOverGiB = 0.05

// displayCapOf is the cap verdict of one row, or nil for a row it says nothing about. A row of a reading that
// could not say which card the monitor is on gets the unknown, whatever display_active says: the allocator closes
// such a card before it asks the cap, and so does this. A card that is not the display card has no cap.
func displayCapOf(r Row, capPct int, floorGiB float64) *DisplayCap {
	if r.DisplayUnknown {
		return &DisplayCap{CapPct: capPct, Unknown: unknownDisplay}
	}
	if !r.Display {
		return nil
	}
	d := &DisplayCap{CapPct: capPct}
	if r.VRAMTotalGiB <= 0 {
		d.Unknown = unknownTotal
		return d
	}
	used := math.Max(r.VRAMTotalGiB-r.VRAMFreeGiB, 0)
	d.UsedGiB, d.UsedPct = round2(used), round1(used/r.VRAMTotalGiB*100)
	if capPct <= 0 {
		d.Closed = true
		return d
	}
	budget, ok := gpulease.DisplayBudgetGiB(r.VRAMTotalGiB, capPct, floorGiB)
	if !ok {
		d.Unknown = unknownTotal
		return d
	}
	d.BudgetGiB = round2(budget)
	if used > budget+capEpsilonGiB {
		d.OverCap, d.OverByGiB = true, math.Max(round4(used-budget), minOverByGiB)
	}
	return d
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

func round4(x float64) float64 { return math.Round(x*10000) / 10000 }

// holderOf finds the lease holding a card: a card-scoped lease that names it wins; a
// whole-node lease holds it otherwise.
func holderOf(c gpuprobe.Card, leases []gpulease.Info) (gpulease.Info, string, bool) {
	id := c.LeaseID()
	var whole *gpulease.Info
	for i := range leases {
		l := leases[i]
		if len(l.Devices) == 0 {
			if whole == nil {
				whole = &leases[i]
			}
			continue
		}
		for _, d := range l.Devices {
			if d == id {
				return l, "card", true
			}
		}
	}
	if whole != nil {
		return *whole, "whole-node", true
	}
	return gpulease.Info{}, "", false
}

// LeaseRows is the per-lease list: every live lease with its device set.
func LeaseRows(leases []gpulease.Info) []map[string]any {
	out := make([]map[string]any, 0, len(leases))
	for _, l := range leases {
		scope := "whole-node"
		if len(l.Devices) > 0 {
			scope = "card"
		}
		row := map[string]any{
			"epoch": l.Epoch, "class": string(l.Class), "pid": l.PID, "scope": scope,
			"age_s": int(l.Age.Seconds()), "reason": l.Reason, "origin": l.Origin,
			"exclusive": l.Exclusive, "draining": l.Draining,
		}
		if len(l.Devices) > 0 {
			row["devices"] = l.Devices
		}
		// The scope the SEAT gates read (plan P4): declared, inferred from evidence (a legacy
		// whole-node record, which stays a whole-node claim in `scope`), or the whole node.
		// scope_why is the evidence or the exact reason a legacy lease stayed whole-node.
		row["seat_scope"] = string(l.ScopeKind())
		if len(l.Inferred) > 0 && len(l.Devices) == 0 {
			row["inferred_devices"] = l.Inferred
		}
		if l.ScopeWidened {
			row["scope_widened"] = true
		}
		if l.ScopeWhy != "" {
			row["scope_why"] = l.ScopeWhy
		}
		if l.Group != "" {
			row["group"] = l.Group
		}
		if l.Command != "" {
			row["command"] = l.Command
		}
		if l.WrapperVersion != "" {
			row["wrapper_version"] = l.WrapperVersion
		}
		if !l.ExpiresAt.IsZero() {
			row["expires_at"] = l.ExpiresAt.Format(time.RFC3339)
		}
		// The host RAM the lease declared, which the next grant counts as still to load until its
		// processes hold it (internal/gpulease/hostram.go).
		if l.HostRAMGiB > 0 {
			row["host_ram_gib"] = round1(l.HostRAMGiB)
		}
		// Host RAM a lease that clears the seat asked for and the host has not admitted yet (F82): it declares nothing
		// until then, so the next grant counts nothing for it; the figure is what an operator reads to see what it waits for.
		if l.HostRAMWaitGiB > 0 {
			row["host_ram_wait_gib"] = round1(l.HostRAMWaitGiB)
		}
		// When the hold bound releases a lease that waits for the host with its cards held (F82): the outer bound a detached reserve
		// stamps before its drain, then the wait's own end; present until the host admits the figure.
		if !l.HostRAMHoldUntil.IsZero() {
			row["host_ram_hold_until"] = l.HostRAMHoldUntil.Format(time.RFC3339)
			if l.HostRAMHoldPhase != "" {
				// outer (a detached reserve still clearing the seat: the host has not been asked), wait, or release.
				row["host_ram_hold_phase"] = l.HostRAMHoldPhase
			}
		}
		// A lease whose hidden holder gave up releasing it: nothing polls it any more.
		if !l.HolderGaveUp.IsZero() {
			row["holder_gave_up"] = l.HolderGaveUp.Format(time.RFC3339)
			if l.HolderGaveUpWhy != "" {
				row["holder_gave_up_why"] = l.HolderGaveUpWhy
			}
		}
		out = append(out, row)
	}
	return out
}

// QueueRows is the queue with each entry's device set. The four keys status always
// had keep their types; devices and scope are new.
func QueueRows(waiters []gpulease.Waiter) []map[string]any {
	out := make([]map[string]any, 0, len(waiters))
	for _, w := range waiters {
		row := map[string]any{"pid": w.PID, "class": w.Class, "reason": w.Reason, "since": w.Since().Format(time.RFC3339)}
		if len(w.Devices) > 0 {
			row["devices"] = w.Devices
			row["scope"] = "card"
		} else {
			row["scope"] = "whole-node"
		}
		// A waiter the cards would admit and the host's memory does not says what it waits for.
		if w.WaitingFor != "" {
			row["waiting_for"] = w.WaitingFor
		}
		if w.HostRAMGiB > 0 {
			row["host_ram_gib"] = round1(w.HostRAMGiB)
		}
		out = append(out, row)
	}
	return out
}

// Section is what `gpu status --json` adds for cards: the table (empty, with a
// note, when there is no card table), the per-lease list and the queue. Only keys are
// added; nothing existing changes type.
func Section(cards []gpuprobe.Card, note string, leases []gpulease.Info, waiters []gpulease.Waiter, capPct int, floorGiB float64) map[string]any {
	sec := map[string]any{
		"cards":  RowsWithCap(cards, leases, capPct, floorGiB),
		"leases": LeaseRows(leases),
		"queued": QueueRows(waiters),
	}
	if note != "" {
		sec["cards_note"] = note
	}
	return sec
}

// Table lays the table out for a terminal: one header and one line per card,
// UUID last so it can be copied whole.
func Table(rows []Row) []string {
	lines := []string{fmt.Sprintf("%-4s %-14s %-8s %-6s %-5s %-34s %s", "idx", "vram free/tot", "display", "comfy", "util", "holder", "name / uuid")}
	unknownLine := "" // a reading that could not say which card is the monitor's says so once, not once per card
	for _, r := range rows {
		display := "-"
		if r.Display {
			display = "display"
		}
		comfy := "?"
		if r.ComfyOrder >= 0 {
			comfy = fmt.Sprint(r.ComfyOrder)
		}
		util := "?"
		if r.UtilKnown {
			util = fmt.Sprintf("%d%%", r.UtilPct)
		}
		holder := "free"
		if r.Holder != nil {
			holder = fmt.Sprintf("epoch %d %s (%s)", r.Holder.Epoch, r.Holder.Class, r.Holder.Scope)
			if r.Holder.Group != "" {
				holder += " " + r.Holder.Group
			}
		}
		lines = append(lines, fmt.Sprintf("%-4d %-14s %-8s %-6s %-5s %-34s %s  %s",
			r.Index, fmt.Sprintf("%.1f/%.1f GiB", r.VRAMFreeGiB, r.VRAMTotalGiB), display, comfy, util, clip(holder, 34), r.Name, r.UUID))
		if r.Holder != nil && r.Holder.Command != "" {
			lines = append(lines, "       running: "+clip(r.Holder.Command, 100))
		}
		switch {
		case r.DisplayCap == nil:
		case r.DisplayUnknown:
			if unknownLine == "" {
				unknownLine = capLine(r)
			}
		default:
			lines = append(lines, capLine(r))
		}
	}
	if unknownLine != "" {
		lines = append(lines, unknownLine)
	}
	return lines
}

// capLine is the one line under the display card that says where it stands against its cap. It says what is
// counted (the desktop and every process) and that it cannot say whose memory it is: nvidia-smi on a WDDM driver
// sizes no process, so this is the card's use, not a seat's.
func capLine(r Row) string {
	const indent = "       display cap: "
	d := r.DisplayCap
	switch {
	case d.Unknown != "":
		return indent + "not judged (" + d.Unknown + ")"
	case d.Closed:
		return indent + fmt.Sprintf("gpu_display_cap_pct is 0, so the card is closed to jobs; %.1f of %.1f GiB in use (%.0f%%), not judged (the desktop's own use cannot be told from a job's)",
			d.UsedGiB, r.VRAMTotalGiB, d.UsedPct)
	}
	verdict := "within"
	if d.OverCap {
		verdict = fmt.Sprintf("OVER by %.1f GiB", d.OverByGiB)
		if d.OverByGiB < smallOverGiB {
			verdict = "OVER by less than 0.1 GiB"
		}
	}
	return indent + fmt.Sprintf("%.1f of %.1f GiB in use (%.0f%%); gpu_display_cap_pct %d (with any declared display floor) allows %.1f GiB: %s (the desktop and every process are counted; this line cannot say whose memory it is)",
		d.UsedGiB, r.VRAMTotalGiB, d.UsedPct, d.CapPct, d.BudgetGiB, verdict)
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
