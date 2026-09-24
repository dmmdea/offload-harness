// Package pairworkloads reports the harness's work to NVIDIA Personal AI
// Router (PAIR) so it appears in PAIR's Jobs list on every cluster member and
// counts in PAIR's scheduler as pending work on the node that runs it.
//
// PAIR's Jobs list is a stream of workload lifecycle frames that only PAIR's
// own proxies produce; the harness routes around those proxies (llama-swap
// :11436, the fleet nodes over the tailnet), so until 2026-09-17 nothing it ran
// was visible there. PAIR's workload manager accepts the same frames on a
// loopback ingress (our patch: fork dmmdea/Personal-AI-Router, branch
// feat/workload-local-ingress, worker 0.14.0; upstream PR "workload-manager:
// opt-in loopback ingress"); this package posts them.
//
// It is fire-and-forget by construction: a 2 s timeout, one warning per
// process on the first failure, and never a harness result changed. PAIR being
// absent or down is normal, not an incident.
//
// Identity comes from PAIR's own app-data files, never from configuration:
// node-id.json (this box's PAIR UUID = originatedFrom) and cluster/members.json
// (member name -> UUID = scheduledOn). Harness node names equal PAIR member
// names (the hostname each box advertises), compared case-insensitively.
package pairworkloads

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

const (
	// DefaultEndpoint is the ingress every node's workload-ingress.json binds.
	DefaultEndpoint = "http://127.0.0.1:14324/v1/workloads/events"
	// identityTTL bounds how often the two identity files are re-read; a
	// member joining the cluster shows up within a minute.
	identityTTL = 60 * time.Second
	// sendTimeout is the whole budget of one frame. PAIR answers in
	// milliseconds; anything slower is PAIR being down, and the harness's own
	// result must never wait on it.
	sendTimeout = 2 * time.Second
	// engineTTL bounds how long a local seat's resolved engine is trusted; a
	// failed roster read is retried sooner (engineRetryTTL) so a llama-swap
	// restart does not pin the wrong label for the full window.
	engineTTL      = 10 * time.Minute
	engineRetryTTL = 30 * time.Second
	rosterTimeout  = 2 * time.Second
)

// Config is the emitter's configuration, normally built by FromConfig.
type Config struct {
	Enabled  bool
	Endpoint string
	// AppDir overrides PAIR's per-user app-data dir (tests). "" = the
	// platform default, mirroring PAIR's shared/appdir.
	AppDir string
	// VLLMSeats is the box's declared `vllm_seats` and SwapEndpoint the
	// llama-swap its seats sit behind. Together they label a LOCAL seat bound
	// by alias: the Qube's agent seat is `agent-pool`, an alias of
	// `qwen3.8-27b-vllm-3card`, so the name alone reads as llama.cpp and PAIR
	// showed a vLLM job as "llamacpp".
	VLLMSeats    []string
	SwapEndpoint string
	// StateDir is the operator's state_dir: the open-card register (orphans.go)
	// lives under the machine-wide state root it resolves to, beside
	// seat-inflight. OpenDir, when set, names the register directory outright
	// (tests).
	StateDir string
	OpenDir  string
}

// FromConfig reads the two harness config keys.
func FromConfig(cfg config.Config) Config {
	ep := strings.TrimSpace(cfg.PairWorkloadsEndpoint)
	if ep == "" {
		ep = DefaultEndpoint
	}
	return Config{Enabled: cfg.PairWorkloadsEnabled, Endpoint: ep,
		VLLMSeats: cfg.VLLMSeats, SwapEndpoint: cfg.Endpoint, StateDir: cfg.StateDir}
}

