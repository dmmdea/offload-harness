// Package displaywatch is the display layer's guard AFTER admission (ADR 0075).
//
// The layer's guards (presence, display_floor) decide once, at the moment of a placement. A twin
// that passed them then sits on the desktop's card until llama-swap's idle ttl (300 s) takes it
// down, however soon the operator is back at the desk or a game takes the card's memory, and a twin
// that ANOTHER llama-swap client loaded (the twins are ordinary llama-swap models once rendered) was
// never asked at all. This package closes that gap from the side that can see it: while a seat of the
// display layer is loaded, it re-asks the layer's own desktop guards (placement.ResidentVerdict) on a
// short period, and when either refuses it unloads the layer's seats through llama-swap's per-model
// route.
//
// What it will and will not do, because a periodic unloader is a new posture for this harness (a
// read never unloads: internal/modelaffinity/seatyield.go):
//
//   - It unloads only models the display layer names (its router's model_map twins, and a seat's
//     own model if one is declared). The pair seat, the memory stack and every other layer's seats
//     are never its to touch, whatever the readings say.
//   - It unloads through the vendored llama-swap client's per-model route and never the total one:
//     a route that is absent is an error, not a licence to unload everything.
//   - It does not drain. The reason it unloads is that the desktop needs the memory now, so a
//     request in flight on the twin dies with llama-swap's 502. A mechanical cascade call answers
//     that as a structured defer and the caller does the task itself; ADR 0066's wait and re-issue
//     is the agent loop's recovery and the display layer declares no agent seat.
//   - It fails loud and recovers nothing: an unload that fails is logged, recorded and retried at
//     the next check, with nothing else unloaded to make room.
//   - Every unreadable input is a refusal, as at admission.
//
// It runs in fleet-serve, the one always-on process of a node, in the console session: the presence
// probe in `auto` mode reads the console session's idle time, which a process in another session
// cannot see. Its last action and a heartbeat go to a small state file under the machine-wide state
// root, because offload_status answers from another process (StatusView reads it).
package displaywatch

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"llamaswap-pp-cli/pkg/llamaswap"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/displaystate"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/placement"
	"github.com/dmmdea/offload-harness/internal/seatload"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

const (
	// heartbeatEvery and checkBudget live with the state file's reader (internal/displaystate), which
	// judges a heartbeat stale by them; the watcher writes at that cadence and checks within that budget.
	heartbeatEvery = displaystate.HeartbeatEvery
	checkBudget    = displaystate.CheckBudget

	// quietFor is how long one kind of recurring problem (an unreadable /running, a state file
	// that cannot be written) stays at one log line.
	quietFor = 5 * time.Minute

	runningTimeout = 3 * time.Second
	unloadTimeout  = 8 * time.Second

	// maxDeviceAge is how stale the device sample the check reads may be. It is the health
	// payload's own bound (internal/fleetnode maxSnapshotAge): a sampler whose probe keeps failing
	// keeps serving its last good snapshot, and a floor read from an old number is no reading.
	maxDeviceAge = 30 * time.Second
)

// Model is one row of llama-swap's /running.
type Model struct {
	ID    string
	State string
}

// Deps are the reads and writes a check makes, so a test assembles a box. A nil Now or Logf takes the
// production one; the rest are required.
type Deps struct {
	// Running lists what llama-swap has loaded. An error means /running could not be read.
	Running func(ctx context.Context) ([]Model, error)
	// Unload takes one model down through llama-swap's per-model route.
	Unload func(ctx context.Context, model string) error
	// Devices is the latest per-card reading; !ok = none, or too old to trust.
	Devices func() ([]gpuprobe.Device, bool)
	// Presence reads the operator's presence for the config's mode.
	Presence func() placement.Presence
	Now      func() time.Time
	Logf     func(format string, args ...any)
	// StatePath is where the last action and the heartbeat are kept for offload_status and for the
	// admission guard. "" keeps none.
	StatePath string
	// StatePathErr is why StatePath could not be resolved, when it could not. The watcher says so at
	// start and in the status note: with no state file there is no heartbeat, and the display layer's
	// admission (which asks whether a watcher is alive) will refuse for want of one.
	StatePathErr error
}

