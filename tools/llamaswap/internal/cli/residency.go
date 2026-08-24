// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Novel command (wave D): reconstruct each seat's residency/eviction timeline
// from mirrored request gaps and answer "what would a different idle-TTL cost or
// save". This is the verb the operator's own open question needs — the uniform
// 5-minute TTL was set by judgment, not measurement.
//
// pp:data-source local
//
// The deferred name was "replay"; it is registered as `residency` (what the
// operator would actually type) with `replay` kept as a hidden alias so the
// original name still resolves. It SIMULATES from history — it never re-issues
// traffic and never edits the config.

package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/mirror"
	"llamaswap-pp-cli/internal/store"
)

// residencySeat is the per-model residency picture at the CURRENT configured TTL.
type residencySeat struct {
	Model          string   `json:"model"`
	ConfiguredTTL  string   `json:"configured_ttl"` // "300s" | "never (-1)" | "unset" | "unknown"
	Requests       int      `json:"requests"`
	InferredLoads  int      `json:"inferred_loads"`
	TTLEvictions   int      `json:"ttl_evictions"`
	ColdLoadP50MS  *int64   `json:"cold_load_p50_ms"`
	ColdLoadMaxMS  *int64   `json:"cold_load_max_ms"`
	ColdMinutes    float64  `json:"cold_minutes_total"`
	KeepSet        bool     `json:"keepset"`
	SharesEviction []string `json:"shares_eviction_group_with,omitempty"`
	CoveragePct    *float64 `json:"coverage_pct"`
	Note           string   `json:"note,omitempty"`
}

// residencyWhatIf is one --ttl counterfactual for one model. Bound directions are
// stated in the field names and enforced in the comments: raising a TTL AVOIDS
// reloads (an optimistic ceiling — this model ignores group-exclusivity and VRAM
// contention, which can force a reload the idle-only model does not) and ADDS
// resident time (an upper bound — the seat cannot stay resident longer than the
// gap, and contention can cut it short).
type residencyWhatIf struct {
	Model              string  `json:"model"`
	FromTTL            string  `json:"from_ttl"`
	ToTTL              string  `json:"to_ttl"`
	ReloadsAvoidedMax  int     `json:"reloads_avoided_ceiling"`
	ReloadsAddedMin    int     `json:"reloads_added_floor"`
	ColdMinSaved       float64 `json:"cold_minutes_saved_ceiling"`
	ResidentMinutesAdd float64 `json:"resident_minutes_added_upper_bound"`
	Safe               *bool   `json:"safe_for_keepset_and_groups"`
	Rationale          string  `json:"rationale"`
}

type residencyReport struct {
	SchemaVersion int               `json:"schema_version"`
	Since         string            `json:"since,omitempty"`
	Seats         []residencySeat   `json:"seats"`
	WhatIf        []residencyWhatIf `json:"what_if,omitempty"`
	Coverage      analyticsCoverage `json:"coverage"`
	Notes         []string          `json:"notes"`
}

