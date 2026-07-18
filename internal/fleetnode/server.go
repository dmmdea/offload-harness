// This file is the HTTP face of the fleet node — the three CONTRACT.md v2
// endpoints (health / dispatch / jobs) over the stores built by the sibling
// files. Handlers hold no GPU lock and spawn nothing: health reads the
// sampler's atomic snapshot + the jobs store's counters, dispatch acks in
// milliseconds and hands the render to Jobs.Accept, and every failure is a
// non-2xx JSON envelope from the spec's error taxonomy. Wrong Content-Type is
// answered 400 (not 415): the taxonomy has exactly one caller-mistake class,
// and one shape beats two.

package fleetnode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// maxDispatchBody is the dispatch request-body cap (1 MiB per the spec — a
// run-graph payload is the biggest legitimate body and fits comfortably).
const maxDispatchBody = 1 << 20

// maxSnapshotAge bounds how stale a VRAM snapshot health may serve. The
// sampler refreshes every 2s and keeps the LAST GOOD snapshot on failure — so
// if nvidia-smi starts failing outright (driver reset), health would otherwise
// serve hours-stale 200s and mislead the dispatcher's routing. Past this bound
// a stale snapshot is the same class of failure as no snapshot: 503.
const maxSnapshotAge = 30 * time.Second

// Runner runs one translated request to completion. The real pipeline
// satisfies it; tests inject fakes.
type Runner interface {
	Run(ctx context.Context, req core.Request) core.Result
}

// Options is the server's static identity + read-only data feeds. Snapshot and
// Footprints are functions (not values) so health always reads live state
// without the handler owning any sampling machinery.
type Options struct {
	NodeID     string
	Snapshot   func() (Snapshot, bool)
	Footprints func() []FootprintEntry
	GpuVendor  string
	GpuArch    string
	Cfg        config.Config
}

// Server is the fleet-node HTTP server: three handlers over a Runner + Jobs
// store. Task/family advertisements are derived once at construction (config
// is immutable while serving).
type Server struct {
	runner   Runner
	jobs     *Jobs
	opts     Options
	tasks    []string
	families []string
}

// New builds a Server. The supported-task/family lists are computed here, not
// per health request — the config cannot change under a running server.
func New(runner Runner, jobs *Jobs, opts Options) *Server {
	return &Server{
		runner:   runner,
		jobs:     jobs,
		opts:     opts,
		tasks:    SupportedTasks(opts.Cfg),
		families: Families(opts.Cfg),
	}
}

// Handler returns the routed mux for the three contract endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", s.handleHealth)
	mux.HandleFunc("POST /fleet/dispatch", s.handleDispatch)
	mux.HandleFunc("GET /fleet/jobs/{id}", s.handleJob)
	return mux
}

// httpServer builds the *http.Server with the spec's timeout table
// (ReadHeader 5s / Read 30s / Write 30s / Idle 120s). Split from Serve so the
// timeouts are unit-assertable.
func (s *Server) httpServer() *http.Server {
	return &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// Serve serves on l until it fails/closes, with the contract timeout table
// applied. The listener is the verb's business (netguard.Validate happens
// there, before the socket ever exists).
func (s *Server) Serve(l net.Listener) error {
	return s.httpServer().Serve(l)
}

// healthPayload is CONTRACT.md v2's health shape, field for field.
type healthPayload struct {
	NodeID                string           `json:"node_id"`
	SchemaVersion         int              `json:"schema_version"`
	GpuVendor             string           `json:"gpu_vendor"`
	GpuArch               string           `json:"gpu_arch"`
	VramTotalGb           float64          `json:"vram_total_gb"`
	VramFreeGb            float64          `json:"vram_free_gb"`
	SupportedTaskTypes    []string         `json:"supported_task_types"`
	LoadableModelFamilies []string         `json:"loadable_model_families"`
	ModelFootprints       []FootprintEntry `json:"model_footprints"`
	QueueDepth            int              `json:"queue_depth"`
}

// handleHealth assembles the contract health JSON from cached/cheap reads
// only. A missing snapshot (sampler never succeeded) is a FAILED probe → 503:
// emitting zeros would advertise a broken node as an empty GPU.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.opts.Snapshot == nil {
		writeError(w, http.StatusServiceUnavailable, "vram snapshot unavailable")
		return
	}
	snap, ok := s.opts.Snapshot()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "vram snapshot unavailable")
		return
	}
	if time.Since(snap.At) > maxSnapshotAge {
		writeError(w, http.StatusServiceUnavailable, "vram snapshot stale")
		return
	}
	fps := []FootprintEntry{}
	if s.opts.Footprints != nil {
		if e := s.opts.Footprints(); e != nil {
			fps = e
		}
	}
	tasks := s.tasks
	if tasks == nil {
		tasks = []string{}
	}
	families := s.families
	if families == nil {
		families = []string{}
	}
	writeJSON(w, http.StatusOK, healthPayload{
		NodeID:                s.opts.NodeID,
		SchemaVersion:         1,
		GpuVendor:             s.opts.GpuVendor,
		GpuArch:               s.opts.GpuArch,
		VramTotalGb:           snap.TotalGiB,
		VramFreeGb:            snap.FreeGiB,
		SupportedTaskTypes:    tasks,
		LoadableModelFamilies: families,
		ModelFootprints:       fps,
		QueueDepth:            s.jobs.QueueDepth(),
	})
}

