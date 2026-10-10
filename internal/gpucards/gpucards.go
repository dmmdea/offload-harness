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
}

// Rows joins the card table with the live leases. A card-scoped lease holds the
// cards it names; a whole-node lease holds every card, and says so.
func Rows(cards []gpuprobe.Card, leases []gpulease.Info) []Row {
	rows := make([]Row, 0, len(cards))
	for _, c := range cards {
		r := Row{
			UUID: c.UUID, Index: c.NvidiaIndex, Name: c.Name, Display: c.Display,
			VRAMTotalGiB: c.VRAMTotalGiB, VRAMFreeGiB: c.VRAMFreeGiB,
			UtilPct: c.UtilPct, UtilKnown: c.UtilKnown, ComfyOrder: c.ComfyOrder, State: "free",
		}
		if l, scope, ok := holderOf(c, leases); ok {
			r.State = "held"
			r.Holder = &Holder{Epoch: l.Epoch, Class: string(l.Class), PID: l.PID, Scope: scope,
				Group: l.Group, Command: l.Command, WrapperVersion: l.WrapperVersion}
		}
		rows = append(rows, r)
	}
	return rows
}

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
func Section(cards []gpuprobe.Card, note string, leases []gpulease.Info, waiters []gpulease.Waiter) map[string]any {
	sec := map[string]any{
		"cards":  Rows(cards, leases),
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
	}
	return lines
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