func (d Deps) withDefaults() Deps {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logf == nil {
		d.Logf = log.Printf
	}
	return d
}

// Production wires the watcher to this box: llama-swap's /running and its per-model unload route, the
// presence probe, and the device sample the caller already holds (fleet-serve's 2 s sampler), so the
// check execs no nvidia-smi of its own.
func Production(cfg config.Config, devices func() ([]gpuprobe.Device, time.Time, bool)) Deps {
	client := &http.Client{Timeout: runningTimeout}
	statePath, statePathErr := StatePath(cfg)
	return Deps{
		Running: func(ctx context.Context) ([]Model, error) {
			rows, err := seatload.Occupants(ctx, client, swapclient.BaseURL(cfg.Endpoint))
			if err != nil {
				return nil, err
			}
			out := make([]Model, 0, len(rows))
			for _, r := range rows {
				out = append(out, Model{ID: r.Model, State: r.State})
			}
			return out, nil
		},
		Unload: func(ctx context.Context, model string) error {
			ls, err := swapclient.New(cfg.Endpoint, unloadTimeout)
			if err != nil {
				return err
			}
			// No drain, and the keep-set refusal stays on: this is the same client the seat-yield path
			// unloads through. The per-model route only; an absent route is an error, never a bulk unload.
			_, err = ls.Unload(ctx, model, &llamaswap.UnloadOpts{})
			return err
		},
		Devices: func() ([]gpuprobe.Device, bool) {
			devs, at, ok := devices()
			if !ok || len(devs) == 0 || time.Since(at) > maxDeviceAge {
				return nil, false
			}
			return devs, true
		},
		Presence: func() placement.Presence {
			return placement.ProbePresence(cfg.PresenceMode(), cfg.OperatorIdle())
		},
		StatePath:    statePath,
		StatePathErr: statePathErr,
	}
}

// layer is one display layer the watcher guards and the models it serves.
type layer struct {
	spec   config.LayerSpec
	models []string
}

// watched lists the display layers a config declares that carry a desktop guard, with their models:
// the layer named display, each seat's own model and each router twin. A layer with no presence or
// display_floor guard declares nothing to hold a loaded seat to; a layer with no models has nothing to
// unload. Dormancy does not matter: a rendered twin is loadable by any client whether or not the
// harness would place on it.
func watched(cfg config.Config) []layer {
	var out []layer
	for _, l := range cfg.Layers {
		if l.Name != placement.LayerDisplay || !hasDesktopGuard(l) {
			continue
		}
		var models []string
		seen := map[string]bool{}
		add := func(m string) {
			if m = strings.TrimSpace(m); m != "" && !seen[strings.ToLower(m)] {
				seen[strings.ToLower(m)] = true
				models = append(models, m)
			}
		}
		for _, s := range l.Seats {
			add(s.Model)
			twins := make([]string, 0, len(s.ModelMap))
			for _, twin := range s.ModelMap {
				twins = append(twins, twin)
			}
			sort.Strings(twins)
			for _, twin := range twins {
				add(twin)
			}
		}
		if len(models) > 0 {
			out = append(out, layer{spec: l, models: models})
		}
	}
	return out
}

func hasDesktopGuard(l config.LayerSpec) bool {
	for _, g := range l.Guards {
		if g == "presence" || g == "display_floor" {
			return true
		}
	}
	return false
}

// Action and State are the state file's shape (internal/displaystate), which the admission guard reads
// as well as offload_status.
type (
	Action = displaystate.Action
	State  = displaystate.State
)

// Outcome is what one check found and did.
type Outcome struct {
	// Loaded are the watched models llama-swap has loaded.
	Loaded []string
	// Reasons is why they are being taken down; empty when every guard holds.
	Reasons  []string
	Unloaded []string
	// Failed lists the unloads that failed, as "model: error".
	Failed []string
	// ReadErr is set when /running could not be read, which unloads nothing.
	ReadErr error
}

