// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
//
// Hand-authored novel command surface (NOT generator output): the job lifecycle
// family — `jobs list`, `status`, `wait`, `sync-history`. (`timing`, which reads only
// the local run table, lives in comfy_timing.go.)
//
// FOUR REAL DEFECTS ARE DESIGNED OUT HERE.
//
//  1. TIMING HAS EXACTLY ONE SOURCE. Every duration printed or stored by these
//     commands comes from a /history entry's execution_start -> execution_success
//     timestamps, parsed by internal/comfy/history and written through
//     store.SetRunTiming. The server log's "Prompt executed in N seconds" line is a
//     STALE read mid-run (it describes the previous prompt) and once produced a
//     published "+49% regression" on a build that had got faster; an s/it progress
//     sample is an instantaneous rate, not a duration. Neither is read here, ever.
//
//  2. /history IS RAM. ComfyUI keeps it as `self.history = {}`, FIFO-evicted at
//     10000 entries and destroyed on restart — and memory-knob experiments are
//     exactly what force restarts. So `jobs list` merges the live view with local run
//     rows, and the local rows are the ones that survive. A prompt that is gone from
//     /history but present locally is shown, labelled, and still carries its timing.
//
//  3. THE /history LAG RACE. A prompt that has just finished can appear with
//     status_str "success" and an EMPTY outputs map, because ComfyUI publishes the
//     success message a beat before the outputs. `wait` treats that as a distinct
//     NON-terminal state and keeps polling with backoff; it never reports "done, no
//     outputs", and if the settle window expires it says exactly what it saw.
//
//  4. A LOST prompt_id IS A LOST RENDER. Local renders run 30 s to 20 min, so `wait`
//     has NO artificial default timeout. When --timeout is given and expires, the
//     command exits with its own distinct code and still prints the prompt_id, so the
//     job can always be re-attached with `status`/`wait` instead of resubmitted. (A
//     wrapper that resubmitted rather than re-attached burned ~30 GPU-minutes.)
//
// Data-source strategy is declared on each constructor below, not here: a leading
// "pp:data-source <word>" comment IS the directive, so this file's overview must not
// open a line with it.

package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"comfyui-pp-cli/internal/comfy/history"
	"comfyui-pp-cli/internal/store"
	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// Typed exit codes
// ---------------------------------------------------------------------------

// comfyExitWaitTimeout — `wait --timeout` expired while the job was still
// non-terminal. Distinct from a failure: the render is very probably still going.
const comfyExitWaitTimeout = 20

// comfyExitJobFailed — the job reached a terminal FAILED state (execution_error or
// interruption). The exception text is printed verbatim alongside.
const comfyExitJobFailed = 21

// comfyExitOutputsPending — the job recorded execution_success but /history still
// published no outputs when the settle window expired. Not a success, not a failure:
// an honest "the server has not told us what it made yet".
const comfyExitOutputsPending = 22

func comfyWaitTimeoutErr(err error) error { return &cliError{code: comfyExitWaitTimeout, err: err} }
func comfyJobFailedErr(err error) error   { return &cliError{code: comfyExitJobFailed, err: err} }
func comfyOutputsPendingErr(err error) error {
	return &cliError{code: comfyExitOutputsPending, err: err}
}

// comfyJobsRequiresInput mirrors the generated commands' bare-invocation contract:
// a human gets help at exit 0, a machine caller (--json/--agent) gets a structured
// usage error at exit 2. Validation lives here rather than in cobra's Args validator
// because Args runs before RunE and would break --dry-run probes.
func comfyJobsRequiresInput(cmd *cobra.Command, flags *rootFlags) error {
	if flags != nil && flags.asJSON {
		if err := printJSONFiltered(cmd.OutOrStdout(), map[string]any{
			"error": "requires a prompt_id",
			"usage": cmd.CommandPath() + " --help",
		}, flags); err != nil {
			return err
		}
		return usageErr(fmt.Errorf("%q requires a prompt_id; run %q for usage", cmd.CommandPath(), cmd.CommandPath()+" --help"))
	}
	return cmd.Help()
}

// ---------------------------------------------------------------------------
// Local store access
// ---------------------------------------------------------------------------

// comfyJobsOpenWritable opens the local store and ensures the ComfyUI domain schema
// exists. Used by the two commands that record what the server told them.
func comfyJobsOpenWritable(ctx context.Context) (*store.Store, error) {
	s, err := store.OpenWithContext(ctx, defaultDBPath("comfyui-pp-cli"))
	if err != nil {
		return nil, fmt.Errorf("opening local store: %w", err)
	}
	if err := store.MigrateComfyUI(ctx, s.DB()); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("preparing comfyui domain schema: %w", err)
	}
	return s, nil
}

// comfyJobsOpenReadable opens the local store read-only. Returns (nil, nil) when
// there is nothing to read — no database file, or a database that has never had the
// domain schema applied. A read command must never create or migrate a database as a
// side effect, so the missing-table case is a clean "no local rows" rather than an
// error or an implicit write.
func comfyJobsOpenReadable(ctx context.Context) (*store.Store, error) {
	s, err := openStoreForRead(ctx, "comfyui-pp-cli")
	if err != nil || s == nil {
		return nil, err
	}
	var name string
	queryErr := s.DB().QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'run'`).Scan(&name)
	if queryErr != nil {
		_ = s.Close()
		if queryErr == sql.ErrNoRows {
			return nil, nil
		}
		return nil, queryErr
	}
	return s, nil
}

// comfyLocalRun is one durable run row — the record that outlives a server restart.
type comfyLocalRun struct {
	PromptID     string `json:"prompt_id"`
	Name         string `json:"name,omitempty"`
	GraphSHA     string `json:"graph_sha,omitempty"`
	ShapeSHA     string `json:"shape_sha,omitempty"`
	State        string `json:"state"`
	ExitClass    string `json:"exit_class,omitempty"`
	Completeness string `json:"completeness,omitempty"`
	DurationMS   int64  `json:"duration_ms,omitempty"`
	StartMS      int64  `json:"execution_start_ms,omitempty"`
	SuccessMS    int64  `json:"execution_success_ms,omitempty"`
	SubmittedSec int64  `json:"submitted_epoch_s,omitempty"`
	ErrorType    string `json:"error_exception_type,omitempty"`
	ErrorMessage string `json:"error_exception_message,omitempty"`
}

// comfyJobsSortMS is the row's position on the timeline: the authoritative success or
// start timestamp when it has one, otherwise its submission time.
func (r comfyLocalRun) comfyJobsSortMS() int64 {
	switch {
	case r.SuccessMS > 0:
		return r.SuccessMS
	case r.StartMS > 0:
		return r.StartMS
	default:
		return r.SubmittedSec * 1000
	}
}

const comfyLocalRunColumns = `prompt_id,
	       COALESCE(name, ''),
	       COALESCE(graph_sha, ''),
	       COALESCE(shape_sha, ''),
	       state,
	       COALESCE(exit_class, ''),
	       COALESCE(completeness, ''),
	       duration_ms,
	       execution_start_ms,
	       execution_success_ms,
	       CAST(COALESCE(strftime('%s', submitted_at), 0) AS INTEGER),
	       COALESCE(error_exception_type, ''),
	       COALESCE(error_exception_message, '')`

// comfyLocalRunOrder orders by the authoritative timestamps first and falls back to
// submission time, so rows without timing still land in a sensible place.
const comfyLocalRunOrder = `ORDER BY COALESCE(execution_success_ms, execution_start_ms,
	       CAST(COALESCE(strftime('%s', submitted_at), 0) AS INTEGER) * 1000) DESC, prompt_id DESC`

func comfyScanLocalRuns(rows *sql.Rows) ([]comfyLocalRun, error) {
	defer rows.Close()
	var out []comfyLocalRun
	for rows.Next() {
		var r comfyLocalRun
		var duration, start, success sql.NullInt64
		if err := rows.Scan(&r.PromptID, &r.Name, &r.GraphSHA, &r.ShapeSHA, &r.State,
			&r.ExitClass, &r.Completeness, &duration, &start, &success, &r.SubmittedSec,
			&r.ErrorType, &r.ErrorMessage); err != nil {
			return nil, err
		}
		r.DurationMS = duration.Int64
		r.StartMS = start.Int64
		r.SuccessMS = success.Int64
		out = append(out, r)
	}
	return out, rows.Err()
}

// comfyQueryLocalRuns reads the most recent runs. limit <= 0 means no limit.
func comfyQueryLocalRuns(ctx context.Context, db *sql.DB, limit int) ([]comfyLocalRun, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := db.QueryContext(ctx,
		`SELECT `+comfyLocalRunColumns+` FROM run `+comfyLocalRunOrder+` LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return comfyScanLocalRuns(rows)
}

