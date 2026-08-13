// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
//
// Hand-authored novel command surface (NOT generator output): output provenance.
//
// WHY THESE COMMANDS EXIST. ComfyUI embeds workflow metadata in PNGs ONLY —
// never in mp4/webm, which is most of what gets produced on this box — and even
// the PNG chunk carries no server argv, no model identity, and no timing. So a
// produced file, on its own, cannot answer "what made this?". `provenance`
// answers it from the durable store instead: prompt_id, graph sha, the
// experiment arm's varied values, the duration, the server identity, and the
// staged input the run consumed.
//
// `outputs` covers the other half: fps, duration, and audio-presence are
// properties of the produced FILE and appear in NO ComfyUI endpoint, which is
// exactly what a cross-model comparison needs. ffprobe has them; ffprobe is
// optional, so its absence is a clearly-noted skip, never a failure.
//
// TIMING RULE, enforced here as everywhere: durations come only from
// /history execution_start -> execution_success (via store.SetRunTiming, read
// back through run.duration_ms). Nothing in this file reads a duration out of
// server log text or an s/it progress sample — a stale log line once produced a
// false "+49% regression", and an s/it sample is a transient, not a rate.

package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"comfyui-pp-cli/internal/comfy/media"
	"comfyui-pp-cli/internal/store"
	"github.com/spf13/cobra"
)

// comfyTimingSource is the single sanctioned provenance of a duration. It is
// emitted alongside every duration so a reader never has to wonder whether a
// log line leaked in.
const comfyTimingSource = "history: execution_start -> execution_success"

// ---------------------------------------------------------------------------
// provenance
// ---------------------------------------------------------------------------

type provenanceOutputFile struct {
	ID        int64    `json:"id"`
	NodeID    string   `json:"node_id,omitempty"`
	OutputKey string   `json:"output_key,omitempty"`
	Filename  string   `json:"filename"`
	Subfolder string   `json:"subfolder,omitempty"`
	Type      string   `json:"type,omitempty"`
	AbsPath   string   `json:"abs_path,omitempty"`
	Bytes     *int64   `json:"bytes,omitempty"`
	Width     *int64   `json:"width,omitempty"`
	Height    *int64   `json:"height,omitempty"`
	FPS       *float64 `json:"fps,omitempty"`
	DurationS *float64 `json:"duration_s,omitempty"`
	HasAudio  *bool    `json:"has_audio,omitempty"`
	ProbedAt  string   `json:"probed_at,omitempty"`
}

type provenanceTiming struct {
	ExecutionStartMS   *int64            `json:"execution_start_ms,omitempty"`
	ExecutionSuccessMS *int64            `json:"execution_success_ms,omitempty"`
	DurationMS         *int64            `json:"duration_ms,omitempty"`
	Duration           string            `json:"duration,omitempty"`
	Source             string            `json:"source"`
	ShapeStats         *store.ShapeStats `json:"shape_stats,omitempty"`
}

type provenanceGraph struct {
	SHA256         string         `json:"sha256"`
	ShapeSHA256    string         `json:"shape_sha256,omitempty"`
	NodeCount      int            `json:"node_count,omitempty"`
	ClassHistogram map[string]int `json:"class_histogram,omitempty"`
	TemplateID     string         `json:"template_id,omitempty"`
}

type provenanceDevice struct {
	UUID        string `json:"uuid"`
	Name        string `json:"name,omitempty"`
	ComfyIndex  *int64 `json:"comfy_index,omitempty"`
	NvidiaIndex *int64 `json:"nvidia_index,omitempty"`
	VRAMTotal   *int64 `json:"vram_total,omitempty"`
}

type provenanceServer struct {
	ID              string             `json:"id"`
	ComfyUIVersion  string             `json:"comfyui_version,omitempty"`
	FrontendVersion string             `json:"frontend_version,omitempty"`
	PythonVersion   string             `json:"python_version,omitempty"`
	TorchVersion    string             `json:"torch_version,omitempty"`
	Argv            json.RawMessage    `json:"argv,omitempty"`
	Devices         []provenanceDevice `json:"devices,omitempty"`
	// DeviceIdentityNote records why the indices below are not identities.
	DeviceIdentityNote string `json:"device_identity_note,omitempty"`
}

type provenanceExperiment struct {
	ExperimentID   int64          `json:"experiment_id,omitempty"`
	ExperimentName string         `json:"experiment_name,omitempty"`
	ArmID          int64          `json:"arm_id"`
	ArmLabel       string         `json:"arm_label,omitempty"`
	Vars           map[string]any `json:"vars,omitempty"`
	ArmServerID    string         `json:"arm_server_id,omitempty"`
}

type provenanceInput struct {
	NodeID        string `json:"node_id"`
	ClassType     string `json:"class_type"`
	InputName     string `json:"input_name"`
	GraphValue    string `json:"graph_value"`
	ContentSHA256 string `json:"content_sha256"`
	HostPath      string `json:"host_path,omitempty"`
	Bytes         int64  `json:"bytes,omitempty"`
	StagedAt      string `json:"staged_at,omitempty"`
}

type provenanceFailure struct {
	NodeID           string `json:"node_id,omitempty"`
	NodeType         string `json:"node_type,omitempty"`
	ExceptionType    string `json:"exception_type,omitempty"`
	ExceptionMessage string `json:"exception_message,omitempty"`
	TracebackTail    string `json:"traceback_tail,omitempty"`
}

