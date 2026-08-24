// ComfyUI experiment surface (`exp new` / `exp run` / `exp show`).
//
// NOT generated — hand-written and preserved across regeneration.
//
// WHY AN EXPERIMENT IS AN OBJECT IN AN EXTERNAL STORE. The sweep that motivated this varied
// virtual_vram_gb over 7 / 10 / 13 and donor_device over cpu / cuda:1: 7 OOM'd, 10 succeeded.
// Arms like that routinely need ComfyUI RESTARTED with different argv, and a restart wipes
// /history (`self.history = {}`, in RAM). So no server-side record of the sweep can survive
// the sweep. The experiment, its arms, each arm's materialised graph, each arm's server
// identity, and each arm's failure all live in SQLite instead.
//
// The three rules this file exists to keep:
//  1. Arms run SERIALLY. They contend for one GPU; two arms in flight invalidate both timings.
//  2. Timing comes only from /history execution_start -> execution_success. Never a log line
//     ("Prompt executed in N seconds" is stale mid-run and once produced a false "+49%
//     regression"), never an s/it sample (a transient, not a rate).
//  3. A failed arm is DATA. It gets a row, an exit class, and its verbatim error text.
package cli

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"comfyui-pp-cli/internal/client"
	"comfyui-pp-cli/internal/cliutil"
	"comfyui-pp-cli/internal/comfy/exp"
	"comfyui-pp-cli/internal/store"
	"github.com/spf13/cobra"
)

// expDefaultArmTimeout bounds the wait for ONE arm. Renders on this box run 30 s to 20 min;
// 45 minutes is generous enough that a legitimate long render is never cut short, and short
// enough that a wedged server does not hold the sweep forever.
const expDefaultArmTimeout = 45 * time.Minute

// expDefaultPollInterval paces /history polling. ComfyUI's history is a RAM dict lookup, so
// polling is cheap; 3 s keeps the reported elapsed time honest without hammering the server.
const expDefaultPollInterval = 3 * time.Second

// expHeartbeatEvery bounds how often a long wait reports progress to stderr.
const expHeartbeatEvery = 30 * time.Second

// ---------------------------------------------------------------------------
// shared plumbing
// ---------------------------------------------------------------------------

// expOpenDomainStore opens the local SQLite store and guarantees the ComfyUI domain tables
// exist. MigrateComfyUI is idempotent, so calling it on every open costs nothing and removes
// the ordering dependency on whichever command happened to run first.
func expOpenDomainStore(ctx context.Context) (*store.Store, error) {
	s, err := store.OpenWithContext(ctx, defaultDBPath("comfyui-pp-cli"))
	if err != nil {
		return nil, err
	}
	if err := store.MigrateComfyUI(ctx, s.DB()); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// expNewPromptID mints the prompt_id CLIENT-SIDE, before the POST. ComfyUI accepts a
// caller-supplied prompt_id, and minting it first means a lost or truncated reply is
// recoverable by lookup instead of by guessing which queued job was ours — the failure mode
// that once had a wrapper resubmit rather than attach, burning ~30 GPU-minutes.
func expNewPromptID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("minting prompt id: %w", err)
	}
	return exp.FormatUUIDv4(b), nil
}

// expLoadGraph reads an API-format graph from disk.
func expLoadGraph(path string) (store.APIGraph, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- operator-supplied graph path.
	if err != nil {
		return nil, fmt.Errorf("reading graph %s: %w", path, err)
	}
	return expDecodeGraph(data, path)
}

func expDecodeGraph(data []byte, source string) (store.APIGraph, error) {
	var g store.APIGraph
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("%s is not an API-format graph (%w)\nhint: the ComfyUI menu's plain \"Save\" writes UI format; use \"Export (API)\" / \"Save (API format)\" instead", source, err)
	}
	if len(g) == 0 {
		return nil, fmt.Errorf("%s contains no nodes", source)
	}
	for id, node := range g {
		if strings.TrimSpace(node.ClassType) == "" {
			return nil, fmt.Errorf("%s: node %q has no class_type — this looks like a UI-format workflow, not an API-format graph", source, id)
		}
	}
	return g, nil
}

// expWarnHostPaths flags LoadImage inputs that carry an absolute host path. ComfyUI resolves
// that input against its OWN input directory and takes a BARE FILENAME; an absolute path is
// accepted by the JSON schema and then fails at execution, or silently loads nothing.
func expWarnHostPaths(g store.APIGraph, w io.Writer) {
	for id, node := range g {
		if node.ClassType != "LoadImage" && node.ClassType != "LoadImageMask" {
			continue
		}
		value, _ := node.Inputs["image"].(string)
		if value == "" {
			continue
		}
		if strings.HasPrefix(value, "/") || strings.HasPrefix(value, `\\`) ||
			(len(value) > 2 && value[1] == ':' && (value[2] == '\\' || value[2] == '/')) {
			fmt.Fprintf(w, "warning: node %s (%s) image=%q is an absolute host path; %s takes a BARE FILENAME inside ComfyUI's input directory\n",
				id, node.ClassType, value, node.ClassType)
		}
	}
}