func comfyQueryLocalRun(ctx context.Context, db *sql.DB, promptID string) (comfyLocalRun, bool, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+comfyLocalRunColumns+` FROM run WHERE prompt_id = ?`, promptID)
	if err != nil {
		return comfyLocalRun{}, false, err
	}
	runs, err := comfyScanLocalRuns(rows)
	if err != nil || len(runs) == 0 {
		return comfyLocalRun{}, false, err
	}
	return runs[0], true, nil
}

// comfyQueryShapeRuns reads recent runs for the timing table, optionally narrowed to
// one performance shape.
func comfyQueryShapeRuns(ctx context.Context, db *sql.DB, shapeSHA string, limit int) ([]comfyLocalRun, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := db.QueryContext(ctx,
		`SELECT `+comfyLocalRunColumns+` FROM run
		  WHERE (? = '' OR shape_sha = ?) `+comfyLocalRunOrder+` LIMIT ?`,
		shapeSHA, shapeSHA, limit)
	if err != nil {
		return nil, err
	}
	return comfyScanLocalRuns(rows)
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

// comfyQueueSnapshot is /queue split into what is executing and what is waiting.
type comfyQueueSnapshot struct {
	Running []history.PromptTuple `json:"running"`
	Pending []history.PromptTuple `json:"pending"`
}

func (q comfyQueueSnapshot) find(promptID string) (history.PromptTuple, string, int, bool) {
	for i, item := range q.Running {
		if item.PromptID == promptID {
			return item, "running", i, true
		}
	}
	for i, item := range q.Pending {
		if item.PromptID == promptID {
			return item, "pending", i, true
		}
	}
	return history.PromptTuple{}, "", 0, false
}

func comfyParseQueue(raw []byte) comfyQueueSnapshot {
	var payload struct {
		Running []json.RawMessage `json:"queue_running"`
		Pending []json.RawMessage `json:"queue_pending"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return comfyQueueSnapshot{}
	}
	decode := func(items []json.RawMessage) []history.PromptTuple {
		out := make([]history.PromptTuple, 0, len(items))
		for _, item := range items {
			if tuple, ok := history.ParsePromptTuple(item); ok {
				out = append(out, tuple)
			}
		}
		// Pending order is the execution order the server will use; sort by the
		// queue number so it is stable regardless of how the server serialised it.
		sort.SliceStable(out, func(i, j int) bool { return out[i].Number < out[j].Number })
		if len(out) == 0 {
			return nil
		}
		return out
	}
	return comfyQueueSnapshot{Running: decode(payload.Running), Pending: decode(payload.Pending)}
}

// comfyFetchQueue reads /queue. Cache-bypassing: the generated response cache holds
// GETs for five minutes, and a five-minute-old queue is worse than no queue at all.
func comfyFetchQueue(ctx context.Context, flags *rootFlags) (comfyQueueSnapshot, error) {
	c, err := flags.newClient()
	if err != nil {
		return comfyQueueSnapshot{}, err
	}
	data, err := c.GetNoCache(ctx, "/queue", nil)
	if err != nil {
		return comfyQueueSnapshot{}, err
	}
	return comfyParseQueue(data), nil
}

// comfyFetchHistory reads the whole /history map. maxItems <= 0 asks for the server
// default. Cache-bypassing for the same reason as the queue.
func comfyFetchHistory(ctx context.Context, flags *rootFlags, maxItems int) ([]history.Entry, []history.SkippedEntry, error) {
	c, err := flags.newClient()
	if err != nil {
		return nil, nil, err
	}
	params := map[string]string{}
	if maxItems > 0 {
		params["max_items"] = formatCLIParamValue(maxItems)
	}
	data, err := c.GetNoCache(ctx, "/history", params)
	if err != nil {
		return nil, nil, err
	}
	return history.ParseAll(data)
}

// comfyFetchHistoryEntry reads /history/{prompt_id}. ComfyUI answers an unknown
// prompt with `{}` rather than a 404, so "not found" is a shape, not a status code.
func comfyFetchHistoryEntry(ctx context.Context, flags *rootFlags, promptID string) (history.Entry, bool, error) {
	c, err := flags.newClient()
	if err != nil {
		return history.Entry{}, false, err
	}
	path := replacePathParam("/history/{prompt_id}", "prompt_id", promptID)
	data, err := c.GetNoCache(ctx, path, nil)
	if err != nil {
		return history.Entry{}, false, err
	}
	return history.ParseOne(data, promptID)
}

// ---------------------------------------------------------------------------
// Ingest — /history entry -> run + output rows
// ---------------------------------------------------------------------------

// comfyRunState maps a parsed history state onto the durable run.state vocabulary the
// schema and the submit lease already use.
func comfyRunState(state history.State) string {
	switch state {
	case history.StatePending:
		// The lease query counts 'submitted' as in-flight, which is what a
		// queued-but-unstarted prompt is.
		return "submitted"
	case history.StateRunning:
		return "running"
	case history.StateCompletedOutputsPending:
		return "completed-outputs-pending"
	case history.StateCompleted:
		return "completed"
	case history.StateFailed:
		return "failed"
	case history.StateInterrupted:
		return "interrupted"
	default:
		return string(state)
	}
}

func comfyExitClass(state history.State) string {
	switch state {
	case history.StateCompleted:
		return "success"
	case history.StateFailed:
		return "error"
	case history.StateInterrupted:
		return "interrupted"
	default:
		return ""
	}
}

// comfyIngestResult is what one entry did to the local store.
type comfyIngestResult struct {
	PromptID     string `json:"prompt_id"`
	State        string `json:"state"`
	Inserted     bool   `json:"inserted"`
	TimingSet    bool   `json:"timing_recorded"`
	OutputsAdded int    `json:"outputs_recorded"`
	ShapeSHA     string `json:"shape_sha,omitempty"`
	Note         string `json:"note,omitempty"`
}

// comfyIngestEntry records one /history entry. Idempotent by construction: the run
// insert is ON CONFLICT DO NOTHING, timing and state are UPDATEs, and output rows are
// inserted only when an identical row is absent (the output table has no natural
// unique key, so re-running would otherwise duplicate every file).
//
// Rows created here are completeness='history-only': they were reconstructed from the
// server's RAM view, not observed from submission, so no websocket node timings or
// VRAM samples exist for them and nothing should pretend otherwise.
func comfyIngestEntry(ctx context.Context, db *sql.DB, entry history.Entry) (comfyIngestResult, error) {
	result := comfyIngestResult{PromptID: entry.PromptID, State: comfyRunState(entry.State)}

	// The entry carries the API graph it ran, so a reconstructed row can still be
	// hashed into a performance shape and compared with everything else.
	graphSHA, shapeSHA := "", ""
	if len(entry.PromptGraph) > 0 {
		var graph store.APIGraph
		if err := json.Unmarshal(entry.PromptGraph, &graph); err == nil && len(graph) > 0 {
			if sha, err := store.UpsertGraph(ctx, db, graph, "", nil); err == nil {
				graphSHA = sha
			}
			if sha, err := store.ShapeSHA(graph); err == nil {
				shapeSHA = sha
			}
		}
	}
	result.ShapeSHA = shapeSHA

	var existing int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(1) FROM run WHERE prompt_id = ?`, entry.PromptID).Scan(&existing); err != nil {
		return result, fmt.Errorf("checking run %s: %w", entry.PromptID, err)
	}
	if existing == 0 {
		if err := store.InsertRun(ctx, db, store.RunRow{
			PromptID:     entry.PromptID,
			GraphSHA:     graphSHA,
			ShapeSHA:     shapeSHA,
			State:        result.State,
			Completeness: "history-only",
		}); err != nil {
			return result, err
		}
		result.Inserted = true
	} else if graphSHA != "" {
		// A row submitted by this CLI already knows its graph; only fill the gap
		// left by a row that was reconstructed before its graph was available.
		if _, err := db.ExecContext(ctx,
			`UPDATE run SET graph_sha = COALESCE(NULLIF(graph_sha, ''), ?),
			                shape_sha = COALESCE(NULLIF(shape_sha, ''), ?)
			  WHERE prompt_id = ?`, graphSHA, shapeSHA, entry.PromptID); err != nil {
			return result, fmt.Errorf("linking graph for %s: %w", entry.PromptID, err)
		}
	}

	// TIMING — the only authoritative source, and refused outright when the pair is
	// inconsistent. A missing duration is recoverable; a wrong one poisons every
	// shape comparison that follows.
	switch {
	case entry.TimingAnomaly != "":
		result.Note = "timing refused: " + entry.TimingAnomaly
	case entry.Timestamps.StartMS > 0 || entry.Timestamps.SuccessMS > 0:
		if err := store.SetRunTiming(ctx, db, entry.PromptID, entry.Timestamps.StartMS, entry.Timestamps.SuccessMS); err != nil {
			result.Note = "timing refused: " + err.Error()
		} else {
			result.TimingSet = true
		}
	}

	if err := store.SetRunState(ctx, db, entry.PromptID, result.State, comfyExitClass(entry.State)); err != nil {
		return result, err
	}

	if entry.Error != nil {
		tail := strings.Join(entry.Error.Traceback, "\n")
		if _, err := db.ExecContext(ctx,
			`UPDATE run SET error_node_id = ?, error_node_type = ?, error_exception_type = ?,
			                error_exception_message = ?, error_traceback_tail = ?
			  WHERE prompt_id = ?`,
			nullIfBlank(entry.Error.NodeID), nullIfBlank(entry.Error.NodeType),
			nullIfBlank(entry.Error.ExceptionType), nullIfBlank(entry.Error.ExceptionMessage),
			nullIfBlank(tail), entry.PromptID); err != nil {
			return result, fmt.Errorf("recording error for %s: %w", entry.PromptID, err)
		}
	}

	added, err := comfyRecordOutputs(ctx, db, entry)
	result.OutputsAdded = added
	return result, err
}

// comfyRecordOutputs inserts output rows that are not already present. The output
// table is append-only with a synthetic key, so the NOT EXISTS guard is what makes
// re-running sync-history a no-op instead of a duplicator.
func comfyRecordOutputs(ctx context.Context, db *sql.DB, entry history.Entry) (int, error) {
	added := 0
	for _, output := range entry.Outputs {
		res, err := db.ExecContext(ctx,
			`INSERT INTO output (run_id, node_id, output_key, filename, subfolder, type)
			 SELECT ?, ?, ?, ?, ?, ?
			  WHERE NOT EXISTS (
			        SELECT 1 FROM output
			         WHERE run_id = ? AND filename = ?
			           AND COALESCE(subfolder, '') = ? AND COALESCE(node_id, '') = ?
			           AND COALESCE(output_key, '') = ?)`,
			entry.PromptID, output.NodeID, output.Key, output.Filename, output.Subfolder, output.Type,
			entry.PromptID, output.Filename, output.Subfolder, output.NodeID, output.Key)
		if err != nil {
			return added, fmt.Errorf("recording output %s for %s: %w", output.Filename, entry.PromptID, err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			added++
		}
	}
	return added, nil
}

func nullIfBlank(s string) interface{} {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// ---------------------------------------------------------------------------
// jobs list
// ---------------------------------------------------------------------------

// comfyJobRow is one merged job: the live queue, the live history, and the durable
// local row folded into a single record with its provenance stated.
type comfyJobRow struct {
	PromptID     string `json:"prompt_id"`
	State        string `json:"state"`
	Source       string `json:"source"`
	Position     string `json:"position,omitempty"`
	QueueNumber  int64  `json:"queue_number,omitempty"`
	Name         string `json:"name,omitempty"`
	ShapeSHA     string `json:"shape_sha,omitempty"`
	DurationMS   int64  `json:"duration_ms,omitempty"`
	Duration     string `json:"duration,omitempty"`
	Outputs      int    `json:"outputs"`
	Completeness string `json:"completeness,omitempty"`
	ExitClass    string `json:"exit_class,omitempty"`
	Error        string `json:"error,omitempty"`
	Note         string `json:"note,omitempty"`

	group  int   // 0 running, 1 pending, 2 finished/other
	sortMS int64 // position on the timeline for the finished group
}

func (r *comfyJobRow) addSource(source string) {
	for _, existing := range strings.Split(r.Source, "+") {
		if existing == source {
			return
		}
	}
	if r.Source == "" {
		r.Source = source
		return
	}
	r.Source += "+" + source
}

// comfyMergeJobs folds the three views into one ordered list: running first, then
// pending in queue order, then everything finished newest-first.
func comfyMergeJobs(queue comfyQueueSnapshot, entries []history.Entry, locals []comfyLocalRun) []*comfyJobRow {
	index := map[string]*comfyJobRow{}
	var order []*comfyJobRow
	get := func(promptID string) *comfyJobRow {
		if row, ok := index[promptID]; ok {
			return row
		}
		row := &comfyJobRow{PromptID: promptID, group: 2}
		index[promptID] = row
		order = append(order, row)
		return row
	}

	for i, item := range queue.Running {
		row := get(item.PromptID)
		row.group = 0
		row.State = "running"
		row.QueueNumber = item.Number
		row.Position = "running"
		if len(queue.Running) > 1 {
			row.Position = fmt.Sprintf("running #%d", i+1)
		}
		row.addSource("queue")
	}
	for i, item := range queue.Pending {
		row := get(item.PromptID)
		row.group = 1
		row.State = "pending"
		row.QueueNumber = item.Number
		row.Position = fmt.Sprintf("pending #%d", i+1)
		row.addSource("queue")
	}

	for _, entry := range entries {
		row := get(entry.PromptID)
		row.addSource("history")
		if row.group == 2 {
			// A queued prompt keeps its queue state; only a finished one takes
			// the history verdict.
			row.State = string(entry.State)
			row.sortMS = entry.LatestMS()
		}
		row.Outputs = len(entry.Outputs)
		if entry.DurationOK {
			row.DurationMS = entry.DurationMS
		}
		if entry.TimingAnomaly != "" {
			row.Note = entry.TimingAnomaly
		}
		if entry.State == history.StateCompletedOutputsPending && row.Note == "" {
			row.Note = "success recorded, outputs not published yet"
		}
		if entry.Error != nil {
			row.Error = comfyErrorSummary(entry.Error)
			row.ExitClass = "error"
		}
	}

	for _, local := range locals {
		row := get(local.PromptID)
		row.addSource("local")
		row.Name = local.Name
		row.ShapeSHA = local.ShapeSHA
		row.Completeness = local.Completeness
		if row.ExitClass == "" {
			row.ExitClass = local.ExitClass
		}
		if row.State == "" {
			row.State = local.State
		}
		if row.DurationMS == 0 && local.DurationMS > 0 {
			row.DurationMS = local.DurationMS
		}
		if row.Error == "" && local.ErrorExcerpt() != "" {
			row.Error = local.ErrorExcerpt()
		}
		if row.sortMS == 0 {
			row.sortMS = local.comfyJobsSortMS()
		}
	}

	for _, row := range order {
		row.Duration = comfyFormatMS(row.DurationMS)
		if row.State == "" {
			row.State = "unknown"
		}
	}

	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.group != b.group {
			return a.group < b.group
		}
		if a.group < 2 {
			return a.QueueNumber < b.QueueNumber
		}
		if a.sortMS != b.sortMS {
			return a.sortMS > b.sortMS
		}
		return a.PromptID < b.PromptID
	})
	return order
}

// ErrorExcerpt renders the stored failure into one line.
func (r comfyLocalRun) ErrorExcerpt() string {
	switch {
	case r.ErrorType != "" && r.ErrorMessage != "":
		return r.ErrorType + ": " + r.ErrorMessage
	case r.ErrorMessage != "":
		return r.ErrorMessage
	default:
		return r.ErrorType
	}
}

func comfyErrorSummary(execErr *history.ExecError) string {
	if execErr == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if execErr.NodeType != "" {
		node := execErr.NodeType
		if execErr.NodeID != "" {
			node += "(" + execErr.NodeID + ")"
		}
		parts = append(parts, node)
	}
	if execErr.ExceptionType != "" {
		parts = append(parts, execErr.ExceptionType)
	}
	if execErr.ExceptionMessage != "" {
		parts = append(parts, execErr.ExceptionMessage)
	}
	return strings.Join(parts, ": ")
}

// newJobsCmd is the `jobs` parent group.
//
// pp:data-source auto
func newJobsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "List render jobs across the live queue, the live history, and the durable local record",
		Long: `Job lifecycle across all three views of a render.

ComfyUI's /history is an in-RAM dict: FIFO-evicted at 10000 entries and destroyed
on every restart. The local run table is the only view that survives, so these
commands merge the two and say which view each row came from.`,
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:parent-group":     "true",
			"pp:typed-exit-codes": "0,2",
		},
		RunE: parentNoSubcommandRunE(flags),
	}
	cmd.AddCommand(newJobsLsCmd(flags))
	return cmd
}

