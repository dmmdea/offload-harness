// Package gpuactivity answers one question about this box's cards: WHAT is the
// harness doing on them right now — not merely whether something holds them.
//
// The gap it closes (2026-09-14, register D-93). Every surface a session could
// read said "busy" and nothing more: `gpu status` and `offload_status.gpu_lease`
// reported a lease's holder pid, age and reason; the drain printed "1 in flight"
// sixty-one times; llama-swap's /running said a seat was loaded. None of them
// said whether the cards were doing work or sitting held and unused, which
// request was in flight, whose run it belonged to, how far along it was, or
// whether it was still generating. A session that cannot tell those apart reads
// every "busy" as "refuse", and that reading is the tenth-time incident ADR 0039
// exists to stop.
//
// Three readings compose the answer, each from the source that owns it:
//
//   - the LEASE (internal/gpulease): who reserved the cards, whether that
//     process is alive, what command it wrapped, when it last heartbeat;
//   - the SEAT (internal/seatload): whether llama-swap has the agent seat
//     loaded, whether it is still loading, and how many requests the engine
//     itself reports running or waiting;
//   - the RUNS (this package's registry): every agent loop in flight on this
//     box, registered by the process that runs it, with its step, its token
//     count, its phase and a heartbeat — the harness's own unit of work, which
//     a seat's per-request gauge cannot see between one step and the next.
//
// Plus one sample of the cards themselves (nvidia-smi utilization and memory,
// and the processes on them), so a lease held over an idle card is named as
// exactly that.
//
// THE REGISTRY. A run is a multi-step loop: the seat is idle for seconds between
// steps while the loop parses a tool call and runs it, and a drain that reads
// only the seat's gauge sees "0 in flight" in that gap, unloads the seat, and
// the run's next step dies behind the fence (10:08:29 on 2026-09-14: the drain
// unloaded the 27B the instant a first step returned; the run's second step
// then waited its whole 600 s wall). Registration is what makes the run
// visible: `Start` writes one small JSON file under `<state root>/gpu/activity/`
// beside the lease, updates it on every step and every 15 s, and removes it at
// the end. Readers apply the lease's own staleness rule (a dead or recycled pid,
// or a heartbeat older than HeartbeatTTL, is debris and is swept), so a crashed
// process cannot hold a drain hostage. The registry never gates anything by
// itself — the drain and the run-slot wait (internal/modelaffinity) read it.
package gpuactivity

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

const (
	// HeartbeatEvery is how often a live run refreshes its record without any
	// step progress — a single 27B completion runs 3–4 minutes, longer than any
	// staleness window worth having.
	HeartbeatEvery = 15 * time.Second
	// HeartbeatTTL is the staleness bound on a record whose process is still
	// alive: a hung process that stopped beating is not a run anyone waits for.
	// Same figure as the lease's DefaultHeartbeatTTL, on purpose.
	HeartbeatTTL = 120 * time.Second
	// goalClip bounds the goal excerpt kept in a record: enough to recognise the
	// task in `gpu status`, never the whole contract.
	goalClip   = 120
	filePrefix = "run-"
	fileSuffix = ".json"
	// DirName is the registry directory, a sibling of the lease directory under
	// the same `gpu/` root — one state root, one resolver (gpulease.LeaseDir).
	DirName = "activity"
)

// Run is one registered agent run on a seat: the harness's unit of work.
type Run struct {
	ID          string `json:"id"`
	PID         int    `json:"pid"`
	StartTimeMs int64  `json:"start_time_ms,omitempty"` // the process's start time (pid-recycling guard, as the lease)
	Seat        string `json:"seat"`
	// Kind names the door the run came through: "agent_run" (the MCP tool),
	// "contract" (a delegation contract run by this node — a local spread leg or
	// a fleet job), "lab" (the CLI replay gate).
	Kind        string `json:"kind"`
	Origin      string `json:"origin,omitempty"` // who asked: node id, job id, host
	Goal        string `json:"goal,omitempty"`   // the first goalClip characters
	StartedAtMs int64  `json:"started_at_ms"`
	// Phase is where the run is: "admission" (waiting for the endpoint to
	// settle), "cold-load" (the seat is loading for it), "running" (planner
	// steps), "final" (the forced final step), "repack" (the structured re-pack).
	Phase       string `json:"phase"`
	Step        int    `json:"step,omitempty"`
	MaxSteps    int    `json:"max_steps,omitempty"`
	TokensOut   int    `json:"tokens_out,omitempty"`
	HeartbeatMs int64  `json:"heartbeat_ms"`
}

// Age is how long the run has been in flight.
func (r Run) Age(now time.Time) time.Duration {
	d := now.Sub(time.UnixMilli(r.StartedAtMs))
	if d < 0 {
		return 0
	}
	return d
}