type provenanceRecord struct {
	PromptID     string                `json:"prompt_id"`
	RunName      string                `json:"run_name,omitempty"`
	State        string                `json:"state,omitempty"`
	ExitClass    string                `json:"exit_class,omitempty"`
	Completeness string                `json:"completeness,omitempty"`
	SubmittedAt  string                `json:"submitted_at,omitempty"`
	BatchID      string                `json:"batch_id,omitempty"`
	Graph        provenanceGraph       `json:"graph"`
	Timing       provenanceTiming      `json:"timing"`
	Server       *provenanceServer     `json:"server,omitempty"`
	Experiment   *provenanceExperiment `json:"experiment,omitempty"`
	Output       provenanceOutputFile  `json:"output"`
	StagedInputs []provenanceInput     `json:"staged_inputs"`
	// NodeErrors is ComfyUI's validation payload, VERBATIM. It is never
	// summarised: a 200-with-node_errors is a partial accept, and the exact
	// per-node text is the only thing that says which branch was rejected.
	NodeErrors json.RawMessage    `json:"node_errors,omitempty"`
	Failure    *provenanceFailure `json:"failure,omitempty"`
	// NodeSet is which node classes and custom-node packs the server offered
	// when this run was submitted. Server identity alone cannot answer "was
	// this the same environment": a custom pack installed or upgraded between
	// two runs changes what a class_type means while every server field stays
	// identical.
	//
	// nil for any run recorded before node-set capture shipped, and for a run
	// submitted with no node schema cached. That absence is reported as
	// "not captured" rather than filled in from the CURRENT node set, which
	// would be a fabricated claim about a past run.
	NodeSet *comfyNodeSetIdentity `json:"node_set,omitempty"`
	Notes   []string              `json:"notes,omitempty"`
}

type provenanceReport struct {
	Query   string             `json:"query"`
	Matches int                `json:"matches"`
	Records []provenanceRecord `json:"records"`
	Notes   []string           `json:"notes,omitempty"`
}

// pp:data-source local
//
// provenance reads only the durable store: the server's own history is a RAM
// dict that a restart destroys, and restarts are exactly what memory-knob
// experiments cause.
func newProvenanceCmd(flags *rootFlags) *cobra.Command {
	var (
		subfolder string
		all       bool
	)

	cmd := &cobra.Command{
		Use:   "provenance <output-filename>",
		Short: "Answer \"what made this file?\" — run, graph, arm, timing, server, and staged input",
		Long: `Given a produced file, find the run that made it and print its full provenance:
prompt_id, graph sha (exact) and shape sha (seed-stripped), the experiment arm's
varied values, the duration, the server identity, and the staged input the run
consumed.

Why this cannot come from the file itself: ComfyUI embeds workflow metadata in
PNGs ONLY — never in mp4/webm, which is most of what gets produced here — and
even the PNG chunk carries no server argv, no model identity, and no timing.

Durations here come only from /history's execution_start -> execution_success
pair. Server log text and s/it progress samples are never a timing source: a
stale log line once produced a false "+49% regression", and an s/it sample is a
transient, not a rate.

Accepts a bare filename, a "subfolder/filename" pair, or a full recorded path.`,
		Example: `  comfyui-pp-cli provenance ComfyUI_00042_.mp4
  comfyui-pp-cli provenance ComfyUI_00042_.png --json
  comfyui-pp-cli provenance clip.webm --all`,
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": "0,2,3",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "provenance")
			}
			if len(args) == 0 {
				return comfyRequiresInput(cmd, flags)
			}
			if len(args) > 1 {
				return usageErr(fmt.Errorf("provenance takes exactly one output filename (got %d)", len(args)))
			}
			if err := validateDataSourceStrategy(flags, "local"); err != nil {
				return usageErr(err)
			}
			return runProvenance(cmd, flags, args[0], subfolder, all)
		},
	}

	cmd.Flags().StringVar(&subfolder, "subfolder", "", "Restrict the match to outputs recorded under this subfolder")
	cmd.Flags().BoolVar(&all, "all", false, "Print every run that produced a file with this name, not just the most recent")

	return cmd
}

func runProvenance(cmd *cobra.Command, flags *rootFlags, target, subfolderFilter string, all bool) error {
	ctx, cancel := boundCtx(cmd.Context(), flags)
	defer cancel()

	s, err := openStagingStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	db := s.DB()

	slashed := filepath.ToSlash(strings.TrimSpace(target))
	base := path.Base(slashed)
	sub := strings.TrimSpace(subfolderFilter)
	if sub == "" {
		if parent := stageParentDir(slashed); parent != "" && !filepath.IsAbs(target) {
			sub = parent
		}
	}

	// An absolute path names ONE file, so an exact abs_path hit wins outright.
	// Falling straight through to the filename match would widen the answer to
	// every run that ever produced a file with that base name.
	var records []provenanceRecord
	if filepath.IsAbs(target) {
		records, err = provenanceLookup(ctx, db, "", target, sub)
		if err != nil {
			return err
		}
	}
	if len(records) == 0 {
		records, err = provenanceLookup(ctx, db, base, target, sub)
		if err != nil {
			return err
		}
	}
	if len(records) == 0 {
		return notFoundErr(fmt.Errorf(
			"no recorded output named %q%s. Outputs are recorded when a run's results are collected; a file produced outside this CLI has no provenance to read",
			base, provenanceSubfolderSuffix(sub)))
	}

	report := provenanceReport{Query: target, Matches: len(records), Records: records}
	if !all && len(records) > 1 {
		report.Records = records[:1]
		report.Notes = append(report.Notes, fmt.Sprintf("%d runs produced a file with this name; showing the most recent. Pass --all for every match.", len(records)))
	}

	for i := range report.Records {
		if err := provenanceEnrich(ctx, db, &report.Records[i]); err != nil {
			return err
		}
	}

	if !wantsHumanTable(cmd.OutOrStdout(), flags) {
		return flags.printJSON(cmd, report)
	}
	return provenanceRender(cmd, report)
}

func provenanceSubfolderSuffix(sub string) string {
	if sub == "" {
		return ""
	}
	return " under subfolder " + sub
}