// newJobsLsCmd merges /queue, /history and the local run table into one list.
//
// Named `list` because that is the verb every printed CLI uses for this shape; `ls`
// stays as an alias so muscle memory and any script written against it keep working.
//
// pp:data-source auto
func newJobsLsCmd(flags *rootFlags) *cobra.Command {
	var limit int
	var all bool

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "One ordered list of running, pending, and finished jobs",
		Long: `Merge GET /queue (running + pending), GET /history, and the local run table
into a single ordered list: running first, then pending in queue order, then
finished newest-first.

Each row states its source. 'local' without 'history' means the server no longer
remembers that render — /history lives in RAM and is destroyed on restart — but
the local row, including its authoritative timing, is intact.

When the server is unreachable the command still lists the local rows and says so
on stderr, because those rows are precisely what a restart cannot take away.`,
		Example: `  comfyui-pp-cli jobs list
  comfyui-pp-cli jobs list --limit 50
  comfyui-pp-cli jobs list --all --json`,
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": "0,2",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "jobs list")
			}
			if len(args) > 0 {
				return usageErr(fmt.Errorf("jobs list takes no positional arguments (got %q)", args[0]))
			}
			effectiveLimit := limit
			if all {
				effectiveLimit = 0
			}

			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()

			serverReachable := true
			var serverError string
			queue, err := comfyFetchQueue(ctx, flags)
			if err != nil {
				serverReachable = false
				serverError = err.Error()
			}
			var entries []history.Entry
			var skipped []history.SkippedEntry
			if serverReachable {
				entries, skipped, err = comfyFetchHistory(ctx, flags, effectiveLimit)
				if err != nil {
					serverReachable = false
					serverError = err.Error()
				}
			}
			if !serverReachable {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: ComfyUI is unreachable (%s); listing local run rows only\n", serverError)
			}

			var locals []comfyLocalRun
			db, err := comfyJobsOpenReadable(ctx)
			if err != nil {
				return err
			}
			if db != nil {
				defer db.Close()
				localLimit := effectiveLimit
				if localLimit > 0 {
					// Over-read so a merge with the live views still has enough
					// rows to fill the requested limit after de-duplication.
					localLimit = effectiveLimit * 3
				}
				locals, err = comfyQueryLocalRuns(ctx, db.DB(), localLimit)
				if err != nil {
					return fmt.Errorf("reading local runs: %w", err)
				}
			}
			if !serverReachable && len(locals) == 0 {
				return apiErr(fmt.Errorf("ComfyUI is unreachable and the local store has no runs: %s", serverError))
			}

			rows := comfyMergeJobs(queue, entries, locals)
			truncated := false
			if effectiveLimit > 0 && len(rows) > effectiveLimit {
				rows = rows[:effectiveLimit]
				truncated = true
			}

			payload := map[string]any{
				"meta": map[string]any{
					"server_reachable": serverReachable,
					"queue_running":    len(queue.Running),
					"queue_pending":    len(queue.Pending),
					"history_entries":  len(entries),
					"local_rows":       len(locals),
					"returned":         len(rows),
					"truncated":        truncated,
					"source":           "queue+history+local",
				},
				"jobs": rows,
			}
			if serverError != "" {
				payload["meta"].(map[string]any)["server_error"] = serverError
			}
			if len(skipped) > 0 {
				payload["skipped"] = skipped
			}

			if !wantsHumanTable(cmd.OutOrStdout(), flags) {
				return printJSONFiltered(cmd.OutOrStdout(), payload, flags)
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no jobs in the queue, in history, or in the local store")
				return nil
			}
			headers := []string{"PROMPT_ID", "STATE", "SOURCE", "DURATION", "OUTPUTS", "POSITION", "NAME"}
			table := make([][]string, 0, len(rows))
			for _, row := range rows {
				outputs := "-"
				if row.Outputs > 0 {
					outputs = fmt.Sprintf("%d", row.Outputs)
				}
				table = append(table, []string{
					row.PromptID, row.State, row.Source, row.Duration, outputs,
					comfyDash(row.Position), comfyDash(row.Name),
				})
			}
			if err := flags.printTable(cmd, headers, table); err != nil {
				return err
			}
			for _, row := range rows {
				if row.Error != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s error: %s\n", row.PromptID, row.Error)
				}
			}
			if truncated {
				fmt.Fprintf(cmd.ErrOrStderr(), "showing %d jobs; pass --all or a larger --limit for the rest\n", len(rows))
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 25, "Maximum jobs to list (0 for no limit)")
	cmd.Flags().BoolVar(&all, "all", false, "List every job the queue, history, and local store know about")
	return cmd
}

func comfyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// comfyFormatMS renders a duration that came from the authoritative timestamps.
// A zero means "no trustworthy duration", never "instant".
func comfyFormatMS(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Second:
		return d.String()
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	default:
		return d.Round(time.Second).String()
	}
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

// comfyJobStatus is the full answer for one job.
type comfyJobStatus struct {
	PromptID      string             `json:"prompt_id"`
	State         string             `json:"state"`
	Terminal      bool               `json:"terminal"`
	Source        string             `json:"source"`
	Position      string             `json:"position,omitempty"`
	QueueNumber   int64              `json:"queue_number,omitempty"`
	DurationMS    int64              `json:"duration_ms,omitempty"`
	Duration      string             `json:"duration,omitempty"`
	ElapsedMS     int64              `json:"elapsed_ms,omitempty"`
	TimingSource  string             `json:"timing_source,omitempty"`
	TimingAnomaly string             `json:"timing_anomaly,omitempty"`
	Timestamps    history.Timestamps `json:"timestamps,omitempty"`
	Outputs       []history.Output   `json:"outputs"`
	OutputNodes   []string           `json:"output_nodes,omitempty"`
	Error         *history.ExecError `json:"error,omitempty"`
	Local         *comfyLocalRun     `json:"local,omitempty"`
	Note          string             `json:"note,omitempty"`
}

