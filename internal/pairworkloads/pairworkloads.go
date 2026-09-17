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
	"github.com/dmmdea/offload-harness/internal/ledger"
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
)

// Config is the emitter's configuration, normally built by FromConfig.
type Config struct {
	Enabled  bool
	Endpoint string
	// AppDir overrides PAIR's per-user app-data dir (tests). "" = the
	// platform default, mirroring PAIR's shared/appdir.
	AppDir string
}

// FromConfig reads the two harness config keys.
func FromConfig(cfg config.Config) Config {
	ep := strings.TrimSpace(cfg.PairWorkloadsEndpoint)
	if ep == "" {
		ep = DefaultEndpoint
	}
	return Config{Enabled: cfg.PairWorkloadsEnabled, Endpoint: ep}
}

// Event is one workload lifecycle frame's content. JobID doubles as PAIR's
// runId so the store key (origin, engine, runId, id) is unique per harness
// job. Timestamps are epoch milliseconds; StartedAt / CompletedAt 0 = null.
type Event struct {
	JobID       string
	Model       string
	Engine      string
	Node        string // harness node name; "" = this box
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
	warnOnce sync.Once
	inflight sync.WaitGroup
	seq      atomic.Int64
}

// New builds an emitter. It reads nothing until the first use.
func New(c Config) *Emitter {
	if c.Endpoint == "" {
		c.Endpoint = DefaultEndpoint
	}
	e := &Emitter{cfg: c, client: &http.Client{Timeout: sendTimeout}, appDir: c.AppDir}
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
	e.idMu.Lock()
	defer e.idMu.Unlock()
	if !e.idAt.IsZero() && time.Since(e.idAt) < identityTTL {
		return e.selfUUID, e.members
	}
	e.idAt = time.Now()
	e.selfUUID, e.members = "", nil
	raw, err := os.ReadFile(filepath.Join(e.appDir, "node-id.json"))
	if err != nil {
		return "", nil
	}
	var nid struct {
		NodeUUID string `json:"node_uuid"`
	}
	if json.Unmarshal(raw, &nid) != nil || nid.NodeUUID == "" {
		return "", nil
	}
	e.selfUUID = nid.NodeUUID
	e.members = map[string]string{}
	if raw, err := os.ReadFile(filepath.Join(e.appDir, "cluster", "members.json")); err == nil {
		var list []struct {
			Name     string `json:"name"`
			NodeUUID string `json:"nodeUuid"`
		}
		if json.Unmarshal(raw, &list) == nil {
			for _, m := range list {
				if m.Name != "" && m.NodeUUID != "" {
					e.members[strings.ToLower(m.Name)] = m.NodeUUID
				}
			}
		}
	}
	return e.selfUUID, e.members
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
// non-text engines the harness drives (whispercpp, comfyui).
func EngineFor(task, seat string) string {
	s := strings.ToLower(seat)
	switch {
	case strings.Contains(s, "vllm"):
		return "vllm"
	case strings.Contains(s, "whisper"), task == "transcribe":
		return "whispercpp"
	}
	switch task {
	case "generate_image", "inpaint_image", "edit_image_generative", "upscale_image",
		"generate_video", "animate_character", "generate_audio", "run_graph":
		return "comfyui"
	}
	return "llamacpp"
}

func (e *Emitter) frame(ev Event) ([]byte, error) {
	self, members := e.identity()
	if self == "" {
		return nil, fmt.Errorf("pairworkloads: PAIR identity unavailable under %s", e.appDir)
	}
	scheduledOn := self
	if n := strings.ToLower(strings.TrimSpace(ev.Node)); n != "" {
		if u, ok := members[n]; ok {
			scheduledOn = u
		}
	}
	null := json.RawMessage("null")
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
		"scheduledOn":    mustJSON(scheduledOn),
		"createdAt":      json.RawMessage(fmt.Sprint(created)),
		"startedAt":      ms(ev.StartedAt),
		"completedAt":    ms(ev.CompletedAt),
		"error":          errRaw,
		"requesterId":    reqRaw,
	}
	return json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  MethodFor(ev.State),
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
	if len(s) <= CardErrorMax {
		return s
	}
	return strings.TrimSpace(s[:CardErrorMax-1]) + "…"
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// Send posts one frame synchronously. A disabled emitter is a silent no-op.
func (e *Emitter) Send(ctx context.Context, ev Event) error {
	if !e.Enabled() {
		return nil
	}
	body, err := e.frame(ev)
	if err != nil {
		return err
	}
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

// Emit posts in the background and never blocks the caller. The first failure
// per process logs one line; later ones are silent — PAIR being down is normal.
func (e *Emitter) Emit(ev Event) {
	if !e.Enabled() {
		return
	}
	e.inflight.Add(1)
	go func() {
		defer e.inflight.Done()
		if err := e.Send(context.Background(), ev); err != nil {
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
		e.Emit(e.FromLedger(row))
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
		Engine:      EngineFor(row.Task, model),
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