// provenanceLookup finds the output rows and their runs. The join is LEFT so an
// output whose run row was pruned still reports the file it knows about instead
// of vanishing.
//
// An empty base restricts the match to the recorded absolute path, which is how
// an absolute argument is resolved to exactly one file.
func provenanceLookup(ctx context.Context, db *sql.DB, base, rawTarget, sub string) ([]provenanceRecord, error) {
	query := `SELECT
	    o.id, o.run_id, COALESCE(o.node_id,''), COALESCE(o.output_key,''),
	    o.filename, COALESCE(o.subfolder,''), COALESCE(o.type,''), COALESCE(o.abs_path,''),
	    o.bytes, o.width, o.height, o.fps, o.duration_s, o.has_audio, o.probed_at,
	    COALESCE(r.name,''), COALESCE(r.state,''), COALESCE(r.exit_class,''), COALESCE(r.completeness,''),
	    COALESCE(r.graph_sha,''), COALESCE(r.shape_sha,''), COALESCE(r.server_id,''),
	    r.execution_start_ms, r.execution_success_ms, r.duration_ms, r.submitted_at,
	    r.node_errors_json,
	    COALESCE(r.error_node_id,''), COALESCE(r.error_node_type,''),
	    COALESCE(r.error_exception_type,''), COALESCE(r.error_exception_message,''),
	    COALESCE(r.error_traceback_tail,''),
	    r.exp_arm_id, COALESCE(r.batch_id,'')
	  FROM output o
	  LEFT JOIN run r ON r.prompt_id = o.run_id
	  WHERE `
	args := []any{}
	if base == "" {
		query += `o.abs_path = ?`
		args = append(args, rawTarget)
	} else {
		query += `(o.filename = ? OR o.abs_path = ?)`
		args = append(args, base, rawTarget)
	}
	if sub != "" {
		query += ` AND COALESCE(o.subfolder,'') = ?`
		args = append(args, sub)
	}
	query += ` ORDER BY COALESCE(r.submitted_at, '') DESC, o.id DESC`

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("looking up output: %w", err)
	}
	defer rows.Close()

	var out []provenanceRecord
	for rows.Next() {
		var (
			rec                                  provenanceRecord
			bytesN, widthN, heightN, hasAudioN   sql.NullInt64
			fpsN, durationSN                     sql.NullFloat64
			probedAt, submittedAt                any
			startMS, successMS, durationMS       sql.NullInt64
			nodeErrors                           sql.NullString
			errNodeID, errNodeType, errExcType   string
			errExcMessage, errTracebackTail      string
			expArmID                             sql.NullInt64
			serverID, graphSHA, shapeSHA, batchI string
		)
		if err := rows.Scan(
			&rec.Output.ID, &rec.PromptID, &rec.Output.NodeID, &rec.Output.OutputKey,
			&rec.Output.Filename, &rec.Output.Subfolder, &rec.Output.Type, &rec.Output.AbsPath,
			&bytesN, &widthN, &heightN, &fpsN, &durationSN, &hasAudioN, &probedAt,
			&rec.RunName, &rec.State, &rec.ExitClass, &rec.Completeness,
			&graphSHA, &shapeSHA, &serverID,
			&startMS, &successMS, &durationMS, &submittedAt,
			&nodeErrors,
			&errNodeID, &errNodeType, &errExcType, &errExcMessage, &errTracebackTail,
			&expArmID, &batchI,
		); err != nil {
			return nil, fmt.Errorf("reading output row: %w", err)
		}

		rec.Output.Bytes = nullInt64Ptr(bytesN)
		rec.Output.Width = nullInt64Ptr(widthN)
		rec.Output.Height = nullInt64Ptr(heightN)
		rec.Output.FPS = nullFloatPtr(fpsN)
		rec.Output.DurationS = nullFloatPtr(durationSN)
		rec.Output.HasAudio = nullBoolPtr(hasAudioN)
		rec.Output.ProbedAt = comfyTimeString(probedAt)
		rec.SubmittedAt = comfyTimeString(submittedAt)
		rec.BatchID = batchI
		rec.Graph = provenanceGraph{SHA256: graphSHA, ShapeSHA256: shapeSHA}
		rec.Timing = provenanceTiming{
			ExecutionStartMS:   nullInt64Ptr(startMS),
			ExecutionSuccessMS: nullInt64Ptr(successMS),
			DurationMS:         nullInt64Ptr(durationMS),
			Duration:           comfyHumanMillis(durationMS),
			Source:             comfyTimingSource,
		}
		if nodeErrors.Valid && strings.TrimSpace(nodeErrors.String) != "" && strings.TrimSpace(nodeErrors.String) != "{}" {
			rec.NodeErrors = json.RawMessage(nodeErrors.String)
		}
		if errNodeID != "" || errExcType != "" || errExcMessage != "" || errTracebackTail != "" {
			rec.Failure = &provenanceFailure{
				NodeID:           errNodeID,
				NodeType:         errNodeType,
				ExceptionType:    errExcType,
				ExceptionMessage: errExcMessage,
				TracebackTail:    errTracebackTail,
			}
		}
		if serverID != "" {
			rec.Server = &provenanceServer{ID: serverID}
		}
		if expArmID.Valid {
			rec.Experiment = &provenanceExperiment{ArmID: expArmID.Int64}
		}
		rec.StagedInputs = []provenanceInput{}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// provenanceEnrich fills in the joins that need their own queries: the graph,
// the server + its devices, the experiment arm, and the staged inputs.
func provenanceEnrich(ctx context.Context, db *sql.DB, rec *provenanceRecord) error {
	var apiJSON []byte
	if rec.Graph.SHA256 != "" {
		var (
			shapeSHA   string
			nodeCount  sql.NullInt64
			histogram  sql.NullString
			templateID string
			raw        sql.NullString
		)
		err := db.QueryRowContext(ctx,
			`SELECT COALESCE(shape_sha256,''), api_json, node_count, class_histogram_json, COALESCE(template_id,'')
			   FROM graph WHERE sha256 = ?`, rec.Graph.SHA256).
			Scan(&shapeSHA, &raw, &nodeCount, &histogram, &templateID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			rec.Notes = append(rec.Notes, "the run's graph body is no longer in the store; only its sha is known.")
		case err != nil:
			return fmt.Errorf("reading graph: %w", err)
		default:
			if rec.Graph.ShapeSHA256 == "" {
				rec.Graph.ShapeSHA256 = shapeSHA
			}
			rec.Graph.NodeCount = int(nodeCount.Int64)
			rec.Graph.TemplateID = templateID
			if histogram.Valid {
				_ = json.Unmarshal([]byte(histogram.String), &rec.Graph.ClassHistogram)
			}
			if raw.Valid {
				apiJSON = []byte(raw.String)
			}
		}
	}

	if rec.Graph.ShapeSHA256 != "" {
		if stats, err := store.ShapeStatsFor(ctx, db, rec.Graph.ShapeSHA256); err == nil && stats.N > 0 {
			rec.Timing.ShapeStats = &stats
		}
	}

	if rec.Server != nil {
		if err := provenanceLoadServer(ctx, db, rec.Server); err != nil {
			return err
		}
	}
	// Node-set identity (comfy_nodeset.go). Absent for runs recorded before
	// capture shipped; left nil rather than back-filled from the current node
	// set, which would assert something about the past that was never
	// observed.
	if identity, ok := comfyLoadNodeSetForRun(ctx, db, rec.PromptID); ok {
		rec.NodeSet = &identity
	} else {
		rec.Notes = append(rec.Notes,
			"node set not captured for this run: it predates node-set capture, or no node schema was cached when it was submitted. "+
				"Runs submitted from now on record which classes and custom-node packs the server offered.")
	}
	if rec.Experiment != nil {
		if err := provenanceLoadArm(ctx, db, rec.Experiment); err != nil {
			return err
		}
	}
	if len(apiJSON) > 0 {
		inputs, err := provenanceStagedInputs(ctx, db, apiJSON)
		if err != nil {
			return err
		}
		rec.StagedInputs = inputs
	}
	if len(rec.StagedInputs) == 0 {
		rec.Notes = append(rec.Notes, "no staged input recorded for this run. If it consumed a LoadImage file, run `comfyui-pp-cli stage <hostpath>` before submitting so the filename stays resolvable to its content.")
	}
	return nil
}

func provenanceLoadServer(ctx context.Context, db *sql.DB, srv *provenanceServer) error {
	var argv sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(comfyui_version,''), COALESCE(frontend_version,''), COALESCE(python_version,''), COALESCE(torch_version,''), argv_json
		   FROM server WHERE id = ?`, srv.ID).
		Scan(&srv.ComfyUIVersion, &srv.FrontendVersion, &srv.PythonVersion, &srv.TorchVersion, &argv)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading server identity: %w", err)
	}
	if argv.Valid && json.Valid([]byte(argv.String)) {
		srv.Argv = json.RawMessage(argv.String)
	}

	rows, err := db.QueryContext(ctx,
		`SELECT uuid, COALESCE(name,''), comfy_index, nvidia_index, vram_total
		   FROM device WHERE server_id = ? ORDER BY COALESCE(comfy_index, 0)`, srv.ID)
	if err != nil {
		return fmt.Errorf("reading devices: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			dev                              provenanceDevice
			comfyIdx, nvidiaIdx, vramTotalIn sql.NullInt64
		)
		if err := rows.Scan(&dev.UUID, &dev.Name, &comfyIdx, &nvidiaIdx, &vramTotalIn); err != nil {
			return fmt.Errorf("reading device row: %w", err)
		}
		dev.ComfyIndex = nullInt64Ptr(comfyIdx)
		dev.NvidiaIndex = nullInt64Ptr(nvidiaIdx)
		dev.VRAMTotal = nullInt64Ptr(vramTotalIn)
		srv.Devices = append(srv.Devices, dev)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(srv.Devices) > 0 {
		srv.DeviceIdentityNote = "devices are identified by UUID; comfy_index and nvidia_index are recorded but are NOT identities — torch's cuda:N order is the inverse of nvidia-smi's on this box."
	}
	return nil
}

func provenanceLoadArm(ctx context.Context, db *sql.DB, exp *provenanceExperiment) error {
	var (
		vars     sql.NullString
		expID    sql.NullInt64
		expName  sql.NullString
		serverID string
	)
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(a.label,''), a.vars_json, COALESCE(a.server_id,''), e.id, e.name
		   FROM exp_arm a LEFT JOIN experiment e ON e.id = a.experiment_id
		  WHERE a.id = ?`, exp.ArmID).
		Scan(&exp.ArmLabel, &vars, &serverID, &expID, &expName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading experiment arm: %w", err)
	}
	exp.ArmServerID = serverID
	exp.ExperimentID = expID.Int64
	exp.ExperimentName = expName.String
	if vars.Valid {
		_ = json.Unmarshal([]byte(vars.String), &exp.Vars)
	}
	return nil
}

