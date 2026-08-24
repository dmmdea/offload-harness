// ComfyUI submit + attach: the idempotent submission lease.
//
// NOT generated — hand-written and preserved across regeneration.
//
// ComfyUI dedupes NOTHING. Every POST to /prompt mints a new prompt_id and starts a new
// render that can run for 20 minutes; a wrapper that resubmitted instead of waiting burned
// ~30 GPU-minutes on this box. `submit` therefore leases a submission on the graph's exact
// content hash: if an identical graph is already in flight, it prints that handle and exits
// 0 WITHOUT posting. `attach` is the same lookup on its own, plus a live liveness check.
//
// The run row is written BEFORE the POST, under a client-minted prompt_id, so a lost reply
// is recoverable by lookup instead of by guessing which job was ours.
package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"comfyui-pp-cli/internal/client"
	"comfyui-pp-cli/internal/cliutil"
	"comfyui-pp-cli/internal/comfy/submit"
	"comfyui-pp-cli/internal/store"
	"github.com/spf13/cobra"
)

// Typed exit codes for the submission outcomes. They sit above the framework's own range
// (2 usage, 3 not found, 4 auth, 5 api, 6 partial, 7 rate-limit, 10 config) and above the
// model-visibility codes (12, 13) so a caller can branch without parsing text.
func comfySubmitRejectedErr(err error) error { return &cliError{code: submit.ExitRejected, err: err} }
func comfySubmitPartialErr(err error) error {
	return &cliError{code: submit.ExitPartialAccept, err: err}
}
func comfySubmitMalformedErr(err error) error { return &cliError{code: submit.ExitMalformed, err: err} }

const comfySubmitStdinArg = "-"