// comfyStatusFromEntry builds the answer from a live /history entry.
func comfyStatusFromEntry(entry history.Entry) comfyJobStatus {
	status := comfyJobStatus{
		PromptID:      entry.PromptID,
		State:         string(entry.State),
		Terminal:      entry.State.Terminal(),
		Source:        "history",
		QueueNumber:   entry.QueueNumber,
		Timestamps:    entry.Timestamps,
		Outputs:       entry.Outputs,
		OutputNodes:   entry.OutputNodes,
		Error:         entry.Error,
		ElapsedMS:     entry.ElapsedMS,
		TimingAnomaly: entry.TimingAnomaly,
	}
	if status.Outputs == nil {
		status.Outputs = []history.Output{}
	}
	if entry.DurationOK {
		status.DurationMS = entry.DurationMS
		status.Duration = comfyFormatMS(entry.DurationMS)
		status.TimingSource = "history execution_start -> execution_success"
	}
	if entry.State == history.StateCompletedOutputsPending {
		status.Note = "/history recorded execution_success but has published no outputs yet; this is not a finished render — poll again with 'wait'"
	}
	return status
}

// newStatusCmd reports one job.
//
// pp:data-source auto
func newStatusCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status <prompt_id>",
		Short: "State, honest duration, outputs, and the verbatim error for one job",
		Long: `Report one job by prompt_id, looking in /history first, then the live queue,
then the durable local run row.

The duration comes only from /history's execution_start -> execution_success
timestamps. When those are missing or inconsistent no duration is printed: a
missing number is recoverable, a wrong one is not.

An execution_error is printed verbatim — node, exception type, message, and
traceback — because that text is the entire diagnosis and it disappears with the
next server restart.

Exit codes:
  0   the job was found (whatever its state)
  2   usage error
  3   no such prompt_id in /history, the queue, or the local store`,
		Example: `  comfyui-pp-cli status 6f0e5c4a-1d2b-4a5e-9c88-2f1b7a0d3e44
  comfyui-pp-cli status 6f0e5c4a-1d2b-4a5e-9c88-2f1b7a0d3e44 --json`,
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": "0,2,3",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && !flags.dryRun {
				return comfyJobsRequiresInput(cmd, flags)
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "status")
			}
			promptID := strings.TrimSpace(args[0])
			if promptID == "" {
				return usageErr(fmt.Errorf("status needs a prompt_id"))
			}

			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()

			status, found, err := comfyResolveStatus(ctx, flags, promptID, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if !found {
				return notFoundErr(fmt.Errorf("prompt %q is not in /history, not in the queue, and not in the local store.\n"+
					"hint: /history is an in-RAM dict that ComfyUI destroys on restart, so a prompt from before a restart is only\n"+
					"      recoverable if it was ingested locally — run 'comfyui-pp-cli sync-history' while the server is up", promptID))
			}
			return comfyRenderStatus(cmd, flags, status)
		},
	}
	return cmd
}

