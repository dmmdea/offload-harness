// ComfyUI run-timing surface (`timing`).
//
// NOT generated — hand-written and preserved across regeneration.
// Do not add the generated-file marker to this file.
//
// Split out of comfy_jobs.go so the file's data-source strategy is unambiguous:
// `timing` reads ONLY the local run table, while its former file-mates reach the
// live server. Strategy is declared per file, so a local-only command and a
// live-only command cannot share one.

package cli

import (
	"fmt"
	"strings"

	"comfyui-pp-cli/internal/store"
	"github.com/spf13/cobra"
)

// comfyTimingRow is one run measured against its own performance shape.
type comfyTimingRow struct {
	PromptID   string  `json:"prompt_id"`
	Name       string  `json:"name,omitempty"`
	State      string  `json:"state"`
	ShapeSHA   string  `json:"shape_sha,omitempty"`
	DurationMS int64   `json:"duration_ms,omitempty"`
	Duration   string  `json:"duration,omitempty"`
	ShapeN     int     `json:"shape_n"`
	ShapeMean  float64 `json:"shape_mean_ms,omitempty"`
	ShapeSD    float64 `json:"shape_sd_ms,omitempty"`
	DeltaMS    float64 `json:"delta_ms,omitempty"`
	DeltaPct   float64 `json:"delta_pct,omitempty"`
	Sigma      float64 `json:"sigma,omitempty"`
	Outlier    bool    `json:"outlier"`
	Note       string  `json:"note,omitempty"`
}

// comfyTimingOutlierSigma is the threshold at which a run is flagged against its own
// shape. Two sample standard deviations, and only once a shape has enough runs for
// the spread to mean anything.
const comfyTimingOutlierSigma = 2.0

// comfyTimingMinN is the smallest shape population that may flag an outlier. With
// fewer runs the standard deviation is noise and every second run looks anomalous.
const comfyTimingMinN = 3

