// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
// Hand-authored novel command.
// pp:data-source local

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/spf13/cobra"

	"llamaswap-pp-cli/internal/store"
)

// benchRow is one recorded bench_runs row.
type benchRow struct {
	ID          int64   `json:"id"`
	TS          string  `json:"ts"`
	Model       string  `json:"model"`
	Depth       int     `json:"kv_depth"`
	DepthSeen   int     `json:"kv_depth_observed"`
	PPMean      float64 `json:"pp_mean"`
	PPStddev    float64 `json:"pp_stddev"`
	TGMean      float64 `json:"tg_mean"`
	TGStddev    float64 `json:"tg_stddev"`
	PromptN     int     `json:"prompt_n"`
	Runs        int     `json:"runs"`
	BuildInfo   string  `json:"build_info"`
	SwapVersion string  `json:"llamaswap_version"`
	SHA         string  `json:"comparability_sha"`
	KeyJSON     string  `json:"-"`
}

type benchDelta struct {
	Metric   string  `json:"metric"`
	A        float64 `json:"a"`
	B        float64 `json:"b"`
	DeltaPct float64 `json:"delta_pct"`
	// Significant is false when the change sits inside the combined run-to-run
	// spread of the two rows: a 2% "regression" between two rows that each
	// wobble 4% is noise wearing a number.
	Significant bool   `json:"significant"`
	Note        string `json:"note,omitempty"`
}

type benchCompareReport struct {
	SchemaVersion int          `json:"schema_version"`
	A             *benchRow    `json:"a"`
	B             *benchRow    `json:"b"`
	Comparable    bool         `json:"comparable"`
	Refusal       string       `json:"refusal,omitempty"`
	KeyDiff       []string     `json:"comparability_key_diff,omitempty"`
	Deltas        []benchDelta `json:"deltas,omitempty"`
	Rows          []benchRow   `json:"rows,omitempty"`
	Notes         []string     `json:"notes,omitempty"`
}