// Event is one workload lifecycle frame's content. JobID doubles as PAIR's
// runId so the store key (origin, engine, runId, id) is unique per harness
// job. Timestamps are epoch milliseconds; StartedAt / CompletedAt 0 = null.
type Event struct {
	JobID  string
	Model  string
	Engine string
	Node   string // harness node name; "" = this box
	// NodeAliases are other names the same node is known by (the fleet node
	// id from its health, the host of its dispatch URL, the name it reported
	// on the wire). The frame resolves Node first, then each alias, against
	// PAIR's member names and addresses; a remote run none of them resolves
	// is stamped with NO node rather than with this box (0.131.2).
	NodeAliases []string
	State       string // queued | running | completed | failed
	Error       string
	Requester   string
	CreatedAt   int64
	StartedAt   int64
	CompletedAt int64
}

// Emitter posts frames to PAIR's loopback ingress.
type Emitter struct {
	cfg    Config
	client *http.Client
	appDir string

	idMu     sync.Mutex
	idAt     time.Time
	selfUUID string
	members  map[string]string // lower(name) -> uuid
	byAddr   map[string]string // lower(ipAddress) -> uuid
	// resolved remembers, per job, the node UUID an in-flight frame resolved
	// to, so a terminal frame whose names resolve to nothing (a node that
	// reports its fleet id, not its hostname) keeps the card where the job
	// ran instead of re-pointing it. Entries die with the terminal frame.
	resolved   map[string]string
	unresolved map[string]struct{} // name sets already warned about
	warnOnce   sync.Once
	inflight   sync.WaitGroup
	seq        atomic.Int64

	engineMu    sync.Mutex
	engineCache map[string]engineAnswer // lower(seat) -> resolved local engine
	// fetchRoster reads the llama-swap roster; swapclient.FetchRoster unless a
	// test swaps it.
	fetchRoster func(ctx context.Context, endpoint string, timeout time.Duration) (swapclient.Roster, error)

	// The open-card register (orphans.go): one marker per job this process
	// has an in-flight frame out for, so a process that dies before the
	// terminal frame leaves a card another process can close.
	openDirOnce sync.Once
	openDir     string // "" = no register
	openMu      sync.Mutex
	open        map[string]string // job id -> marker path
	selfOnce    sync.Once
	selfStart   int64
	sweepOnce   sync.Once
	// Open running cards for long tool calls (calls.go), by task.
	callMu sync.Mutex
	calls  map[string][]*openCall
	// Seams (tests): process liveness, process start identity, the clock.
	alive     func(pid int) bool
	procStart func(pid int) (int64, bool)
	now       func() time.Time
}

type engineAnswer struct {
	vllm bool
	exp  time.Time
}

// New builds an emitter. It reads nothing until the first use.
func New(c Config) *Emitter {
	if c.Endpoint == "" {
		c.Endpoint = DefaultEndpoint
	}
	e := &Emitter{cfg: c, client: &http.Client{Timeout: sendTimeout}, appDir: c.AppDir,
		fetchRoster: swapclient.FetchRoster,
		alive:       gpulease.PIDAlive, procStart: gpulease.ProcessStart, now: time.Now}
	if e.appDir == "" {
		// OFFLOAD_PAIR_APPDIR points at a PAIR data dir that is not at the
		// platform default (a portable install, a test fixture).
		e.appDir = strings.TrimSpace(os.Getenv("OFFLOAD_PAIR_APPDIR"))
	}
	if e.appDir == "" {
		e.appDir = defaultAppDir()
	}
	return e
}

// defaultAppDir mirrors PAIR's shared/appdir: %LOCALAPPDATA% on Windows,
// $XDG_CONFIG_HOME or ~/.config on Linux, ~/Library/Application Support on
// macOS, then "Nvidia Corporation/Personal AI Router".
func defaultAppDir() string {
	var base string
	if runtime.GOOS == "windows" {
		base = os.Getenv("LOCALAPPDATA")
	}
	if base == "" {
		if d, err := os.UserConfigDir(); err == nil {
			base = d
		}
	}
	if base == "" {
		return ""
	}
	return filepath.Join(base, "Nvidia Corporation", "Personal AI Router")
}

// Enabled is true when the config opts in AND PAIR is installed on this box
// (its node-id.json exists and names a UUID).
func (e *Emitter) Enabled() bool {
	if e == nil || !e.cfg.Enabled {
		return false
	}
	self, _ := e.identity()
	return self != ""
}