// expProbeServer reads /system_stats, records the server, and returns its identity.
//
// The identity INCLUDES argv, because that is the thing a memory-knob experiment changes
// between arms. Devices are deliberately not recorded from here: /system_stats identifies GPUs
// by index and name only, and on this box torch's cuda:N order is the INVERSE of nvidia-smi's,
// so an index is not an identity.
func expProbeServer(ctx context.Context, c *client.Client, db *sql.DB) (exp.ServerIdentity, string, error) {
	raw, err := c.GetNoCache(ctx, "/system_stats", nil)
	if err != nil {
		return exp.ServerIdentity{}, "", err
	}
	identity, err := exp.ParseSystemStats(raw)
	if err != nil {
		return exp.ServerIdentity{}, "", err
	}
	id := identity.ID()
	if id == "" {
		return identity, "", nil
	}
	var argvJSON any
	if identity.ArgvKnown {
		if b, mErr := json.Marshal(identity.Argv); mErr == nil {
			argvJSON = string(b)
		}
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO server (id, comfyui_version, frontend_version, python_version, torch_version, argv_json)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET last_seen = CURRENT_TIMESTAMP`,
		id, identity.ComfyUIVersion, identity.FrontendVersion, identity.PythonVersion,
		identity.TorchVersion, argvJSON,
	); err != nil {
		return identity, id, fmt.Errorf("recording server: %w", err)
	}
	return identity, id, nil
}

// expSubmission is one /prompt round trip.
type expSubmission struct {
	PromptID   string          `json:"prompt_id"`
	Status     int             `json:"http_status"`
	Class      string          `json:"outcome"`
	NodeErrors json.RawMessage `json:"node_errors,omitempty"`
	Body       string          `json:"body,omitempty"`
}

// expSubmitGraph POSTs one graph. It returns the outcome CLASS rather than a bare error so the
// caller can tell an HTTP-200-with-node_errors partial accept (one output branch queued, one
// rejected) apart from a clean accept and from a 400 where no branch was executable.
func expSubmitGraph(ctx context.Context, c *client.Client, g store.APIGraph, promptID string) (expSubmission, error) {
	body := map[string]any{"prompt": g, "prompt_id": promptID, "client_id": "comfyui-pp-cli"}
	data, status, err := c.PostWithParams(ctx, "/prompt", nil, body)
	if err != nil {
		var apiError *client.APIError
		if errors.As(err, &apiError) {
			outcome := exp.ParseSubmitResponse([]byte(apiError.Body))
			return expSubmission{
				PromptID:   promptID,
				Status:     apiError.StatusCode,
				Class:      exp.ClassifySubmit(apiError.StatusCode, outcome),
				NodeErrors: outcome.NodeErrors,
				Body:       apiError.Body,
			}, nil
		}
		return expSubmission{PromptID: promptID}, err
	}
	outcome := exp.ParseSubmitResponse(data)
	returned := outcome.PromptID
	if returned == "" {
		returned = promptID
	}
	return expSubmission{
		PromptID:   returned,
		Status:     status,
		Class:      exp.ClassifySubmit(status, outcome),
		NodeErrors: outcome.NodeErrors,
		Body:       string(data),
	}, nil
}

// expPollRun waits for one prompt to reach a terminal state in /history.
//
// This is the ONLY timing source. The loop reads execution_start / execution_success
// timestamps and nothing else — not the server log, not a progress sample.
func expPollRun(ctx context.Context, c *client.Client, promptID string, budget, interval time.Duration, progress io.Writer) (exp.HistoryOutcome, error) {
	if interval <= 0 {
		interval = expDefaultPollInterval
	}
	deadline := time.Now().Add(budget)
	lastBeat := time.Now()
	path := replacePathParam("/history/{prompt_id}", "prompt_id", promptID)
	for {
		raw, err := c.GetNoCache(ctx, path, nil)
		if err != nil {
			var apiError *client.APIError
			if !errors.As(err, &apiError) {
				return exp.HistoryOutcome{}, err
			}
			// A 404 while the job is still queued is normal: the entry appears
			// only once execution begins.
		} else if entry, ok := exp.FindHistoryEntry(raw, promptID); ok {
			outcome := exp.ParseHistoryOutcome(entry)
			if outcome.Terminal() {
				return outcome, nil
			}
		}
		if time.Now().After(deadline) {
			return exp.HistoryOutcome{}, fmt.Errorf("timed out after %s waiting for %s; the run may still be executing — re-run this command to pick it up, or check 'comfyui-pp-cli history get --prompt-id %s'", budget, promptID, promptID)
		}
		if progress != nil && time.Since(lastBeat) >= expHeartbeatEvery {
			lastBeat = time.Now()
			remaining := time.Until(deadline).Round(time.Second)
			if humanFriendly {
				fmt.Fprintf(progress, "waiting on %s (%s of budget left)...\n", promptID, remaining)
			} else {
				fmt.Fprintf(progress, `{"event":"waiting","prompt_id":%q,"budget_left_s":%d}`+"\n", promptID, int(remaining.Seconds()))
			}
		}
		select {
		case <-ctx.Done():
			return exp.HistoryOutcome{}, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// expFinaliseRun writes the authoritative timing and the terminal state.
func expFinaliseRun(ctx context.Context, db *sql.DB, promptID string, outcome exp.HistoryOutcome) (string, string, error) {
	if outcome.HasStart || outcome.HasSuccess {
		// SetRunTiming REFUSES an inverted pair rather than storing a negative duration.
		if err := store.SetRunTiming(ctx, db, promptID, outcome.StartMS, outcome.SuccessMS); err != nil {
			return "", "", err
		}
	}
	state := "completed"
	exitClass := ""
	if outcome.ErrorType != "" || outcome.StatusStr == "error" {
		state = "failed"
		exitClass = exp.ClassifyFailure(outcome.ErrorType, outcome.ErrorMessage)
		if exitClass == "" {
			exitClass = exp.ExitError
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE run SET error_node_id = NULLIF(?, ''), error_node_type = NULLIF(?, ''),
			                error_exception_type = NULLIF(?, ''), error_exception_message = NULLIF(?, '')
			  WHERE prompt_id = ?`,
			outcome.ErrorNodeID, outcome.ErrorNode, outcome.ErrorType, outcome.ErrorMessage, promptID,
		); err != nil {
			return "", "", fmt.Errorf("recording failure detail: %w", err)
		}
		expRecordFailureFingerprint(ctx, db, outcome)
	}
	if err := store.SetRunState(ctx, db, promptID, state, exitClass); err != nil {
		return "", "", err
	}
	return state, exitClass, nil
}