// Summary is the one-line human form used by the drain and `gpu status`.
func (r Run) Summary(now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s pid %d", r.Kind, r.PID)
	if r.Phase != "" && r.Phase != "running" {
		fmt.Fprintf(&b, " [%s]", r.Phase)
	}
	if r.Step > 0 {
		if r.MaxSteps > 0 {
			fmt.Fprintf(&b, " step %d/%d", r.Step, r.MaxSteps)
		} else {
			fmt.Fprintf(&b, " step %d", r.Step)
		}
	}
	if r.TokensOut > 0 {
		fmt.Fprintf(&b, " %d tokens", r.TokensOut)
	}
	fmt.Fprintf(&b, " %s in", r.Age(now).Round(time.Second))
	if r.Origin != "" {
		fmt.Fprintf(&b, " (from %s)", r.Origin)
	}
	if r.Goal != "" {
		fmt.Fprintf(&b, " %q", r.Goal)
	}
	return b.String()
}

// Registry is the directory of live run records.
type Registry struct {
	dir       string
	now       func() time.Time
	pidAlive  func(int) bool
	procStart func(int) (int64, bool)
}

// DirBeside is the registry directory for a lease directory: `<root>/gpu/activity`
// next to `<root>/gpu/lease`. Deriving it from the lease directory — not from a
// second resolution of the state root — is what keeps the two from ever
// pointing at different roots.
func DirBeside(leaseDir string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(leaseDir)), DirName)
}

// Open resolves the registry the same way every lease consumer resolves the
// lease (gpulease.LeaseDir: gpu_lock_path, then state_dir, then the platform
// default), refusing the same cloud-synced roots.
func Open(lockOverride, stateDir string) (*Registry, error) {
	leaseDir, err := gpulease.LeaseDir(lockOverride, stateDir)
	if err != nil {
		return nil, err
	}
	return OpenAt(DirBeside(leaseDir)), nil
}

// OpenAt binds a registry to an explicit directory (tests; readers handed the
// lease directory by another resolver).
func OpenAt(dir string) *Registry {
	return &Registry{dir: dir, now: time.Now, pidAlive: gpulease.PIDAlive, procStart: gpulease.ProcessStart}
}

// Dir is the registry directory.
func (r *Registry) Dir() string { return r.dir }

// Handle is one live registration. Every method is safe on a nil *Handle, so a
// launcher that could not open the registry keeps running unchanged.
type Handle struct {
	reg  *Registry
	path string
	mu   sync.Mutex
	run  Run
	stop chan struct{}
	once sync.Once
}

// Begin registers a run and starts its heartbeat. The record carries this
// process's pid and start time; Seat, Kind, Origin, Goal, MaxSteps and Phase
// come from the caller.
func (r *Registry) Begin(run Run) (*Handle, error) {
	if strings.TrimSpace(run.Seat) == "" {
		return nil, errors.New("gpuactivity: a run must name its seat")
	}
	if err := os.MkdirAll(r.dir, 0o777); err != nil {
		return nil, fmt.Errorf("gpuactivity: %w", err)
	}
	now := r.now()
	run.PID = os.Getpid()
	run.StartTimeMs, _ = r.procStart(run.PID)
	if run.StartedAtMs == 0 {
		run.StartedAtMs = now.UnixMilli()
	}
	run.HeartbeatMs = now.UnixMilli()
	run.Goal = clip(strings.Join(strings.Fields(run.Goal), " "), goalClip)
	if run.Phase == "" {
		run.Phase = "running"
	}
	run.ID = fmt.Sprintf("%d-%d", run.PID, now.UnixNano())
	h := &Handle{reg: r, path: filepath.Join(r.dir, filePrefix+run.ID+fileSuffix), run: run, stop: make(chan struct{})}
	if err := h.write(); err != nil {
		return nil, err
	}
	go h.beat()
	return h, nil
}

// Start is Begin for launchers: it resolves the registry from the box config's
// two lease fields, logs once when it cannot, and never fails the run — a nil
// handle is a no-op.
func Start(lockOverride, stateDir string, run Run) *Handle {
	reg, err := Open(lockOverride, stateDir)
	if err != nil {
		log.Printf("gpuactivity: run not registered: %v", err)
		return nil
	}
	h, err := reg.Begin(run)
	if err != nil {
		log.Printf("gpuactivity: run not registered: %v", err)
		return nil
	}
	return h
}