func (e *Emitter) identity() (self string, members map[string]string) {
	self, members, _ = e.identityFull()
	return self, members
}

// identityFull is identity plus the member address map (lower(ipAddress) ->
// uuid), for a node whose dispatch base is an address literal.
func (e *Emitter) identityFull() (self string, members, byAddr map[string]string) {
	e.idMu.Lock()
	defer e.idMu.Unlock()
	if !e.idAt.IsZero() && time.Since(e.idAt) < identityTTL {
		return e.selfUUID, e.members, e.byAddr
	}
	e.idAt = time.Now()
	e.selfUUID, e.members, e.byAddr = "", nil, nil
	raw, err := os.ReadFile(filepath.Join(e.appDir, "node-id.json"))
	if err != nil {
		return "", nil, nil
	}
	var nid struct {
		NodeUUID string `json:"node_uuid"`
	}
	if json.Unmarshal(raw, &nid) != nil || nid.NodeUUID == "" {
		return "", nil, nil
	}
	e.selfUUID = nid.NodeUUID
	e.members = map[string]string{}
	e.byAddr = map[string]string{}
	if raw, err := os.ReadFile(filepath.Join(e.appDir, "cluster", "members.json")); err == nil {
		var list []struct {
			Name      string `json:"name"`
			NodeUUID  string `json:"nodeUuid"`
			IPAddress string `json:"ipAddress"`
		}
		if json.Unmarshal(raw, &list) == nil {
			for _, m := range list {
				if m.NodeUUID == "" {
					continue
				}
				if m.Name != "" {
					e.members[strings.ToLower(m.Name)] = m.NodeUUID
				}
				if a := strings.ToLower(strings.TrimSpace(m.IPAddress)); a != "" && a != "127.0.0.1" && a != "::1" {
					e.byAddr[a] = m.NodeUUID
				}
			}
		}
	}
	return e.selfUUID, e.members, e.byAddr
}

// resolveNode turns the names a job's node is known by into the PAIR UUID
// the frame stamps as scheduledOn. "" as Node means this box. Otherwise the
// first of Node + NodeAliases that matches a member name or address wins;
// failing that, the UUID an earlier frame of the same job resolved to; and
// failing THAT the job is stamped with no node at all (ok=false) — PAIR then
// draws no line and names no node, which is true, where naming this box was
// not (2026-09-20: every terminal frame of a node that reports its fleet id
// re-pointed the card at the delegator; a node dispatched by address never
// got a line). Each unresolved name set is logged once.
func (e *Emitter) resolveNode(ev Event) (uuid string, ok bool) {
	self, members, byAddr := e.identityFull()
	node := strings.ToLower(strings.TrimSpace(ev.Node))
	if node == "" {
		return self, true
	}
	names := make([]string, 0, 1+len(ev.NodeAliases))
	names = append(names, node)
	for _, a := range ev.NodeAliases {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			names = append(names, a)
		}
	}
	terminal := ev.State == "completed" || ev.State == "failed"
	e.idMu.Lock()
	defer e.idMu.Unlock()
	for _, n := range names {
		if u, hit := members[n]; hit {
			e.remember(ev.JobID, u, terminal)
			return u, true
		}
		if u, hit := byAddr[n]; hit {
			e.remember(ev.JobID, u, terminal)
			return u, true
		}
	}
	if u, hit := e.resolved[ev.JobID]; hit {
		if terminal {
			delete(e.resolved, ev.JobID)
		}
		return u, true
	}
	key := strings.Join(names, "|")
	if e.unresolved == nil {
		e.unresolved = map[string]struct{}{}
	}
	if _, seen := e.unresolved[key]; !seen {
		e.unresolved[key] = struct{}{}
		log.Printf("pairworkloads: no PAIR member is named %q; its jobs get no node in PAIR's Jobs list (members: %d)", key, len(members))
	}
	return "", false
}