func newMeasureBenchCompareCmd(flags *rootFlags) *cobra.Command {
	var (
		flagDepth int
		flagList  bool
		flagLimit int
	)

	cmd := &cobra.Command{
		Use:   "compare <model> [other-model]",
		Short: "Diff two recorded bench rows, refusing when their serving configs differ",
		Long: `Compares recorded benchmark rows from the local store.

With one model, the two most recent rows for it are compared. With two, the
most recent row of each is compared.

The comparison REFUSES (exit 29) when the two rows carry different
comparability shas. That key covers the llama.cpp build, the host, the weights
file, and every seat flag that moves a number - so a differing key means the
two rows were produced by two different machines-as-configured, and their
difference measures the configuration change, not the thing being compared.
The refusal names the fields that differ, so the next step is obvious rather
than mysterious.

A delta smaller than the combined run-to-run spread of the two rows is
reported as NOT significant. A 2% swing between two rows that each wobble 4%
is noise, and publishing it as a regression is how a build gets blamed for a
thermal event.

Reads only the local store: no model is loaded and no request is issued.`,
		Example: `  llamaswap-pp-cli bench compare gemma-4-e2b
  llamaswap-pp-cli bench compare gemma-4-e2b gemma-4-e4b --depth 0
  llamaswap-pp-cli bench compare gemma-4-e2b --list --json`,
		Annotations: map[string]string{
			"mcp:read-only":        "true",
			"pp:typed-exit-codes":  "3=no recorded rows for that model, 29=rows are not comparable (differing comparability_sha)",
			"pp:measurement-owner": "wave-ls1",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				if wantsAgentErrorEnvelope(flags) {
					return usageEnvelopeErr(flags, fmt.Errorf("%q requires a model name", cmd.CommandPath()))
				}
				return cmd.Help()
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "bench compare "+strings.Join(args, " "))
			}
			if err := validateDataSourceStrategy(flags, "local"); err != nil {
				return usageErr(err)
			}
			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()

			s, err := mcOpenDomainStore(ctx)
			if err != nil {
				return configErr(fmt.Errorf("opening the local store: %w", err))
			}
			defer s.Close()

			report := &benchCompareReport{SchemaVersion: 1}
			depth := -1
			if cmd.Flags().Changed("depth") {
				depth = flagDepth
			}

			if flagList {
				rows, err := benchLoadRows(ctx, s, args[0], depth, flagLimit)
				if err != nil {
					return err
				}
				if len(rows) == 0 {
					return benchNoRows(args[0])
				}
				report.Rows = rows
				return mcEmit(cmd, flags, report, func(w io.Writer) { benchCompareListPrint(w, args[0], rows) })
			}

			var a, b *benchRow
			if len(args) >= 2 {
				ra, err := benchLoadRows(ctx, s, args[0], depth, 1)
				if err != nil {
					return err
				}
				rb, err := benchLoadRows(ctx, s, args[1], depth, 1)
				if err != nil {
					return err
				}
				if len(ra) == 0 {
					return benchNoRows(args[0])
				}
				if len(rb) == 0 {
					return benchNoRows(args[1])
				}
				a, b = &ra[0], &rb[0]
			} else {
				rows, err := benchLoadRows(ctx, s, args[0], depth, 2)
				if err != nil {
					return err
				}
				if len(rows) == 0 {
					return benchNoRows(args[0])
				}
				if len(rows) < 2 {
					return &cliError{code: ExitModelNotFound, err: fmt.Errorf(
						"only one recorded bench row for %q: there is nothing to compare it against. Run `llamaswap-pp-cli bench %s` again to produce a second row",
						args[0], args[0])}
				}
				// rows are newest-first: A is the older row, B the newer, so a
				// positive delta reads as an improvement over time.
				a, b = &rows[1], &rows[0]
			}
			report.A, report.B = a, b

			if a.Depth != b.Depth {
				report.Notes = append(report.Notes, fmt.Sprintf(
					"the two rows were measured at different KV depths (d%d vs d%d); both rates decay with depth, so this comparison mixes the depth effect into whatever else changed. Pass --depth N to pin it",
					a.Depth, b.Depth))
			}

			report.Comparable = a.SHA != "" && a.SHA == b.SHA
			if !report.Comparable {
				report.KeyDiff = benchKeyDiffFromJSON(a.KeyJSON, b.KeyJSON)
				switch {
				case a.SHA == "" || b.SHA == "":
					report.Refusal = "one of the rows predates the comparability key (no comparability_sha recorded), so the two configurations cannot be shown to match. Re-bench both to produce keyed rows"
				default:
					report.Refusal = fmt.Sprintf(
						"comparability_sha differs (%s vs %s): these rows describe two different serving configurations, so their difference measures the configuration change and not the models",
						short12(a.SHA), short12(b.SHA))
				}
				if err := mcEmit(cmd, flags, report, func(w io.Writer) { benchComparePrint(w, report) }); err != nil {
					return err
				}
				return &cliError{code: ExitNotComparable, err: fmt.Errorf("REFUSING to diff: %s", report.Refusal)}
			}

			report.Deltas = []benchDelta{
				benchMakeDelta("pp_tokens_per_second", a.PPMean, a.PPStddev, b.PPMean, b.PPStddev),
				benchMakeDelta("tg_tokens_per_second", a.TGMean, a.TGStddev, b.TGMean, b.TGStddev),
			}
			return mcEmit(cmd, flags, report, func(w io.Writer) { benchComparePrint(w, report) })
		},
	}
	cmd.Flags().IntVar(&flagDepth, "depth", 0, "Only consider rows recorded at this KV depth")
	cmd.Flags().BoolVar(&flagList, "list", false, "List the recorded rows instead of comparing")
	cmd.Flags().IntVar(&flagLimit, "limit", 20, "Rows to list with --list")
	return cmd
}

func benchNoRows(model string) error {
	return &cliError{code: ExitModelNotFound, err: fmt.Errorf(
		"no recorded bench rows for %q in the local store. Run `llamaswap-pp-cli bench %s` first", model, model)}
}

// benchMakeDelta expresses B relative to A and decides whether the change
// clears the noise floor. The floor is the two rows' standard deviations
// added in quadrature and expressed as a percentage of A: that is the spread
// a difference has to beat before it is a finding rather than a wobble.
func benchMakeDelta(metric string, aMean, aSD, bMean, bSD float64) benchDelta {
	d := benchDelta{Metric: metric, A: aMean, B: bMean}
	if aMean <= 0 {
		d.Note = "no baseline rate recorded; nothing to compare against"
		return d
	}
	d.DeltaPct = (bMean - aMean) / aMean * 100
	noise := 0.0
	if aSD > 0 || bSD > 0 {
		noise = sqrtSum(aSD*aSD, bSD*bSD) / aMean * 100
	}
	switch {
	case noise == 0:
		d.Significant = d.DeltaPct != 0
		d.Note = "neither row recorded a standard deviation (single-run bench?), so there is no noise floor to clear: treat this delta as unqualified"
	case absFloat(d.DeltaPct) > noise:
		d.Significant = true
		d.Note = fmt.Sprintf("%.1f%% change clears the %.1f%% combined run-to-run spread", d.DeltaPct, noise)
	default:
		d.Note = fmt.Sprintf("%.1f%% change is INSIDE the %.1f%% combined run-to-run spread: not distinguishable from noise", d.DeltaPct, noise)
	}
	return d
}