// comfyResolveStatus looks in /history, then the live queue, then the local store.
// A live-transport failure is a warning, not an error: the local row is still the
// durable answer and is what survives a restart.
func comfyResolveStatus(ctx context.Context, flags *rootFlags, promptID string, warn io.Writer) (comfyJobStatus, bool, error) {
	var status comfyJobStatus
	found := false

	entry, entryFound, err := comfyFetchHistoryEntry(ctx, flags, promptID)
	switch {
	case err != nil:
		fmt.Fprintf(warn, "warning: /history unreachable (%v); falling back to the local record\n", err)
	case entryFound:
		status = comfyStatusFromEntry(entry)
		found = true
	}

	if !found {
		if queue, queueErr := comfyFetchQueue(ctx, flags); queueErr == nil {
			if item, where, index, ok := queue.find(promptID); ok {
				status = comfyJobStatus{
					PromptID:    promptID,
					State:       where,
					Source:      "queue",
					QueueNumber: item.Number,
					Position:    fmt.Sprintf("%s #%d", where, index+1),
					Outputs:     []history.Output{},
					Note:        "still in the queue; /history records nothing until the prompt finishes",
				}
				found = true
			}
		}
	}

	db, err := comfyJobsOpenReadable(ctx)
	if err != nil {
		return status, found, err
	}
	if db != nil {
		defer db.Close()
		local, localFound, localErr := comfyQueryLocalRun(ctx, db.DB(), promptID)
		if localErr != nil {
			return status, found, fmt.Errorf("reading local run: %w", localErr)
		}
		if localFound {
			localCopy := local
			if !found {
				status = comfyJobStatus{
					PromptID:   promptID,
					State:      local.State,
					Source:     "local",
					DurationMS: local.DurationMS,
					Duration:   comfyFormatMS(local.DurationMS),
					Outputs:    []history.Output{},
					Timestamps: history.Timestamps{StartMS: local.StartMS, SuccessMS: local.SuccessMS},
					Note: "the server no longer remembers this prompt (/history is RAM-only and is destroyed on restart); " +
						"this is the durable local record",
				}
				if local.DurationMS > 0 {
					status.TimingSource = "local run row (recorded from /history timestamps)"
				}
				status.Terminal = local.State == "completed" || local.State == "failed" || local.State == "interrupted"
				found = true
			} else {
				status.Source += "+local"
			}
			status.Local = &localCopy
		}
	}
	return status, found, nil
}