func newNovelResidencyCmd(flags *rootFlags) *cobra.Command {
	var flagSince string
	var flagDB string
	var flagTTL []string

	cmd := &cobra.Command{
		Use:     "residency",
		Aliases: []string{"replay"},
		Short:   "Reconstruct each seat's load/evict timeline from request gaps, and cost a different idle-TTL.",
		Long: strings.Trim(`
Reads the local mirror and the llama-swap config, both READ-ONLY. Reconstructs,
per seat, how often it was loaded and TTL-evicted, and what its cold reloads
cost — then, with --ttl, what a different idle-TTL would have saved or cost.

This is a SIMULATION over mirrored history, not a traffic re-run. It models the
idle-TTL eviction rule ONLY: it does not know about group exclusivity or VRAM
contention, which can force a reload this model would not predict. So a
counterfactual's "reloads avoided" is an optimistic CEILING and its "resident
minutes added" is an upper BOUND, both labelled as such. For keep-set seats
(ttl -1) and seats sharing an eviction group, raising a TTL is flagged unsafe
or moot rather than recommended.

Idle time is measured correctly for llama-swap's completion-stamped activity:
gap = next.start - prev.end = (next.ts - next.duration) - prev.ts. Gaps that
cross an epoch boundary (a server restart) are NOT counted as TTL evictions.

Because the live config's TTL may have changed during the mirrored window, scope
with --since to the period the current TTL actually applied (the caveat prints
the assumption). This command PRINTS. It never edits ttl, groups, or placement.`, "\n"),
		Example: strings.Trim(`
  # Residency + cold-load cost per seat over the last week
  llamaswap-pp-cli residency --since 7d

  # What would a 15-minute TTL on the 27B seat save?
  llamaswap-pp-cli residency --ttl qwen3.8-27b=900 --since 7d --json

  # Compare two seats at once
  llamaswap-pp-cli residency --ttl qwen3.8-27b=900 --ttl gemma-4-12b=600
`, "\n"),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "residency")
			}
			var since time.Time
			if strings.TrimSpace(flagSince) != "" {
				t, err := parseSinceDuration(flagSince)
				if err != nil {
					return usageErr(fmt.Errorf("invalid --since value %q: %w", flagSince, err))
				}
				since = t
			}
			whatif, err := parseTTLOverrides(flagTTL)
			if err != nil {
				return usageErr(err)
			}
			ctx := cmd.Context()
			db, err := spineOpenDB(ctx, flagDB)
			if err != nil {
				return err
			}
			defer db.Close()

			rep, err := buildResidencyReport(ctx, db, since, flagSince, whatif)
			if err != nil {
				return err
			}
			return writeResidencyReport(cmd, flags, rep)
		},
	}
	cmd.Flags().StringVar(&flagSince, "since", "", "Only consider mirrored requests newer than this (e.g. 7d, 24h). Scope this to when the current TTL applied.")
	cmd.Flags().StringVar(&flagDB, "db", "", "SQLite database file path (default: resolved data directory data.db).")
	cmd.Flags().StringArrayVar(&flagTTL, "ttl", nil, "Counterfactual TTL for a seat, model=seconds (repeatable), e.g. --ttl qwen3.8-27b=900.")
	return cmd
}

func parseTTLOverrides(in []string) (map[string]int, error) {
	out := map[string]int{}
	for _, s := range in {
		kv := strings.SplitN(s, "=", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) == "" {
			return nil, fmt.Errorf("invalid --ttl %q: want model=seconds", s)
		}
		var sec int
		if _, err := fmt.Sscanf(strings.TrimSpace(kv[1]), "%d", &sec); err != nil || sec < 0 {
			return nil, fmt.Errorf("invalid --ttl %q: seconds must be a non-negative integer", s)
		}
		out[strings.TrimSpace(kv[0])] = sec
	}
	return out, nil
}

// residencyGap is one same-epoch idle gap for a model, in seconds, plus the
// duration of the request that FOLLOWS it (the cold-load candidate).
type residencyGap struct {
	idleSec    float64
	followMS   int64
	crossEpoch bool
}