// remember records (or, on a terminal frame, forgets) a job's resolved node.
// Caller holds idMu.
func (e *Emitter) remember(jobID, uuid string, terminal bool) {
	if jobID == "" {
		return
	}
	if terminal {
		delete(e.resolved, jobID)
		return
	}
	if e.resolved == nil {
		e.resolved = map[string]string{}
	}
	e.resolved[jobID] = uuid
}

// MethodFor maps a workload state to the lifecycle method PAIR expects.
func MethodFor(state string) string {
	switch state {
	case "queued":
		return "workload:submitted"
	case "running":
		return "workload:started"
	case "completed":
		return "workload:completed"
	default:
		return "workload:errored"
	}
}

// EngineFor names the real engine behind a harness task and seat with the
// identifiers PAIR's upstream engine PRs use (llamacpp, vllm) plus the two
// non-text engines the harness drives (whispercpp, comfyui) and the
// accelerators (coral-edgetpu, hailo-8l): an NPU call is not a llama.cpp job,
// and the card's engine badge is how PAIR tells them apart.
func EngineFor(task, seat string) string {
	s := strings.ToLower(seat)
	switch {
	case strings.Contains(s, "coral"), strings.Contains(s, "edgetpu"):
		return "coral-edgetpu"
	case strings.Contains(s, "hailo"):
		return "hailo-8l"
	case strings.Contains(s, "vllm"):
		return "vllm"
	case strings.Contains(s, "whisper"), task == "transcribe":
		return "whispercpp"
	}
	switch task {
	case "generate_image", "inpaint_image", "edit_image_generative", "upscale_image",
		"generate_video", "animate_character", "generate_audio", "run_graph":
		return "comfyui"
	case "compose_video":
		// HyperFrames on the CPU (ADR 0059): no model and no llama.cpp job.
		return "hyperframes"
	}
	return "llamacpp"
}

// LocalEngine is EngineFor for a seat served by THIS box's llama-swap: a seat
// the name reads as llama.cpp is labelled vllm when the box declares it in
// vllm_seats, directly or through the alias the roster resolves it to. Only a
// local seat can be resolved — a remote node's aliases live in ITS roster —
// so remote placements keep EngineFor. A roster that cannot be read leaves
// the name-based answer, which is what every card carried before.
func (e *Emitter) LocalEngine(task, seat string) string {
	engine := EngineFor(task, seat)
	if engine != "llamacpp" || e == nil || len(e.cfg.VLLMSeats) == 0 {
		return engine
	}
	if e.localVLLM(seat) {
		return "vllm"
	}
	return engine
}

func (e *Emitter) localVLLM(seat string) bool {
	seat = strings.TrimSpace(seat)
	if seat == "" {
		return false
	}
	if declaresVLLM(e.cfg.VLLMSeats, seat) {
		return true
	}
	key := strings.ToLower(seat)
	now := time.Now()
	e.engineMu.Lock()
	if a, ok := e.engineCache[key]; ok && now.Before(a.exp) {
		e.engineMu.Unlock()
		return a.vllm
	}
	e.engineMu.Unlock()

	ans := engineAnswer{exp: now.Add(engineRetryTTL)}
	if e.fetchRoster != nil && e.cfg.SwapEndpoint != "" {
		roster, err := e.fetchRoster(context.Background(), e.cfg.SwapEndpoint, rosterTimeout)
		if err == nil {
			canonical, _ := roster.Canonical(seat)
			ans = engineAnswer{vllm: declaresVLLM(e.cfg.VLLMSeats, canonical), exp: now.Add(engineTTL)}
		}
	}
	e.engineMu.Lock()
	if e.engineCache == nil {
		e.engineCache = map[string]engineAnswer{}
	}
	e.engineCache[key] = ans
	e.engineMu.Unlock()
	return ans.vllm
}

// declaresVLLM mirrors config.DeclaresVLLMSeat over the copied list.
func declaresVLLM(seats []string, id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	for _, s := range seats {
		if strings.EqualFold(strings.TrimSpace(s), id) {
			return true
		}
	}
	return false
}

