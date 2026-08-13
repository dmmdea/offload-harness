// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

// Package fakeswap is an in-process fake llama-swap proxy for tests.
//
// It exists because the behavior that matters most in this CLI — what happens
// across a llama-swap RESTART — cannot be exercised against the live server.
// The live proxy on 127.0.0.1:11436 carries the mem0 memory stack; restarting
// it (or unloading its keep-set) is forbidden. So every epoch, loss-accounting,
// drain, and keep-set-refusal path is acceptance-tested here instead.
//
// Faithfulness matters more than convenience: response shapes below were copied
// from live v249 probes (GET /running, /v1/models, /api/metrics/activity,
// /api/metrics/stats, /api/events, /upstream/{model}/slots and /props) taken on
// 2026-08-13. Notably reproduced quirks:
//
//   - /running reports ttl:0 for a model configured ttl:-1. The server's ttl
//     field is a LIE for keep-set purposes; the CLI must never derive the
//     keep-set from it. The fake reproduces the lie so a regression that starts
//     trusting ttl fails a test instead of an outage.
//   - Activity is an in-memory RING with 1-based, dense, monotonically
//     increasing ids that RESTART AT 1 when the proxy restarts. The ids alone
//     therefore cannot distinguish "new requests" from "a different server".
//   - /api/metrics/activity defaults to newest-first (sort=id, order=desc) and
//     is 1-based paginated; `total` counts rows still in the ring, not requests
//     ever served.
//   - /api/metrics/stats total_requests is CUMULATIVE and survives eviction,
//     which is what makes it usable as an independent restart witness.
package fakeswap

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SlotMode controls how GET /upstream/{model}/slots answers, so drain logic can
// be tested against every branch the real deployment produces.
type SlotMode string

const (
	// SlotIdle answers 200 with is_processing=false on every slot.
	SlotIdle SlotMode = "idle"
	// SlotProcessing answers 200 with is_processing=true (drain must wait).
	SlotProcessing SlotMode = "processing"
	// SlotTimeout never answers; the caller's context deadline fires.
	SlotTimeout SlotMode = "timeout"
	// SlotStatus404 answers 404: llama-server was started without --slots.
	// This is endpoint-absent, NOT unobservable — callers fall back.
	SlotStatus404 SlotMode = "status404"
	// SlotStatus500 answers 500: the upstream is broken. Unobservable.
	SlotStatus500 SlotMode = "status500"
)

// Tokens mirrors the live activity row's nested token block. The live server
// uses -1 (not 0, not null) for "not measured".
type Tokens struct {
	CacheTokens     int     `json:"cache_tokens"`
	DraftTokens     int     `json:"draft_tokens"`
	DraftAccTokens  int     `json:"draft_acc_tokens"`
	InputTokens     int     `json:"input_tokens"`
	OutputTokens    int     `json:"output_tokens"`
	PromptPerSecond float64 `json:"prompt_per_second"`
	TokensPerSecond float64 `json:"tokens_per_second"`
}