func comfyRenderStatus(cmd *cobra.Command, flags *rootFlags, status comfyJobStatus) error {
	if !wantsHumanTable(cmd.OutOrStdout(), flags) {
		return printJSONFiltered(cmd.OutOrStdout(), status, flags)
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "%s\n", bold(status.PromptID))
	fmt.Fprintf(w, "  state:    %s\n", status.State)
	fmt.Fprintf(w, "  source:   %s\n", status.Source)
	if status.Position != "" {
		fmt.Fprintf(w, "  position: %s\n", status.Position)
	}
	if status.DurationMS > 0 {
		fmt.Fprintf(w, "  duration: %s  (%s)\n", status.Duration, status.TimingSource)
	} else if status.ElapsedMS > 0 {
		fmt.Fprintf(w, "  elapsed:  %s  (execution_start -> terminal event; NOT a success duration)\n", comfyFormatMS(status.ElapsedMS))
	} else {
		fmt.Fprintf(w, "  duration: - (no execution_start/execution_success pair recorded)\n")
	}
	if status.TimingAnomaly != "" {
		fmt.Fprintf(w, "  %s %s\n", yellow("timing:"), status.TimingAnomaly+" — duration withheld rather than reported wrong")
	}
	if status.Local != nil {
		fmt.Fprintf(w, "  local:    completeness=%s", comfyDash(status.Local.Completeness))
		if status.Local.ShapeSHA != "" {
			fmt.Fprintf(w, " shape=%s", comfyShort(status.Local.ShapeSHA))
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "  outputs:  %d\n", len(status.Outputs))
	for _, output := range status.Outputs {
		location := output.Filename
		if output.Subfolder != "" {
			location = output.Subfolder + "/" + output.Filename
		}
		fmt.Fprintf(w, "    - [%s] %s (%s)\n", output.NodeID, location, comfyDash(output.Type))
	}
	if status.Error != nil {
		fmt.Fprintf(w, "  %s\n", red("error (verbatim):"))
		if status.Error.NodeType != "" || status.Error.NodeID != "" {
			fmt.Fprintf(w, "    node:      %s (%s)\n", comfyDash(status.Error.NodeType), comfyDash(status.Error.NodeID))
		}
		if status.Error.ExceptionType != "" {
			fmt.Fprintf(w, "    exception: %s\n", status.Error.ExceptionType)
		}
		if status.Error.ExceptionMessage != "" {
			fmt.Fprintf(w, "    message:   %s\n", status.Error.ExceptionMessage)
		}
		for _, line := range status.Error.Traceback {
			fmt.Fprintf(w, "    | %s\n", strings.TrimRight(line, "\n"))
		}
	}
	if status.Note != "" {
		fmt.Fprintf(w, "  note:     %s\n", status.Note)
	}
	return nil
}

func comfyShort(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

// ---------------------------------------------------------------------------
// wait
// ---------------------------------------------------------------------------

// comfyWaitResult is what `wait` returns whether it succeeded, timed out, or gave up
// on the outputs. The prompt_id is always present so the job can be re-attached.
type comfyWaitResult struct {
	PromptID   string         `json:"prompt_id"`
	Outcome    string         `json:"outcome"`
	Status     comfyJobStatus `json:"status"`
	WaitedMS   int64          `json:"waited_ms"`
	Waited     string         `json:"waited"`
	Polls      int            `json:"polls"`
	TimedOut   bool           `json:"timed_out"`
	Reattach   string         `json:"reattach,omitempty"`
	Ingested   bool           `json:"ingested_locally"`
	IngestNote string         `json:"ingest_note,omitempty"`
}

// newWaitCmd polls until the job reaches a terminal state.
//
// pp:data-source auto
func newWaitCmd(flags *rootFlags) *cobra.Command {
	var timeout time.Duration
	var interval time.Duration
	var outputsSettle time.Duration
	var missingGrace time.Duration
	var noRecord bool

	cmd := &cobra.Command{
		Use:   "wait <prompt_id>",
		Short: "Poll /history until a job is genuinely finished — no artificial timeout",
		Long: `Poll GET /history/<prompt_id> until the job reaches a terminal state.

There is NO default timeout. A local render legitimately runs from 30 seconds to
20 minutes, and a wrapper that gave up early and resubmitted instead of
re-attaching once burned about 30 GPU-minutes. Pass --timeout only when you
actually want a bound; when it expires the command exits with its own code and
still prints the prompt_id, so the running job is never lost.

The /history lag race is handled explicitly: a just-finished prompt can appear
with status_str "success" and an EMPTY outputs map, because ComfyUI publishes the
success message a beat before the outputs. That is a distinct non-terminal state
(completed-outputs-pending) and is retried with backoff for --outputs-settle. It
is never reported as "done with no outputs".

On a terminal state the authoritative timing is recorded into the local run table,
so it survives the next server restart.

Exit codes:
  0   the job completed and its outputs are published
  2   usage error
  3   the prompt is in neither the queue nor /history (wrong id, or the server
      restarted and dropped its RAM-only history)
  20  --timeout expired while the job was still running
  21  the job ended in execution_error or was interrupted
  22  execution_success recorded but no outputs published before --outputs-settle`,
		Example: `  comfyui-pp-cli wait 6f0e5c4a-1d2b-4a5e-9c88-2f1b7a0d3e44
  comfyui-pp-cli wait 6f0e5c4a-1d2b-4a5e-9c88-2f1b7a0d3e44 --timeout 25m
  comfyui-pp-cli wait 6f0e5c4a-1d2b-4a5e-9c88-2f1b7a0d3e44 --json --interval 5s`,
		Annotations: map[string]string{
			"mcp:local-write":     "true",
			"pp:typed-exit-codes": "0,2,3,20,21,22",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && !flags.dryRun {
				return comfyJobsRequiresInput(cmd, flags)
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "wait")
			}
			promptID := strings.TrimSpace(args[0])
			if promptID == "" {
				return usageErr(fmt.Errorf("wait needs a prompt_id"))
			}
			if interval <= 0 {
				return usageErr(fmt.Errorf("--interval must be positive (got %s)", interval))
			}
			// The wait loop deliberately does NOT inherit --timeout: that flag
			// bounds each HTTP request, and binding the whole poll to it would
			// reintroduce the artificial deadline this command exists to avoid.
			ctx := cmd.Context()
			return comfyRunWait(ctx, cmd, flags, promptID, comfyWaitOptions{
				Timeout:       timeout,
				Interval:      interval,
				OutputsSettle: outputsSettle,
				MissingGrace:  missingGrace,
				Record:        !noRecord,
			})
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "Give up after this long (0 = wait indefinitely, the default for local renders)")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "Poll interval while the job is running")
	cmd.Flags().DurationVar(&outputsSettle, "outputs-settle", 20*time.Second, "How long to keep polling after execution_success while /history still publishes no outputs")
	cmd.Flags().BoolVar(&noRecord, "no-record", false, "Do not write the finished job's authoritative timing into the local store")
	cmd.Flags().DurationVar(&missingGrace, "missing-grace", 10*time.Second, "How long a prompt may be absent from both the queue and /history before it is called not-found")
	return cmd
}

// comfyWaitOptions are the poll-loop knobs, grouped so the loop signature stays
// readable and a new knob cannot be passed in the wrong positional slot.
type comfyWaitOptions struct {
	Timeout       time.Duration
	Interval      time.Duration
	OutputsSettle time.Duration
	MissingGrace  time.Duration
	Record        bool
}

func comfyRunWait(ctx context.Context, cmd *cobra.Command, flags *rootFlags, promptID string, opts comfyWaitOptions) error {
	timeout, interval := opts.Timeout, opts.Interval
	outputsSettle, missingGrace := opts.OutputsSettle, opts.MissingGrace

	started := time.Now()
	var deadline time.Time
	if timeout > 0 {
		deadline = started.Add(timeout)
	}

	result := comfyWaitResult{
		PromptID: promptID,
		Reattach: fmt.Sprintf("comfyui-pp-cli wait %s", promptID),
	}
	var settleStarted time.Time
	var missingSince time.Time
	pendingAttempt := 0

	finish := func(outcome string, status comfyJobStatus) comfyWaitResult {
		result.Outcome = outcome
		result.Status = status
		result.WaitedMS = time.Since(started).Milliseconds()
		result.Waited = comfyFormatMS(result.WaitedMS)
		return result
	}

	for {
		result.Polls++

		requestCtx, cancel := boundCtx(ctx, flags)
		entry, found, err := comfyFetchHistoryEntry(requestCtx, flags, promptID)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A transport blip must not end a 20-minute wait. Warn and retry.
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: poll %d failed (%v); retrying in %s\n", result.Polls, err, interval)
			if waitErr := comfySleep(ctx, interval); waitErr != nil {
				return waitErr
			}
			if !deadline.IsZero() && time.Now().After(deadline) {
				out := finish("timed-out", comfyJobStatus{PromptID: promptID, State: "unknown", Source: "none", Outputs: []history.Output{}})
				out.TimedOut = true
				return comfyEmitWait(cmd, flags, out, comfyWaitTimeoutErr(fmt.Errorf(
					"timed out after %s waiting for prompt %s (last poll failed: %v); the job may still be running — re-attach with: %s",
					timeout, promptID, err, out.Reattach)))
			}
			continue
		}

		if found {
			missingSince = time.Time{}
			status := comfyStatusFromEntry(entry)

			if entry.State == history.StateCompletedOutputsPending {
				if settleStarted.IsZero() {
					settleStarted = time.Now()
				}
				if time.Since(settleStarted) < outputsSettle {
					// Backoff inside the settle window: outputs usually land within
					// a beat, so start tight and widen rather than sleeping a full
					// poll interval on a job that is already done.
					if waitErr := comfySleep(ctx, comfyBackoff(pendingAttempt)); waitErr != nil {
						return waitErr
					}
					pendingAttempt++
					continue
				}
				out := finish("outputs-pending", status)
				comfyIngestOnTerminal(ctx, entry, &out, opts.Record)
				return comfyEmitWait(cmd, flags, out, comfyOutputsPendingErr(fmt.Errorf(
					"prompt %s recorded execution_success but /history published no outputs within %s. "+
						"This is NOT a completed render with zero outputs — it is an unfinished publish. "+
						"Re-check with: comfyui-pp-cli status %s", promptID, outputsSettle, promptID)))
			}

			if entry.State.Terminal() {
				out := finish(string(entry.State), status)
				comfyIngestOnTerminal(ctx, entry, &out, opts.Record)
				switch entry.State {
				case history.StateFailed:
					return comfyEmitWait(cmd, flags, out, comfyJobFailedErr(fmt.Errorf(
						"prompt %s failed: %s", promptID, comfyErrorSummary(entry.Error))))
				case history.StateInterrupted:
					return comfyEmitWait(cmd, flags, out, comfyJobFailedErr(fmt.Errorf(
						"prompt %s was interrupted before it finished", promptID)))
				}
				return comfyEmitWait(cmd, flags, out, nil)
			}
			// Running or pending inside history: reset the settle bookkeeping.
			settleStarted = time.Time{}
			pendingAttempt = 0
		} else {
			// Not in /history yet. It may still be queued; it may also never have
			// existed, or have been lost to a restart. Give the queue a look before
			// deciding, and bound the ambiguity with --missing-grace.
			inQueue := false
			if queue, queueErr := comfyFetchQueue(ctx, flags); queueErr == nil {
				_, _, _, inQueue = queue.find(promptID)
			}
			if inQueue {
				missingSince = time.Time{}
			} else {
				if missingSince.IsZero() {
					missingSince = time.Now()
				}
				if time.Since(missingSince) >= missingGrace {
					out := finish("not-found", comfyJobStatus{PromptID: promptID, State: "not-found", Source: "none", Outputs: []history.Output{}})
					return comfyEmitWait(cmd, flags, out, notFoundErr(fmt.Errorf(
						"prompt %s appeared in neither the queue nor /history within %s.\n"+
							"hint: either the prompt_id is wrong, or ComfyUI restarted — /history is an in-RAM dict that a restart destroys",
						promptID, missingGrace)))
				}
			}
		}

		if !deadline.IsZero() && time.Now().After(deadline) {
			status, _, _ := comfyResolveStatus(ctx, flags, promptID, cmd.ErrOrStderr())
			if status.PromptID == "" {
				status = comfyJobStatus{PromptID: promptID, State: "unknown", Source: "none", Outputs: []history.Output{}}
			}
			out := finish("timed-out", status)
			out.TimedOut = true
			return comfyEmitWait(cmd, flags, out, comfyWaitTimeoutErr(fmt.Errorf(
				"timed out after %s waiting for prompt %s (last state: %s). The render is probably still going — "+
					"re-attach instead of resubmitting: %s", timeout, promptID, status.State, out.Reattach)))
		}

		sleepFor := interval
		if !deadline.IsZero() {
			if remaining := time.Until(deadline); remaining > 0 && remaining < sleepFor {
				sleepFor = remaining
			}
		}
		if waitErr := comfySleep(ctx, sleepFor); waitErr != nil {
			return waitErr
		}
	}
}