// Watcher is the post-admission guard of the display layer.
type Watcher struct {
	cfg      config.Config
	deps     Deps
	layers   []layer
	interval time.Duration

	last      *Action
	lastWrite time.Time
	warned    map[string]time.Time

	// readErr and blindSince are the current unreadable-/running streak (empty/zero while it reads): a
	// watcher that cannot see what is loaded is not watching, and says so in the state file.
	readErr    string
	blindSince time.Time
}

// New builds the watcher for a config, or nil when there is nothing to watch: no display layer with a
// desktop guard, or display_watch_sec negative (the operator's switch).
func New(cfg config.Config, deps Deps) *Watcher {
	interval := cfg.DisplayWatchInterval()
	if interval <= 0 {
		return nil
	}
	layers := watched(cfg)
	if len(layers) == 0 {
		return nil
	}
	w := &Watcher{cfg: cfg, deps: deps.withDefaults(), layers: layers, interval: interval, warned: map[string]time.Time{}}
	if st, ok := readState(w.deps.StatePath); ok {
		w.last = st.LastAction // a restart keeps the last action visible
	}
	return w
}

// Models lists every model the watcher may unload, for the startup banner.
func (w *Watcher) Models() []string {
	var out []string
	for _, l := range w.layers {
		out = append(out, l.models...)
	}
	return out
}

// Interval is the period between checks.
func (w *Watcher) Interval() time.Duration { return w.interval }

// RunEvery checks on the configured period until ctx ends.
func (w *Watcher) RunEvery(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	w.Run(ctx, t.C)
}

// Run checks once at start (a twin loaded before this process came up is no less on the desktop's
// card), then on every tick, until ctx ends.
func (w *Watcher) Run(ctx context.Context, tick <-chan time.Time) {
	w.announce()
	w.checkOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			w.checkOnce(ctx)
		}
	}
}

func (w *Watcher) checkOnce(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, checkBudget)
	defer cancel()
	w.Check(cctx)
}

// announce says once what is armed and how presence reads from this process: an `auto` mode that
// cannot read the desk from here (a process outside the console session) would make every check
// refuse, and the operator should hear that from the log and not from a twin that never stays up.
func (w *Watcher) announce() {
	w.deps.Logf("display-watch: armed every %s over %s — a loaded display twin is unloaded when the layer's presence or display_floor guard refuses; the check keeps no twin warm and unloads nothing else", w.interval, strings.Join(w.Models(), ", "))
	if w.deps.StatePathErr != nil {
		w.deps.Logf("display-watch: WARNING the status file path could not be resolved (%v): no heartbeat will be written, so offload_status shows this watcher as not watching and the display layer's admission refuses until it can be", w.deps.StatePathErr)
	}
	p := w.deps.Presence()
	w.deps.Logf("display-watch: presence reads mode=%s known=%v away=%v (%s)", p.Mode, p.Known, p.Away, p.Note)
	if p.Mode == "auto" && !p.Known && !p.Locked {
		w.deps.Logf("display-watch: WARNING presence cannot be read from this process, so every check will unload a loaded display twin unless the console is locked; run fleet-serve in the console session (%s)", p.Note)
	}
	w.persist(w.deps.Now(), true)
}