// provenanceStagedInputs recovers the staged assets a graph consumed.
//
// There is no foreign key to follow: LoadImage stores a bare filename as a
// widget value, so the link is made the only way it exists — by matching every
// string input value in the archived graph against input_asset.comfy_filename.
func provenanceStagedInputs(ctx context.Context, db *sql.DB, apiJSON []byte) ([]provenanceInput, error) {
	var graph store.APIGraph
	if err := json.Unmarshal(apiJSON, &graph); err != nil {
		// A graph body that no longer parses is not a reason to fail the
		// whole provenance read.
		return nil, nil
	}

	type ref struct{ nodeID, classType, inputName string }
	refs := map[string]ref{}
	var values []string
	for nodeID, node := range graph {
		for inputName, raw := range node.Inputs {
			s, ok := raw.(string)
			if !ok || strings.TrimSpace(s) == "" {
				continue
			}
			if _, seen := refs[s]; seen {
				continue
			}
			refs[s] = ref{nodeID: nodeID, classType: node.ClassType, inputName: inputName}
			values = append(values, s)
		}
	}
	if len(values) == 0 {
		return nil, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(values)), ",")
	args := make([]any, 0, len(values))
	for _, v := range values {
		args = append(args, v)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT content_sha256, comfy_filename, host_path, bytes, staged_at
		   FROM input_asset WHERE comfy_filename IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("matching staged inputs: %w", err)
	}
	defer rows.Close()

	var out []provenanceInput
	for rows.Next() {
		var (
			in        provenanceInput
			hostPath  sql.NullString
			byteCount sql.NullInt64
			stagedAt  any
		)
		if err := rows.Scan(&in.ContentSHA256, &in.GraphValue, &hostPath, &byteCount, &stagedAt); err != nil {
			return nil, fmt.Errorf("reading staged input: %w", err)
		}
		in.HostPath = hostPath.String
		in.Bytes = byteCount.Int64
		in.StagedAt = comfyTimeString(stagedAt)
		if r, ok := refs[in.GraphValue]; ok {
			in.NodeID, in.ClassType, in.InputName = r.nodeID, r.classType, r.inputName
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeID != out[j].NodeID {
			return out[i].NodeID < out[j].NodeID
		}
		return out[i].GraphValue < out[j].GraphValue
	})
	return out, nil
}

