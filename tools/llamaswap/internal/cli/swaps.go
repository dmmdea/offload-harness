// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Novel command (wave A spine), implemented against the mirrored domain tables.
// pp:data-source local
// Supported strategies: auto, local, live, or computed. `local` is deliberate:
// swap economics is a question about HISTORY, and the proxy exposes no endpoint
// that answers it — only the local mirror has the timeline.

package cli

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"llamaswap-pp-cli/internal/store"
)

// spineSwapModelStat is per-seat cold-load economics.
type spineSwapModelStat struct {
	Model       string  `json:"model"`
	Loads       int     `json:"loads"`
	ColdLoadP50 *int64  `json:"cold_load_p50_ms"`
	ColdLoadP95 *int64  `json:"cold_load_p95_ms"`
	ColdLoadMax *int64  `json:"cold_load_max_ms"`
	TotalMS     int64   `json:"total_cold_load_ms"`
	MinutesLost float64 `json:"minutes_lost"`
	// MinutesLostPerDay divides by the observed span, not by a calendar
	// constant: a two-hour mirror window would otherwise be reported as if it
	// were a day.
	MinutesLostPerDay *float64 `json:"minutes_lost_per_day"`
	Requests          int      `json:"requests_in_window"`
}

// spineThrashPair is a mutual-eviction pair.
type spineThrashPair struct {
	A         string `json:"a"`
	B         string `json:"b"`
	AEvictedB int    `json:"a_evicted_b"`
	BEvictedA int    `json:"b_evicted_a"`
	Total     int    `json:"total"`
}

type spineSwapsReport struct {
	SchemaVersion int                  `json:"schema_version"`
	Since         string               `json:"since,omitempty"`
	WindowStart   string               `json:"window_start,omitempty"`
	WindowEnd     string               `json:"window_end,omitempty"`
	ObservedDays  float64              `json:"observed_days"`
	Models        []spineSwapModelStat `json:"models"`
	// Thrash is a POINTER so the JSON can distinguish "not computed" from
	// "computed, found nothing". With a plain slice + omitempty, a --thrash run
	// that found no mutual-eviction pairs emitted output BYTE-IDENTICAL to a run
	// without the flag — the reader could not tell whether the analysis had run.
	// nil => key absent (flag not passed); non-nil empty => "thrash": [].
	Thrash   *[]spineThrashPair `json:"thrash,omitempty"`
	Coverage spineSwapCoverage  `json:"coverage"`
	Notes    []string           `json:"notes"`
}

// spineSwapCoverage states what the numbers rest on. Percentiles over a mirror
// with holes are not percentiles over the truth, and the output has to say so.
type spineSwapCoverage struct {
	SwapEvents     int      `json:"swap_events"`
	RequestRows    int      `json:"request_rows"`
	Epochs         int      `json:"epochs"`
	SealedEpochs   int      `json:"sealed_epochs"`
	CensoredRows   int      `json:"censored_rows"`
	EpochsWithHole int      `json:"epochs_with_unknown_loss"`
	Sampled        bool     `json:"sampled"`
	Caveats        []string `json:"caveats"`
}