func buildResidencyReport(ctx context.Context, db *store.Store, since time.Time, sinceText string, whatif map[string]int) (*residencyReport, error) {
	reqs, err := loadMirrorRequests(ctx, db, since, "")
	if err != nil {
		return nil, err
	}
	rep := &residencyReport{SchemaVersion: analyticsSchemaVersion, Since: sinceText}

	seatIdx, seatWarns := analyticsYAMLSeats()
	keep := mirror.LoadKeepSet(mirror.KeepSetOptions{})
	evictGroups := analyticsEvictionGroups()

	// Group requests by model, preserving order (reqs is ts,activity_id sorted).
	byModel := map[string][]mirrorRequest{}
	order := []string{}
	for _, r := range reqs {
		if _, ok := byModel[r.Model]; !ok {
			order = append(order, r.Model)
		}
		byModel[r.Model] = append(byModel[r.Model], r)
	}
	sort.Strings(order)

	cov, err := fillAnalyticsCoverage(ctx, db, reqs)
	if err != nil {
		return nil, err
	}
	rep.Coverage = cov

	for _, m := range order {
		rs := byModel[m]
		gaps := residencyGaps(rs)
		seat := seatIdx[m]
		ttl, ttlText := seatTTL(seatIdx, m)
		_, isKeep := keep.MatchAny(append([]string{m}, seat.Aliases...)...)

		st := residencySeat{
			Model:         m,
			ConfiguredTTL: ttlText,
			Requests:      len(rs),
			KeepSet:       isKeep,
			CoveragePct:   cov.CoveragePct,
			SharesEviction: evictGroups[m],
		}
		// Cold-load cost samples: the request following a TTL-eviction gap, minus
		// this model's steady median duration. A negative delta is clamped to 0
		// (a fast follow-up is not a cold load), never fabricated.
		steady := steadyMedianMS(rs)
		var coldSamples []int64
		loads := 1 // the first mirrored request is a load (or the seat was already up — a lower bound either way)
		if len(rs) == 0 {
			loads = 0
		}
		for _, g := range gaps {
			if g.crossEpoch {
				loads++ // a restart reloads on next use; not a TTL eviction
				continue
			}
			if ttl > 0 && g.idleSec > float64(ttl) {
				st.TTLEvictions++
				loads++
				if d := g.followMS - steady; d > 0 {
					coldSamples = append(coldSamples, d)
				}
			}
		}
		st.InferredLoads = loads
		if len(coldSamples) > 0 {
			sort.Slice(coldSamples, func(i, j int) bool { return coldSamples[i] < coldSamples[j] })
			p50 := analyticsPercentile(coldSamples, 0.50)
			mx := coldSamples[len(coldSamples)-1]
			st.ColdLoadP50MS = &p50
			st.ColdLoadMaxMS = &mx
			var tot int64
			for _, c := range coldSamples {
				tot += c
			}
			st.ColdMinutes = round2(float64(tot) / 60000)
		}
		switch {
		case ttl == -1:
			st.Note = "keep-set / never-evict (ttl -1): no idle TTL evictions; a TTL what-if is moot."
		case ttl == 0:
			st.Note = "no per-seat TTL configured; evictions shown are 0 unless a globalTTL applies (not modelled here)."
		}
		rep.Seats = append(rep.Seats, st)
	}
	sort.SliceStable(rep.Seats, func(i, j int) bool {
		if rep.Seats[i].TTLEvictions != rep.Seats[j].TTLEvictions {
			return rep.Seats[i].TTLEvictions > rep.Seats[j].TTLEvictions
		}
		return rep.Seats[i].Model < rep.Seats[j].Model
	})

	// Counterfactuals.
	wmodels := make([]string, 0, len(whatif))
	for m := range whatif {
		wmodels = append(wmodels, m)
	}
	sort.Strings(wmodels)
	for _, m := range wmodels {
		alt := whatif[m]
		rs := byModel[m]
		ttl, ttlText := seatTTL(seatIdx, m)
		wi := residencyWhatIf{
			Model:   m,
			FromTTL: ttlText,
			ToTTL:   fmt.Sprintf("%ds", alt),
		}
		if len(rs) == 0 {
			wi.Rationale = "no mirrored requests for this seat in the window; nothing to simulate."
			safe := true
			wi.Safe = &safe
			rep.WhatIf = append(rep.WhatIf, wi)
			continue
		}
		seat := seatIdx[m]
		_, isKeep := keep.MatchAny(append([]string{m}, seat.Aliases...)...)
		gaps := residencyGaps(rs)
		steady := steadyMedianMS(rs)
		coldCost := medianColdCostMS(gaps, ttl, steady)
		if alt > ttl && ttl > 0 {
			// Raising the TTL: gaps in (cur, alt] no longer evict.
			var avoided int
			var residentAddSec float64
			for _, g := range gaps {
				if g.crossEpoch {
					continue
				}
				if g.idleSec > float64(ttl) && g.idleSec <= float64(alt) {
					avoided++
					residentAddSec += g.idleSec - float64(ttl)
				} else if g.idleSec > float64(alt) {
					// still evicts, but later: resident from cur..alt extra.
					residentAddSec += float64(alt - ttl)
				}
			}
			wi.ReloadsAvoidedMax = avoided
			wi.ColdMinSaved = round2(float64(avoided) * float64(coldCost) / 60000)
			wi.ResidentMinutesAdd = round2(residentAddSec / 60)
			wi.Rationale = fmt.Sprintf("raising ttl %s→%ds avoids up to %d reloads (idle-only ceiling); resident time grows by up to %.1f min.", ttlText, alt, avoided, wi.ResidentMinutesAdd)
		} else if ttl > 0 && alt < ttl {
			// Lowering the TTL: gaps in (alt, cur] now evict.
			var added int
			var residentFreeSec float64
			for _, g := range gaps {
				if g.crossEpoch {
					continue
				}
				if g.idleSec > float64(alt) && g.idleSec <= float64(ttl) {
					added++
					residentFreeSec += g.idleSec - float64(alt)
				} else if g.idleSec > float64(ttl) {
					residentFreeSec += float64(ttl - alt)
				}
			}
			wi.ReloadsAddedMin = added
			wi.ColdMinSaved = -round2(float64(added) * float64(coldCost) / 60000)
			wi.ResidentMinutesAdd = -round2(residentFreeSec / 60)
			wi.Rationale = fmt.Sprintf("lowering ttl %s→%ds adds at least %d reloads (floor); frees up to %.1f resident-min.", ttlText, alt, added, -wi.ResidentMinutesAdd)
		} else {
			wi.Rationale = fmt.Sprintf("ttl %s→%ds: no idle-TTL change to simulate (seat is keep-set/unset, or the value is unchanged).", ttlText, alt)
		}
		safe := !isKeep && len(evictGroups[m]) == 0
		wi.Safe = &safe
		if isKeep {
			wi.Rationale += " UNSAFE-BASE: keep-set seat — do not shorten its residency."
		} else if len(evictGroups[m]) > 0 {
			wi.Rationale += fmt.Sprintf(" CAUTION: shares an eviction group with %s — raising residency may crowd them; VRAM contention is NOT modelled.", strings.Join(evictGroups[m], ", "))
		}
		rep.WhatIf = append(rep.WhatIf, wi)
	}

	rep.Notes = append(rep.Notes, seatWarns...)
	rep.Notes = append(rep.Notes,
		"loads/evictions are LOWER bounds: activity lost before a sync (coverage) hides reloads.",
		"cold-load cost = first-after-idle duration minus this seat's steady median; confounded by prompt size and epoch-boundary restarts, so treat as indicative.",
		"a counterfactual assumes the CURRENT config TTL applied across the whole --since window; scope --since to when it actually did.",
		"prints only: never edits ttl, groups, or placement.")
	return rep, nil
}