// Check runs one check. It reads /running first and touches nothing else while no watched model is
// loaded, so an idle display layer costs one local GET.
func (w *Watcher) Check(ctx context.Context) Outcome {
	now := w.deps.Now()
	rows, err := w.deps.Running(ctx)
	if err != nil {
		w.warnOnce(now, "running", "display-watch: llama-swap /running could not be read (%v): whether a display twin is loaded is unknown, so nothing is unloaded; checking again in %s", err, w.interval)
		// The first failure of a streak is written at once: a status that lags by a heartbeat would show
		// a blind watcher as watching.
		first := w.blindSince.IsZero()
		if first {
			w.blindSince = now
		}
		w.readErr = clipErr(err)
		w.persist(now, first)
		return Outcome{ReadErr: err}
	}
	recovered := !w.blindSince.IsZero()
	if recovered {
		w.deps.Logf("display-watch: llama-swap /running is readable again after %s", now.Sub(w.blindSince).Round(time.Second))
		w.readErr, w.blindSince = "", time.Time{}
	}
	var out Outcome
	pending := w.loadedByLayer(rows)
	if len(pending) == 0 {
		w.persist(now, recovered)
		return out
	}
	live := w.live()
	acted := false
	for _, l := range w.layers {
		mine := pending[l.spec.Name]
		if len(mine) == 0 {
			continue
		}
		out.Loaded = append(out.Loaded, mine...)
		ok, reasons := placement.ResidentVerdict(l.spec, live)
		if ok {
			continue
		}
		acted = true
		out.Reasons = append(out.Reasons, reasons...)
		w.leave(ctx, now, l, mine, reasons, &out)
	}
	w.persist(now, acted || recovered)
	return out
}

// clipErr is an error's first line, bounded: it goes into a state file a status reader prints.
func clipErr(err error) string {
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > 200 {
		s = string(r[:199]) + "..."
	}
	return s
}

// live assembles the readers the guards read through from this box's own readings.
func (w *Watcher) live() placement.Live {
	pres := w.deps.Presence()
	devs, _ := w.deps.Devices() // nil on !ok: the floor then fails closed on an unreadable card
	return placement.LiveFromReadings(devs, nil, &pres)
}

// loadedByLayer maps each watched layer to the models of it that llama-swap holds: any state but one
// that is already leaving. Models compare lower-cased (the harness's own rule); the ID kept is
// llama-swap's, which is what the unload names.
func (w *Watcher) loadedByLayer(rows []Model) map[string][]string {
	byModel := map[string]string{}
	for _, l := range w.layers {
		for _, m := range l.models {
			byModel[strings.ToLower(m)] = l.spec.Name
		}
	}
	out := map[string][]string{}
	for _, r := range rows {
		if leaving(r.State) {
			continue
		}
		if name, ok := byModel[strings.ToLower(strings.TrimSpace(r.ID))]; ok {
			out[name] = append(out[name], r.ID)
		}
	}
	return out
}

// leaving: a llama-swap state that holds nothing a guard needs to protect the desktop from.
func leaving(state string) bool {
	switch strings.ToLower(state) {
	case "stopped", "stopping", "shutdown":
		return true
	}
	return false
}

// leave unloads the layer's loaded models, one by one, and records what happened. A failure is logged
// and kept in the record; the model stays loaded and the next check asks again.
func (w *Watcher) leave(ctx context.Context, now time.Time, l layer, models, reasons []string, out *Outcome) {
	act := &Action{At: now, Layer: l.spec.Name, Models: append([]string(nil), models...), Reasons: reasons}
	var failures []string
	for _, m := range models {
		if err := w.deps.Unload(ctx, m); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", m, err))
			out.Failed = append(out.Failed, fmt.Sprintf("%s: %v", m, err))
			w.deps.Logf("display-watch: FAILED to unload %s (layer %s): %v — it stays on the display card and the next check retries; reason: %s", m, l.spec.Name, err, strings.Join(reasons, "; "))
			continue
		}
		act.Unloaded = append(act.Unloaded, m)
		out.Unloaded = append(out.Unloaded, m)
		w.deps.Logf("display-watch: unloaded %s (layer %s): %s", m, l.spec.Name, strings.Join(reasons, "; "))
	}
	act.Error = strings.Join(failures, "; ")
	w.last = act
}

// warnOnce logs a recurring problem at most once per quietFor.
func (w *Watcher) warnOnce(now time.Time, key, format string, args ...any) {
	if at, ok := w.warned[key]; ok && now.Sub(at) < quietFor {
		return
	}
	w.warned[key] = now
	w.deps.Logf(format, args...)
}