func newNovelSwapsCmd(flags *rootFlags) *cobra.Command {
	var flagThrash bool
	var flagSince string
	var flagWindow time.Duration
	var flagDB string

	cmd := &cobra.Command{
		Use:   "swaps",
		Short: "Cold-load percentiles per model, time lost to swapping, and which model pairs repeatedly evict each other.",
		Long: strings.Trim(`
Reads the local mirror only — never the config, never the network. The
proxy has no endpoint that answers "what does swapping cost me", because
that question needs a timeline it does not keep.

Every number is SAMPLED: it covers the swap events this CLI happened to
mirror. Requests lost to ring eviction, and any epoch whose id density
broke, are reported as coverage caveats rather than folded silently into
the percentiles. Run 'sync' more often to narrow the gap.

This command PRINTS. It never edits groups, ttl, or model placement —
auto-tuning is an anti-feature here.`, "\n"),
		Example: strings.Trim(`
  # Cold-load p50/p95 per seat over everything mirrored
  llamaswap-pp-cli swaps

  # Last week, with mutual-eviction pairs
  llamaswap-pp-cli swaps --since 7d --thrash --json

  # Widen the attribution window used to blame an eviction on the next load
  llamaswap-pp-cli swaps --thrash --evict-window 2m
`, "\n"),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "swaps")
			}
			var since time.Time
			if strings.TrimSpace(flagSince) != "" {
				t, err := parseSinceDuration(flagSince)
				if err != nil {
					return usageErr(fmt.Errorf("invalid --since value %q: %w", flagSince, err))
				}
				since = t
			}
			ctx := cmd.Context()
			db, err := spineOpenDB(ctx, flagDB)
			if err != nil {
				return err
			}
			defer db.Close()

			rep, err := spineBuildSwapsReport(ctx, db, since, flagSince, flagThrash, flagWindow)
			if err != nil {
				return err
			}
			return spineWriteSwapsReport(cmd, flags, rep, flagThrash)
		},
	}
	cmd.Flags().BoolVar(&flagThrash, "thrash", false, "Also report mutual-eviction pairs: seats that repeatedly push each other out.")
	cmd.Flags().StringVar(&flagSince, "since", "", "Only consider mirrored events newer than this (e.g. 7d, 24h, 90m).")
	cmd.Flags().DurationVar(&flagWindow, "evict-window", time.Minute, "How close a load must follow an unload to be attributed as an eviction.")
	cmd.Flags().StringVar(&flagDB, "db", "", "SQLite database file path (default: resolved data directory data.db).")
	return cmd
}

type spineSwapEvent struct {
	TS       time.Time
	Model    string
	Event    string
	ColdLoad *int64
}

func spineBuildSwapsReport(ctx context.Context, db *store.Store, since time.Time, sinceText string, wantThrash bool, evictWindow time.Duration) (*spineSwapsReport, error) {
	rep := &spineSwapsReport{SchemaVersion: spineSchemaVersion, Since: sinceText}
	events, err := spineLoadSwapEvents(ctx, db, since)
	if err != nil {
		return nil, err
	}
	rep.Coverage.SwapEvents = len(events)

	if len(events) > 0 {
		rep.WindowStart = events[0].TS.UTC().Format(time.RFC3339)
		rep.WindowEnd = events[len(events)-1].TS.UTC().Format(time.RFC3339)
		span := events[len(events)-1].TS.Sub(events[0].TS)
		rep.ObservedDays = math.Round(span.Hours()/24*1000) / 1000
	}

	byModel := map[string][]int64{}
	for _, e := range events {
		if e.Event == "ready" && e.ColdLoad != nil && *e.ColdLoad >= 0 {
			byModel[e.Model] = append(byModel[e.Model], *e.ColdLoad)
		}
	}
	// Seats that loaded but whose cold-load duration was never observed still
	// deserve a row: a missing duration is not a zero-cost load.
	loadCounts := map[string]int{}
	for _, e := range events {
		if e.Event == "loading" || e.Event == "ready" {
			loadCounts[e.Model]++
		}
	}
	reqCounts, err := spineRequestCountsByModel(ctx, db, since)
	if err != nil {
		return nil, err
	}

	models := map[string]bool{}
	for m := range loadCounts {
		models[m] = true
	}
	for m := range reqCounts {
		models[m] = true
	}
	names := make([]string, 0, len(models))
	for m := range models {
		names = append(names, m)
	}
	sort.Strings(names)

	for _, m := range names {
		samples := append([]int64(nil), byModel[m]...)
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		stat := spineSwapModelStat{
			Model:    m,
			Loads:    len(samples),
			Requests: reqCounts[m],
		}
		if len(samples) > 0 {
			p50 := spinePercentile(samples, 0.50)
			p95 := spinePercentile(samples, 0.95)
			max := samples[len(samples)-1]
			stat.ColdLoadP50 = &p50
			stat.ColdLoadP95 = &p95
			stat.ColdLoadMax = &max
			for _, s := range samples {
				stat.TotalMS += s
			}
			stat.MinutesLost = math.Round(float64(stat.TotalMS)/60000*100) / 100
			if rep.ObservedDays > 0 {
				perDay := math.Round(stat.MinutesLost/rep.ObservedDays*100) / 100
				stat.MinutesLostPerDay = &perDay
			}
		}
		rep.Models = append(rep.Models, stat)
	}

	if wantThrash {
		// Always materialize a non-nil slice: an empty result is a FINDING
		// ("no pair evicts each other in this window"), not a missing field.
		pairs := spineThrashPairs(events, evictWindow)
		if pairs == nil {
			pairs = []spineThrashPair{}
		}
		rep.Thrash = &pairs
	}

	if err := spineFillSwapCoverage(ctx, db, rep); err != nil {
		return nil, err
	}
	rep.Notes = append(rep.Notes,
		"SAMPLED: derived from mirrored swap events only; anything that rolled out of the ring before a sync is absent.",
		"cold-load durations are measured from this CLI's receipt of the loading and ready log lines, so they are an UPPER bound on the server-side load.",
		"prints only: this command never edits groups, ttl, or model placement.",
	)
	if rep.ObservedDays == 0 {
		rep.Notes = append(rep.Notes, "observed span is under a day: minutes_lost_per_day is null rather than extrapolated.")
	}
	return rep, nil
}