// residencyGaps computes same-epoch idle gaps between consecutive requests of one
// model, using the corrected interval (gap = next.start - prev.end).
func residencyGaps(rs []mirrorRequest) []residencyGap {
	var out []residencyGap
	for i := 1; i < len(rs); i++ {
		prev, cur := rs[i-1], rs[i]
		g := residencyGap{
			idleSec:    cur.Start().Sub(prev.End()).Seconds(),
			followMS:   cur.Duration.Milliseconds(),
			crossEpoch: cur.EpochID != prev.EpochID,
		}
		if g.idleSec < 0 {
			g.idleSec = 0 // overlapping second-resolution stamps; not a negative idle
		}
		out = append(out, g)
	}
	return out
}

// steadyMedianMS is the median request duration for a model — the baseline a
// cold load is measured against.
func steadyMedianMS(rs []mirrorRequest) int64 {
	if len(rs) == 0 {
		return 0
	}
	ds := make([]int64, 0, len(rs))
	for _, r := range rs {
		ds = append(ds, r.Duration.Milliseconds())
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

// medianColdCostMS is the median cold-load cost across the eviction gaps at the
// current ttl — the per-reload cost applied to a counterfactual.
func medianColdCostMS(gaps []residencyGap, ttl int, steady int64) int64 {
	var samples []int64
	for _, g := range gaps {
		if g.crossEpoch {
			continue
		}
		if ttl > 0 && g.idleSec > float64(ttl) {
			if d := g.followMS - steady; d > 0 {
				samples = append(samples, d)
			}
		}
	}
	if len(samples) == 0 {
		return 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return samples[len(samples)/2]
}

// seatTTL returns the configured idle TTL in seconds for a model (or its alias)
// and a human label. Unknown config yields (0, "unknown").
func seatTTL(idx map[string]mirror.YAMLSeat, model string) (int, string) {
	s, ok := idx[model]
	if !ok {
		return 0, "unknown"
	}
	if !s.TTLSet {
		return 0, "unset"
	}
	if s.TTL == -1 {
		return -1, "never (-1)"
	}
	return s.TTL, fmt.Sprintf("%ds", s.TTL)
}

func writeResidencyReport(cmd *cobra.Command, flags *rootFlags, rep *residencyReport) error {
	w := cmd.OutOrStdout()
	if flags != nil && (flags.asJSON || !isTerminal(w)) {
		return printJSONFiltered(w, rep, flags)
	}
	if len(rep.Seats) == 0 {
		fmt.Fprintln(w, "no requests mirrored yet — run 'llamaswap-pp-cli sync' first")
	} else {
		tw := newTabWriter(w)
		fmt.Fprintln(tw, "MODEL\tTTL\tREQUESTS\tLOADS\tTTL-EVICT\tCOLD-P50\tCOLD-MIN\tKEEPSET")
		for _, s := range rep.Seats {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\t%.2f\t%t\n",
				s.Model, s.ConfiguredTTL, s.Requests, s.InferredLoads, s.TTLEvictions,
				msPtrText(s.ColdLoadP50MS), s.ColdMinutes, s.KeepSet)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if len(rep.WhatIf) > 0 {
		fmt.Fprintln(w)
		tw := newTabWriter(w)
		fmt.Fprintln(tw, "MODEL\tFROM\tTO\tRELOADS-AVOIDED\tCOLD-MIN-SAVED\tRESIDENT-MIN-ADDED\tSAFE")
		for _, wi := range rep.WhatIf {
			safe := "unknown"
			if wi.Safe != nil {
				safe = fmt.Sprintf("%t", *wi.Safe)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%.2f\t%.2f\t%s\n",
				wi.Model, wi.FromTTL, wi.ToTTL, wi.ReloadsAvoidedMax,
				wi.ColdMinSaved, wi.ResidentMinutesAdd, safe)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		for _, wi := range rep.WhatIf {
			fmt.Fprintf(w, "  %s: %s\n", wi.Model, wi.Rationale)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "coverage: %d request rows, %d epoch(s), %d prepoll-loss (SAMPLED, second-resolution)\n",
		rep.Coverage.RequestRows, rep.Coverage.Epochs, rep.Coverage.PrepollLoss)
	for _, c := range rep.Coverage.Caveats {
		fmt.Fprintf(w, "  caveat: %s\n", c)
	}
	return nil
}

func msPtrText(v *int64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%dms", *v)
}

// analyticsEvictionGroups returns, per model, the other models it shares a
// swap/eviction grouping with — read structurally from the config's matrix sets
// (v239+) or the legacy exclusive/swap groups. Best-effort and caveated: it
// reports co-membership, not a simulation of the solver.
func analyticsEvictionGroups() map[string][]string {
	out := map[string][]string{}
	path := yamlConfigPath()
	if path == "" {
		return out
	}
	f, err := loadConfigFile(path)
	if err != nil || f == nil {
		return out
	}
	add := func(members []string) {
		for _, a := range members {
			for _, b := range members {
				if a == b {
					continue
				}
				out[a] = appendUnique(out[a], b)
			}
		}
	}
	// Legacy groups with exclusive/swap semantics.
	for _, g := range f.Groups {
		if (g.Exclusive != nil && *g.Exclusive) || (g.Swap != nil && *g.Swap) {
			add(g.Members)
		}
	}
	// Matrix sets (v239+): each set value lists co-scheduled members.
	if f.Matrix != nil {
		for _, v := range f.Matrix.Sets {
			add(strings.Fields(strings.NewReplacer(",", " ", "[", " ", "]", " ", "\"", " ").Replace(v)))
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}
