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
	ColdSamples    int      `json:"cold_load_samples"`
	ColdMinutes    *float64 `json:"cold_minutes_total"`
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
	Model              string   `json:"model"`
	FromTTL            string   `json:"from_ttl"`
	ToTTL              string   `json:"to_ttl"`
	ReloadsAvoidedMax  int      `json:"reloads_avoided_ceiling"`
	ReloadsAddedMin    int      `json:"reloads_added_floor"`
	ColdMinSaved       *float64 `json:"cold_minutes_saved_ceiling"`
	ResidentMinutesAdd float64  `json:"resident_minutes_added_upper_bound"`
	Safe               *bool    `json:"safe_for_keepset_and_groups"`
	Rationale          string   `json:"rationale"`
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
	idleSec     float64
	followMS    int64
	followKnown bool // false when the following request's duration was NULL
	crossEpoch  bool
}

func buildResidencyReport(ctx context.Context, db *store.Store, since time.Time, sinceText string, whatif map[string]int) (*residencyReport, error) {
	reqs, skipped, err := loadMirrorRequests(ctx, db, since, "")
	if err != nil {
		return nil, err
	}
	rep := &residencyReport{SchemaVersion: analyticsSchemaVersion, Since: sinceText}

	seatIdx, seatWarns := analyticsYAMLSeats()
	keep := mirror.LoadKeepSet(mirror.KeepSetOptions{})
	evictGroups, matrixPresent := analyticsEvictionGroups()

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

	cov, err := fillAnalyticsCoverage(ctx, db, reqs, skipped)
	if err != nil {
		return nil, err
	}
	rep.Coverage = cov

	for _, m := range order {
		rs := byModel[m]
		gaps := residencyGaps(rs)
		seat := seatIdx[m]
		ttl, ttlText, ttlSet, found := seatTTL(seatIdx, m)
		_, isKeep := keep.MatchAny(append([]string{m}, seat.Aliases...)...)

		st := residencySeat{
			Model:          m,
			ConfiguredTTL:  ttlText,
			Requests:       len(rs),
			KeepSet:        isKeep,
			CoveragePct:    cov.CoveragePct,
			SharesEviction: evictGroups[m],
		}
		// Cold-load cost samples: the request following a TTL-eviction gap, minus
		// this model's steady median duration. A negative delta is clamped out
		// (a fast follow-up is not a cold load), never fabricated. Rows with an
		// unknown duration cannot contribute a sample.
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
				if g.followKnown {
					if d := g.followMS - steady; d > 0 {
						coldSamples = append(coldSamples, d)
					}
				}
			}
		}
		st.InferredLoads = loads
		st.ColdSamples = len(coldSamples)
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
			cm := round2(float64(tot) / 60000)
			st.ColdMinutes = &cm // stays nil when no priced sample: null != a measured 0
		}
		// Note branches are distinct so they never contradict the keepset field.
		switch {
		case !found:
			st.Note = "seat not found in the llama-swap config: TTL unknown; evictions shown are 0 (no TTL to apply)."
		case !ttlSet:
			st.Note = "no per-seat ttl key (inherits globalTTL, which is not modelled here): evictions shown are 0."
		case ttl == -1 || ttl == 0:
			st.Note = fmt.Sprintf("resident / never-evict (ttl %s): no idle-TTL evictions; matches keepset=%t. A raise what-if is moot.", ttlText, isKeep)
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
		ttl, ttlText, ttlSet, _ := seatTTL(seatIdx, m)
		seat := seatIdx[m]
		_, isKeep := keep.MatchAny(append([]string{m}, seat.Aliases...)...)
		wi := residencyWhatIf{
			Model:   m,
			FromTTL: ttlText,
			ToTTL:   fmt.Sprintf("%ds", alt),
			Safe:    evictionSafety(isKeep, evictGroups[m], matrixPresent),
		}
		if len(rs) == 0 {
			wi.Rationale = "no mirrored requests for this seat in the window; nothing to simulate."
			rep.WhatIf = append(rep.WhatIf, wi)
			continue
		}
		gaps := residencyGaps(rs)
		steady := steadyMedianMS(rs)
		// setSaved records a cold-minutes figure, or leaves it nil when there is
		// no priced cold-load sample — a null, never a fabricated 0 next to a
		// nonzero reload count.
		setSaved := func(reloads int, coldCost int64, sign float64) {
			if reloads > 0 && coldCost > 0 {
				v := sign * round2(float64(reloads)*float64(coldCost)/60000)
				wi.ColdMinSaved = &v
			} else if reloads > 0 {
				wi.Rationale += " (no priced cold-load sample at the base TTL, so cold_minutes_saved is null, not zero.)"
			} else {
				zero := 0.0
				wi.ColdMinSaved = &zero
			}
		}
		resident := ttl == -1 || (ttl == 0 && ttlSet)
		switch {
		case resident && alt > 0:
			// From never-evict to a finite TTL: every same-epoch idle gap > alt
			// becomes a new eviction. No cold-load samples exist at the base
			// (nothing evicted), so cost is unknown.
			var added int
			var residentFreeSec float64
			for _, g := range gaps {
				if g.crossEpoch {
					continue
				}
				if g.idleSec > float64(alt) {
					added++
					residentFreeSec += g.idleSec - float64(alt)
				}
			}
			wi.ReloadsAddedMin = added
			wi.ResidentMinutesAdd = -round2(residentFreeSec / 60)
			wi.Rationale = fmt.Sprintf("setting ttl %s→%ds would START evicting this resident seat: at least %d reloads (floor), freeing up to %.1f resident-min.", ttlText, alt, added, -wi.ResidentMinutesAdd)
			setSaved(added, 0, -1) // no base samples → cost null
		case alt > ttl && ttl > 0:
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
					residentAddSec += float64(alt - ttl)
				}
			}
			wi.ReloadsAvoidedMax = avoided
			wi.ResidentMinutesAdd = round2(residentAddSec / 60)
			wi.Rationale = fmt.Sprintf("raising ttl %s→%ds avoids up to %d reloads (idle-only ceiling); resident time grows by up to %.1f min.", ttlText, alt, avoided, wi.ResidentMinutesAdd)
			setSaved(avoided, medianColdCostMS(gaps, ttl, steady), 1)
		case ttl > 0 && alt < ttl:
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
			wi.ResidentMinutesAdd = -round2(residentFreeSec / 60)
			wi.Rationale = fmt.Sprintf("lowering ttl %s→%ds adds at least %d reloads (floor); frees up to %.1f resident-min.", ttlText, alt, added, -wi.ResidentMinutesAdd)
			setSaved(added, medianColdCostMS(gaps, ttl, steady), -1)
		default:
			zero := 0.0
			wi.ColdMinSaved = &zero
			if !ttlSet {
				wi.Rationale = fmt.Sprintf("ttl %s→%ds: this seat has no per-seat ttl (inherits globalTTL, not modelled); nothing to simulate.", ttlText, alt)
			} else {
				wi.Rationale = fmt.Sprintf("ttl %s→%ds: unchanged; nothing to simulate.", ttlText, alt)
			}
		}
		if isKeep {
			wi.Rationale += " UNSAFE: keep-set seat — do not shorten its residency."
		} else if len(evictGroups[m]) > 0 {
			wi.Rationale += fmt.Sprintf(" CAUTION: shares an eviction group with %s — raising residency may crowd them; VRAM contention is NOT modelled.", strings.Join(evictGroups[m], ", "))
		} else if matrixPresent {
			wi.Rationale += " SAFETY UNKNOWN: this deployment uses a matrix eviction policy that is not modelled here — verify manually before raising residency."
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
			idleSec:     cur.Start().Sub(prev.End()).Seconds(),
			followMS:    cur.Duration.Milliseconds(),
			followKnown: cur.DurationKnown,
			crossEpoch:  cur.EpochID != prev.EpochID,
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
		if r.DurationKnown { // a NULL duration is unknown, not 0 — don't drag the median down
			ds = append(ds, r.Duration.Milliseconds())
		}
	}
	if len(ds) == 0 {
		return 0
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

// medianColdCostMS is the median cold-load cost across the eviction gaps at the
// current ttl — the per-reload cost applied to a counterfactual.
func medianColdCostMS(gaps []residencyGap, ttl int, steady int64) int64 {
	var samples []int64
	for _, g := range gaps {
		if g.crossEpoch || !g.followKnown {
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

// seatTTL returns the configured idle TTL in seconds for a model (or its alias),
// a human label, whether a ttl key was set, and whether the seat was found in
// the config at all. The four return values let the caller keep "not found",
// "no ttl key (inherits globalTTL)", "resident (ttl 0/-1)", and "finite ttl"
// distinct — collapsing them was how the note contradicted the keepset field.
func seatTTL(idx map[string]mirror.YAMLSeat, model string) (ttl int, label string, ttlSet bool, found bool) {
	s, ok := idx[model]
	if !ok {
		return 0, "unknown", false, false
	}
	if !s.TTLSet {
		return 0, "unset", false, true
	}
	if s.TTL == -1 {
		return -1, "never (-1)", true, true
	}
	return s.TTL, fmt.Sprintf("%ds", s.TTL), true, true
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
			fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\t%s\t%t\n",
				s.Model, s.ConfiguredTTL, s.Requests, s.InferredLoads, s.TTLEvictions,
				msPtrText(s.ColdLoadP50MS), floatPtrText(s.ColdMinutes), s.KeepSet)
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
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%.2f\t%s\n",
				wi.Model, wi.FromTTL, wi.ToTTL, wi.ReloadsAvoidedMax,
				floatPtrText(wi.ColdMinSaved), wi.ResidentMinutesAdd, safe)
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

// floatPtrText renders a *float64 as its value or "unknown" (nil) — nil is a
// real state here (no priced cold-load sample), distinct from a measured 0.
func floatPtrText(v *float64) string {
	if v == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.2f", *v)
}

// analyticsEvictionGroups returns, per model, the other models it shares a
// swap/eviction grouping with — read from the config's LEGACY exclusive/swap
// groups only — plus whether the config uses a v239+ matrix eviction policy.
//
// The matrix `sets` are NOT parsed into co-membership: they are solver boolean
// expressions over `vars` aliases (e.g. "+residents & (w | s)"), not model
// lists, and resolving them wrongly would let a crowding raise be reported
// "safe" — the one error that could evict a keep-set model. So a matrix
// deployment yields matrixPresent=true and the safety verdict for its seats is
// UNKNOWN (nil), with a caveat, rather than a fabricated true. Legacy groups
// resolve precisely.
func analyticsEvictionGroups() (groups map[string][]string, matrixPresent bool) {
	out := map[string][]string{}
	path := yamlConfigPath()
	if path == "" {
		return out, false
	}
	f, err := loadConfigFile(path)
	if err != nil || f == nil {
		return out, false
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
	for _, g := range f.Groups {
		if (g.Exclusive != nil && *g.Exclusive) || (g.Swap != nil && *g.Swap) {
			add(g.Members)
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	matrixPresent = f.Matrix != nil && (len(f.Matrix.Sets) > 0 || len(f.Matrix.Vars) > 0)
	return out, matrixPresent
}

// evictionSafety decides whether raising a seat's residency is safe from
// crowding a protected seat. false = unsafe (keep-set, or a known exclusive
// co-member); nil = UNKNOWN (a matrix policy governs eviction and is not
// modelled here — verify manually); true = affirmatively no exclusivity known.
func evictionSafety(isKeep bool, coMembers []string, matrixPresent bool) *bool {
	f, tr := false, true
	if isKeep || len(coMembers) > 0 {
		return &f
	}
	if matrixPresent {
		return nil
	}
	return &tr
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}