// Run is a copy of the current record.
func (h *Handle) Run() Run {
	if h == nil {
		return Run{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.run
}

// Update mutates the record under the lock and writes it.
func (h *Handle) Update(fn func(*Run)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(&h.run)
	h.run.HeartbeatMs = h.reg.now().UnixMilli()
	_ = h.write()
}

// Phase records where the run is.
func (h *Handle) Phase(p string) { h.Update(func(r *Run) { r.Phase = p }) }

// OnStep implements the agent loop's RunObserver: the step just completed and
// the tokens the seat has generated for this run so far.
func (h *Handle) OnStep(step, tokensOut int) {
	h.Update(func(r *Run) {
		r.Step = step
		r.TokensOut = tokensOut
		// The pre-run phases heal on the first step: a run that is generating
		// is not still being admitted, cold-loaded or coherence-probed. The
		// probe phase is on this list because only ONE of the two agent doors
		// resets it explicitly, and a run advertised as probing for its whole
		// life misreads a multi-minute run in `gpu status` / offload_status
		// (reviewer finding, D-118).
		if r.Phase == "admission" || r.Phase == "cold-load" || r.Phase == "coherence-probe" {
			r.Phase = "running"
		}
	})
}

// OnPhase implements the agent loop's RunObserver.
func (h *Handle) OnPhase(p string) { h.Phase(p) }

// End removes the record and stops the heartbeat. Idempotent.
func (h *Handle) End() {
	if h == nil {
		return
	}
	h.once.Do(func() {
		close(h.stop)
		h.mu.Lock()
		defer h.mu.Unlock()
		_ = os.Remove(h.path)
		_ = os.Remove(h.path + ".tmp")
	})
}

// write persists the record atomically (tmp + rename), so a reader never sees a
// half-written file and a stale tmp never reads as a run.
//
// On Windows the rename fails with a sharing violation while a reader (a
// drain, a status call) has the record open for the microseconds of its
// ReadFile — measured in the test suite: one step update in three was lost.
// The rename is retried briefly; if it still cannot land, the record is
// written in place: a reader that catches the torn write skips the record for
// that one tick (unparseable is not "stale" until HeartbeatTTL) and the next
// heartbeat repairs it. Losing a progress update silently would be worse — a
// drain then waits on stale step numbers.
func (h *Handle) write() error {
	b, err := json.Marshal(&h.run)
	if err != nil {
		return fmt.Errorf("gpuactivity: encoding run record: %w", err)
	}
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o666); err != nil {
		return fmt.Errorf("gpuactivity: writing run record: %w", err)
	}
	const attempts, pause = 20, 5 * time.Millisecond
	var rerr error
	for i := 0; i < attempts; i++ {
		if rerr = os.Rename(tmp, h.path); rerr == nil {
			return nil
		}
		time.Sleep(pause)
	}
	_ = os.Remove(tmp)
	if werr := os.WriteFile(h.path, b, 0o666); werr != nil {
		return fmt.Errorf("gpuactivity: publishing run record: %w (in-place fallback: %v)", rerr, werr)
	}
	return nil
}

func (h *Handle) beat() {
	t := time.NewTicker(HeartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			h.mu.Lock()
			select {
			case <-h.stop: // ended while we waited for the lock: never resurrect the file
				h.mu.Unlock()
				return
			default:
			}
			h.run.HeartbeatMs = h.reg.now().UnixMilli()
			_ = h.write()
			h.mu.Unlock()
		}
	}
}

// List returns every LIVE run, oldest first, and sweeps the stale records it
// finds on the way (a dead or recycled pid, or a heartbeat past HeartbeatTTL).
// An unreadable directory is an empty list: the registry is advisory.
func (r *Registry) List(now time.Time) []Run {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil
	}
	var out []Run
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, fileSuffix) {
			continue
		}
		path := filepath.Join(r.dir, name)
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		var run Run
		if json.Unmarshal(b, &run) != nil {
			// Debris only once it is old enough not to be a write in progress.
			if fi, serr := e.Info(); serr == nil && now.Sub(fi.ModTime()) > HeartbeatTTL {
				_ = os.Remove(path)
			}
			continue
		}
		if r.stale(run, now) {
			_ = os.Remove(path)
			continue
		}
		out = append(out, run)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAtMs < out[j].StartedAtMs })
	return out
}

// OnSeat is List filtered to the runs on any of the named seats (id or alias,
// case-insensitive) — the drain asks for the configured name and the roster's
// canonical id together.
func (r *Registry) OnSeat(now time.Time, names ...string) []Run {
	all := r.List(now)
	out := all[:0]
	for _, run := range all {
		for _, n := range names {
			if n != "" && strings.EqualFold(run.Seat, n) {
				out = append(out, run)
				break
			}
		}
	}
	return out
}

// stale applies the lease's rule to a run record: provably gone (dead or
// recycled pid) or silent past the TTL.
func (r *Registry) stale(run Run, now time.Time) bool {
	if run.PID <= 0 || !r.pidAlive(run.PID) {
		return true
	}
	if run.StartTimeMs != 0 {
		if st, ok := r.procStart(run.PID); ok && st != run.StartTimeMs {
			return true
		}
	}
	return now.UnixMilli()-run.HeartbeatMs > HeartbeatTTL.Milliseconds()
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