// isAcceleratorEngine reports whether an EngineFor result names an NPU rather
// than a text / media engine.
func isAcceleratorEngine(engine string) bool {
	switch engine {
	case "coral-edgetpu", "hailo-8l":
		return true
	}
	return false
}

func (e *Emitter) frame(ev Event) ([]byte, error) {
	body, _, err := e.build(ev)
	return body, err
}

// build is frame plus the workloadInfo it carries, which the open-card
// register keeps so an orphan's terminal frame names exactly the card PAIR
// holds (same origin, node, engine, ids and timestamps).
func (e *Emitter) build(ev Event) ([]byte, map[string]json.RawMessage, error) {
	self, _ := e.identity()
	if self == "" {
		return nil, nil, fmt.Errorf("pairworkloads: PAIR identity unavailable under %s", e.appDir)
	}
	null := json.RawMessage("null")
	scheduledOn := null
	if u, ok := e.resolveNode(ev); ok {
		scheduledOn = mustJSON(u)
	}
	ms := func(v int64) json.RawMessage {
		if v == 0 {
			return null
		}
		return json.RawMessage(fmt.Sprint(v))
	}
	errRaw, reqRaw := null, null
	if ev.Error != "" {
		errRaw = mustJSON(CardError(ev.Error))
	}
	if ev.Requester != "" {
		reqRaw = mustJSON(ev.Requester)
	}
	created := ev.CreatedAt
	if created == 0 {
		created = time.Now().UnixMilli()
	}
	info := map[string]json.RawMessage{
		"id":             mustJSON(ev.JobID),
		"model":          mustJSON(ev.Model),
		"engine":         mustJSON(ev.Engine),
		"runId":          mustJSON(ev.JobID),
		"state":          mustJSON(ev.State),
		"originatedFrom": mustJSON(self),
		"scheduledOn":    scheduledOn,
		"createdAt":      json.RawMessage(fmt.Sprint(created)),
		"startedAt":      ms(ev.StartedAt),
		"completedAt":    ms(ev.CompletedAt),
		"error":          errRaw,
		"requesterId":    reqRaw,
	}
	body, err := frameBody(MethodFor(ev.State), info)
	return body, info, err
}

// frameBody wraps a workloadInfo in the JSON-RPC notification PAIR expects.
func frameBody(method string, info map[string]json.RawMessage) ([]byte, error) {
	return json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  map[string]any{"workloadInfo": info},
	})
}

// CardErrorMax bounds the failure text a PAIR card shows. A jsonschema
// re-pack dump or a full acceptance report runs to hundreds of characters
// and turned the Jobs list into a wall of red (2026-09-17, first live day);
// the card wants the verdict, the ledger keeps the whole reason.
const CardErrorMax = 140

// CardError collapses a failure reason to one short line: whitespace folded,
// cut at CardErrorMax with an ellipsis.
func CardError(reason string) string {
	s := strings.Join(strings.Fields(reason), " ")
	r := []rune(s)
	if len(r) <= CardErrorMax {
		return s
	}
	return strings.TrimSpace(string(r[:CardErrorMax-1])) + "…"
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// Send posts one frame synchronously. A disabled emitter is a silent no-op.
// An in-flight frame is recorded in the open-card register before it is
// posted; a terminal frame's marker is removed once the post was attempted.
func (e *Emitter) Send(ctx context.Context, ev Event) error {
	if !e.Enabled() {
		return nil
	}
	body, info, err := e.build(ev)
	if err != nil {
		return err
	}
	done := e.track(ev, info)
	err = e.post(ctx, body)
	e.untrack(done, info, err)
	return err
}

// post delivers one built frame within sendTimeout.
func (e *Emitter) post(ctx context.Context, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pairworkloads: %s answered %d", e.cfg.Endpoint, resp.StatusCode)
	}
	return nil
}