// newTimingCmd tabulates recent durations against their shape statistics.
//
// pp:data-source local
func newTimingCmd(flags *rootFlags) *cobra.Command {
	var last int
	var shape string

	cmd := &cobra.Command{
		Use:   "timing",
		Short: "Recent run durations compared against their own performance shape",
		Long: `Table of recent runs with their authoritative durations, each compared against
the distribution of every completed run that shares its performance shape.

The shape (shape_sha) is the graph with seed and other volatile widgets stripped,
so runs that differ only by seed are legitimately comparable and runs that changed
resolution or model are not silently averaged together.

Every duration here came from a /history execution_start -> execution_success pair.
Nothing in this table was ever read from the server log's "Prompt executed in N
seconds" line (which reports the PREVIOUS prompt while one is running) or from an
s/it progress sample (an instantaneous rate, not a duration). Reading those is what
once produced a false "+49% regression" on a build that had got faster.

A run is flagged as an outlier only against a shape with at least ` + fmt.Sprintf("%d", comfyTimingMinN) + ` completed
runs, at ` + fmt.Sprintf("%.0f", comfyTimingOutlierSigma) + ` sample standard deviations. Shape statistics include the run
itself, so a single wild run also drags its own baseline — read the n column.

Reads only the local store; run 'sync-history' first if the table looks empty.`,
		Example: `  comfyui-pp-cli timing
  comfyui-pp-cli timing --last 50
  comfyui-pp-cli timing --shape 9f2c1b0a4d7e --json`,
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": "0,2",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "timing")
			}
			if len(args) > 0 {
				return usageErr(fmt.Errorf("timing takes no positional arguments (got %q)", args[0]))
			}
			if last < 0 {
				return usageErr(fmt.Errorf("--last must be zero or positive (got %d)", last))
			}

			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()

			db, err := comfyJobsOpenReadable(ctx)
			if err != nil {
				return err
			}
			if db == nil {
				return comfyEmptyTiming(cmd, flags)
			}
			defer db.Close()

			runs, err := comfyQueryShapeRuns(ctx, db.DB(), strings.TrimSpace(shape), last)
			if err != nil {
				return fmt.Errorf("reading local runs: %w", err)
			}
			if len(runs) == 0 {
				return comfyEmptyTiming(cmd, flags)
			}

			statsCache := map[string]store.ShapeStats{}
			rows := make([]comfyTimingRow, 0, len(runs))
			for _, run := range runs {
				row := comfyTimingRow{
					PromptID:   run.PromptID,
					Name:       run.Name,
					State:      run.State,
					ShapeSHA:   run.ShapeSHA,
					DurationMS: run.DurationMS,
					Duration:   comfyFormatMS(run.DurationMS),
				}
				if run.DurationMS <= 0 {
					row.Note = "no execution_start/execution_success pair recorded"
				}
				if run.ShapeSHA != "" {
					stats, ok := statsCache[run.ShapeSHA]
					if !ok {
						stats, err = store.ShapeStatsFor(ctx, db.DB(), run.ShapeSHA)
						if err != nil {
							return fmt.Errorf("reading shape stats: %w", err)
						}
						statsCache[run.ShapeSHA] = stats
					}
					row.ShapeN = stats.N
					row.ShapeMean = stats.MeanMS
					row.ShapeSD = stats.StdDevMS
					if run.DurationMS > 0 && stats.N > 0 {
						row.DeltaMS = float64(run.DurationMS) - stats.MeanMS
						if stats.MeanMS > 0 {
							row.DeltaPct = row.DeltaMS / stats.MeanMS * 100
						}
						if stats.StdDevMS > 0 && stats.N >= comfyTimingMinN {
							row.Sigma = row.DeltaMS / stats.StdDevMS
							row.Outlier = row.Sigma >= comfyTimingOutlierSigma || row.Sigma <= -comfyTimingOutlierSigma
						}
					}
				} else if row.Note == "" {
					row.Note = "no shape recorded; not comparable"
				}
				rows = append(rows, row)
			}

			payload := map[string]any{
				"meta": map[string]any{
					"source":        "local",
					"returned":      len(rows),
					"shape_filter":  strings.TrimSpace(shape),
					"timing_source": "history execution_start -> execution_success",
					"outlier_rule":  fmt.Sprintf(">= %.0f sample sd against a shape with n >= %d", comfyTimingOutlierSigma, comfyTimingMinN),
				},
				"runs": rows,
			}
			if !wantsHumanTable(cmd.OutOrStdout(), flags) {
				return printJSONFiltered(cmd.OutOrStdout(), payload, flags)
			}
			headers := []string{"PROMPT_ID", "STATE", "DURATION", "SHAPE", "N", "MEAN", "SD", "DELTA", "FLAG"}
			table := make([][]string, 0, len(rows))
			for _, row := range rows {
				flag := ""
				if row.Outlier {
					flag = yellow(fmt.Sprintf("outlier %+.1fσ", row.Sigma))
				}
				delta := "-"
				if row.ShapeN > 0 && row.DurationMS > 0 {
					delta = fmt.Sprintf("%+.0fms (%+.1f%%)", row.DeltaMS, row.DeltaPct)
				}
				table = append(table, []string{
					row.PromptID,
					row.State,
					row.Duration,
					comfyDash(comfyShort(row.ShapeSHA)),
					fmt.Sprintf("%d", row.ShapeN),
					comfyFormatMS(int64(row.ShapeMean)),
					comfyFormatMS(int64(row.ShapeSD)),
					delta,
					flag,
				})
			}
			if err := flags.printTable(cmd, headers, table); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(),
				"durations from /history execution_start -> execution_success only; shape stats cover every completed run of that shape\n")
			return nil
		},
	}
	cmd.Flags().IntVar(&last, "last", 20, "How many recent runs to show (0 for all)")
	cmd.Flags().StringVar(&shape, "shape", "", "Restrict to one shape_sha so only comparable runs are listed")
	return cmd
}

func comfyEmptyTiming(cmd *cobra.Command, flags *rootFlags) error {
	const hint = "no local runs recorded yet; run 'comfyui-pp-cli sync-history' while the ComfyUI server is up (its /history is RAM-only and a restart destroys it)"
	if !wantsHumanTable(cmd.OutOrStdout(), flags) {
		return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
			"meta": map[string]any{"source": "local", "returned": 0, "hint": hint},
			"runs": []comfyTimingRow{},
		}, flags)
	}
	fmt.Fprintln(cmd.OutOrStdout(), hint)
	return nil
}