func provenanceRender(cmd *cobra.Command, report provenanceReport) error {
	w := cmd.OutOrStdout()
	for i, rec := range report.Records {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s\n", bold(provenanceDisplayName(rec.Output)))
		fmt.Fprintf(w, "  prompt_id:    %s\n", rec.PromptID)
		state := rec.State
		if rec.ExitClass != "" {
			state += " (exit_class=" + rec.ExitClass + ")"
		}
		if rec.Completeness != "" && rec.Completeness != "full" {
			state += " completeness=" + rec.Completeness
		}
		fmt.Fprintf(w, "  run:          %s\n", strings.TrimSpace(state))
		if rec.RunName != "" {
			fmt.Fprintf(w, "  run name:     %s\n", rec.RunName)
		}
		fmt.Fprintf(w, "  graph_sha:    %s\n", rec.Graph.SHA256)
		if rec.Graph.ShapeSHA256 != "" {
			fmt.Fprintf(w, "  shape_sha:    %s  (seed-stripped; groups comparable runs)\n", rec.Graph.ShapeSHA256)
		}
		if rec.Graph.TemplateID != "" {
			fmt.Fprintf(w, "  template:     %s\n", rec.Graph.TemplateID)
		}
		if rec.Timing.Duration != "" {
			fmt.Fprintf(w, "  duration:     %s  [%s]\n", rec.Timing.Duration, comfyTimingSource)
		} else {
			fmt.Fprintf(w, "  duration:     not recorded  [%s]\n", comfyTimingSource)
		}
		if st := rec.Timing.ShapeStats; st != nil {
			fmt.Fprintf(w, "  shape stats:  n=%d mean=%s sd=%s min=%s max=%s\n",
				st.N, comfyHumanFloatMillis(st.MeanMS), comfyHumanFloatMillis(st.StdDevMS),
				comfyHumanFloatMillis(float64(st.MinMS)), comfyHumanFloatMillis(float64(st.MaxMS)))
		}
		if rec.SubmittedAt != "" {
			fmt.Fprintf(w, "  submitted:    %s\n", rec.SubmittedAt)
		}
		if srv := rec.Server; srv != nil {
			fmt.Fprintf(w, "  server:       %s%s\n", srv.ID, provenanceServerSuffix(srv))
			for _, dev := range srv.Devices {
				fmt.Fprintf(w, "                device %s %s%s\n", dev.UUID, dev.Name, provenanceDeviceIndices(dev))
			}
			if srv.DeviceIdentityNote != "" {
				fmt.Fprintf(w, "                (%s)\n", srv.DeviceIdentityNote)
			}
			if len(srv.Argv) > 0 {
				fmt.Fprintf(w, "  server argv:  %s\n", truncate(string(srv.Argv), 160))
			}
		}
		if exp := rec.Experiment; exp != nil {
			label := exp.ArmLabel
			if label == "" {
				label = fmt.Sprintf("arm %d", exp.ArmID)
			}
			fmt.Fprintf(w, "  experiment:   %s / %s\n", provenanceOrDash(exp.ExperimentName), label)
			if len(exp.Vars) > 0 {
				fmt.Fprintf(w, "  arm vars:     %s\n", provenanceVarsLine(exp.Vars))
			}
		}
		fmt.Fprintf(w, "  file:         %s\n", provenanceFileLine(rec.Output))
		if rec.Output.AbsPath != "" {
			fmt.Fprintf(w, "  path:         %s\n", rec.Output.AbsPath)
		}
		if len(rec.StagedInputs) == 0 {
			fmt.Fprintf(w, "  inputs:       none recorded\n")
		}
		for _, in := range rec.StagedInputs {
			fmt.Fprintf(w, "  input:        %s (%s.%s) sha256=%s\n", in.GraphValue, in.NodeID, in.InputName, media.ShortSHA(in.ContentSHA256))
			if in.HostPath != "" {
				fmt.Fprintf(w, "                from %s%s\n", in.HostPath, provenanceStagedSuffix(in.StagedAt))
			}
		}
		if len(rec.NodeErrors) > 0 {
			// Verbatim, never summarised: the per-node text is the only thing
			// that identifies which output branch ComfyUI rejected.
			fmt.Fprintf(w, "  node_errors (verbatim):\n%s\n", provenanceIndent(string(rec.NodeErrors), "    "))
		}
		if f := rec.Failure; f != nil {
			fmt.Fprintf(w, "  failure:      %s in node %s (%s)\n", provenanceOrDash(f.ExceptionType), provenanceOrDash(f.NodeID), provenanceOrDash(f.NodeType))
			if f.ExceptionMessage != "" {
				fmt.Fprintf(w, "                %s\n", f.ExceptionMessage)
			}
		}
		for _, note := range rec.Notes {
			fmt.Fprintf(w, "  note:         %s\n", note)
		}
	}
	for _, note := range report.Notes {
		fmt.Fprintf(w, "\n%s\n", note)
	}
	return nil
}

// comfyJoinSubfolder renders the value a caller sees for a produced file:
// "<subfolder>/<filename>" when a subfolder was recorded, the bare filename
// otherwise. Always forward-slashed, matching what /history reports.
func comfyJoinSubfolder(subfolder, filename string) string {
	if strings.TrimSpace(subfolder) != "" {
		return strings.Trim(filepath.ToSlash(subfolder), "/") + "/" + filename
	}
	return filename
}

func provenanceDisplayName(o provenanceOutputFile) string {
	return comfyJoinSubfolder(o.Subfolder, o.Filename)
}