// Emit posts in the background and never blocks the caller on PAIR. The first
// failure per process logs one line; later ones are silent — PAIR being down
// is normal.
//
// The frame is built and the open-card register updated HERE, on the caller's
// goroutine, so a job's markers follow its frames in order (queued, running,
// terminal) whatever order the background posts finish in. The first Emit of
// an emitter also sweeps the register once for cards a dead process left open.
func (e *Emitter) Emit(ev Event) {
	if !e.Enabled() {
		return
	}
	e.SweepOrphansAsync()
	body, info, err := e.build(ev)
	done := ""
	if err == nil {
		done = e.track(ev, info)
	}
	e.inflight.Add(1)
	go func() {
		defer e.inflight.Done()
		if err == nil {
			err = e.post(context.Background(), body)
		}
		// The terminal marker goes only after the post was attempted: a
		// process killed in between still leaves a marker to close the card,
		// and a post that failed leaves the terminal frame for the sweep.
		e.untrack(done, info, err)
		if err != nil {
			e.warnOnce.Do(func() {
				log.Printf("pairworkloads: PAIR ingress unreachable; harness jobs will not appear in PAIR's Jobs list (%v)", err)
			})
		}
	}()
}

// Wait blocks until every background Emit has finished (tests, shutdown).
func (e *Emitter) Wait() {
	if e != nil {
		e.inflight.Wait()
	}
}

// AttachLedger turns every non-delegation ledger row into one terminal frame.
//
// Skipped on purpose: agent_delegate rows (the delegate runner emits those,
// together with the in-flight states, under one identity so PAIR shows one
// card); agent rows (this box serving SOMEONE ELSE's delegation, already
// reported by the box that asked); cache hits (no GPU work happened).
func (e *Emitter) AttachLedger(l *ledger.Ledger) {
	if e == nil || l == nil || !e.cfg.Enabled {
		return
	}
	l.Observe(func(row ledger.Entry) {
		switch row.Task {
		case "agent_delegate", "agent", "":
			return
		}
		if row.CacheHit {
			return
		}
		// FromLedger may read the llama-swap roster (LocalEngine); keep that
		// off the ledger writer's path.
		// A call that opened a running card (Begin) is closed by its own
		// row, on the same card; claim it here, in row order.
		open := e.claim(row.Task)
		e.inflight.Add(1)
		go func() {
			defer e.inflight.Done()
			ev := e.FromLedger(row)
			if open != nil {
				ev = closeWith(ev, open)
			}
			e.Emit(ev)
		}()
	})
}

// FromLedger builds the terminal event for a finished tool call: the row's
// timestamp is the completion, and the latency walks it back to the start.
func (e *Emitter) FromLedger(row ledger.Entry) Event {
	ts := row.TS
	if ts == 0 {
		ts = time.Now().Unix()
	}
	completed := ts * 1000
	created := completed - row.LatencyMs
	if created <= 0 || created > completed {
		created = completed
	}
	model := row.ModelTier
	if model == "" {
		model = row.Task
	}
	engine := e.LocalEngine(row.Task, model)
	// A forwarded accelerator call is recorded as "<node>:<device>" (E-04):
	// the card runs on that node and shows the device, not the pair.
	node := ""
	if isAcceleratorEngine(engine) {
		if i := strings.Index(model, ":"); i > 0 {
			node, model = model[:i], model[i+1:]
		}
	}
	state, errText := "completed", ""
	if row.Deferred {
		state = "failed"
		errText = row.Reason
		if errText == "" {
			errText = "deferred"
		}
	}
	return Event{
		JobID:       fmt.Sprintf("led-%d-%d", ts, e.seq.Add(1)),
		Model:       model,
		Engine:      engine,
		Node:        node,
		State:       state,
		Error:       errText,
		Requester:   Requester(row.OriginSession),
		CreatedAt:   created,
		StartedAt:   created,
		CompletedAt: completed,
	}
}

// Requester is the requesterId the harness stamps: its own name plus the
// session that asked, when one is known.
func Requester(session string) string {
	if session == "" {
		return "offload-harness"
	}
	return "offload-harness/" + session
}
