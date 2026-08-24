// ComfyUI replay surface (`replay <run|name>`).
//
// NOT generated — hand-written and preserved across regeneration.
//
// WHY REPLAY IS ATTRIBUTED, NOT JUST TIMED. Re-running an old graph and printing "now 89 s,
// was 60 s, +49%" is the single most confidently wrong thing this tool could do. That exact
// shape of claim has already been made on this box off a stale log line, and it was false.
// A duration change is only meaningful next to what each side ran under, so replay prints the
// server identity and the argv of BOTH runs and, when the original's argv was never recorded,
// says out loud that the delta cannot be attributed instead of quoting a percentage.
package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"comfyui-pp-cli/internal/comfy/exp"
	"comfyui-pp-cli/internal/store"
	"github.com/spf13/cobra"
)

// replayRunRow is the stored record of the run being replayed.
type replayRunRow struct {
	PromptID    string
	Name        string
	GraphSHA    string
	ShapeSHA    string
	ServerID    string
	State       string
	ExitClass   string
	DurationMS  int64
	HasDuration bool
	Argv        []string
	ArgvKnown   bool
	SubmittedAt string
}

// replayResolveRun accepts a prompt_id, a run name, or an unambiguous prompt_id prefix.
func replayResolveRun(ctx context.Context, db *sql.DB, ref string) (replayRunRow, error) {
	row, err := replayScanRun(ctx, db,
		`SELECT prompt_id, COALESCE(name,''), COALESCE(graph_sha,''), COALESCE(shape_sha,''),
		        COALESCE(server_id,''), state, COALESCE(exit_class,''), duration_ms,
		        COALESCE(argv_json,''), COALESCE(submitted_at,'')
		   FROM run WHERE prompt_id = ? OR name = ? ORDER BY submitted_at DESC LIMIT 1`, ref, ref)
	if err == nil {
		return row, nil
	}
	if err != sql.ErrNoRows {
		return replayRunRow{}, err
	}

	rows, qerr := db.QueryContext(ctx, `SELECT prompt_id FROM run WHERE prompt_id LIKE ? ORDER BY submitted_at DESC LIMIT 6`, ref+"%")
	if qerr != nil {
		return replayRunRow{}, qerr
	}
	defer rows.Close()
	var matches []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return replayRunRow{}, err
		}
		matches = append(matches, id)
	}
	if err := rows.Err(); err != nil {
		return replayRunRow{}, err
	}
	switch len(matches) {
	case 0:
		return replayRunRow{}, notFoundErr(fmt.Errorf("no stored run matches %q (try 'comfyui-pp-cli search' or pass a full prompt_id)", ref))
	case 1:
		return replayScanRun(ctx, db,
			`SELECT prompt_id, COALESCE(name,''), COALESCE(graph_sha,''), COALESCE(shape_sha,''),
			        COALESCE(server_id,''), state, COALESCE(exit_class,''), duration_ms,
			        COALESCE(argv_json,''), COALESCE(submitted_at,'')
			   FROM run WHERE prompt_id = ? OR name = ? LIMIT 1`, matches[0], matches[0])
	default:
		return replayRunRow{}, usageErr(fmt.Errorf("%q matches %d runs: %s", ref, len(matches), strings.Join(matches, ", ")))
	}
}

func replayScanRun(ctx context.Context, db *sql.DB, query string, args ...any) (replayRunRow, error) {
	var (
		row      replayRunRow
		duration sql.NullInt64
		argvJSON string
	)
	err := db.QueryRowContext(ctx, query, args...).Scan(
		&row.PromptID, &row.Name, &row.GraphSHA, &row.ShapeSHA, &row.ServerID,
		&row.State, &row.ExitClass, &duration, &argvJSON, &row.SubmittedAt)
	if err != nil {
		return replayRunRow{}, err
	}
	row.DurationMS = duration.Int64
	row.HasDuration = duration.Valid && duration.Int64 > 0
	if argvJSON != "" {
		if json.Unmarshal([]byte(argvJSON), &row.Argv) == nil {
			row.ArgvKnown = true
		}
	}
	return row, nil
}