// persist writes the state file when something happened or the heartbeat is due. A write that fails
// is said (once per quiet window) and never stops the check.
func (w *Watcher) persist(now time.Time, force bool) {
	if w.deps.StatePath == "" {
		return
	}
	if !force && !w.lastWrite.IsZero() && now.Sub(w.lastWrite) < heartbeatEvery {
		return
	}
	st := State{CheckedAt: now, IntervalSec: int(w.interval / time.Second), LastAction: w.last, ReadErr: w.readErr}
	if !w.blindSince.IsZero() {
		since := w.blindSince
		st.BlindSince = &since
	}
	if err := writeState(w.deps.StatePath, st); err != nil {
		w.warnOnce(now, "state", "display-watch: the status file %s could not be written (%v): offload_status will not show this watcher's last action", w.deps.StatePath, err)
		return
	}
	w.lastWrite = now
}

// StatePath is where the watcher leaves its state: under the machine-wide state root, beside the GPU
// lease, never inside a cloud-synced folder.
func StatePath(cfg config.Config) (string, error) { return displaystate.StatePath(cfg) }

func readState(path string) (State, bool) { return displaystate.Read(path) }

func writeState(path string, st State) error { return displaystate.Write(path, st) }

// StatusView is the display_guard field of offload_status for this box: whether the watcher is alive,
// and the last thing it did. nil (omitted) when there is nothing to say: no display layer with a
// desktop guard, or one that is dormant and has never acted.
//
// "Alive" is a heartbeat the watcher's process wrote recently. offload_status runs in another process
// from fleet-serve, so a box whose fleet-serve is not running (or was started before the layer was
// seeded) shows watching=false: an awake display layer with nothing re-checking it is the state this
// field exists to make visible.
func StatusView(cfg config.Config, now time.Time) map[string]any {
	layers := watched(cfg)
	if len(layers) == 0 {
		return nil
	}
	awake := false
	for _, l := range layers {
		if !l.spec.Dormant {
			awake = true
		}
	}
	interval := cfg.DisplayWatchInterval()
	var st State
	var have bool
	var pathErr error
	if path, err := StatePath(cfg); err == nil {
		st, have = readState(path)
	} else {
		pathErr = err
	}
	if !awake && !(have && st.LastAction != nil) {
		return nil
	}
	view := map[string]any{"interval_sec": int(interval / time.Second)}
	switch {
	case interval <= 0:
		view["watching"] = false
		view["note"] = "display_watch_sec is negative: the post-admission check is off, so a loaded display twin is held only by the admission guards and llama-swap's 300 s idle ttl"
	case !have && pathErr != nil:
		view["watching"] = false
		view["note"] = fmt.Sprintf("the watcher's status file path cannot be resolved (%v): no heartbeat can be shown or written, so nothing is known to be re-checking a loaded display twin on this box", pathErr)
	case !have:
		view["watching"] = false
		view["note"] = "no heartbeat from fleet-serve: nothing is re-checking a loaded display twin on this box (is fleet-serve running, and was it started after the layers were seeded?)"
	default:
		age := now.Sub(st.CheckedAt)
		view["checked_at"] = st.CheckedAt
		fresh := age <= staleAfter(interval)
		view["watching"] = fresh
		switch {
		case !fresh:
			view["note"] = fmt.Sprintf("the last heartbeat is %s old: fleet-serve has stopped checking, or is not running", age.Round(time.Second))
		case st.ReadErr != "":
			// Alive and blind: it cannot read llama-swap's /running, so it cannot see a twin to unload.
			// A fresh heartbeat does not make that watching.
			view["watching"] = false
			view["read_err"] = st.ReadErr
			note := fmt.Sprintf("fleet-serve is running but cannot read llama-swap /running (%s): a loaded display twin would not be seen, so nothing is checking it", st.ReadErr)
			if st.BlindSince != nil {
				view["blind_since"] = *st.BlindSince
				note += fmt.Sprintf("; blind for %s", now.Sub(*st.BlindSince).Round(time.Second))
			}
			view["note"] = note
		}
	}
	if have && st.LastAction != nil {
		view["last_action"] = st.LastAction
	}
	return view
}

func staleAfter(interval time.Duration) time.Duration { return displaystate.StaleAfter(interval) }