// dispatchEnvelope is the strict dispatch wire shape. DisallowUnknownFields
// applies to THIS struct only — payload passes through raw for the per-task
// translators. model_family/quant and the sizing fields are contract-reserved
// scheduler inputs: accepted and ignored here (footprint keys come from the
// machine's own bindings, and admission math is the dispatcher's job).
type dispatchEnvelope struct {
	JobID       string          `json:"job_id"`
	TaskType    string          `json:"task_type"`
	ModelFamily string          `json:"model_family"`
	Quant       string          `json:"quant"`
	Priority    json.RawMessage `json:"priority"`
	Width       json.RawMessage `json:"width"`
	Height      json.RawMessage `json:"height"`
	NumFrames   json.RawMessage `json:"num_frames"`
	ParamsB     json.RawMessage `json:"params_b"`
	ContextLen  json.RawMessage `json:"context_len"`
	NumLayers   json.RawMessage `json:"num_layers"`
	HiddenDim   json.RawMessage `json:"hidden_dim"`
	BatchSize   json.RawMessage `json:"batch_size"`
	Payload     json.RawMessage `json:"payload"`
}

// handleDispatch is the ack path: validate everything a caller can get wrong
// (the 400s), refuse during drain (503), then Accept — which either starts the
// run (202 exact echo) or reveals a duplicate (202 re-ack for anything not
// failed, 409 for a previously failed job; never a second render).
func (s *Server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxDispatchBody)

	// Soft Content-Type check: a set-but-wrong type is a caller mistake; an
	// absent header is tolerated (curl-friendly, and the body decode is the
	// real gate).
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("content-type must be application/json (got %q)", ct))
			return
		}
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var env dispatchEnvelope
	if err := dec.Decode(&env); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusBadRequest, "request body too large (limit 1 MiB)")
			return
		}
		writeError(w, http.StatusBadRequest, "malformed dispatch body: "+err.Error())
		return
	}
	if env.JobID == "" {
		writeError(w, http.StatusBadRequest, "job_id required")
		return
	}
	if s.jobs.Draining() {
		writeError(w, http.StatusServiceUnavailable, "node draining")
		return
	}

	req, cleanup, err := BuildRequest(s.opts.Cfg, env.TaskType, env.Payload)
	if err != nil {
		cleanup()
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	run := func(ctx context.Context) (json.RawMessage, error) {
		defer cleanup() // temp files live exactly as long as the job
		res := s.runner.Run(ctx, req)
		if res.OK {
			return res.Data, nil
		}
		reason := res.Reason
		if reason == "" {
			reason = "deferred" // Jobs.finish treats "" as success; never let that lie
		}
		return nil, errors.New(reason)
	}

	if !s.jobs.Accept(env.JobID, run) {
		cleanup() // duplicate/drain refusal: this request's materialized files never run
		view, ok := s.jobs.Get(env.JobID)
		if !ok {
			// Accept refused but the id is absent: drain began between the
			// Draining() check and Accept.
			writeError(w, http.StatusServiceUnavailable, "node draining")
			return
		}
		// Contract refusal semantics: the dispatcher treats ANY non-202
		// dispatch response as a REFUSAL and may re-dispatch the same job_id
		// to another node. So a duplicate for a job in accepted/running — or
		// DONE (the lost-ack + fast-render scenario) — must re-ack 202, or a
		// lost ack would buy a duplicate render fleet-wide; after the re-ack
		// the dispatcher's tracker polls /fleet/jobs/{id} and immediately
		// finds the terminal state with data. Only a previously FAILED job
		// answers non-202: 409 is an explicit refusal, so the dispatcher may
		// legitimately try the failed job on another node.
		if view.State == JobError {
			writeError(w, http.StatusConflict, "job previously failed on this node: "+view.Error)
			return
		}
		writeAck(w, env.JobID) // accepted/running/done: idempotent re-ack, still just one render
		return
	}
	writeAck(w, env.JobID)
}

// handleJob is the poll path: the job's current wire state, or 404 for an id
// we never acked (or already evicted).
func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	view, ok := s.jobs.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown job")
		return
	}
	writeJobView(w, http.StatusOK, view)
}

// jobWire is the jobs-endpoint shape: `state` (not `status`), `data` only on
// done, `error` only on error.
type jobWire struct {
	JobID string          `json:"job_id"`
	State JobState        `json:"state"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

func writeJobView(w http.ResponseWriter, status int, v *JobView) {
	writeJSON(w, status, jobWire{JobID: v.ID, State: v.State, Data: v.Data, Error: v.Error})
}

// writeAck emits the ONLY acceptance shape the contract allows: 202 + exact
// job_id echo + "accepted".
func writeAck(w http.ResponseWriter, jobID string) {
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": jobID, "status": "accepted"})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"status": "error", "error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