func spineLoadSwapEvents(ctx context.Context, db *store.Store, since time.Time) ([]spineSwapEvent, error) {
	query := `SELECT ts, model, event, cold_load_ms FROM swap_events`
	args := []any{}
	if !since.IsZero() {
		query += ` WHERE ts >= ?`
		args = append(args, since.UTC().Format(time.RFC3339))
	}
	query += ` ORDER BY ts, id`
	rows, err := db.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read swap_events: %w", err)
	}
	defer rows.Close()
	var out []spineSwapEvent
	for rows.Next() {
		var ts, model, event string
		var cold *int64
		if err := rows.Scan(&ts, &model, &event, &cold); err != nil {
			return nil, err
		}
		t, perr := time.Parse(time.RFC3339, ts)
		if perr != nil {
			continue
		}
		out = append(out, spineSwapEvent{TS: t, Model: model, Event: event, ColdLoad: cold})
	}
	return out, rows.Err()
}

func spineRequestCountsByModel(ctx context.Context, db *store.Store, since time.Time) (map[string]int, error) {
	query := `SELECT COALESCE(model,''), COUNT(*) FROM requests`
	args := []any{}
	if !since.IsZero() {
		query += ` WHERE ts >= ?`
		args = append(args, since.UTC().Format(time.RFC3339))
	}
	query += ` GROUP BY model`
	rows, err := db.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read requests: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var m string
		var n int
		if err := rows.Scan(&m, &n); err != nil {
			return nil, err
		}
		if m != "" {
			out[m] = n
		}
	}
	return out, rows.Err()
}

// spineThrashPairs attributes each load to the unload that immediately preceded
// it within evictWindow, then reports the pairs where the blame runs BOTH ways.
// One-directional eviction is normal capacity behavior; only the mutual case is
// the pathology worth an operator's attention.
func spineThrashPairs(events []spineSwapEvent, window time.Duration) []spineThrashPair {
	if window <= 0 {
		window = time.Minute
	}
	type edge struct{ from, to string }
	counts := map[edge]int{}
	for i, e := range events {
		if e.Event != "loading" {
			continue
		}
		for j := i - 1; j >= 0; j-- {
			prev := events[j]
			if e.TS.Sub(prev.TS) > window {
				break
			}
			if prev.Model == e.Model {
				continue
			}
			if prev.Event == "unloading" || prev.Event == "unloaded" {
				counts[edge{from: prev.Model, to: e.Model}]++
				break
			}
		}
	}
	seen := map[string]bool{}
	var out []spineThrashPair
	for e, n := range counts {
		rev := counts[edge{from: e.to, to: e.from}]
		if rev == 0 {
			continue
		}
		a, b := e.from, e.to
		ab, ba := n, rev
		if a > b {
			a, b = b, a
			ab, ba = ba, ab
		}
		key := a + "\x00" + b
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, spineThrashPair{A: a, B: b, AEvictedB: ab, BEvictedA: ba, Total: ab + ba})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].A < out[j].A
	})
	return out
}