// comfySubmitOutput is the envelope for `submit`, shared by the human and --json paths.
type comfySubmitOutput struct {
	Action    string `json:"action"`  // submitted | attached | rejected | partial-accept | malformed
	Outcome   string `json:"outcome"` // submit.Outcome, or "attached"
	Attached  bool   `json:"attached"`
	Submitted bool   `json:"submitted"`
	PromptID  string `json:"prompt_id,omitempty"`

	GraphSHA  string `json:"graph_sha,omitempty"`
	ShapeSHA  string `json:"shape_sha,omitempty"`
	NodeCount int    `json:"node_count,omitempty"`

	HTTPStatus  int    `json:"http_status,omitempty"`
	QueueNumber *int   `json:"queue_number,omitempty"`
	State       string `json:"state,omitempty"`
	SubmittedAt string `json:"submitted_at,omitempty"`
	AgeSeconds  int64  `json:"age_seconds,omitempty"`

	// NodeErrors is ComfyUI's node_errors value VERBATIM; NodeErrorsDetail is the
	// additive structured breakdown. Neither is ever a summary of the other.
	NodeErrors       json.RawMessage    `json:"node_errors,omitempty"`
	NodeErrorsDetail []submit.NodeError `json:"node_errors_detail,omitempty"`
	DroppedOutputs   []string           `json:"dropped_output_branches,omitempty"`

	ErrorType    string `json:"error_type,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	ErrorDetails string `json:"error_details,omitempty"`

	Reason   string                 `json:"reason,omitempty"`
	RawBody  string                 `json:"raw_body,omitempty"`
	Warnings []string               `json:"warnings,omitempty"`
	Hint     string                 `json:"hint,omitempty"`
	Wait     *comfySubmitWaitResult `json:"wait,omitempty"`
	ExitCode int                    `json:"exit_code"`
}

// comfySubmitWaitResult reports the outcome of --wait.
//
// TIMING RULE: the two timestamps come ONLY from /history's execution_start and
// execution_success messages. The server log's "Prompt executed in N seconds" line is stale
// mid-run (it once produced a false "+49% regression") and an s/it sample is a transient,
// not a rate; neither is ever consulted.
type comfySubmitWaitResult struct {
	Waited        bool   `json:"waited"`
	TimedOut      bool   `json:"timed_out,omitempty"`
	Found         bool   `json:"found"`
	StatusStr     string `json:"status_str,omitempty"`
	Completed     bool   `json:"completed"`
	StartMS       int64  `json:"execution_start_ms,omitempty"`
	SuccessMS     int64  `json:"execution_success_ms,omitempty"`
	DurationMS    int64  `json:"duration_ms,omitempty"`
	OutputNodes   int    `json:"output_nodes,omitempty"`
	Failed        bool   `json:"failed,omitempty"`
	ErrorNodeID   string `json:"error_node_id,omitempty"`
	ErrorNodeType string `json:"error_node_type,omitempty"`
	ExceptionType string `json:"exception_type,omitempty"`
	ExceptionMsg  string `json:"exception_message,omitempty"`
	Note          string `json:"note,omitempty"`
}

// comfyAttachOutput is the envelope for `attach`.
type comfyAttachOutput struct {
	Found     bool   `json:"found"`
	Attached  bool   `json:"attached"`
	InFlight  bool   `json:"in_flight"`
	Ref       string `json:"ref"`
	RefKind   string `json:"ref_kind"`
	PromptID  string `json:"prompt_id,omitempty"`
	Name      string `json:"name,omitempty"`
	State     string `json:"state,omitempty"`
	ExitClass string `json:"exit_class,omitempty"`

	GraphSHA     string `json:"graph_sha,omitempty"`
	ShapeSHA     string `json:"shape_sha,omitempty"`
	SubmittedAt  string `json:"submitted_at,omitempty"`
	AgeSeconds   int64  `json:"age_seconds,omitempty"`
	DurationMS   int64  `json:"duration_ms,omitempty"`
	Completeness string `json:"completeness,omitempty"`

	LiveState     string `json:"live_state,omitempty"` // running | pending | history | unknown
	QueuePosition int    `json:"queue_position,omitempty"`

	Warnings []string               `json:"warnings,omitempty"`
	Hint     string                 `json:"hint,omitempty"`
	Wait     *comfySubmitWaitResult `json:"wait,omitempty"`
}

// comfySubmitRunRecord is the durable record of one submission, as stored.
type comfySubmitRunRecord struct {
	PromptID     string
	Name         string
	State        string
	ExitClass    string
	GraphSHA     string
	ShapeSHA     string
	SubmittedAt  string
	AgeSeconds   int64
	DurationMS   int64
	Completeness string
}

// comfySubmitActiveStates mirrors store.FindActiveRunByGraphSHA's definition of "in flight".
var comfySubmitActiveStates = map[string]bool{
	"submitted":                 true,
	"running":                   true,
	"completed-outputs-pending": true,
}

// ---------------------------------------------------------------------------
// submit
// ---------------------------------------------------------------------------

// pp:data-source live
func newSubmitCmd(flags *rootFlags) *cobra.Command {
	var (
		name         string
		wait         bool
		waitTimeout  time.Duration
		pollInterval time.Duration
		force        bool
		skipLint     bool
	)
	cmd := &cobra.Command{
		Use:   "submit <graph.json>",
		Short: "Queue an API-format graph — attaching to an identical in-flight run instead of resubmitting",
		Long: `POST an API-format graph to /prompt, guarded by an idempotent submission lease.

ComfyUI dedupes nothing: every POST mints a new prompt_id and starts another render that can
run for 20 minutes. So before posting, submit hashes the graph (exact content identity) and
asks the local store whether that graph is already in flight. If it is, submit prints that
prompt_id and exits 0 WITHOUT posting — re-running the same command is safe and free.

The run row is written under a client-minted prompt_id BEFORE the POST, so a dropped reply is
recovered with 'attach <prompt_id>', never by submitting again.

Submission is ASYNC: the handle comes back immediately. Pass --wait to block until the run
reaches a terminal state and record its authoritative timing (execution_start ->
execution_success from /history; the server log's "Prompt executed in N seconds" line and s/it
samples are never used).

Accepting is not binary. ComfyUI validates each output branch independently and only returns
400 when NO branch survives, so a 200 can carry a non-empty node_errors map with some outputs
silently dropped. That case is reported as PARTIAL ACCEPT with its own exit code, never as
success, and node_errors is always printed verbatim alongside the structured breakdown.

Exit codes:
  0   accepted, or attached to an identical in-flight run
  2   usage, unreadable graph, or a graph that fails the pre-submit lint
  21  rejected: the server queued nothing (validation)
  22  partial accept: some output branches were queued, others were dropped
  23  malformed reply: HTTP 2xx with no prompt_id — there is no handle to attach to`,
		Example: `  comfyui-pp-cli submit graph.json
  comfyui-pp-cli submit graph.json --name "wan-i2v arm B" --wait
  comfyui-pp-cli submit graph.json --json
  cat graph.json | comfyui-pp-cli submit -
  comfyui-pp-cli submit graph.json --force   # deliberately render the same graph twice`,
		Annotations: map[string]string{
			"pp:endpoint":         "prompt.post",
			"pp:method":           "POST",
			"pp:path":             "/prompt",
			"pp:typed-exit-codes": "0,2,21,22,23",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validation lives inside RunE (never in Args/MarkFlagRequired) so the
			// harness's --dry-run probe reaches a short-circuit instead of tripping a
			// pre-RunE check.
			if len(args) == 0 {
				if dryRunOK(flags) {
					return writeDryRun(cmd.OutOrStdout(), flags, "submit")
				}
				return comfySubmitRequiresInput(cmd, flags, "a graph file")
			}
			if len(args) > 1 {
				return usageErr(fmt.Errorf("submit takes exactly one graph file (got %d arguments)", len(args)))
			}
			if flags.dataSource == "local" {
				return usageErr(errors.New("submit always dials the live ComfyUI server; --data-source local is incompatible"))
			}

			raw, err := comfySubmitReadGraph(cmd, args[0])
			if err != nil {
				return usageErr(err)
			}
			graph, graphJSON, err := submit.ParseGraph(raw)
			if err != nil {
				return usageErr(err)
			}
			identity, err := submit.Identify(graph)
			if err != nil {
				return err
			}
			if !skipLint {
				if findings := submit.Lint(graph); len(findings) > 0 {
					return comfySubmitLintError(cmd, flags, findings)
				}
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags,
					fmt.Sprintf("POST /prompt with %s (%d nodes, graph_sha %s)", args[0], identity.NodeCount, submit.ShortSHA(identity.GraphSHA)))
			}
			// Verify mode short-circuits before any local write, so a verification run
			// never leaves a phantom run row holding the lease.
			if cliutil.IsVerifyEnv() && !cliutil.IsVerifyLiveHTTPEnv() {
				return noopOK(writeNoop(cmd.OutOrStdout(), flags, "verify_short_circuit", "verify mode: no POST /prompt was issued and no run was recorded"))
			}

			ctx := cmd.Context()
			s, err := comfySubmitOpenStore(ctx)
			if err != nil {
				return err
			}
			defer s.Close()
			db := s.DB()

			// ---- the lease ------------------------------------------------
			active, found, err := store.FindActiveRunByGraphSHA(ctx, db, identity.GraphSHA)
			if err != nil {
				return err
			}
			decision := submit.DecideLease(active, found, force)
			if decision.Attach {
				out := comfySubmitOutput{
					Action:    "attached",
					Outcome:   "attached",
					Attached:  true,
					Submitted: false,
					PromptID:  decision.PromptID,
					GraphSHA:  identity.GraphSHA,
					ShapeSHA:  identity.ShapeSHA,
					NodeCount: identity.NodeCount,
					Reason:    decision.Reason,
					Hint:      "no new render was queued. ComfyUI dedupes nothing, so a second POST would have started a second render of the same graph. Use --force to do that deliberately.",
				}
				if rec, ok, recErr := comfySubmitLoadRun(ctx, db, decision.PromptID); recErr == nil && ok {
					out.State, out.SubmittedAt, out.AgeSeconds = rec.State, rec.SubmittedAt, rec.AgeSeconds
				}
				if wait {
					c, clientErr := flags.newClient()
					if clientErr != nil {
						return clientErr
					}
					waitOut, waitErr := comfySubmitWaitForRun(ctx, cmd, c, db, decision.PromptID, pollInterval, waitTimeout)
					if waitErr != nil {
						return waitErr
					}
					out.Wait = &waitOut
				}
				return comfySubmitRender(cmd, flags, out)
			}

			// ---- record BEFORE posting -------------------------------------
			promptID, err := submit.NewPromptID()
			if err != nil {
				return err
			}
			if _, err := store.UpsertGraph(ctx, db, graph, "", nil); err != nil {
				return err
			}
			if err := store.InsertRun(ctx, db, store.RunRow{
				PromptID:     promptID,
				Name:         name,
				GraphSHA:     identity.GraphSHA,
				ShapeSHA:     identity.ShapeSHA,
				State:        "submitted",
				Completeness: "full",
			}); err != nil {
				if name != "" && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
					return usageErr(fmt.Errorf("run name %q is already taken by another run: %w", name, err))
				}
				return err
			}

			// ---- node-set fingerprint ---------------------------------------
			// Which node classes and custom packs the server offered, captured
			// alongside the run so "same server, different result" has an
			// explanation to point at later (comfy_nodeset.go).
			//
			// Reads the cached schema, so this adds NO network round trip to
			// the submit path, and fails open: a run that cannot be
			// fingerprinted is still a run, and refusing to submit over a
			// missing provenance detail would be a far worse trade.
			if err := comfyCaptureNodeSetForRun(ctx, flags, db, promptID, ""); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: node-set capture skipped: %v\n", err)
			}

			// ---- POST -------------------------------------------------------
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			var warnings []string
			data, status, postErr := c.Post(ctx, "/prompt", submit.BuildRequest(graphJSON, promptID))

			var res submit.Result
			var apiError *client.APIError
			switch {
			case postErr != nil && errors.As(postErr, &apiError):
				res = submit.Classify(apiError.StatusCode, []byte(apiError.Body))
				if comfySubmitBodyTruncated(apiError.Body) {
					warnings = append(warnings, "the HTTP client truncated the error body at 4096 bytes: the verbatim node_errors below may be clipped")
				}
			case postErr != nil:
				// Ambiguous transport failure: the POST may or may not have landed.
				// The run stays 'submitted' so the lease keeps blocking a blind
				// resubmit — a stuck lease costs one --force, a double submit costs
				// 20 GPU-minutes.
				_ = store.SetRunState(ctx, db, promptID, "submitted", "transport-error")
				return apiErr(fmt.Errorf("POST /prompt failed after the run was recorded as %s: %w\n"+
					"the submission may or may not have landed — run 'comfyui-pp-cli attach %s' before submitting again",
					promptID, postErr, promptID))
			default:
				res = submit.Classify(status, data)
			}

			// ComfyUI honours a client-supplied prompt_id; a different one coming back
			// means the handle we recorded is not the handle that will run. Re-key the
			// row rather than orphan it.
			if res.PromptID != "" && res.PromptID != promptID {
				warnings = append(warnings, fmt.Sprintf("server returned prompt_id %s instead of the minted %s; the run row was re-keyed to the server's id", res.PromptID, promptID))
				if _, err := db.ExecContext(ctx, `UPDATE OR IGNORE run SET prompt_id = ? WHERE prompt_id = ?`, res.PromptID, promptID); err != nil {
					warnings = append(warnings, "re-keying the run row failed: "+err.Error())
				} else {
					promptID = res.PromptID
				}
			}
			if err := comfySubmitRecordResult(ctx, db, promptID, res); err != nil {
				warnings = append(warnings, "recording the submission outcome failed: "+err.Error())
			}

			out := comfySubmitOutput{
				Action:           string(res.Outcome),
				Outcome:          string(res.Outcome),
				Submitted:        true,
				PromptID:         promptID,
				GraphSHA:         identity.GraphSHA,
				ShapeSHA:         identity.ShapeSHA,
				NodeCount:        identity.NodeCount,
				HTTPStatus:       res.HTTPStatus,
				QueueNumber:      res.QueueNumber,
				NodeErrors:       res.NodeErrorsRaw,
				NodeErrorsDetail: res.NodeErrors,
				DroppedOutputs:   res.DroppedOutputs,
				ErrorType:        res.ErrorType,
				ErrorMessage:     res.ErrorMessage,
				ErrorDetails:     res.ErrorDetails,
				Reason:           res.Reason,
				RawBody:          res.RawBody,
				Warnings:         warnings,
				ExitCode:         res.Outcome.ExitCode(),
			}
			if res.Outcome == submit.OutcomeAccepted {
				out.Action = "submitted"
				out.State = "submitted"
				out.Hint = fmt.Sprintf("async: the render runs on the server. Follow it with 'comfyui-pp-cli attach %s --wait'. Re-running this submit attaches instead of queueing a second render.", promptID)
			}

			if wait && (res.Outcome == submit.OutcomeAccepted || res.Outcome == submit.OutcomePartialAccept) {
				waitOut, waitErr := comfySubmitWaitForRun(ctx, cmd, c, db, promptID, pollInterval, waitTimeout)
				if waitErr != nil {
					return waitErr
				}
				out.Wait = &waitOut
			}

			if err := comfySubmitRender(cmd, flags, out); err != nil {
				return err
			}
			return comfySubmitOutcomeError(res, promptID)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Human label recorded on the run row (must be unique across runs)")
	cmd.Flags().BoolVar(&wait, "wait", false, "Block until the run reaches a terminal state and record its authoritative timing")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 30*time.Minute, "Give up waiting after this long (the run keeps going server-side)")
	cmd.Flags().DurationVar(&pollInterval, "poll-interval", 2*time.Second, "How often --wait polls /history")
	cmd.Flags().BoolVar(&force, "force", false, "Submit even when an identical graph is already in flight (starts a SECOND render)")
	cmd.Flags().BoolVar(&skipLint, "skip-lint", false, "Skip the pre-submit graph lint")
	return cmd
}