// expRecordFailureFingerprint grows the negative-result corpus. Best-effort: a bookkeeping
// failure must never mask the render failure it is describing.
func expRecordFailureFingerprint(ctx context.Context, db *sql.DB, outcome exp.HistoryOutcome) {
	if outcome.ErrorType == "" && outcome.ErrorMessage == "" {
		return
	}
	normalised := firstLineOf(outcome.ErrorMessage)
	fingerprint := exp.ClassifyFailure(outcome.ErrorType, outcome.ErrorMessage) + "|" + outcome.ErrorType + "|" + outcome.ErrorNode
	_, _ = db.ExecContext(ctx,
		`INSERT INTO failure_fingerprint (fingerprint, exception_type, normalised_message, node_class)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(fingerprint) DO UPDATE SET occurrences = occurrences + 1, last_seen = CURRENT_TIMESTAMP`,
		fingerprint, outcome.ErrorType, normalised, outcome.ErrorNode)
}

func firstLineOf(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// expVerifyShortCircuit reports whether the process is running under the verify harness with
// live HTTP withheld. Submitting under that mode would record a run that never happened.
func expVerifyShortCircuit() bool {
	return cliutil.IsVerifyEnv() && !cliutil.IsVerifyLiveHTTPEnv()
}

// ---------------------------------------------------------------------------
// stored experiment shapes
// ---------------------------------------------------------------------------

type expExperimentRow struct {
	ID    int64
	Name  string
	Spec  exp.ArmSpec
	Notes string
}

type expArmRow struct {
	ID       int64
	Label    string
	ServerID string
	Record   exp.ArmRecord
}

func expLoadExperiment(ctx context.Context, db *sql.DB, name string) (expExperimentRow, error) {
	var row expExperimentRow
	var spec sql.NullString
	var notes sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT id, name, spec_json, notes FROM experiment WHERE name = ?`, name).
		Scan(&row.ID, &row.Name, &spec, &notes)
	if err == sql.ErrNoRows {
		return row, notFoundErr(fmt.Errorf("no experiment named %q (create one with 'comfyui-pp-cli exp new %s --vary <addr>=<v1>,<v2>')", name, name))
	}
	if err != nil {
		return row, err
	}
	if spec.Valid && spec.String != "" {
		_ = json.Unmarshal([]byte(spec.String), &row.Spec)
	}
	row.Notes = notes.String
	return row, nil
}

func expLoadArms(ctx context.Context, db *sql.DB, experimentID int64) ([]expArmRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, COALESCE(label, ''), COALESCE(server_id, ''), COALESCE(vars_json, '')
		   FROM exp_arm WHERE experiment_id = ? ORDER BY id`, experimentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []expArmRow
	for rows.Next() {
		var arm expArmRow
		var varsJSON string
		if err := rows.Scan(&arm.ID, &arm.Label, &arm.ServerID, &varsJSON); err != nil {
			return nil, err
		}
		if varsJSON != "" {
			_ = json.Unmarshal([]byte(varsJSON), &arm.Record)
		}
		if arm.Record.Label == "" {
			arm.Record.Label = arm.Label
		}
		out = append(out, arm)
	}
	return out, rows.Err()
}

// expLoadArmFacts reads the LATEST run for each arm. Latest, not first: an arm that OOM'd and
// was re-run should report the re-run, while the OOM stays in the store as evidence.
func expLoadArmFacts(ctx context.Context, db *sql.DB, arms []expArmRow) (map[string]exp.RunFacts, error) {
	facts := map[string]exp.RunFacts{}
	for _, arm := range arms {
		var (
			promptID     string
			serverID     sql.NullString
			state        string
			exitClass    sql.NullString
			completeness sql.NullString
			duration     sql.NullInt64
			nodeErrors   sql.NullString
			errType      sql.NullString
			errMessage   sql.NullString
		)
		err := db.QueryRowContext(ctx,
			`SELECT prompt_id, server_id, state, exit_class, completeness, duration_ms,
			        node_errors_json, error_exception_type, error_exception_message
			   FROM run WHERE exp_arm_id = ?
			  ORDER BY submitted_at DESC, rowid DESC LIMIT 1`, arm.ID).
			Scan(&promptID, &serverID, &state, &exitClass, &completeness, &duration,
				&nodeErrors, &errType, &errMessage)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		facts[arm.Record.Label] = exp.RunFacts{
			PromptID:     promptID,
			ServerID:     serverID.String,
			State:        state,
			ExitClass:    exitClass.String,
			Completeness: completeness.String,
			DurationMS:   duration.Int64,
			HasDuration:  duration.Valid && duration.Int64 > 0,
			NodeErrors:   nodeErrors.String,
			ErrorType:    errType.String,
			ErrorMessage: errMessage.String,
		}
	}
	return facts, nil
}

func expArmsFromRows(rows []expArmRow) []exp.Arm {
	out := make([]exp.Arm, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Record.ToArm())
	}
	return out
}