func spineFillSwapCoverage(ctx context.Context, db *store.Store, rep *spineSwapsReport) error {
	c := &rep.Coverage
	c.Sampled = true
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM requests`).Scan(&c.RequestRows); err != nil {
		return err
	}
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE censored=1`).Scan(&c.CensoredRows); err != nil {
		return err
	}
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM epochs`).Scan(&c.Epochs); err != nil {
		return err
	}
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM epochs WHERE state='sealed'`).Scan(&c.SealedEpochs); err != nil {
		return err
	}
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM epochs WHERE loss_evicted IS NULL OR ids_dense=0`).Scan(&c.EpochsWithHole); err != nil {
		return err
	}
	if c.EpochsWithHole > 0 {
		c.Caveats = append(c.Caveats, fmt.Sprintf("%d epoch(s) have unknown eviction loss (id density was violated); request counts below are lower bounds", c.EpochsWithHole))
	}
	if c.CensoredRows > 0 {
		c.Caveats = append(c.Caveats, fmt.Sprintf("%d request(s) are censored (in flight when an epoch was sealed); their durations are unknown, not zero, and long requests are over-represented", c.CensoredRows))
	}
	if c.Epochs > 1 {
		c.Caveats = append(c.Caveats, fmt.Sprintf("%d epochs known — and that is a LOWER bound on restarts", c.Epochs))
	}
	if c.SwapEvents == 0 {
		c.Caveats = append(c.Caveats, "no swap events mirrored yet: run 'sync' while models load and unload")
	}
	return nil
}

func spinePercentile(sorted []int64, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := int(math.Ceil(q*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func spineWriteSwapsReport(cmd *cobra.Command, flags *rootFlags, rep *spineSwapsReport, wantThrash bool) error {
	if flags != nil && (flags.asJSON || !isTerminal(cmd.OutOrStdout())) {
		return printJSONFiltered(cmd.OutOrStdout(), rep, flags)
	}
	w := cmd.OutOrStdout()
	if len(rep.Models) == 0 {
		fmt.Fprintln(w, "no swap history mirrored yet — run 'llamaswap-pp-cli sync' first")
	} else {
		tw := newTabWriter(w)
		fmt.Fprintln(tw, "MODEL\tLOADS\tCOLD-P50\tCOLD-P95\tMIN-LOST\tMIN-LOST/DAY\tREQUESTS")
		for _, m := range rep.Models {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%.2f\t%s\t%d\n",
				m.Model, m.Loads, spineMSText(m.ColdLoadP50), spineMSText(m.ColdLoadP95),
				m.MinutesLost, spineFloatText(m.MinutesLostPerDay), m.Requests)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if wantThrash {
		fmt.Fprintln(w)
		var pairs []spineThrashPair
		if rep.Thrash != nil {
			pairs = *rep.Thrash
		}
		if len(pairs) == 0 {
			fmt.Fprintln(w, "no mutual-eviction pairs in the mirrored window")
		} else {
			tw := newTabWriter(w)
			fmt.Fprintln(tw, "A\tB\tA-EVICTED-B\tB-EVICTED-A\tTOTAL")
			for _, p := range pairs {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\n", p.A, p.B, p.AEvictedB, p.BEvictedA, p.Total)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "coverage: %d swap events, %d request rows, %d epoch(s), %d censored (SAMPLED)\n",
		rep.Coverage.SwapEvents, rep.Coverage.RequestRows, rep.Coverage.Epochs, rep.Coverage.CensoredRows)
	for _, c := range rep.Coverage.Caveats {
		fmt.Fprintf(w, "  caveat: %s\n", c)
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(w, "  note: %s\n", n)
	}
	return nil
}

func spineMSText(v *int64) string {
	if v == nil {
		return "unknown"
	}
	return fmt.Sprintf("%dms", *v)
}

func spineFloatText(v *float64) string {
	if v == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.2f", *v)
}