// ---------------------------------------------------------------------------
// attach
// ---------------------------------------------------------------------------

// pp:data-source auto
func newAttachCmd(flags *rootFlags) *cobra.Command {
	var (
		wait         bool
		waitTimeout  time.Duration
		pollInterval time.Duration
	)
	cmd := &cobra.Command{
		Use:   "attach <graph.json|graph_sha|prompt_id>",
		Short: "Attach to an in-flight run — never submits anything",
		Long: `Resolve a reference to a recorded run and report where it stands, without POSTing.

The reference is resolved by shape (an existing file always wins):
  graph.json   the graph is hashed and its in-flight run is looked up by graph_sha
  graph_sha    a full 64-char hash or a >=8-char prefix
  prompt_id    a UUID

Local state is authoritative for the run's identity; the live server is then asked where the
prompt actually sits (running, queued at position N, or already in /history). A server that
cannot be reached degrades to the local record with a warning rather than failing.

Use this after a dropped connection, an interrupted submit, or any time the question is
"is it still running?" — the answer must never be another POST.

Exit codes:
  0   a matching run was found
  2   the reference could not be interpreted
  3   no run matches the reference`,
		Example: `  comfyui-pp-cli attach 550e8400-e29b-41d4-a716-446655440000
  comfyui-pp-cli attach graph.json --wait
  comfyui-pp-cli attach 9f2c1ab4 --json`,
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:endpoint":         "history.get",
			"pp:method":           "GET",
			"pp:path":             "/history/{prompt_id}",
			"pp:typed-exit-codes": "0,2,3",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				if dryRunOK(flags) {
					return writeDryRun(cmd.OutOrStdout(), flags, "attach")
				}
				return comfySubmitRequiresInput(cmd, flags, "a graph file, graph_sha, or prompt_id")
			}
			if len(args) > 1 {
				return usageErr(fmt.Errorf("attach takes exactly one reference (got %d arguments)", len(args)))
			}
			ref := strings.TrimSpace(args[0])
			kind, err := submit.ClassifyRef(ref, comfySubmitFileExists)
			if err != nil {
				return usageErr(err)
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, fmt.Sprintf("attach to the run for %s (%s)", ref, kind))
			}

			ctx := cmd.Context()
			out := comfyAttachOutput{Ref: ref, RefKind: string(kind)}

			var graphSHA, promptID string
			switch kind {
			case submit.RefFile:
				raw, readErr := comfySubmitReadGraph(cmd, ref)
				if readErr != nil {
					return usageErr(readErr)
				}
				graph, _, parseErr := submit.ParseGraph(raw)
				if parseErr != nil {
					return usageErr(parseErr)
				}
				identity, idErr := submit.Identify(graph)
				if idErr != nil {
					return idErr
				}
				graphSHA = identity.GraphSHA
				out.GraphSHA, out.ShapeSHA = identity.GraphSHA, identity.ShapeSHA
			case submit.RefGraphSHA:
				graphSHA = strings.ToLower(ref)
			case submit.RefPromptID:
				promptID = ref
			}

			s, err := comfySubmitOpenStore(ctx)
			if err != nil {
				return err
			}
			defer s.Close()
			db := s.DB()

			if promptID == "" {
				resolved, resolveErr := comfySubmitResolveGraphSHA(ctx, db, graphSHA)
				if resolveErr != nil {
					return resolveErr
				}
				graphSHA = resolved
				out.GraphSHA = resolved
				active, found, findErr := store.FindActiveRunByGraphSHA(ctx, db, graphSHA)
				if findErr != nil {
					return findErr
				}
				if found {
					promptID = active
				} else {
					latest, latestOK, latestErr := comfySubmitLatestRunForGraph(ctx, db, graphSHA)
					if latestErr != nil {
						return latestErr
					}
					if !latestOK {
						return notFoundErr(fmt.Errorf("no run recorded for graph_sha %s — nothing to attach to; submit it with 'comfyui-pp-cli submit <graph.json>'", submit.ShortSHA(graphSHA)))
					}
					promptID = latest
				}
			}

			rec, ok, err := comfySubmitLoadRun(ctx, db, promptID)
			if err != nil {
				return err
			}
			if !ok {
				return notFoundErr(fmt.Errorf("no run recorded for prompt_id %s — this CLI has no record of that submission", promptID))
			}
			out.Found = true
			out.PromptID = rec.PromptID
			out.Name = rec.Name
			out.State = rec.State
			out.ExitClass = rec.ExitClass
			out.SubmittedAt = rec.SubmittedAt
			out.AgeSeconds = rec.AgeSeconds
			out.DurationMS = rec.DurationMS
			out.Completeness = rec.Completeness
			if out.GraphSHA == "" {
				out.GraphSHA = rec.GraphSHA
			}
			if out.ShapeSHA == "" {
				out.ShapeSHA = rec.ShapeSHA
			}
			// The LOCAL record decides whether waiting is still meaningful: a run this
			// CLI has not finalised has no recorded timing yet, even when the server
			// already finished it. Captured before the live check, which may clear
			// InFlight.
			localInFlight := comfySubmitActiveStates[rec.State]
			out.InFlight = localInFlight
			out.Attached = localInFlight

			// Live liveness check. Read-only, cheap on loopback, and the difference
			// between "the lease is real" and "the lease is stale".
			if flags.dataSource != "local" {
				c, clientErr := flags.newClient()
				if clientErr != nil {
					out.Warnings = append(out.Warnings, "live check skipped: "+clientErr.Error())
				} else {
					comfySubmitAnnotateLive(ctx, c, &out)
					if wait && localInFlight {
						waitOut, waitErr := comfySubmitWaitForRun(ctx, cmd, c, db, rec.PromptID, pollInterval, waitTimeout)
						if waitErr != nil {
							return waitErr
						}
						out.Wait = &waitOut
						// The wait may have finalised the row (state + the
						// authoritative timing); report what is stored now, not the
						// snapshot taken before waiting.
						if refreshed, ok, refreshErr := comfySubmitLoadRun(ctx, db, rec.PromptID); refreshErr == nil && ok {
							out.State, out.ExitClass, out.DurationMS = refreshed.State, refreshed.ExitClass, refreshed.DurationMS
							out.Completeness = refreshed.Completeness
							out.InFlight = comfySubmitActiveStates[refreshed.State]
							out.Attached = out.InFlight
						}
					}
				}
			}

			switch {
			case out.InFlight:
				out.Hint = "still in flight — wait for it (--wait), do NOT submit the same graph again"
			case out.LiveState == "history":
				out.Hint = "already finished: read its outputs with 'comfyui-pp-cli history get --prompt-id " + rec.PromptID + "'"
			default:
				out.Hint = "this run is no longer in flight; submitting the same graph again will queue a new render"
			}

			if wantsHumanTable(cmd.OutOrStdout(), flags) {
				comfySubmitRenderAttachHuman(cmd.OutOrStdout(), out)
				return nil
			}
			return printJSONFiltered(cmd.OutOrStdout(), out, flags)
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "Block until the attached run reaches a terminal state")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 30*time.Minute, "Give up waiting after this long (the run keeps going server-side)")
	cmd.Flags().DurationVar(&pollInterval, "poll-interval", 2*time.Second, "How often --wait polls /history")
	return cmd
}