func benchLoadRows(ctx context.Context, s *store.Store, model string, depth, limit int) ([]benchRow, error) {
	query := `SELECT id, ts, model, kv_depth, COALESCE(kv_depth_observed,0),
	                 COALESCE(pp_mean, COALESCE(pp_median,0)), COALESCE(pp_stddev,0),
	                 COALESCE(tg_mean, COALESCE(tg_median,0)), COALESCE(tg_stddev,0),
	                 COALESCE(prompt_n,0), COALESCE(runs,0),
	                 COALESCE(build_info,''), COALESCE(llamaswap_version,''),
	                 COALESCE(comparability_sha,''), COALESCE(comparability_key,'')
	          FROM bench_runs WHERE model = ?`
	args := []any{model}
	if depth >= 0 {
		query += " AND kv_depth = ?"
		args = append(args, depth)
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading bench_runs: %w", err)
	}
	defer rows.Close()
	var out []benchRow
	for rows.Next() {
		var r benchRow
		if err := rows.Scan(&r.ID, &r.TS, &r.Model, &r.Depth, &r.DepthSeen,
			&r.PPMean, &r.PPStddev, &r.TGMean, &r.TGStddev, &r.PromptN, &r.Runs,
			&r.BuildInfo, &r.SwapVersion, &r.SHA, &r.KeyJSON); err != nil {
			return nil, fmt.Errorf("scanning bench_runs: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading bench_runs: %w", err)
	}
	return out, nil
}

// benchKeyDiffFromJSON names the differing fields. When either key is absent
// or unparseable it says so instead of returning an empty (and therefore
// falsely reassuring) diff.
func benchKeyDiffFromJSON(a, b string) []string {
	var ka, kb benchComparabilityKey
	if a == "" || b == "" {
		return []string{"one of the rows carries no stored comparability key, so the differing fields cannot be named"}
	}
	if err := json.Unmarshal([]byte(a), &ka); err != nil {
		return []string{"row A's stored comparability key could not be parsed: " + err.Error()}
	}
	if err := json.Unmarshal([]byte(b), &kb); err != nil {
		return []string{"row B's stored comparability key could not be parsed: " + err.Error()}
	}
	diff := diffKeys(ka, kb)
	if len(diff) == 0 {
		return []string{"the stored keys list identical fields yet their hashes differ; the stored key JSON and sha disagree, which should not happen"}
	}
	return diff
}

func benchCompareListPrint(w io.Writer, model string, rows []benchRow) {
	fmt.Fprintf(w, "%s  %s\n", bold("bench rows"), model)
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "ID\tWHEN\tDEPTH\tPP tok/s\tTG tok/s\tRUNS\tBUILD\tKEY")
	for _, r := range rows {
		fmt.Fprintf(tw, "%d\t%s\td%d\t%.2f ± %.2f\t%.2f ± %.2f\t%d\t%s\t%s\n",
			r.ID, r.TS, r.Depth, r.PPMean, r.PPStddev, r.TGMean, r.TGStddev, r.Runs, r.BuildInfo, short12(r.SHA))
	}
	tw.Flush()
}

func benchComparePrint(w io.Writer, r *benchCompareReport) {
	fmt.Fprintf(w, "%s\n", bold("bench compare"))
	fmt.Fprintf(w, "  A  #%d  %s  %s  d%d  pp %.2f ± %.2f  tg %.2f ± %.2f  [%s]\n",
		r.A.ID, r.A.TS, r.A.Model, r.A.Depth, r.A.PPMean, r.A.PPStddev, r.A.TGMean, r.A.TGStddev, short12(r.A.SHA))
	fmt.Fprintf(w, "  B  #%d  %s  %s  d%d  pp %.2f ± %.2f  tg %.2f ± %.2f  [%s]\n",
		r.B.ID, r.B.TS, r.B.Model, r.B.Depth, r.B.PPMean, r.B.PPStddev, r.B.TGMean, r.B.TGStddev, short12(r.B.SHA))
	if !r.Comparable {
		fmt.Fprintf(w, "\n  %s %s\n", red("NOT COMPARABLE:"), r.Refusal)
		for _, d := range r.KeyDiff {
			fmt.Fprintf(w, "    differs  %s\n", d)
		}
		return
	}
	fmt.Fprintln(w)
	for _, d := range r.Deltas {
		marker := yellow("noise")
		if d.Significant {
			marker = green("significant")
		}
		fmt.Fprintf(w, "  %-24s %8.2f -> %8.2f  %+6.1f%%  %s\n", d.Metric, d.A, d.B, d.DeltaPct, marker)
		fmt.Fprintf(w, "  %-24s %s\n", "", d.Note)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "  note  %s\n", n)
	}
}

// sqrtSum adds two variances and returns the combined standard deviation.
func sqrtSum(a, b float64) float64 {
	s := a + b
	if s <= 0 {
		return 0
	}
	return math.Sqrt(s)
}

func absFloat(v float64) float64 { return math.Abs(v) }