func provenanceServerSuffix(srv *provenanceServer) string {
	var parts []string
	if srv.ComfyUIVersion != "" {
		parts = append(parts, "ComfyUI "+srv.ComfyUIVersion)
	}
	if srv.TorchVersion != "" {
		parts = append(parts, "torch "+srv.TorchVersion)
	}
	if srv.PythonVersion != "" {
		parts = append(parts, "python "+srv.PythonVersion)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func provenanceDeviceIndices(dev provenanceDevice) string {
	var parts []string
	if dev.ComfyIndex != nil {
		parts = append(parts, fmt.Sprintf("comfy_index=%d", *dev.ComfyIndex))
	}
	if dev.NvidiaIndex != nil {
		parts = append(parts, fmt.Sprintf("nvidia_index=%d", *dev.NvidiaIndex))
	}
	if len(parts) == 0 {
		return ""
	}
	return " [" + strings.Join(parts, " ") + "]"
}

func provenanceFileLine(o provenanceOutputFile) string {
	var parts []string
	if o.Type != "" {
		parts = append(parts, o.Type)
	}
	if o.Bytes != nil {
		parts = append(parts, stageHumanBytes(*o.Bytes))
	}
	if o.Width != nil && o.Height != nil {
		parts = append(parts, fmt.Sprintf("%dx%d", *o.Width, *o.Height))
	}
	if o.FPS != nil {
		parts = append(parts, fmt.Sprintf("%.3g fps", *o.FPS))
	}
	if o.DurationS != nil {
		parts = append(parts, fmt.Sprintf("%.3gs", *o.DurationS))
	}
	if o.HasAudio != nil {
		parts = append(parts, fmt.Sprintf("audio=%t", *o.HasAudio))
	}
	if o.ProbedAt == "" {
		parts = append(parts, "not probed (run `outputs --probe`)")
	}
	if len(parts) == 0 {
		return o.Filename
	}
	return strings.Join(parts, "  ")
}

func provenanceStagedSuffix(stagedAt string) string {
	if stagedAt == "" {
		return ""
	}
	return " (staged " + stagedAt + ")"
}

func provenanceVarsLine(vars map[string]any) string {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, vars[k]))
	}
	return strings.Join(parts, " ")
}

func provenanceOrDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func provenanceIndent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
// outputs
// ---------------------------------------------------------------------------

type outputListRow struct {
	ID          int64    `json:"id"`
	RunID       string   `json:"run_id"`
	RunState    string   `json:"run_state,omitempty"`
	NodeID      string   `json:"node_id,omitempty"`
	Filename    string   `json:"filename"`
	Subfolder   string   `json:"subfolder,omitempty"`
	Type        string   `json:"type,omitempty"`
	AbsPath     string   `json:"abs_path,omitempty"`
	Bytes       *int64   `json:"bytes,omitempty"`
	Width       *int64   `json:"width,omitempty"`
	Height      *int64   `json:"height,omitempty"`
	FPS         *float64 `json:"fps,omitempty"`
	DurationS   *float64 `json:"duration_s,omitempty"`
	HasAudio    *bool    `json:"has_audio,omitempty"`
	ProbedAt    string   `json:"probed_at,omitempty"`
	StillImage  *bool    `json:"still_image,omitempty"`
	ProbeStatus string   `json:"probe_status,omitempty"`
	ProbeError  string   `json:"probe_error,omitempty"`
}

type outputsProbeSummary struct {
	Requested bool   `json:"requested"`
	Available bool   `json:"available"`
	Binary    string `json:"binary,omitempty"`
	Probed    int    `json:"probed"`
	Skipped   int    `json:"skipped"`
	Failed    int    `json:"failed"`
	Note      string `json:"note,omitempty"`
}

type outputsReport struct {
	Count   int                  `json:"count"`
	Outputs []outputListRow      `json:"outputs"`
	Probe   *outputsProbeSummary `json:"probe,omitempty"`
}

// pp:data-source local
//
// outputs lists rows recorded by the collection path. --probe additionally
// writes probed media properties back into the LOCAL store; nothing leaves this
// machine, which is why the annotation is mcp:local-write rather than
// mcp:read-only.
func newOutputsCmd(flags *rootFlags) *cobra.Command {
	var (
		last      int
		probe     bool
		runID     string
		outputDir string
		ffprobe   string
	)

	cmd := &cobra.Command{
		Use:   "outputs",
		Short: "List recorded outputs; --probe fills width/height/fps/duration/audio via ffprobe",
		Long: `List the produced files recorded against runs in the local store.

With --probe, each resolvable file is measured with ffprobe and the result is
written back onto the output row. Those columns exist because fps,
duration_s, and has_audio are properties of the produced FILE and appear in NO
ComfyUI endpoint — they are exactly what a cross-model comparison needs, and
they are absent from mp4/webm metadata entirely.

ffprobe is optional. When it is not on PATH the listing still prints in full and
exits 0, with a single clear note explaining what was skipped.

Still images are recorded with no fps and no duration on purpose: ffprobe
fabricates a frame rate (25/1) for a PNG, and storing that would poison the very
comparison these columns serve.`,
		Example: `  comfyui-pp-cli outputs
  comfyui-pp-cli outputs --last 50
  comfyui-pp-cli outputs --probe --output-dir C:\ComfyUI\output
  comfyui-pp-cli outputs --run 550e8400-e29b-41d4-a716-446655440000 --json`,
		Annotations: map[string]string{
			"mcp:local-write":     "true",
			"pp:typed-exit-codes": "0,2",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "outputs")
			}
			if len(args) > 0 {
				return usageErr(fmt.Errorf("outputs takes no positional arguments (got %q); filter with --run or --last", args[0]))
			}
			if err := validateDataSourceStrategy(flags, "local"); err != nil {
				return usageErr(err)
			}
			if last <= 0 {
				return usageErr(fmt.Errorf("--last must be positive (got %d)", last))
			}
			return runOutputs(cmd, flags, last, probe, runID, outputDir, ffprobe)
		},
	}

	cmd.Flags().IntVar(&last, "last", 20, "Show the most recent N outputs")
	cmd.Flags().BoolVar(&probe, "probe", false, "Measure each resolvable file with ffprobe and record width/height/fps/duration_s/has_audio")
	cmd.Flags().StringVar(&runID, "run", "", "Only outputs produced by this prompt_id")
	cmd.Flags().StringVar(&outputDir, "output-dir", "", "ComfyUI's output directory, used to locate files whose absolute path was never recorded (env: COMFYUI_OUTPUT_DIR)")
	cmd.Flags().StringVar(&ffprobe, "ffprobe", "", "Path to the ffprobe binary; defaults to the one on PATH")

	return cmd
}