// ---------------------------------------------------------------------------
// store access
// ---------------------------------------------------------------------------

func comfySubmitOpenStore(ctx context.Context) (*store.Store, error) {
	s, err := store.OpenWithContext(ctx, defaultDBPath("comfyui-pp-cli"))
	if err != nil {
		return nil, configErr(fmt.Errorf("opening the local run store: %w", err))
	}
	if err := store.MigrateComfyUI(ctx, s.DB()); err != nil {
		_ = s.Close()
		return nil, configErr(fmt.Errorf("preparing the ComfyUI domain schema: %w", err))
	}
	return s, nil
}

// comfySubmitLoadRun reads one run row. Age is computed in SQL so the DATETIME text written
// by SQLite's CURRENT_TIMESTAMP never has to be parsed (and mis-parsed) in Go.
func comfySubmitLoadRun(ctx context.Context, db *sql.DB, promptID string) (comfySubmitRunRecord, bool, error) {
	var (
		rec                                       comfySubmitRunRecord
		name, exitClass, graphSHA, shapeSHA, comp sql.NullString
		submittedAt                               sql.NullString
		age, duration                             sql.NullInt64
	)
	err := db.QueryRowContext(ctx, `
		SELECT prompt_id, name, state, exit_class, graph_sha, shape_sha,
		       COALESCE(submitted_at, ''),
		       CAST(COALESCE(strftime('%s','now') - strftime('%s', submitted_at), 0) AS INTEGER),
		       duration_ms, completeness
		  FROM run
		 WHERE prompt_id = ?`, promptID).
		Scan(&rec.PromptID, &name, &rec.State, &exitClass, &graphSHA, &shapeSHA, &submittedAt, &age, &duration, &comp)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, false, nil
	}
	if err != nil {
		return rec, false, fmt.Errorf("reading run %s: %w", promptID, err)
	}
	rec.Name = name.String
	rec.ExitClass = exitClass.String
	rec.GraphSHA = graphSHA.String
	rec.ShapeSHA = shapeSHA.String
	rec.SubmittedAt = submittedAt.String
	rec.AgeSeconds = age.Int64
	rec.DurationMS = duration.Int64
	rec.Completeness = comp.String
	return rec, true, nil
}