// replayArgvForRun falls back to the server row when the run itself has no argv.
//
// The fallback is exact, not a guess: a server's id is a hash OF its argv (plus its version
// strings), so the argv stored against that id is by construction the argv that identity ran
// under. What cannot be recovered is a run whose server was never identified at all — and that
// is precisely the case this command refuses to paper over.
func replayArgvForRun(ctx context.Context, db *sql.DB, row replayRunRow) ([]string, bool) {
	if row.ArgvKnown {
		return row.Argv, true
	}
	if row.ServerID == "" {
		return nil, false
	}
	var argvJSON sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT argv_json FROM server WHERE id = ?`, row.ServerID).Scan(&argvJSON); err != nil {
		return nil, false
	}
	if !argvJSON.Valid || argvJSON.String == "" {
		return nil, false
	}
	var argv []string
	if json.Unmarshal([]byte(argvJSON.String), &argv) != nil {
		return nil, false
	}
	return argv, true
}

func replayLoadGraph(ctx context.Context, db *sql.DB, graphSHA string) (store.APIGraph, error) {
	if graphSHA == "" {
		return nil, fmt.Errorf("that run has no stored graph, so there is nothing to replay")
	}
	var apiJSON string
	err := db.QueryRowContext(ctx, `SELECT api_json FROM graph WHERE sha256 = ?`, graphSHA).Scan(&apiJSON)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("graph %s is referenced by the run but not stored", expShortSHA(graphSHA))
	}
	if err != nil {
		return nil, err
	}
	return expDecodeGraph([]byte(apiJSON), "stored graph "+expShortSHA(graphSHA))
}

type replayReport struct {
	Ref        string            `json:"ref"`
	GraphSHA   string            `json:"graph_sha"`
	ShapeSHA   string            `json:"shape_sha,omitempty"`
	Delta      exp.Delta         `json:"delta"`
	Attached   bool              `json:"attached,omitempty"`
	Outcome    string            `json:"submit_outcome,omitempty"`
	NodeErrors json.RawMessage   `json:"node_errors,omitempty"`
	ShapeStats *replayShapeStats `json:"shape_stats,omitempty"`
	Waited     bool              `json:"waited"`
}

// replayShapeStats projects store.ShapeStats into this surface's snake_case JSON contract.
// The distribution matters here: a single before/after pair says far less than "this shape has
// run 9 times with a mean of 63 s and a standard deviation of 4 s", which is what turns a
// delta into a signal or into noise.
type replayShapeStats struct {
	ShapeSHA string  `json:"shape_sha"`
	N        int     `json:"n"`
	MeanMS   float64 `json:"mean_ms"`
	StdDevMS float64 `json:"stddev_ms"`
	MinMS    int64   `json:"min_ms"`
	MaxMS    int64   `json:"max_ms"`
}

// newComfyReplayCmd reconstructs and resubmits a past run, then reports an ATTRIBUTED delta.
//
// Both sources are load-bearing: the original run, its graph, and the argv it ran
// under come from the local store (the server has long since forgotten them), and the
// replay itself is a live submit polled through /history.
//
// pp:data-source auto
func newComfyReplayCmd(flags *rootFlags) *cobra.Command {
	var (
		newName      string
		waitTimeout  time.Duration
		pollInterval time.Duration
		noWait       bool
		force        bool
	)
	cmd := &cobra.Command{
		Use:   "replay [prompt-id|name]",
		Short: "Re-run a stored graph and report an attributed before/after delta",
		Long: `Re-run a stored graph and report an attributed before/after delta.

The graph is reconstructed from the local store — not from /history, which ComfyUI keeps in
RAM and destroys on restart — so a run archived months ago is still replayable.

The report always states what each side ran under: server identity and launch argv, then and
now. When the original run has no stored argv the delta is reported as NOT ATTRIBUTABLE
rather than as a percentage, because a duration change with nothing recorded about its
conditions is exactly how a false "+49% regression" gets published.`,
		Example: `  comfyui-pp-cli replay 550e8400-e29b-41d4-a716-446655440000
  comfyui-pp-cli replay nightly-baseline --json
  comfyui-pp-cli replay 550e8400 --no-wait`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "replay")
			}
			// comfyRequiresInput, not cmd.Help(): help on stdout with exit 0 is a
			// success-shaped non-JSON answer, which silently breaks --json/--agent
			// parsing. This matches the sibling contract in `provenance` and `stage` —
			// a human still gets help, a machine caller gets a JSON usage error.
			if len(args) == 0 {
				return comfyRequiresInput(cmd, flags)
			}
			if len(args) > 1 {
				return usageErr(fmt.Errorf("replay takes exactly one prompt-id or run name (got %d)", len(args)))
			}
			if expVerifyShortCircuit() {
				return noopOK(writeNoop(cmd.OutOrStdout(), flags, "verify_short_circuit", "verify mode: refusing to queue a real render"))
			}
			ref := strings.TrimSpace(args[0])

			s, err := expOpenDomainStore(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			db := s.DB()

			original, err := replayResolveRun(cmd.Context(), db, ref)
			if err != nil {
				return err
			}
			graph, err := replayLoadGraph(cmd.Context(), db, original.GraphSHA)
			if err != nil {
				return err
			}
			expWarnHostPaths(graph, cmd.ErrOrStderr())

			beforeArgv, beforeArgvKnown := replayArgvForRun(cmd.Context(), db, original)
			before := exp.Side{
				PromptID:    original.PromptID,
				Name:        original.Name,
				ServerID:    original.ServerID,
				Argv:        beforeArgv,
				ArgvKnown:   beforeArgvKnown,
				DurationMS:  original.DurationMS,
				HasDuration: original.HasDuration,
				State:       original.State,
				SubmittedAt: original.SubmittedAt,
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}
			identity, serverID, err := expProbeServer(cmd.Context(), c, db)
			if err != nil {
				return classifyAPIError(cmd.OutOrStdout(), err, flags)
			}

			report := replayReport{Ref: ref, GraphSHA: original.GraphSHA, ShapeSHA: original.ShapeSHA}
			after := exp.Side{ServerID: serverID, Argv: identity.Argv, ArgvKnown: identity.ArgvKnown}

			promptID := ""
			if activeID, found, err := store.FindActiveRunByGraphSHA(cmd.Context(), db, original.GraphSHA); err != nil {
				return err
			} else if found && !force {
				// Attach rather than resubmit. ComfyUI dedupes nothing, so a second POST of
				// an identical graph is a second full render.
				promptID = activeID
				report.Attached = true
				fmt.Fprintf(cmd.ErrOrStderr(), "an identical graph is already in flight as %s; attaching instead of resubmitting (--force to submit anyway)\n", activeID)
			}

			if promptID == "" {
				shapeSHA := original.ShapeSHA
				if shapeSHA == "" {
					if shapeSHA, err = store.ShapeSHA(graph); err != nil {
						return err
					}
				}
				promptID, err = expNewPromptID()
				if err != nil {
					return err
				}
				argvJSON := ""
				if identity.ArgvKnown {
					if b, mErr := json.Marshal(identity.Argv); mErr == nil {
						argvJSON = string(b)
					}
				}
				if err := store.InsertRun(cmd.Context(), db, store.RunRow{
					PromptID: promptID,
					Name:     newName,
					GraphSHA: original.GraphSHA,
					ShapeSHA: shapeSHA,
					ServerID: serverID,
					State:    "submitted",
					BatchID:  sql.NullString{String: "replay:" + original.PromptID, Valid: true},
				}); err != nil {
					return err
				}
				// argv is recorded ON THE RUN, not just on the server row: it is the half of
				// the attribution that a future replay of THIS run will need.
				if _, err := db.ExecContext(cmd.Context(),
					`UPDATE run SET argv_json = NULLIF(?, '') WHERE prompt_id = ?`, argvJSON, promptID); err != nil {
					return err
				}

				submission, err := expSubmitGraph(cmd.Context(), c, graph, promptID)
				if err != nil {
					_ = store.SetRunState(cmd.Context(), db, promptID, "failed", "transport")
					return classifyAPIError(cmd.OutOrStdout(), err, flags)
				}
				if submission.PromptID != promptID {
					if _, err := db.ExecContext(cmd.Context(),
						`UPDATE run SET prompt_id = ? WHERE prompt_id = ?`, submission.PromptID, promptID); err != nil {
						return err
					}
					promptID = submission.PromptID
				}
				report.Outcome = submission.Class
				report.NodeErrors = submission.NodeErrors
				if len(submission.NodeErrors) > 0 {
					if _, err := db.ExecContext(cmd.Context(),
						`UPDATE run SET node_errors_json = ?, completeness = 'partial' WHERE prompt_id = ?`,
						string(submission.NodeErrors), promptID); err != nil {
						return err
					}
					expPrintVerbatim(cmd.ErrOrStderr(), "node_errors", submission.NodeErrors)
				}
				if submission.Class == exp.SubmitRejected || submission.Class == exp.SubmitUnrecognisable {
					if err := store.SetRunState(cmd.Context(), db, promptID, "failed", exp.ExitValidation); err != nil {
						return err
					}
					after.PromptID = promptID
					after.State = "failed"
					report.Delta = exp.AttributeDelta(before, after)
					if flags.asJSON {
						if printErr := printJSONFiltered(cmd.OutOrStdout(), report, flags); printErr != nil {
							return printErr
						}
					} else {
						replayRenderReport(cmd, report)
					}
					return apiErr(fmt.Errorf("replay was rejected at validation (HTTP %d); the stored graph no longer validates against this server", submission.Status))
				}
			}
			after.PromptID = promptID

			if noWait {
				report.Delta = exp.AttributeDelta(before, after)
				report.Delta.Caveats = append(report.Delta.Caveats,
					"--no-wait: the replay is still rendering, so there is no new duration yet; re-run 'replay' or read the run once it finishes")
				if flags.asJSON {
					return printJSONFiltered(cmd.OutOrStdout(), report, flags)
				}
				replayRenderReport(cmd, report)
				return nil
			}

			outcome, waitErr := expPollRun(cmd.Context(), c, promptID, waitTimeout, pollInterval, cmd.ErrOrStderr())
			if waitErr != nil {
				return waitErr
			}
			report.Waited = true
			state, exitClass, err := expFinaliseRun(cmd.Context(), db, promptID, outcome)
			if err != nil {
				return err
			}
			after.State = state
			if d, ok := outcome.DurationMS(); ok {
				after.DurationMS, after.HasDuration = d, true
			}
			// A fully cached replay measures ComfyUI's execution cache, not the render.
			after.CacheHit = len(outcome.CachedNodes) > 0 && len(outcome.CachedNodes) >= len(graph)
			report.Delta = exp.AttributeDelta(before, after)
			if exitClass != "" {
				report.Delta.Caveats = append(report.Delta.Caveats, "the replay ended in "+state+" ("+exitClass+")")
			}
			if original.ShapeSHA != "" {
				if stats, statsErr := store.ShapeStatsFor(cmd.Context(), db, original.ShapeSHA); statsErr == nil && stats.N > 0 {
					report.ShapeStats = &replayShapeStats{
						ShapeSHA: stats.ShapeSHA, N: stats.N, MeanMS: stats.MeanMS,
						StdDevMS: stats.StdDevMS, MinMS: stats.MinMS, MaxMS: stats.MaxMS,
					}
				}
			}

			if flags.asJSON {
				return printJSONFiltered(cmd.OutOrStdout(), report, flags)
			}
			replayRenderReport(cmd, report)
			return nil
		},
	}
	cmd.Flags().StringVar(&newName, "name", "", "Name to record the replay run under")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", expDefaultArmTimeout, "Maximum wait for the replay to finish")
	cmd.Flags().DurationVar(&pollInterval, "poll-interval", expDefaultPollInterval, "How often to poll /history while the replay renders")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Submit and return the handle without waiting for the delta")
	cmd.Flags().BoolVar(&force, "force", false, "Submit even when an identical graph is already in flight")
	return cmd
}

// replayRenderReport prints the human-facing delta. Attribution comes FIRST when the delta
// cannot be attributed, so the number is never read without its disclaimer.
func replayRenderReport(cmd *cobra.Command, report replayReport) {
	w := cmd.OutOrStdout()
	d := report.Delta

	fmt.Fprintf(w, "\nreplay of graph %s\n\n", expShortSHA(report.GraphSHA))
	rows := [][]string{
		{"prompt_id", orDashText(d.Before.PromptID), orDashText(d.After.PromptID)},
		{"state", orDashText(d.Before.State), orDashText(d.After.State)},
		{"duration", replayDurationCell(d.Before), replayDurationCell(d.After)},
		{"server", orDashText(d.Before.ServerID), orDashText(d.After.ServerID)},
		{"argv", replayArgvCell(d.Before), replayArgvCell(d.After)},
	}
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "\tTHEN\tNOW")
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", row[0], row[1], row[2])
	}
	_ = tw.Flush()

	fmt.Fprintln(w)
	if d.HasDelta {
		label := fmt.Sprintf("delta: %s (%+.1f%%)", exp.FormatSignedDuration(d.DeltaMS), d.PercentChange)
		switch {
		case !d.Attributable:
			fmt.Fprintln(w, yellow(label))
		case d.DeltaMS > 0:
			fmt.Fprintln(w, red(label))
		default:
			fmt.Fprintln(w, green(label))
		}
	} else {
		fmt.Fprintln(w, yellow("delta: unavailable"))
	}

	if d.Attributable {
		fmt.Fprintf(w, "attribution: %s\n", d.Attribution)
	} else {
		fmt.Fprintf(w, "%s %s\n", red("NOT ATTRIBUTABLE:"), d.Attribution)
	}
	for _, change := range d.ArgvChanges {
		fmt.Fprintf(w, "  %s\n", change)
	}
	for _, caveat := range d.Caveats {
		fmt.Fprintf(w, "  note: %s\n", caveat)
	}
	if report.ShapeStats != nil {
		st := report.ShapeStats
		fmt.Fprintf(w, "\nshape history (%d comparable runs): mean %s, min %s, max %s, stddev %s\n",
			st.N, exp.FormatDuration(int64(st.MeanMS)), exp.FormatDuration(st.MinMS),
			exp.FormatDuration(st.MaxMS), exp.FormatDuration(int64(st.StdDevMS)))
	}
}

func replayDurationCell(side exp.Side) string {
	if !side.HasDuration {
		return "not recorded"
	}
	out := exp.FormatDuration(side.DurationMS)
	if side.CacheHit {
		out += " (cache hit)"
	}
	return out
}

func replayArgvCell(side exp.Side) string {
	if !side.ArgvKnown {
		return "NOT RECORDED"
	}
	if len(side.Argv) == 0 {
		return "(none)"
	}
	return strings.Join(side.Argv, " ")
}

func orDashText(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