func runOutputs(cmd *cobra.Command, flags *rootFlags, last int, probe bool, runID, outputDir, ffprobePath string) error {
	ctx, cancel := boundCtx(cmd.Context(), flags)
	defer cancel()

	s, err := openStagingStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	db := s.DB()

	query := `SELECT
	    o.id, o.run_id, COALESCE(o.node_id,''), o.filename, COALESCE(o.subfolder,''),
	    COALESCE(o.type,''), COALESCE(o.abs_path,''), o.bytes,
	    o.width, o.height, o.fps, o.duration_s, o.has_audio, o.probed_at,
	    COALESCE(r.state,'')
	  FROM output o
	  LEFT JOIN run r ON r.prompt_id = o.run_id`
	args := []any{}
	if strings.TrimSpace(runID) != "" {
		query += ` WHERE o.run_id = ?`
		args = append(args, strings.TrimSpace(runID))
	}
	query += ` ORDER BY o.id DESC LIMIT ?`
	args = append(args, last)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("listing outputs: %w", err)
	}
	defer rows.Close()

	report := outputsReport{Outputs: []outputListRow{}}
	for rows.Next() {
		var (
			item                               outputListRow
			bytesN, widthN, heightN, hasAudioN sql.NullInt64
			fpsN, durationSN                   sql.NullFloat64
			probedAt                           any
		)
		if err := rows.Scan(
			&item.ID, &item.RunID, &item.NodeID, &item.Filename, &item.Subfolder,
			&item.Type, &item.AbsPath, &bytesN,
			&widthN, &heightN, &fpsN, &durationSN, &hasAudioN, &probedAt,
			&item.RunState,
		); err != nil {
			return fmt.Errorf("reading output row: %w", err)
		}
		item.Bytes = nullInt64Ptr(bytesN)
		item.Width = nullInt64Ptr(widthN)
		item.Height = nullInt64Ptr(heightN)
		item.FPS = nullFloatPtr(fpsN)
		item.DurationS = nullFloatPtr(durationSN)
		item.HasAudio = nullBoolPtr(hasAudioN)
		item.ProbedAt = comfyTimeString(probedAt)
		report.Outputs = append(report.Outputs, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	report.Count = len(report.Outputs)

	if probe {
		report.Probe = outputsRunProbe(ctx, db, report.Outputs, outputDir, ffprobePath)
		// The human renderer prints the note inline below the table; every other
		// mode skips that path entirely and used to report probe_status
		// "file-not-found" on every row with nothing on stderr at all.
		if report.Probe != nil && report.Probe.Note != "" && !wantsHumanTable(cmd.OutOrStdout(), flags) {
			fmt.Fprintf(cmd.ErrOrStderr(), "probe: %s\n", report.Probe.Note)
		}
	}

	if !wantsHumanTable(cmd.OutOrStdout(), flags) {
		return flags.printJSON(cmd, report)
	}
	return outputsRender(cmd, flags, report)
}

// outputsRunProbe measures each resolvable file and writes the result back onto
// its output row. A missing ffprobe is a documented skip, not an error: the
// listing is still useful without it.
func outputsRunProbe(ctx context.Context, db *sql.DB, items []outputListRow, outputDir, ffprobePath string) *outputsProbeSummary {
	summary := &outputsProbeSummary{Requested: true}

	if strings.TrimSpace(ffprobePath) == "" {
		availability := media.FFprobeAvailability(nil)
		if !availability.Available {
			summary.Note = availability.Note
			summary.Skipped = len(items)
			for i := range items {
				items[i].ProbeStatus = "skipped-no-ffprobe"
			}
			return summary
		}
		summary.Binary = availability.Path
	} else {
		summary.Binary = ffprobePath
	}
	summary.Available = true

	if strings.TrimSpace(outputDir) == "" {
		outputDir = strings.TrimSpace(os.Getenv("COMFYUI_OUTPUT_DIR"))
	}

	for i := range items {
		item := &items[i]
		resolved, ok := outputsResolvePath(*item, outputDir)
		if !ok {
			item.ProbeStatus = "file-not-found"
			summary.Skipped++
			continue
		}
		info, err := media.Probe(ctx, nil, summary.Binary, resolved)
		if err != nil {
			item.ProbeStatus = "probe-failed"
			item.ProbeError = err.Error()
			summary.Failed++
			continue
		}
		var size int64
		if st, statErr := os.Stat(resolved); statErr == nil {
			size = st.Size()
		}
		if err := outputsRecordProbe(ctx, db, item.ID, info, resolved, size); err != nil {
			item.ProbeStatus = "probe-not-recorded"
			item.ProbeError = err.Error()
			summary.Failed++
			continue
		}
		outputsApplyProbe(item, info, resolved, size)
		item.ProbeStatus = "probed"
		summary.Probed++
	}

	// A probe that resolved nothing is the failure this command is most likely to
	// hit: rows recorded through /history carry no absolute path, so without a
	// search root every row comes back "file-not-found" with ffprobe sitting right
	// there on PATH. Say so — and say where we looked — instead of returning a
	// clean-looking listing with an empty probed column. The note rides in the
	// JSON payload AND is printed to stderr by the caller, so --json/--agent
	// callers are not left guessing the way they were before.
	if summary.Skipped > 0 {
		switch {
		case outputDir == "":
			summary.Note = fmt.Sprintf(
				"%d of %d rows have no recorded absolute path and no output directory was given, so nothing could be located to probe; pass --output-dir <ComfyUI output dir> or set COMFYUI_OUTPUT_DIR",
				summary.Skipped, len(items))
		default:
			summary.Note = fmt.Sprintf(
				"%d of %d rows could not be located under %s (tried the recorded absolute path, then <output-dir>/<subfolder>/<filename>); check --output-dir points at ComfyUI's output directory",
				summary.Skipped, len(items), outputDir)
		}
	}
	return summary
}

// outputsResolvePath finds the produced file on disk: the recorded absolute
// path first, then <output-dir>/<subfolder>/<filename>.
func outputsResolvePath(item outputListRow, outputDir string) (string, bool) {
	if item.AbsPath != "" {
		if st, err := os.Stat(item.AbsPath); err == nil && !st.IsDir() {
			return item.AbsPath, true
		}
	}
	if outputDir == "" {
		return "", false
	}
	candidate := filepath.Join(outputDir, filepath.FromSlash(item.Subfolder), item.Filename)
	if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
		return candidate, true
	}
	return "", false
}