// Activity mirrors one live /api/metrics/activity row.
type Activity struct {
	ID              int64             `json:"id"`
	Timestamp       string            `json:"timestamp"`
	Model           string            `json:"model"`
	ReqPath         string            `json:"req_path"`
	RespContentType string            `json:"resp_content_type"`
	RespStatusCode  int               `json:"resp_status_code"`
	Tokens          Tokens            `json:"tokens"`
	DurationMS      int64             `json:"duration_ms"`
	HasCapture      bool              `json:"has_capture"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

// Model is a roster entry as reported by GET /v1/models.
type Model struct {
	ID      string
	Name    string
	Aliases []string
	// ConfigTTL is the ttl from the YAML (-1 = keep-set resident). It is
	// deliberately NOT what /running reports; see RunningTTL.
	ConfigTTL int
}

// Running is one entry of GET /running.
type Running struct {
	Model       string
	State       string
	Cmd         string
	Proxy       string
	Name        string
	Description string
}

// UnloadCall records a mutation the server received, so tests can assert that
// a refusal path called NOTHING.
type UnloadCall struct {
	Method string
	Path   string
	Model  string // "" for unload-all
	All    bool
	At     time.Time
}

// Server is the fake proxy. All exported methods are safe for concurrent use.
type Server struct {
	mu sync.Mutex

	srv *httptest.Server

	// activity is the in-memory ring, ordered oldest-first.
	activity []Activity
	nextID   int64

	// Cumulative counters. Survive ring eviction; zeroed only by ResetEpoch.
	totalRequests int64
	totalInput    int64
	totalOutput   int64
	totalCache    int64

	models  []Model
	running []Running
	slots   map[string]SlotMode

	unloadCalls []UnloadCall
	reqCounts   map[string]int

	// Route-shape toggles for version-drift compatibility testing.
	unloadModelRouteMissing bool
	unloadAllRouteMissing   bool
	legacyUnloadEnabled     bool

	events []string

	// now is injectable so seeded rows get deterministic, distinct timestamps.
	now func() time.Time
	seq int64

	closed chan struct{}
}

// New starts a fake proxy with an empty roster. Callers seed it with AddModel,
// SetRunning, and AddActivity. Close it with Close (t.Cleanup is the idiom).
func New() *Server {
	s := &Server{
		nextID:    1,
		slots:     map[string]SlotMode{},
		reqCounts: map[string]int{},
		closed:    make(chan struct{}),
	}
	base := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time {
		s.seq++
		return base.Add(time.Duration(s.seq) * time.Second)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/running", s.handleRunning)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/api/metrics/activity", s.handleActivity)
	mux.HandleFunc("/api/metrics/stats", s.handleStats)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/models/unload", s.handleUnloadAll)
	mux.HandleFunc("/api/models/unload/", s.handleUnloadModel)
	mux.HandleFunc("/unload", s.handleLegacyUnload)
	mux.HandleFunc("/upstream/", s.handleUpstream)
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		s.count("/api/version")
		writeJSON(w, http.StatusOK, map[string]any{"version": "v249-fake"})
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		s.count("/health")
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	s.srv = httptest.NewServer(mux)
	return s
}

// URL is the base URL to point a client at. Always 127.0.0.1 — httptest binds
// loopback by IP, never a hostname.
func (s *Server) URL() string { return s.srv.URL }

// Close shuts the fake down and releases any blocked SlotTimeout handlers.
func (s *Server) Close() {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	s.srv.Close()
}

// ---------------------------------------------------------------- test controls

// ResetEpoch simulates a proxy RESTART: the activity ring is emptied, ids
// restart from 1, and every cumulative counter zeroes. The roster and /running
// survive (llama-swap re-reads its config on boot), but nothing that identified
// the previous process remains. This is the single control the whole epoch
// contract is tested against.
func (s *Server) ResetEpoch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activity = nil
	s.nextID = 1
	s.totalRequests = 0
	s.totalInput = 0
	s.totalOutput = 0
	s.totalCache = 0
}

// EvictOldest drops the n oldest rows from the ring, exactly as
// metricsMaxInMemory eviction does. Cumulative counters are untouched — that
// asymmetry is what makes counter-derived loss possible.
func (s *Server) EvictOldest(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 {
		return
	}
	if n >= len(s.activity) {
		s.activity = nil
		return
	}
	s.activity = append([]Activity(nil), s.activity[n:]...)
}

// EvictID removes one row by id, punching a hole in the id space. Real eviction
// is never like this; the control exists to prove the density assertion fires
// (and that loss degrades to unknown) instead of fabricating a loss number.
func (s *Server) EvictID(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.activity[:0]
	for _, a := range s.activity {
		if a.ID != id {
			out = append(out, a)
		}
	}
	s.activity = append([]Activity(nil), out...)
}

// SetSlots sets the slot behavior for one model id.
func (s *Server) SetSlots(model string, mode SlotMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slots[model] = mode
}

// AddActivity appends one terminal request row and bumps the cumulative
// counters. Returns the assigned id.
func (s *Server) AddActivity(model, reqPath string, status int, durationMS int64) int64 {
	return s.addActivity(model, reqPath, status, durationMS, 11, 0)
}

// AddActivityTokens is AddActivity with explicit token counts.
func (s *Server) AddActivityTokens(model, reqPath string, status int, durationMS int64, in, out int) int64 {
	return s.addActivity(model, reqPath, status, durationMS, in, out)
}

// AddInFlight appends a NON-TERMINAL row: no status, no duration. These are the
// rows that must be marked censored (not failed) when an epoch is sealed — a
// request that was still running when the proxy died has no observable outcome,
// and long requests are over-represented in that set.
func (s *Server) AddInFlight(model, reqPath string) int64 {
	return s.addActivity(model, reqPath, 0, 0, 0, 0)
}

func (s *Server) addActivity(model, reqPath string, status int, durationMS int64, in, out int) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	s.totalRequests++
	s.totalInput += int64(in)
	s.totalOutput += int64(out)
	row := Activity{
		ID:              id,
		Timestamp:       s.now().Format(time.RFC3339),
		Model:           model,
		ReqPath:         reqPath,
		RespContentType: "application/json; charset=utf-8",
		RespStatusCode:  status,
		Tokens: Tokens{
			CacheTokens: -1, DraftTokens: -1, DraftAccTokens: -1,
			InputTokens: in, OutputTokens: out,
			PromptPerSecond: -1, TokensPerSecond: -1,
		},
		DurationMS: durationMS,
		HasCapture: true,
		Metadata:   map[string]string{"fifo_priority": "0"},
	}
	s.activity = append(s.activity, row)
	return id
}

// AddModel registers a roster entry.
func (s *Server) AddModel(m Model) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models = append(s.models, m)
}

// SetRunning replaces the /running set.
func (s *Server) SetRunning(rs ...Running) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = append([]Running(nil), rs...)
}

// RunningIDs is a convenience for SetRunning with default cmd/proxy shapes.
func (s *Server) RunningIDs(ids ...string) {
	rs := make([]Running, 0, len(ids))
	for i, id := range ids {
		rs = append(rs, Running{
			Model: id,
			State: "ready",
			Cmd:   fmt.Sprintf("C:/llama.cpp/llama-server.exe --port %d --host 127.0.0.1 -m V:/models/%s.gguf --ctx-size 4096", 9200+i, id),
			Proxy: fmt.Sprintf("http://127.0.0.1:%d", 9200+i),
			Name:  id,
		})
	}
	s.SetRunning(rs...)
}

// UnloadCalls returns every mutation the fake received, in order. A refusal
// test asserts this is empty.
func (s *Server) UnloadCalls() []UnloadCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]UnloadCall(nil), s.unloadCalls...)
}

// Hits reports how many times a path prefix was requested.
func (s *Server) Hits(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqCounts[path]
}

// SetUnloadModelRouteMissing makes POST /api/models/unload/{model} answer 404,
// exercising the version-drift fallback absorbed from gpu-lock.mjs.
func (s *Server) SetUnloadModelRouteMissing(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unloadModelRouteMissing = v
}

// SetUnloadAllRouteMissing makes POST /api/models/unload answer 404.
func (s *Server) SetUnloadAllRouteMissing(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unloadAllRouteMissing = v
}

// SetLegacyUnload enables the pre-v2xx GET /unload route.
func (s *Server) SetLegacyUnload(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.legacyUnloadEnabled = v
}

// PushEvent queues one SSE frame payload (the JSON that follows "data:").
func (s *Server) PushEvent(payload string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, payload)
}

// TotalRequests is the cumulative counter the restart witness reads.
func (s *Server) TotalRequests() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalRequests
}

// VisibleIDs returns the ids still in the ring, ascending.
func (s *Server) VisibleIDs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, 0, len(s.activity))
	for _, a := range s.activity {
		out = append(out, a.ID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ---------------------------------------------------------------- handlers

func (s *Server) count(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqCounts[path]++
}

func (s *Server) handleRunning(w http.ResponseWriter, r *http.Request) {
	s.count("/running")
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]map[string]any, 0, len(s.running))
	for _, rn := range s.running {
		items = append(items, map[string]any{
			"model": rn.Model,
			"state": orDefault(rn.State, "ready"),
			"cmd":   rn.Cmd,
			"proxy": rn.Proxy,
			// VERIFIED LIE (live v249): a model configured ttl:-1 is reported
			// here as ttl:0. Reproduced deliberately.
			"ttl":         0,
			"name":        rn.Name,
			"description": rn.Description,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"running": items})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	s.count("/v1/models")
	s.mu.Lock()
	defer s.mu.Unlock()
	loaded := map[string]bool{}
	for _, rn := range s.running {
		loaded[rn.Model] = true
	}
	data := make([]map[string]any, 0, len(s.models))
	for _, m := range s.models {
		state := "unloaded"
		if loaded[m.ID] {
			state = "loaded"
		}
		aliases := m.Aliases
		if aliases == nil {
			aliases = []string{}
		}
		data = append(data, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"created":  1786602289,
			"owned_by": "llama-swap",
			"name":     orDefault(m.Name, m.ID),
			"meta":     map[string]any{"llamaswap": map[string]any{"aliases": aliases, "type": "model"}},
			"status":   map[string]any{"value": state},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	s.count("/api/metrics/activity")
	q := r.URL.Query()
	model := q.Get("model")
	page := atoiDefault(q.Get("page"), 1)
	limit := atoiDefault(q.Get("limit"), 100)
	order := strings.ToLower(q.Get("order"))
	if order == "" {
		order = "desc" // live default: newest first
	}

	s.mu.Lock()
	rows := make([]Activity, 0, len(s.activity))
	for _, a := range s.activity {
		if model != "" && a.Model != model {
			continue
		}
		rows = append(rows, a)
	}
	s.mu.Unlock()

	sort.Slice(rows, func(i, j int) bool {
		if order == "asc" {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].ID > rows[j].ID
	})

	total := len(rows)
	totalPages := 0
	if limit > 0 {
		totalPages = (total + limit - 1) / limit
	}
	if page < 1 {
		page = 1
	}
	start := (page - 1) * limit
	if start > total {
		start = total
	}
	end := start + limit
	if limit <= 0 || end > total {
		end = total
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":        rows[start:end],
		"page":        page,
		"limit":       limit,
		"total":       total,
		"total_pages": totalPages,
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	s.count("/api/metrics/stats")
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"total_requests":      s.totalRequests,
		"total_input_tokens":  s.totalInput,
		"total_output_tokens": s.totalOutput,
		"total_cache_tokens":  s.totalCache,
		"prompt_histogram":    nil,
		"gen_histogram":       nil,
	})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	s.count("/api/events")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	s.mu.Lock()
	frames := append([]string(nil), s.events...)
	s.mu.Unlock()
	for _, f := range frames {
		fmt.Fprintf(w, "event:message\ndata:%s\n\n", f)
	}
	if flusher != nil {
		flusher.Flush()
	}
	// Hold the stream open like the real SSE endpoint; the client drains for a
	// bounded window and disconnects.
	select {
	case <-r.Context().Done():
	case <-s.closed:
	case <-time.After(30 * time.Second):
	}
}

func (s *Server) handleUnloadAll(w http.ResponseWriter, r *http.Request) {
	s.count("/api/models/unload")
	s.mu.Lock()
	missing := s.unloadAllRouteMissing
	if !missing {
		s.unloadCalls = append(s.unloadCalls, UnloadCall{Method: r.Method, Path: r.URL.Path, All: true, At: time.Now()})
		s.running = nil
	}
	s.mu.Unlock()
	if missing {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "route not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleUnloadModel(w http.ResponseWriter, r *http.Request) {
	s.count("/api/models/unload/")
	model := strings.TrimPrefix(r.URL.Path, "/api/models/unload/")
	s.mu.Lock()
	missing := s.unloadModelRouteMissing
	known := false
	for _, m := range s.models {
		if m.ID == model {
			known = true
			break
		}
	}
	if !missing && known {
		s.unloadCalls = append(s.unloadCalls, UnloadCall{Method: r.Method, Path: r.URL.Path, Model: model, At: time.Now()})
		out := s.running[:0]
		for _, rn := range s.running {
			if rn.Model != model {
				out = append(out, rn)
			}
		}
		s.running = append([]Running(nil), out...)
	}
	s.mu.Unlock()
	switch {
	case missing:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "route not found"})
	case !known:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "model not found: " + model})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "model": model})
	}
}

func (s *Server) handleLegacyUnload(w http.ResponseWriter, r *http.Request) {
	s.count("/unload")
	s.mu.Lock()
	enabled := s.legacyUnloadEnabled
	if enabled {
		s.unloadCalls = append(s.unloadCalls, UnloadCall{Method: r.Method, Path: r.URL.Path, All: true, At: time.Now()})
		s.running = nil
	}
	s.mu.Unlock()
	if !enabled {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "route not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleUpstream(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/upstream/")
	model, tail, _ := strings.Cut(rest, "/")
	switch tail {
	case "slots":
		s.count("/upstream/slots")
		s.mu.Lock()
		mode := s.slots[model]
		s.mu.Unlock()
		if mode == "" {
			mode = SlotIdle
		}
		switch mode {
		case SlotStatus404:
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "slots endpoint disabled (llama-server started without --slots)"})
		case SlotStatus500:
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "upstream failure"})
		case SlotTimeout:
			select {
			case <-r.Context().Done():
			case <-s.closed:
			case <-time.After(30 * time.Second):
			}
		case SlotProcessing:
			writeJSON(w, http.StatusOK, []map[string]any{{"id": 0, "n_ctx": 4096, "is_processing": true, "id_task": 77}})
		default:
			writeJSON(w, http.StatusOK, []map[string]any{{"id": 0, "n_ctx": 4096, "is_processing": false, "id_task": -1}})
		}
	case "props":
		s.count("/upstream/props")
		writeJSON(w, http.StatusOK, map[string]any{
			"default_generation_settings": map[string]any{"n_ctx": 4096},
			"total_slots":                 1,
			"model_path":                  "V:/models/" + model + ".gguf",
			"build_info":                  "b10356",
		})
	case "health":
		s.count("/upstream/health")
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown upstream path"})
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