// comfyEmitWait prints the result BEFORE returning the (possibly non-zero) error, so
// the prompt_id survives every exit path.
func comfyEmitWait(cmd *cobra.Command, flags *rootFlags, result comfyWaitResult, err error) error {
	if !wantsHumanTable(cmd.OutOrStdout(), flags) {
		if printErr := printJSONFiltered(cmd.OutOrStdout(), result, flags); printErr != nil {
			return printErr
		}
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s  outcome=%s  waited=%s  polls=%d\n",
		bold(result.PromptID), result.Outcome, result.Waited, result.Polls)
	if renderErr := comfyRenderStatus(cmd, flags, result.Status); renderErr != nil {
		return renderErr
	}
	if result.IngestNote != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "note: %s\n", result.IngestNote)
	}
	return err
}

// comfyIngestOnTerminal records the authoritative timing locally the moment the job
// ends, so the number survives the next restart. Best-effort: a store failure is
// reported but never turns a finished render into a failed command.
func comfyIngestOnTerminal(ctx context.Context, entry history.Entry, result *comfyWaitResult, record bool) {
	if !record {
		result.IngestNote = "local record skipped (--no-record)"
		return
	}
	db, err := comfyJobsOpenWritable(ctx)
	if err != nil {
		result.IngestNote = "local record not updated: " + err.Error()
		return
	}
	defer db.Close()
	ingest, err := comfyIngestEntry(ctx, db.DB(), entry)
	if err != nil {
		result.IngestNote = "local record not updated: " + err.Error()
		return
	}
	result.Ingested = true
	if ingest.Note != "" {
		result.IngestNote = ingest.Note
	}
}

// comfyBackoff is the retry schedule inside the outputs-settle window: 250ms doubling
// to a 2s ceiling.
func comfyBackoff(attempt int) time.Duration {
	wait := 250 * time.Millisecond
	for i := 0; i < attempt && wait < 2*time.Second; i++ {
		wait *= 2
	}
	if wait > 2*time.Second {
		wait = 2 * time.Second
	}
	return wait
}

func comfySleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// sync-history
// ---------------------------------------------------------------------------

type comfySyncHistoryReport struct {
	Scanned         int                    `json:"scanned"`
	Inserted        int                    `json:"inserted"`
	Existing        int                    `json:"existing"`
	TimingRecorded  int                    `json:"timing_recorded"`
	OutputsRecorded int                    `json:"outputs_recorded"`
	Failed          int                    `json:"failed"`
	Results         []comfyIngestResult    `json:"results,omitempty"`
	Skipped         []history.SkippedEntry `json:"skipped,omitempty"`
	Errors          []string               `json:"errors,omitempty"`
}

// newSyncHistoryCmd ingests /history into the durable run/output tables.
//
// pp:data-source live
func newSyncHistoryCmd(flags *rootFlags) *cobra.Command {
	var maxItems int
	var verbose bool

	cmd := &cobra.Command{
		Use:   "sync-history",
		Short: "Ingest /history into the local run and output tables before the server forgets it",
		Long: `Read GET /history and record every entry into the durable local tables.

This exists because ComfyUI's history is 'self.history = {}' in RAM: FIFO-evicted
at 10000 entries and destroyed on every restart. Memory-knob experiments are
exactly what force restarts, so the server cannot keep the data an experiment
needs. Run this before restarting, and the timings, outputs, and error text stay.

Per entry it parses status.messages for execution_start / execution_success /
execution_error, normalises the timestamps (they arrive as epoch seconds or
milliseconds depending on the build), and records them through the store's timing
setter, which refuses an inconsistent pair rather than storing a wrong duration.
Rows created here are marked completeness='history-only': they were reconstructed
from the server's view, so no websocket node timings or VRAM samples exist for
them and nothing pretends otherwise.

Idempotent: re-running inserts nothing twice.`,
		Example: `  comfyui-pp-cli sync-history
  comfyui-pp-cli sync-history --max-items 200 --json
  comfyui-pp-cli sync-history --verbose`,
		Annotations: map[string]string{
			"mcp:local-write":     "true",
			"pp:typed-exit-codes": "0,2,5",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "sync-history")
			}
			if len(args) > 0 {
				return usageErr(fmt.Errorf("sync-history takes no positional arguments (got %q)", args[0]))
			}

			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()

			entries, skipped, err := comfyFetchHistory(ctx, flags, maxItems)
			if err != nil {
				return classifyAPIError(err, flags)
			}

			db, err := comfyJobsOpenWritable(ctx)
			if err != nil {
				return err
			}
			defer db.Close()

			report := comfySyncHistoryReport{Scanned: len(entries), Skipped: skipped}
			for _, entry := range entries {
				result, ingestErr := comfyIngestEntry(ctx, db.DB(), entry)
				if ingestErr != nil {
					report.Failed++
					report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", entry.PromptID, ingestErr))
					continue
				}
				if result.Inserted {
					report.Inserted++
				} else {
					report.Existing++
				}
				if result.TimingSet {
					report.TimingRecorded++
				}
				report.OutputsRecorded += result.OutputsAdded
				if verbose {
					report.Results = append(report.Results, result)
				}
			}

			if !wantsHumanTable(cmd.OutOrStdout(), flags) {
				return printJSONFiltered(cmd.OutOrStdout(), report, flags)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "scanned %d history entries\n", report.Scanned)
			fmt.Fprintf(w, "  inserted:         %d\n", report.Inserted)
			fmt.Fprintf(w, "  already present:  %d\n", report.Existing)
			fmt.Fprintf(w, "  timing recorded:  %d\n", report.TimingRecorded)
			fmt.Fprintf(w, "  outputs recorded: %d\n", report.OutputsRecorded)
			if verbose {
				for _, result := range report.Results {
					fmt.Fprintf(w, "    %s  %s  timing=%v outputs=%d %s\n",
						result.PromptID, result.State, result.TimingSet, result.OutputsAdded, result.Note)
				}
			}
			for _, entry := range report.Skipped {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: skipped %s: %s\n", entry.PromptID, entry.Reason)
			}
			for _, msg := range report.Errors {
				fmt.Fprintf(cmd.ErrOrStderr(), "error: %s\n", msg)
			}
			if report.Failed > 0 {
				return apiErr(fmt.Errorf("%d of %d history entries could not be ingested", report.Failed, report.Scanned))
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&maxItems, "max-items", 0, "Cap how many history records to request (0 = server default)")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "Report the per-entry outcome")
	return cmd
}