func outputsRecordProbe(ctx context.Context, db *sql.DB, id int64, info media.Info, absPath string, size int64) error {
	var width, height, fps, durationS any
	if info.Width > 0 {
		width = info.Width
	}
	if info.Height > 0 {
		height = info.Height
	}
	if info.FPS > 0 {
		fps = info.FPS
	}
	if info.DurationS > 0 {
		durationS = info.DurationS
	}
	hasAudio := 0
	if info.HasAudio {
		hasAudio = 1
	}
	var byteCount any
	if size > 0 {
		byteCount = size
	}
	_, err := db.ExecContext(ctx,
		`UPDATE output
		    SET width = ?, height = ?, fps = ?, duration_s = ?, has_audio = ?,
		        probed_at = CURRENT_TIMESTAMP,
		        abs_path = COALESCE(NULLIF(?, ''), abs_path),
		        bytes = COALESCE(?, bytes)
		  WHERE id = ?`,
		width, height, fps, durationS, hasAudio, absPath, byteCount, id)
	if err != nil {
		return fmt.Errorf("recording probe for output %d: %w", id, err)
	}
	return nil
}

func outputsApplyProbe(item *outputListRow, info media.Info, absPath string, size int64) {
	item.AbsPath = absPath
	if size > 0 {
		item.Bytes = &size
	}
	if info.Width > 0 {
		w := int64(info.Width)
		item.Width = &w
	}
	if info.Height > 0 {
		h := int64(info.Height)
		item.Height = &h
	}
	if info.FPS > 0 {
		fps := info.FPS
		item.FPS = &fps
	} else {
		item.FPS = nil
	}
	if info.DurationS > 0 {
		d := info.DurationS
		item.DurationS = &d
	} else {
		item.DurationS = nil
	}
	audio := info.HasAudio
	item.HasAudio = &audio
	still := info.StillImage
	item.StillImage = &still
	item.ProbedAt = time.Now().UTC().Format(time.RFC3339)
}

func outputsRender(cmd *cobra.Command, flags *rootFlags, report outputsReport) error {
	w := cmd.OutOrStdout()
	if report.Count == 0 {
		fmt.Fprintln(w, "no outputs recorded yet")
	} else {
		headers := []string{"ID", "RUN", "FILENAME", "TYPE", "SIZE", "DIMS", "FPS", "DUR", "AUDIO", "PROBED"}
		rows := make([][]string, 0, len(report.Outputs))
		for _, item := range report.Outputs {
			rows = append(rows, []string{
				fmt.Sprintf("%d", item.ID),
				media.ShortSHA(item.RunID),
				truncate(comfyJoinSubfolder(item.Subfolder, item.Filename), 44),
				provenanceOrDash(item.Type),
				outputsBytesCell(item.Bytes),
				outputsDimsCell(item),
				outputsFloatCell(item.FPS),
				outputsDurationCell(item.DurationS),
				outputsBoolCell(item.HasAudio),
				outputsProbedCell(item),
			})
		}
		if err := flags.printTable(cmd, headers, rows); err != nil {
			return err
		}
	}
	if p := report.Probe; p != nil {
		fmt.Fprintln(w)
		if !p.Available {
			fmt.Fprintf(w, "  %s %s\n", yellow("SKIP"), p.Note)
		} else {
			fmt.Fprintf(w, "  probe: %d measured, %d unresolved, %d failed (%s)\n", p.Probed, p.Skipped, p.Failed, p.Binary)
			if p.Note != "" {
				fmt.Fprintf(w, "  %s %s\n", yellow("hint:"), p.Note)
			}
		}
	}
	return nil
}

func outputsBytesCell(v *int64) string {
	if v == nil {
		return "-"
	}
	return stageHumanBytes(*v)
}

func outputsDimsCell(item outputListRow) string {
	if item.Width == nil || item.Height == nil {
		return "-"
	}
	return fmt.Sprintf("%dx%d", *item.Width, *item.Height)
}

func outputsFloatCell(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.3g", *v)
}

func outputsDurationCell(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.2fs", *v)
}

func outputsBoolCell(v *bool) string {
	if v == nil {
		return "-"
	}
	if *v {
		return "yes"
	}
	return "no"
}

func outputsProbedCell(item outputListRow) string {
	if item.ProbeStatus != "" && item.ProbeStatus != "probed" {
		return item.ProbeStatus
	}
	if item.ProbedAt == "" {
		return "-"
	}
	return item.ProbedAt
}

// ---------------------------------------------------------------------------
// small shared helpers
// ---------------------------------------------------------------------------

func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

func nullFloatPtr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

func nullBoolPtr(v sql.NullInt64) *bool {
	if !v.Valid {
		return nil
	}
	b := v.Int64 != 0
	return &b
}

// comfyHumanMillis renders a duration sourced from the authoritative timestamp
// pair. A NULL duration prints as empty rather than "0s", because "not
// recorded" and "instant" are different facts.
func comfyHumanMillis(v sql.NullInt64) string {
	if !v.Valid {
		return ""
	}
	return comfyHumanFloatMillis(float64(v.Int64))
}

func comfyHumanFloatMillis(ms float64) string {
	if ms <= 0 {
		return "0s"
	}
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", int64(ms))
	case d < time.Minute:
		return fmt.Sprintf("%.2fs", d.Seconds())
	default:
		minutes := int64(d / time.Minute)
		seconds := d.Seconds() - float64(minutes*60)
		return fmt.Sprintf("%dm %.1fs", minutes, seconds)
	}
}