// expGraphForArm returns the arm's materialised graph, preferring the stored one. The stored
// graph is the whole point: months later, after the template file has moved and the server has
// restarted a dozen times, the arm still renders exactly what it rendered before.
func expGraphForArm(ctx context.Context, db *sql.DB, arm expArmRow, template store.APIGraph) (store.APIGraph, string, error) {
	if arm.Record.GraphSHA != "" {
		var apiJSON string
		err := db.QueryRowContext(ctx, `SELECT api_json FROM graph WHERE sha256 = ?`, arm.Record.GraphSHA).Scan(&apiJSON)
		if err == nil {
			g, decodeErr := expDecodeGraph([]byte(apiJSON), "stored graph "+arm.Record.GraphSHA[:8])
			if decodeErr != nil {
				return nil, "", decodeErr
			}
			return g, arm.Record.GraphSHA, nil
		}
		if err != sql.ErrNoRows {
			return nil, "", err
		}
	}
	if template == nil {
		return nil, "", fmt.Errorf("arm %q has no materialised graph; pass --graph <g.json> so it can be built now", arm.Record.Label)
	}
	patched, patches, err := exp.ApplyArm(template, arm.Record.ToArm())
	if err != nil {
		return nil, "", err
	}
	sha, err := expStoreArmGraph(ctx, db, template, patched, patches, arm.ID)
	if err != nil {
		return nil, "", err
	}
	return patched, sha, nil
}