// comfySubmitResolveGraphSHA expands a graph_sha prefix against recorded runs.
func comfySubmitResolveGraphSHA(ctx context.Context, db *sql.DB, prefix string) (string, error) {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if len(prefix) == 64 {
		return prefix, nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT graph_sha FROM run WHERE graph_sha LIKE ? || '%' ORDER BY graph_sha LIMIT 5`, prefix)
	if err != nil {
		return "", fmt.Errorf("resolving graph_sha prefix: %w", err)
	}
	defer rows.Close()
	var matches []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return "", err
		}
		matches = append(matches, sha)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		return "", notFoundErr(fmt.Errorf("no recorded run has a graph_sha starting with %q", prefix))
	case 1:
		return matches[0], nil
	default:
		return "", usageErr(fmt.Errorf("graph_sha prefix %q is ambiguous across %d runs (%s...) — pass more characters",
			prefix, len(matches), strings.Join(comfySubmitShortAll(matches), ", ")))
	}
}

func comfySubmitLatestRunForGraph(ctx context.Context, db *sql.DB, graphSHA string) (string, bool, error) {
	var promptID string
	err := db.QueryRowContext(ctx,
		`SELECT prompt_id FROM run WHERE graph_sha = ? ORDER BY submitted_at DESC LIMIT 1`, graphSHA).Scan(&promptID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("looking up the latest run for a graph: %w", err)
	}
	return promptID, true, nil
}

// comfySubmitRecordResult stores the classified outcome on the run row.
//
// A MALFORMED reply deliberately leaves the run 'submitted': the server may still have
// queued the graph, and keeping the lease costs one --force while releasing it costs a
// duplicate 20-minute render.
func comfySubmitRecordResult(ctx context.Context, db *sql.DB, promptID string, res submit.Result) error {
	state, exitClass, completeness := "submitted", "", "full"
	switch res.Outcome {
	case submit.OutcomeAccepted:
		exitClass = "queued"
	case submit.OutcomePartialAccept:
		exitClass, completeness = "partial-accept", "partial"
	case submit.OutcomeRejected:
		state, exitClass, completeness = "rejected", "validation-rejected", "none"
	default:
		exitClass = "malformed-response"
	}

	var nodeErrors interface{}
	if len(res.NodeErrorsRaw) > 0 {
		nodeErrors = string(res.NodeErrorsRaw)
	}
	var errorNodeID, errorNodeType interface{}
	if len(res.NodeErrors) > 0 {
		errorNodeID = res.NodeErrors[0].NodeID
		if res.NodeErrors[0].ClassType != "" {
			errorNodeType = res.NodeErrors[0].ClassType
		}
	}
	var excType, excMsg interface{}
	if res.ErrorType != "" {
		excType = res.ErrorType
	}
	if res.ErrorMessage != "" {
		excMsg = res.ErrorMessage
	}

	_, err := db.ExecContext(ctx, `
		UPDATE run
		   SET state = ?, exit_class = ?, completeness = ?,
		       node_errors_json = COALESCE(?, node_errors_json),
		       error_node_id = COALESCE(?, error_node_id),
		       error_node_type = COALESCE(?, error_node_type),
		       error_exception_type = COALESCE(?, error_exception_type),
		       error_exception_message = COALESCE(?, error_exception_message)
		 WHERE prompt_id = ?`,
		state, exitClass, completeness, nodeErrors, errorNodeID, errorNodeType, excType, excMsg, promptID)
	if err != nil {
		return fmt.Errorf("recording submission outcome: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// waiting
// ---------------------------------------------------------------------------

// comfySubmitWaitForRun polls /history until the prompt reaches a terminal state.
//
// Polling (rather than the websocket feed) keeps the wait honest about restarts: ComfyUI's
// history is in RAM, so a server that died mid-render answers "not found" forever and the
// wait ends on its own deadline instead of hanging on a socket that never speaks again.
func comfySubmitWaitForRun(ctx context.Context, cmd *cobra.Command, c *client.Client, db *sql.DB, promptID string, interval, timeout time.Duration) (comfySubmitWaitResult, error) {
	out := comfySubmitWaitResult{Waited: true}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	path := "/history/" + url.PathEscape(promptID)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	warned, markedRunning := false, false

	for {
		body, err := c.GetNoCache(waitCtx, path, nil)
		if err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			if waitCtx.Err() == nil && !warned {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: polling /history failed (%v); still waiting\n", err)
				warned = true
			}
		} else {
			status, parseErr := submit.ParseHistory(body, promptID)
			if parseErr != nil {
				if !warned {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v; still waiting\n", parseErr)
					warned = true
				}
			} else {
				if status.Found && status.Started && !status.Terminal && !markedRunning {
					markedRunning = true
					_ = store.SetRunState(ctx, db, promptID, "running", "")
				}
				if status.Found && status.Terminal {
					return comfySubmitFinishRun(ctx, cmd, db, promptID, status), nil
				}
			}
		}

		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			out.TimedOut = true
			out.Note = fmt.Sprintf("still running after %s — the render continues server-side; re-attach with 'comfyui-pp-cli attach %s --wait'", timeout, promptID)
			return out, nil
		case <-ticker.C:
		}
	}
}

// comfySubmitFinishRun records the authoritative timing and the terminal state.
func comfySubmitFinishRun(ctx context.Context, cmd *cobra.Command, db *sql.DB, promptID string, status submit.HistoryStatus) comfySubmitWaitResult {
	out := comfySubmitWaitResult{
		Waited:        true,
		Found:         true,
		StatusStr:     status.StatusStr,
		Completed:     status.Completed,
		StartMS:       status.StartMS,
		SuccessMS:     status.SuccessMS,
		DurationMS:    status.DurationMS(),
		OutputNodes:   status.OutputNodeCount,
		Failed:        status.Failed(),
		ErrorNodeID:   status.ErrorNodeID,
		ErrorNodeType: status.ErrorNodeType,
		ExceptionType: status.ExceptionType,
		ExceptionMsg:  status.ExceptionMsg,
	}
	if status.TimingInverted {
		out.Note = "execution_success precedes execution_start in /history; no duration was recorded (a wrong duration is worse than a missing one)"
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", out.Note)
	} else if status.StartMS > 0 || status.SuccessMS > 0 {
		if err := store.SetRunTiming(ctx, db, promptID, status.StartMS, status.SuccessMS); err != nil {
			out.Note = err.Error()
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
		}
	}
	switch {
	case status.Failed():
		_ = store.SetRunState(ctx, db, promptID, "failed", "execution-error")
		if status.ErrorNodeID != "" || status.ExceptionMsg != "" {
			_, _ = db.ExecContext(ctx, `
				UPDATE run SET error_node_id = ?, error_node_type = ?,
				               error_exception_type = ?, error_exception_message = ?
				 WHERE prompt_id = ?`,
				comfySubmitNullable(status.ErrorNodeID), comfySubmitNullable(status.ErrorNodeType),
				comfySubmitNullable(status.ExceptionType), comfySubmitNullable(status.ExceptionMsg), promptID)
		}
	case strings.EqualFold(status.StatusStr, "interrupted"):
		_ = store.SetRunState(ctx, db, promptID, "interrupted", "interrupted")
	default:
		_ = store.SetRunState(ctx, db, promptID, "completed", "ok")
	}
	return out
}

// comfySubmitAnnotateLive asks the server where the prompt actually is. Failures degrade to
// a warning: a stale local record is still more useful than a hard error.
func comfySubmitAnnotateLive(ctx context.Context, c *client.Client, out *comfyAttachOutput) {
	body, err := c.GetNoCache(ctx, "/history/"+url.PathEscape(out.PromptID), nil)
	if err == nil {
		if status, parseErr := submit.ParseHistory(body, out.PromptID); parseErr == nil && status.Found {
			out.LiveState = "history"
			if status.Terminal {
				out.InFlight = false
				out.Attached = false
			}
			return
		}
	} else {
		out.Warnings = append(out.Warnings, "live /history check failed: "+err.Error())
	}

	queueBody, queueErr := c.GetNoCache(ctx, "/queue", nil)
	if queueErr != nil {
		out.Warnings = append(out.Warnings, "live /queue check failed: "+queueErr.Error())
		return
	}
	qs, parseErr := submit.ParseQueue(queueBody, out.PromptID)
	if parseErr != nil {
		out.Warnings = append(out.Warnings, parseErr.Error())
		return
	}
	if !qs.Found {
		out.LiveState = "unknown"
		if out.InFlight {
			out.Warnings = append(out.Warnings, "the local record says this run is in flight, but the server has it in neither /queue nor /history — the server was probably restarted (its history lives in RAM). Re-submit with --force if you still want this render.")
		}
		return
	}
	out.LiveState = qs.State
	out.QueuePosition = qs.Position
	out.InFlight = true
	out.Attached = true
}

// ---------------------------------------------------------------------------
// input + rendering
// ---------------------------------------------------------------------------

// comfySubmitReadGraph reads a graph from a path, or from stdin when the path is "-".
func comfySubmitReadGraph(cmd *cobra.Command, path string) ([]byte, error) {
	if path == comfySubmitStdinArg {
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, fmt.Errorf("reading the graph from stdin: %w", err)
		}
		return data, nil
	}
	data, err := os.ReadFile(filepath.Clean(path)) // #nosec G304 -- the path is the user's own argument.
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return data, nil
}

func comfySubmitFileExists(path string) bool {
	if path == comfySubmitStdinArg {
		return true
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func comfySubmitRequiresInput(cmd *cobra.Command, flags *rootFlags, what string) error {
	if flags != nil && flags.asJSON {
		if err := printJSONFiltered(cmd.OutOrStdout(), map[string]any{
			"error": "requires input",
			"needs": what,
			"usage": cmd.CommandPath() + " --help",
		}, flags); err != nil {
			return err
		}
		return usageErr(fmt.Errorf("%q requires %s; run %q for usage", cmd.CommandPath(), what, cmd.CommandPath()+" --help"))
	}
	return cmd.Help()
}

// comfySubmitLintError refuses a graph that is certain to fail server-side.
func comfySubmitLintError(cmd *cobra.Command, flags *rootFlags, findings []submit.Finding) error {
	if flags != nil && flags.asJSON {
		if err := printJSONFiltered(cmd.OutOrStdout(), map[string]any{
			"error":    "graph lint failed",
			"findings": findings,
			"hint":     "fix the graph, or pass --skip-lint to submit it anyway",
		}, flags); err != nil {
			return err
		}
	} else {
		w := cmd.ErrOrStderr()
		fmt.Fprintf(w, "%s graph lint failed — not submitted:\n", red("FAIL"))
		for _, f := range findings {
			fmt.Fprintf(w, "  node %s (%s): %s\n", f.NodeID, f.ClassType, f.Message)
			fmt.Fprintf(w, "    value: %s\n", f.Value)
		}
		fmt.Fprintf(w, "  pass --skip-lint to submit anyway.\n")
	}
	return usageErr(fmt.Errorf("graph lint failed: %d finding(s)", len(findings)))
}

// comfySubmitRender writes the envelope in whichever mode the caller asked for. The verbatim
// node_errors reach the user in BOTH modes — inside the JSON envelope, or on stderr.
func comfySubmitRender(cmd *cobra.Command, flags *rootFlags, out comfySubmitOutput) error {
	if wantsHumanTable(cmd.OutOrStdout(), flags) {
		comfySubmitRenderHuman(cmd, out)
		return nil
	}
	return printJSONFiltered(cmd.OutOrStdout(), out, flags)
}

func comfySubmitRenderHuman(cmd *cobra.Command, out comfySubmitOutput) {
	w := cmd.OutOrStdout()
	errw := cmd.ErrOrStderr()

	switch out.Outcome {
	case "attached":
		fmt.Fprintf(w, "%s attached to in-flight run %s\n", green("OK"), out.PromptID)
		if out.State != "" {
			fmt.Fprintf(w, "  state: %s", out.State)
			if out.AgeSeconds > 0 {
				fmt.Fprintf(w, "  (submitted %s ago)", comfySubmitDuration(out.AgeSeconds*1000))
			}
			fmt.Fprintln(w)
		}
	case string(submit.OutcomeAccepted):
		fmt.Fprintf(w, "%s submitted  prompt_id %s", green("OK"), out.PromptID)
		if out.QueueNumber != nil {
			fmt.Fprintf(w, "  queue #%d", *out.QueueNumber)
		}
		fmt.Fprintln(w)
	case string(submit.OutcomePartialAccept):
		fmt.Fprintf(w, "%s PARTIAL ACCEPT  prompt_id %s\n", yellow("WARN"), out.PromptID)
		fmt.Fprintf(w, "  ComfyUI queued some output branches and DROPPED others — this is not a clean run.\n")
		if len(out.DroppedOutputs) > 0 {
			fmt.Fprintf(w, "  dropped output branches: %s\n", strings.Join(out.DroppedOutputs, ", "))
		}
	case string(submit.OutcomeRejected):
		fmt.Fprintf(w, "%s rejected (HTTP %d) — nothing was queued\n", red("FAIL"), out.HTTPStatus)
	default:
		fmt.Fprintf(w, "%s malformed reply (HTTP %d) — no prompt_id, so there is no handle to attach to\n", red("FAIL"), out.HTTPStatus)
		if out.Reason != "" {
			fmt.Fprintf(w, "  %s\n", out.Reason)
		}
	}

	if out.GraphSHA != "" {
		fmt.Fprintf(w, "  graph_sha %s  shape_sha %s  nodes %d\n",
			submit.ShortSHA(out.GraphSHA), submit.ShortSHA(out.ShapeSHA), out.NodeCount)
	}

	// node_errors, verbatim first (never summarised), then the breakdown.
	if len(out.NodeErrors) > 0 {
		fmt.Fprintf(errw, "\nnode_errors (verbatim, exactly as ComfyUI returned it):\n%s\n", string(out.NodeErrors))
	}
	if report := submit.FormatReport(submit.Result{
		NodeErrors:   out.NodeErrorsDetail,
		ErrorType:    out.ErrorType,
		ErrorMessage: out.ErrorMessage,
		ErrorDetails: out.ErrorDetails,
	}); report != "" {
		fmt.Fprintf(errw, "\nbreakdown:\n%s", report)
	}
	if out.RawBody != "" {
		fmt.Fprintf(errw, "\nresponse body:\n%s\n", truncate(out.RawBody, 2000))
	}
	for _, warning := range out.Warnings {
		fmt.Fprintf(errw, "warning: %s\n", warning)
	}
	if out.Wait != nil {
		comfySubmitRenderWaitHuman(w, *out.Wait)
	}
	if out.Hint != "" {
		fmt.Fprintf(w, "  %s\n", out.Hint)
	}
}

func comfySubmitRenderWaitHuman(w io.Writer, wait comfySubmitWaitResult) {
	switch {
	case wait.TimedOut:
		fmt.Fprintf(w, "  %s wait timed out: %s\n", yellow("WARN"), wait.Note)
	case wait.Failed:
		fmt.Fprintf(w, "  %s run failed on node %s (%s): %s\n", red("FAIL"),
			orDashCLI(wait.ErrorNodeID), orDashCLI(wait.ErrorNodeType), orDashCLI(wait.ExceptionMsg))
	case wait.Found:
		fmt.Fprintf(w, "  %s finished in %s (%d output node(s))\n", green("OK"),
			comfySubmitDuration(wait.DurationMS), wait.OutputNodes)
	}
	if wait.Note != "" && !wait.TimedOut {
		fmt.Fprintf(w, "  note: %s\n", wait.Note)
	}
}

func comfySubmitRenderAttachHuman(w io.Writer, out comfyAttachOutput) {
	if !out.Found {
		fmt.Fprintf(w, "%s no run recorded for %s\n", red("FAIL"), out.Ref)
		return
	}
	indicator := green("OK")
	if !out.InFlight {
		indicator = yellow("INFO")
	}
	fmt.Fprintf(w, "%s %s  state %s", indicator, out.PromptID, out.State)
	if out.LiveState != "" {
		fmt.Fprintf(w, "  live %s", out.LiveState)
		if out.QueuePosition > 0 {
			fmt.Fprintf(w, " (#%d in queue)", out.QueuePosition)
		}
	}
	fmt.Fprintln(w)
	if out.Name != "" {
		fmt.Fprintf(w, "  name: %s\n", out.Name)
	}
	if out.SubmittedAt != "" {
		fmt.Fprintf(w, "  submitted: %s", out.SubmittedAt)
		if out.AgeSeconds > 0 {
			fmt.Fprintf(w, " (%s ago)", comfySubmitDuration(out.AgeSeconds*1000))
		}
		fmt.Fprintln(w)
	}
	if out.DurationMS > 0 {
		fmt.Fprintf(w, "  duration: %s\n", comfySubmitDuration(out.DurationMS))
	}
	if out.GraphSHA != "" {
		fmt.Fprintf(w, "  graph_sha %s  shape_sha %s\n", submit.ShortSHA(out.GraphSHA), submit.ShortSHA(out.ShapeSHA))
	}
	if out.Completeness != "" && out.Completeness != "full" {
		fmt.Fprintf(w, "  completeness: %s\n", out.Completeness)
	}
	for _, warning := range out.Warnings {
		fmt.Fprintf(w, "  %s %s\n", yellow("WARN"), warning)
	}
	if out.Wait != nil {
		comfySubmitRenderWaitHuman(w, *out.Wait)
	}
	if out.Hint != "" {
		fmt.Fprintf(w, "  %s\n", out.Hint)
	}
}

// comfySubmitOutcomeError converts a non-accepted outcome into the typed exit code.
func comfySubmitOutcomeError(res submit.Result, promptID string) error {
	switch res.Outcome {
	case submit.OutcomeAccepted:
		return nil
	case submit.OutcomePartialAccept:
		return comfySubmitPartialErr(fmt.Errorf("partial accept: ComfyUI queued %s but dropped output branch(es) %s — node_errors were printed above",
			promptID, strings.Join(res.DroppedOutputs, ", ")))
	case submit.OutcomeRejected:
		message := res.ErrorMessage
		if message == "" {
			message = fmt.Sprintf("HTTP %d", res.HTTPStatus)
		}
		return comfySubmitRejectedErr(fmt.Errorf("rejected, nothing was queued: %s", message))
	default:
		return comfySubmitMalformedErr(fmt.Errorf("malformed reply: %s", res.Reason))
	}
}

// comfySubmitBodyTruncated detects the HTTP client's 4096-byte error-body cap, which would
// otherwise clip the verbatim node_errors without saying so.
func comfySubmitBodyTruncated(body string) bool {
	return len(body) >= 4096 && strings.HasSuffix(body, "...")
}

func comfySubmitNullable(s string) interface{} {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func comfySubmitShortAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, submit.ShortSHA(s))
	}
	return out
}

func comfySubmitDuration(ms int64) string {
	if ms <= 0 {
		return "unknown"
	}
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func orDashCLI(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