// expStoreArmGraph content-addresses the patched graph and records the slot mutations that
// produced it, so a run can be reconstructed from template + patch chain.
func expStoreArmGraph(ctx context.Context, db *sql.DB, template, patched store.APIGraph, patches []exp.PatchRecord, armID int64) (string, error) {
	inSHA, err := store.GraphSHA(template)
	if err != nil {
		return "", err
	}
	outSHA, err := store.UpsertGraph(ctx, db, patched, "", nil)
	if err != nil {
		return "", err
	}
	for _, patch := range patches {
		oldJSON, _ := json.Marshal(patch.OldValue)
		newJSON, _ := json.Marshal(patch.NewValue)
		if _, err := db.ExecContext(ctx,
			`INSERT INTO patch (graph_in_sha, graph_out_sha, address, node_id, expected_class, old_value, new_value)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			inSHA, outSHA, patch.Address, patch.NodeID, patch.ClassType, string(oldJSON), string(newJSON),
		); err != nil {
			return "", fmt.Errorf("recording patch: %w", err)
		}
	}
	if armID > 0 {
		var varsJSON string
		if err := db.QueryRowContext(ctx, `SELECT COALESCE(vars_json, '') FROM exp_arm WHERE id = ?`, armID).Scan(&varsJSON); err == nil && varsJSON != "" {
			var record exp.ArmRecord
			if json.Unmarshal([]byte(varsJSON), &record) == nil {
				record.GraphSHA = outSHA
				if updated, mErr := json.Marshal(record); mErr == nil {
					_, _ = db.ExecContext(ctx, `UPDATE exp_arm SET vars_json = ? WHERE id = ?`, string(updated), armID)
				}
			}
		}
	}
	return outSHA, nil
}

// ---------------------------------------------------------------------------
// exp
// ---------------------------------------------------------------------------

// newComfyExpCmd is the experiment parent command.
//
// The family reads both sides on purpose: `exp new` and `exp show` work entirely
// from SQLite (an experiment must survive the restarts its own arms force), while
// `exp run` submits to the live server and reads /history for each arm's timing.
//
// pp:data-source auto
func newComfyExpCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exp",
		Short: "Multi-arm render experiments that survive a ComfyUI restart",
		Long: `Multi-arm render experiments that survive a ComfyUI restart.

A memory-knob sweep varies one or more slots across a set of values, renders each
combination, and compares them. Arms routinely need ComfyUI restarted with different
argv, and a restart wipes /history — so the experiment, its arms, each arm's graph, and
each arm's failure are recorded locally instead of on the server.

Failed arms are first-class results. An arm that OOM'd is the answer to the question the
sweep was asking.`,
		Annotations: map[string]string{"mcp:read-only": "true", "pp:parent-group": "true"},
		RunE:        parentNoSubcommandRunE(flags),
	}
	cmd.AddCommand(newComfyExpNewCmd(flags))
	cmd.AddCommand(newComfyExpRunCmd(flags))
	cmd.AddCommand(newComfyExpShowCmd(flags))
	return cmd
}

// ---------------------------------------------------------------------------
// exp new
// ---------------------------------------------------------------------------

type expNewReport struct {
	Experiment string         `json:"experiment"`
	Mode       string         `json:"mode"`
	Arms       int            `json:"arms"`
	GraphPath  string         `json:"graph_path,omitempty"`
	Vary       []exp.Var      `json:"vary"`
	Records    []expArmDigest `json:"arm_list"`
}

type expArmDigest struct {
	Index    int               `json:"index"`
	Label    string            `json:"label"`
	Vars     map[string]string `json:"vars"`
	GraphSHA string            `json:"graph_sha,omitempty"`
}

func newComfyExpNewCmd(flags *rootFlags) *cobra.Command {
	var (
		varySpecs []string
		graphPath string
		zip       bool
		notes     string
	)
	cmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Define an experiment and expand its arms",
		Long: `Define an experiment and expand its arms.

Each --vary contributes one dimension: --vary <node>.<input>=<v1>,<v2>,...
Dimensions combine as a cartesian product, or pairwise with --zip.

With --graph, every arm's patched graph is materialised and content-addressed now, so the
sweep stays reproducible after the template file moves and the server restarts.`,
		Example: `  comfyui-pp-cli exp new vram-sweep \
    --vary 12.virtual_vram_gb=7,10,13 \
    --vary 12.donor_device=cpu,cuda:1 \
    --graph flux.api.json

  comfyui-pp-cli exp new paired --zip \
    --vary 12.virtual_vram_gb=7,13 --vary 12.donor_device=cpu,cuda:1 --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "exp new")
			}
			if len(args) == 0 {
				return cmd.Help()
			}
			name := strings.TrimSpace(args[0])
			if name == "" {
				return usageErr(errors.New("experiment name must not be empty"))
			}
			if len(varySpecs) == 0 {
				return usageErr(fmt.Errorf("no dimensions given: pass at least one --vary <node>.<input>=<v1>,<v2>\nexample: %s exp new %s --vary 12.virtual_vram_gb=7,10,13", cmd.Root().Name(), name))
			}

			vars := make([]exp.Var, 0, len(varySpecs))
			for _, spec := range varySpecs {
				v, err := exp.ParseVary(spec)
				if err != nil {
					return usageErr(err)
				}
				vars = append(vars, v)
			}
			arms, err := exp.Expand(vars, exp.ParseMode(zip))
			if err != nil {
				return usageErr(err)
			}

			var template store.APIGraph
			if graphPath != "" {
				template, err = expLoadGraph(graphPath)
				if err != nil {
					return err
				}
				expWarnHostPaths(template, cmd.ErrOrStderr())
				// Validate every arm BEFORE writing anything: a mistyped address should
				// fail the whole command, not leave half an experiment in the store.
				for _, arm := range arms {
					if _, _, err := exp.ApplyArm(template, arm); err != nil {
						return usageErr(err)
					}
				}
			}

			s, err := expOpenDomainStore(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			db := s.DB()

			var existing int64
			switch err := db.QueryRowContext(cmd.Context(), `SELECT id FROM experiment WHERE name = ?`, name).Scan(&existing); {
			case err == nil:
				return usageErr(fmt.Errorf("experiment %q already exists (id %d); pick another name or inspect it with 'exp show %s'", name, existing, name))
			case err != sql.ErrNoRows:
				return err
			}

			spec := exp.ArmSpec{Mode: exp.ParseMode(zip).String(), Vary: vars, GraphPath: graphPath}
			if template != nil {
				if sha, shaErr := store.GraphSHA(template); shaErr == nil {
					spec.GraphSHA = sha
				}
			}
			for _, arm := range arms {
				spec.Labels = append(spec.Labels, arm.Label)
			}
			specJSON, err := json.Marshal(spec)
			if err != nil {
				return err
			}
			result, err := db.ExecContext(cmd.Context(),
				`INSERT INTO experiment (name, spec_json, notes) VALUES (?, ?, ?)`,
				name, string(specJSON), nullString(notes))
			if err != nil {
				return fmt.Errorf("creating experiment: %w", err)
			}
			experimentID, err := result.LastInsertId()
			if err != nil {
				return err
			}

			report := expNewReport{
				Experiment: name,
				Mode:       spec.Mode,
				Arms:       len(arms),
				GraphPath:  graphPath,
				Vary:       vars,
			}
			for _, arm := range arms {
				graphSHA := ""
				if template != nil {
					patched, patches, applyErr := exp.ApplyArm(template, arm)
					if applyErr != nil {
						return applyErr
					}
					graphSHA, err = expStoreArmGraph(cmd.Context(), db, template, patched, patches, 0)
					if err != nil {
						return err
					}
				}
				record := arm.ToRecord(graphSHA)
				recordJSON, mErr := json.Marshal(record)
				if mErr != nil {
					return mErr
				}
				// server_id is deliberately left NULL here and written when the arm RUNS.
				// Arms legitimately span restarts with different argv; recording the
				// server that happened to be up at definition time would attribute the
				// arm to a process that never rendered it.
				if _, err := db.ExecContext(cmd.Context(),
					`INSERT INTO exp_arm (experiment_id, label, vars_json) VALUES (?, ?, ?)`,
					experimentID, arm.Label, string(recordJSON)); err != nil {
					return fmt.Errorf("creating arm %q: %w", arm.Label, err)
				}
				report.Records = append(report.Records, expArmDigest{
					Index: arm.Index, Label: arm.Label, Vars: arm.Vars, GraphSHA: graphSHA,
				})
			}

			if flags.asJSON {
				return printJSONFiltered(cmd.OutOrStdout(), report, flags)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "experiment %s: %d arms (%s)\n", name, len(arms), spec.Mode)
			headers := append([]string{"#", "ARM"}, arms[0].Addrs...)
			if template != nil {
				headers = append(headers, "GRAPH")
			}
			rows := make([][]string, 0, len(report.Records))
			for i, digest := range report.Records {
				row := []string{fmt.Sprintf("%d", digest.Index), digest.Label}
				row = append(row, arms[i].Values...)
				if template != nil {
					row = append(row, expShortSHA(digest.GraphSHA))
				}
				rows = append(rows, row)
			}
			if err := flags.printTable(cmd, headers, rows); err != nil {
				return err
			}
			if template == nil {
				fmt.Fprintf(w, "\nno --graph given: arm graphs will be materialised on 'exp run %s --graph <g.json>'\n", name)
			} else {
				fmt.Fprintf(w, "\nrun it: %s exp run %s\n", cmd.Root().Name(), name)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&varySpecs, "vary", nil, "Varied dimension as <node>.<input>=<v1>,<v2>,... (repeatable; escape a literal comma as \\,)")
	cmd.Flags().StringVar(&graphPath, "graph", "", "API-format graph to patch per arm")
	cmd.Flags().BoolVar(&zip, "zip", false, "Pair dimensions index-by-index instead of taking the cartesian product")
	cmd.Flags().StringVar(&notes, "notes", "", "Free-text note stored with the experiment")
	return cmd
}

func expShortSHA(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

func nullString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// ---------------------------------------------------------------------------
// exp run
// ---------------------------------------------------------------------------

type expArmRunReport struct {
	Label      string          `json:"label"`
	PromptID   string          `json:"prompt_id,omitempty"`
	ServerID   string          `json:"server_id,omitempty"`
	GraphSHA   string          `json:"graph_sha,omitempty"`
	Outcome    string          `json:"outcome"`
	State      string          `json:"state,omitempty"`
	ExitClass  string          `json:"exit_class,omitempty"`
	DurationMS int64           `json:"duration_ms,omitempty"`
	NodeErrors json.RawMessage `json:"node_errors,omitempty"`
	Error      string          `json:"error,omitempty"`
	Skipped    string          `json:"skipped,omitempty"`
	Attached   bool            `json:"attached,omitempty"`
}

type expRunReport struct {
	Experiment string            `json:"experiment"`
	Arms       []expArmRunReport `json:"arms"`
	Submitted  int               `json:"submitted"`
	Completed  int               `json:"completed"`
	Failed     int               `json:"failed"`
	Skipped    int               `json:"skipped"`
	Comparison *exp.Comparison   `json:"comparison,omitempty"`
}

func newComfyExpRunCmd(flags *rootFlags) *cobra.Command {
	var (
		graphPath    string
		armTimeout   time.Duration
		pollInterval time.Duration
		noWait       bool
		force        bool
		only         string
	)
	cmd := &cobra.Command{
		Use:   "run <name>",
		Short: "Render every arm of an experiment, one at a time",
		Long: `Render every arm of an experiment, one at a time.

Arms are submitted SERIALLY and each is waited out before the next is queued. That is not
politeness: the arms contend for one GPU, and two renders in flight make both durations
meaningless. Each arm records the server identity (including argv) it actually ran under,
because arms legitimately span restarts.

An arm already in flight is ATTACHED to rather than resubmitted — the submit lease dedupes
on the exact graph hash. Resubmitting instead of attaching once burned ~30 GPU-minutes.`,
		Example: `  comfyui-pp-cli exp run vram-sweep
  comfyui-pp-cli exp run vram-sweep --graph flux.api.json --arm-timeout 20m
  comfyui-pp-cli exp run vram-sweep --only virtual_vram_gb=10+donor_device=cpu --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "exp run")
			}
			if len(args) == 0 {
				return cmd.Help()
			}
			if expVerifyShortCircuit() {
				return writeNoop(cmd.OutOrStdout(), flags, "verify_short_circuit", "verify mode: refusing to queue real renders")
			}
			name := strings.TrimSpace(args[0])

			s, err := expOpenDomainStore(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			db := s.DB()

			experiment, err := expLoadExperiment(cmd.Context(), db, name)
			if err != nil {
				return err
			}
			arms, err := expLoadArms(cmd.Context(), db, experiment.ID)
			if err != nil {
				return err
			}
			if len(arms) == 0 {
				return fmt.Errorf("experiment %q has no arms", name)
			}

			var template store.APIGraph
			if graphPath != "" {
				template, err = expLoadGraph(graphPath)
				if err != nil {
					return err
				}
				expWarnHostPaths(template, cmd.ErrOrStderr())
			} else if experiment.Spec.GraphPath != "" {
				if loaded, loadErr := expLoadGraph(experiment.Spec.GraphPath); loadErr == nil {
					template = loaded
				}
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}

			facts, err := expLoadArmFacts(cmd.Context(), db, arms)
			if err != nil {
				return err
			}

			report := expRunReport{Experiment: name}
			progress := cmd.ErrOrStderr()
			for _, arm := range arms {
				label := arm.Record.Label
				if only != "" && only != label {
					continue
				}
				armReport := expArmRunReport{Label: label}

				if prior, seen := facts[label]; seen && !force {
					verdict := exp.Verdict(prior, true)
					if verdict == exp.VerdictPass || verdict == exp.VerdictFail || verdict == exp.VerdictPartial {
						armReport.Outcome = "skipped"
						armReport.Skipped = "already has a terminal run (" + prior.State + "); pass --force to re-run"
						armReport.PromptID = prior.PromptID
						armReport.State = prior.State
						armReport.ExitClass = prior.ExitClass
						armReport.DurationMS = prior.DurationMS
						report.Skipped++
						report.Arms = append(report.Arms, armReport)
						continue
					}
				}

				graph, graphSHA, err := expGraphForArm(cmd.Context(), db, arm, template)
				if err != nil {
					armReport.Outcome = "error"
					armReport.Error = err.Error()
					report.Arms = append(report.Arms, armReport)
					report.Failed++
					continue
				}
				armReport.GraphSHA = graphSHA

				identity, serverID, err := expProbeServer(cmd.Context(), c, db)
				if err != nil {
					return classifyAPIError(cmd.OutOrStdout(), err, flags)
				}
				armReport.ServerID = serverID
				if serverID != "" {
					// Written per ARM, per RUN: the arm before this one may have executed
					// under a different process with different argv.
					if _, err := db.ExecContext(cmd.Context(), `UPDATE exp_arm SET server_id = ? WHERE id = ?`, serverID, arm.ID); err != nil {
						return err
					}
				}

				shapeSHA, err := store.ShapeSHA(graph)
				if err != nil {
					return err
				}

				// Submit lease: an identical graph already in flight is ATTACHED to.
				if activeID, found, err := store.FindActiveRunByGraphSHA(cmd.Context(), db, graphSHA); err != nil {
					return err
				} else if found {
					armReport.PromptID = activeID
					armReport.Attached = true
					armReport.Outcome = "attached"
					fmt.Fprintf(progress, "arm %s: attaching to in-flight run %s instead of resubmitting\n", label, activeID)
					if noWait {
						report.Arms = append(report.Arms, armReport)
						break
					}
					outcome, waitErr := expPollRun(cmd.Context(), c, activeID, armTimeout, pollInterval, progress)
					if waitErr != nil {
						armReport.Error = waitErr.Error()
						report.Arms = append(report.Arms, armReport)
						return expFinishRunReport(cmd, flags, db, arms, &report, waitErr)
					}
					state, exitClass, finErr := expFinaliseRun(cmd.Context(), db, activeID, outcome)
					if finErr != nil {
						return finErr
					}
					armReport.State, armReport.ExitClass = state, exitClass
					if d, ok := outcome.DurationMS(); ok {
						armReport.DurationMS = d
					}
					expTallyArm(&report, armReport)
					report.Arms = append(report.Arms, armReport)
					continue
				}

				promptID, err := expNewPromptID()
				if err != nil {
					return err
				}
				argvJSON := ""
				if identity.ArgvKnown {
					if b, mErr := json.Marshal(identity.Argv); mErr == nil {
						argvJSON = string(b)
					}
				}
				// The row is written BEFORE the POST so a lost reply is recoverable.
				if err := store.InsertRun(cmd.Context(), db, store.RunRow{
					PromptID: promptID,
					GraphSHA: graphSHA,
					ShapeSHA: shapeSHA,
					ServerID: serverID,
					State:    "submitted",
					BatchID:  sql.NullString{String: "exp:" + name, Valid: true},
				}); err != nil {
					return err
				}
				if _, err := db.ExecContext(cmd.Context(),
					`UPDATE run SET exp_arm_id = ?, argv_json = NULLIF(?, '') WHERE prompt_id = ?`,
					arm.ID, argvJSON, promptID); err != nil {
					return err
				}

				submission, err := expSubmitGraph(cmd.Context(), c, graph, promptID)
				if err != nil {
					_ = store.SetRunState(cmd.Context(), db, promptID, "failed", "transport")
					return classifyAPIError(cmd.OutOrStdout(), err, flags)
				}
				if submission.PromptID != promptID {
					// The server minted its own id; follow it rather than tracking a phantom.
					if _, err := db.ExecContext(cmd.Context(),
						`UPDATE run SET prompt_id = ? WHERE prompt_id = ?`, submission.PromptID, promptID); err != nil {
						return err
					}
					promptID = submission.PromptID
				}
				armReport.PromptID = promptID
				armReport.Outcome = submission.Class
				armReport.NodeErrors = submission.NodeErrors
				report.Submitted++

				if len(submission.NodeErrors) > 0 {
					if _, err := db.ExecContext(cmd.Context(),
						`UPDATE run SET node_errors_json = ?, completeness = 'partial' WHERE prompt_id = ?`,
						string(submission.NodeErrors), promptID); err != nil {
						return err
					}
				}

				switch submission.Class {
				case exp.SubmitRejected, exp.SubmitUnrecognisable:
					// Rejected is a RESULT, not an abort: the next arm may well be valid,
					// and a rejected arm belongs in the comparison table.
					if err := store.SetRunState(cmd.Context(), db, promptID, "failed", exp.ExitValidation); err != nil {
						return err
					}
					armReport.State, armReport.ExitClass = "failed", exp.ExitValidation
					report.Failed++
					fmt.Fprintf(progress, "arm %s: REJECTED (HTTP %d)\n", label, submission.Status)
					expPrintVerbatim(progress, "node_errors", submission.NodeErrors)
					if submission.NodeErrors == nil && submission.Body != "" {
						fmt.Fprintf(progress, "  response body: %s\n", submission.Body)
					}
					report.Arms = append(report.Arms, armReport)
					continue
				case exp.SubmitPartialAccept:
					fmt.Fprintf(progress, "arm %s: PARTIAL ACCEPT — one or more output branches were rejected while the rest were queued\n", label)
					expPrintVerbatim(progress, "node_errors", submission.NodeErrors)
				default:
					fmt.Fprintf(progress, "arm %s: queued as %s\n", label, promptID)
				}

				if noWait {
					fmt.Fprintf(progress, "--no-wait: stopping after one arm so the remaining arms cannot run concurrently; re-run to continue\n")
					report.Arms = append(report.Arms, armReport)
					break
				}

				outcome, waitErr := expPollRun(cmd.Context(), c, promptID, armTimeout, pollInterval, progress)
				if waitErr != nil {
					armReport.Error = waitErr.Error()
					report.Arms = append(report.Arms, armReport)
					return expFinishRunReport(cmd, flags, db, arms, &report, waitErr)
				}
				state, exitClass, err := expFinaliseRun(cmd.Context(), db, promptID, outcome)
				if err != nil {
					return err
				}
				armReport.State, armReport.ExitClass = state, exitClass
				if d, ok := outcome.DurationMS(); ok {
					armReport.DurationMS = d
				}
				if state == "failed" {
					fmt.Fprintf(progress, "arm %s: FAILED (%s) %s\n", label, exitClass, firstLineOf(outcome.ErrorMessage))
				} else {
					fmt.Fprintf(progress, "arm %s: completed in %s\n", label, exp.FormatDuration(armReport.DurationMS))
				}
				expTallyArm(&report, armReport)
				report.Arms = append(report.Arms, armReport)
			}

			return expFinishRunReport(cmd, flags, db, arms, &report, nil)
		},
	}
	cmd.Flags().StringVar(&graphPath, "graph", "", "API-format graph to materialise arms from when they were defined without one")
	cmd.Flags().DurationVar(&armTimeout, "arm-timeout", expDefaultArmTimeout, "Maximum wait for a single arm to finish")
	cmd.Flags().DurationVar(&pollInterval, "poll-interval", expDefaultPollInterval, "How often to poll /history while an arm renders")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Submit only the next pending arm and return its handle (arms must not run concurrently)")
	cmd.Flags().BoolVar(&force, "force", false, "Re-run arms that already have a terminal run")
	cmd.Flags().StringVar(&only, "only", "", "Run just the arm with this label")
	return cmd
}

func expTallyArm(report *expRunReport, arm expArmRunReport) {
	switch arm.State {
	case "completed":
		report.Completed++
	case "failed":
		report.Failed++
	}
}

// expFinishRunReport prints the run report, always including the full comparison table so a
// sweep that aborted halfway still shows which arms produced what.
func expFinishRunReport(cmd *cobra.Command, flags *rootFlags, db *sql.DB, arms []expArmRow, report *expRunReport, runErr error) error {
	facts, err := expLoadArmFacts(cmd.Context(), db, arms)
	if err == nil {
		comparison := exp.BuildComparison(expArmsFromRows(arms), facts)
		report.Comparison = &comparison
	}
	if flags.asJSON {
		if printErr := printJSONFiltered(cmd.OutOrStdout(), report, flags); printErr != nil {
			return printErr
		}
		return runErr
	}
	if report.Comparison != nil {
		if printErr := expRenderComparison(cmd, flags, report.Experiment, *report.Comparison); printErr != nil {
			return printErr
		}
	}
	return runErr
}

func expPrintVerbatim(w io.Writer, label string, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var pretty any
	if json.Unmarshal(raw, &pretty) == nil {
		if b, err := json.MarshalIndent(pretty, "  ", "  "); err == nil {
			fmt.Fprintf(w, "  %s (verbatim):\n  %s\n", label, b)
			return
		}
	}
	fmt.Fprintf(w, "  %s (verbatim): %s\n", label, raw)
}

// ---------------------------------------------------------------------------
// exp show
// ---------------------------------------------------------------------------

type expShowReport struct {
	Experiment string         `json:"experiment"`
	Mode       string         `json:"mode,omitempty"`
	Notes      string         `json:"notes,omitempty"`
	Vary       []exp.Var      `json:"vary,omitempty"`
	Comparison exp.Comparison `json:"comparison"`
}

func newComfyExpShowCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Compare every arm of an experiment, failures included",
		Long: `Compare every arm of an experiment, failures included.

Every arm gets a row. An arm that OOM'd shows as FAIL with its exit class and the verbatim
exception; an arm that never ran shows as NOT-RUN. Negative results are the highest-value
artifact of a memory sweep and the easiest to lose — a table of only the arms that worked
has deleted the answer.`,
		Example: `  comfyui-pp-cli exp show vram-sweep
  comfyui-pp-cli exp show vram-sweep --json`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "exp show")
			}
			if len(args) == 0 {
				return cmd.Help()
			}
			name := strings.TrimSpace(args[0])

			s, err := expOpenDomainStore(cmd.Context())
			if err != nil {
				return err
			}
			defer s.Close()
			db := s.DB()

			experiment, err := expLoadExperiment(cmd.Context(), db, name)
			if err != nil {
				return err
			}
			armRows, err := expLoadArms(cmd.Context(), db, experiment.ID)
			if err != nil {
				return err
			}
			facts, err := expLoadArmFacts(cmd.Context(), db, armRows)
			if err != nil {
				return err
			}
			comparison := exp.BuildComparison(expArmsFromRows(armRows), facts)

			if flags.asJSON {
				return printJSONFiltered(cmd.OutOrStdout(), expShowReport{
					Experiment: experiment.Name,
					Mode:       experiment.Spec.Mode,
					Notes:      experiment.Notes,
					Vary:       experiment.Spec.Vary,
					Comparison: comparison,
				}, flags)
			}
			return expRenderComparison(cmd, flags, experiment.Name, comparison)
		},
	}
	return cmd
}

// expRenderComparison prints the comparison table plus a negative-results section.
func expRenderComparison(cmd *cobra.Command, flags *rootFlags, name string, comparison exp.Comparison) error {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "\nexperiment %s — %d arms: %s, %s, %s, %s, %s\n\n", name, comparison.Total,
		green(fmt.Sprintf("%d pass", comparison.Passed)),
		red(fmt.Sprintf("%d fail", comparison.Failed)),
		yellow(fmt.Sprintf("%d partial", comparison.Partial)),
		yellow(fmt.Sprintf("%d pending", comparison.Pending)),
		yellow(fmt.Sprintf("%d not-run", comparison.NotRun)))
	if err := flags.printTable(cmd, comparison.Headers, comparison.TableRows()); err != nil {
		return err
	}
	if comparison.BaselineMS > 0 {
		fmt.Fprintf(w, "\nVS BEST is relative to the fastest passing arm (%s).\n", exp.FormatDuration(comparison.BaselineMS))
	}

	var negatives []exp.Row
	for _, row := range comparison.Rows {
		if row.Verdict == exp.VerdictFail || row.Verdict == exp.VerdictPartial || row.Verdict == exp.VerdictNotRun {
			negatives = append(negatives, row)
		}
	}
	if len(negatives) == 0 {
		return nil
	}
	fmt.Fprintf(w, "\nnegative results (%d) — these are the answer, not the gap:\n", len(negatives))
	for _, row := range negatives {
		fmt.Fprintf(w, "  %s  %s", row.Label, row.Verdict)
		if row.ExitClass != "" {
			fmt.Fprintf(w, " [%s]", row.ExitClass)
		}
		fmt.Fprintln(w)
		if row.Note != "" {
			fmt.Fprintf(w, "      %s\n", row.Note)
		}
		if row.NodeErrors != "" {
			expPrintVerbatim(w, "    node_errors", json.RawMessage(row.NodeErrors))
		}
		if row.PromptID != "" {
			fmt.Fprintf(w, "      prompt_id: %s\n", row.PromptID)
		}
	}
	return nil
}
