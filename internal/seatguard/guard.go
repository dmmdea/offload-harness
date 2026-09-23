// Package seatguard keeps a Tier-1 cascade call from EVICTING a loaded vLLM
// seat.
//
// The measured defect (the reference box's llama-swap log): the three-card
// vLLM seat shares the mutually exclusive `interactive` set with every cascade
// rung, so a summarize / classify / extract / triage call that lands on this
// box's llama-swap while the seat is loaded makes llama-swap unload the seat —
// `model=gemma-4-e4b set=interactive … evict=[qwen3.8-27b-vllm-3card]` — and
// the session that was using it pays a 3–5 minute cold load and loses the
// whole prefix cache. The existing busy-aware lane (C-41) never saw it,
// because a loaded seat with nothing in flight is not "busy": llama-swap swaps
// it out at once, which is exactly the harm.
//
// This package answers ONE question per rung, from two readings the harness
// already trusts elsewhere:
//
//   - WHICH models hold the cards: llama-swap's /running, through
//     seatload.Occupants — the same decode behind offload_status's
//     local_agent_seat, `gpu status` and the drain; a declared vLLM seat is one
//     `vllm_seats` names (alias-resolved through the serving config);
//   - WHETHER loading the rung would unload one: llama-swap's own routing,
//     read from the config the box serves (serving_config_path) and solved
//     with llama-swap's semantics (residency.go). Nothing here names a model.
//
// The caller (the pipeline) turns a Protect verdict into a non-evicting
// choice: the rung's cascade lane when one is resident, else the loaded seat
// itself as the rung (D-129 made a vLLM seat an eligible cascade rung).
//
// Failure direction. A reading that cannot be refreshed is UNKNOWN, and
// unknown fails toward the seat, never toward the rung: the last reading that
// saw a seat loaded keeps protecting it for StaleMax, and with no usable
// reading at all the verdict is still Protect — naming no seat, so the caller
// may route the call off this box but has nothing to substitute. An unreadable
// serving config makes co-residency unknown the same way. What the guard never
// does is refuse: every verdict leaves the call a place to run.
package seatguard

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/seatload"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// SnapshotTTL is how long one /running reading (and one look at the serving
// config's stamp) backs every verdict. The lane busy gate's window (5 s): a
// burst of cascade calls costs one loopback read, and a seat that loads or
// leaves is seen within a window.
const SnapshotTTL = 5 * time.Second

// StaleMax is how long the last GOOD reading may stand in for one that cannot
// be taken. It is the seats' idle-unload ttl (300 s, the house rule): a seat
// seen loaded less than that ago has not been seen to leave, and may still be
// holding the cards.
const StaleMax = 5 * time.Minute

// probeTimeout bounds the /running read. It is a loopback probe on the call's
// critical path — the same 3 s the lane busy gate and the gpu-status seat read
// spend — so a wedged llama-swap costs at most this once per window.
const probeTimeout = 3 * time.Second

// warnEvery rate-limits the guard's own "could not read" lines per cause.
const warnEvery = time.Minute

// errNoServingConfig is the co-residency answer on a box whose config names no
// serving_config_path: the rule lives in a file the guard was not told about,
// and guessing a path would read some other config's rule as this box's.
var errNoServingConfig = errors.New("serving_config_path is unset, so llama-swap's routing (matrix sets / groups) cannot be read")

// Verdict is the guard's answer for one model.
type Verdict struct {
	// Protect is true when serving the model on this box's llama-swap would —
	// or, on an unknown reading, might — unload a loaded vLLM seat.
	Protect bool
	// Seat names the protected seat as `vllm_seats` declares it: loaded now,
	// or loaded in the last good reading when Stale. "" when no loaded seat is
	// known (an unknown reading), so there is nothing to substitute.
	Seat string
	// Seats names every loaded seat the verdict protects (Seat is the first).
	Seats []string
	// Stale marks a verdict made without a fresh /running reading.
	Stale bool
	// Reason is the one line the caller logs, in the shape of llama-swap's
	// own matrix log line where the config could be solved.
	Reason string
}

// Guard is one box's seat guard: its llama-swap, its declared vLLM seats and
// its serving config, with one memoised reading of each. Safe for concurrent
// use; a nil *Guard is inert (every verdict is the zero Verdict).
type Guard struct {
	root       string
	seats      []string
	configPath string

	// Seams (production wiring in New).
	now         func() time.Time
	readRunning func(ctx context.Context) ([]seatload.Occupant, error)
	statConfig  func() (os.FileInfo, error)
	readConfig  func() ([]byte, error)

	mu       sync.Mutex
	at       time.Time // when /running was last asked
	ok       bool      // that read succeeded
	probeErr error
	rows     []seatload.Occupant
	goodAt   time.Time // the last SUCCESSFUL read
	good     []seatload.Occupant
	resAt    time.Time // when the config's stamp was last looked at
	resStamp string
	res      Residency
	resErr   error
	warned   map[string]warnMemo
	// alone records the models already reported as appearing in no matrix
	// set (warned once per guard, not per window).
	alone map[string]bool
}

// warnMemo is the last line logged for one cause: the rate limit applies to
// the same text, never to a changed one.
type warnMemo struct {
	at   time.Time
	text string
}

// New builds the guard for cfg, or nil when there is nothing to guard: the
// flag is off (cascade_seat_guard: false), the box declares no vllm_seats, or
// it names no llama-swap endpoint. A nil guard leaves the cascade exactly as
// it was.
func New(cfg config.Config) *Guard {
	if !guardable(cfg) {
		return nil
	}
	// /running hangs off llama-swap's ROOT, and `endpoint` may carry /v1.
	constructed.Add(1)
	root := swapclient.BaseURL(cfg.Endpoint)
	path := strings.TrimSpace(cfg.ServingConfigPath)
	hc := &http.Client{Timeout: probeTimeout}
	return &Guard{
		root:       root,
		seats:      append([]string(nil), cfg.VLLMSeats...),
		configPath: path,
		now:        time.Now,
		readRunning: func(ctx context.Context) ([]seatload.Occupant, error) {
			return seatload.Occupants(ctx, hc, root)
		},
		statConfig: func() (os.FileInfo, error) { return os.Stat(path) },
		readConfig: func() ([]byte, error) { return os.ReadFile(path) },
		warned:     map[string]warnMemo{},
	}
}

// guardable reports whether cfg needs a guard at all: the flag is on, the box
// declares a vLLM seat, and it names a llama-swap endpoint.
func guardable(cfg config.Config) bool {
	return cfg.CascadeSeatGuardOn() && len(cfg.VLLMSeats) > 0 && strings.TrimSpace(cfg.Endpoint) != ""
}

var (
	sharedMu sync.Mutex
	shared   = map[string]*Guard{}
	// constructed counts New's builds, so a test can prove a Shared hit
	// builds nothing.
	constructed atomic.Int64
)

// Shared returns the process-wide guard for cfg's identity (endpoint, seats,
// serving config, flag), building it on first use. Every pipeline in a
// process reads through one guard: the MCP server builds a pipeline per
// contract for the in-loop offload, and each must not pay its own /running
// read per window. nil exactly when New would be.
func Shared(cfg config.Config) *Guard {
	// The cheap predicate, never New: Shared is called once per pipeline (per
	// contract on the MCP server), and building a Guard — with its HTTP client
	// — only to learn whether one is needed threw one away on every hit.
	if !guardable(cfg) {
		return nil
	}
	key := strings.Join([]string{swapclient.BaseURL(cfg.Endpoint), strings.Join(cfg.VLLMSeats, ","), strings.TrimSpace(cfg.ServingConfigPath)}, "|")
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if g, ok := shared[key]; ok {
		return g
	}
	g := New(cfg)
	shared[key] = g
	return g
}

// Check answers whether serving model on this box's llama-swap would unload a
// loaded vLLM seat. It reads at most one /running and one config stamp per
// SnapshotTTL, and never touches a seat or an /upstream path.
func (g *Guard) Check(ctx context.Context, model string) Verdict {
	if g == nil {
		return Verdict{}
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return Verdict{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.refresh(ctx, now)
	res, resErr := g.residency(now)
	if g.ok {
		return g.judge(model, g.rows, res, resErr, "")
	}
	if !g.goodAt.IsZero() && now.Sub(g.goodAt) <= StaleMax && len(g.loadedSeats(g.good, res)) > 0 {
		return g.judge(model, g.good, res, resErr, fmt.Sprintf(" (from a reading %s old: /running could not be refreshed: %v)", now.Sub(g.goodAt).Round(time.Second), g.probeErr))
	}
	return Verdict{Protect: true, Stale: true, Reason: fmt.Sprintf("model=%s: this box's llama-swap could not be read (%v), so whether a vLLM seat holds the cards is unknown; treated as protecting one", model, g.probeErr)}
}

// refresh takes a new /running reading once the last one is SnapshotTTL old.
// The probe runs under the lock (the FleetLaneGates posture): it is bounded by
// probeTimeout and happens once per window, and concurrent callers want that
// one reading rather than their own.
func (g *Guard) refresh(ctx context.Context, now time.Time) {
	if !g.at.IsZero() && now.Sub(g.at) < SnapshotTTL {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	rows, err := g.readRunning(pctx)
	cancel()
	g.at = now
	if err != nil {
		g.ok, g.probeErr, g.rows = false, err, nil
		g.warn(now, "probe", "cascade seat guard: /running of the local llama-swap at %s could not be read; until it can, a cascade rung is treated as possibly evicting a vLLM seat (served off this box when a lane is resident): %v", g.root, err)
		return
	}
	g.ok, g.probeErr, g.rows = true, nil, rows
	g.goodAt, g.good = now, rows
}

// residency returns the serving config's co-residency rule, re-read only when
// the file's stamp (mtime + size) changes and looked at once per window.
func (g *Guard) residency(now time.Time) (Residency, error) {
	if !g.resAt.IsZero() && now.Sub(g.resAt) < SnapshotTTL {
		return g.res, g.resErr
	}
	g.resAt = now
	if g.configPath == "" {
		g.res, g.resErr, g.resStamp = Residency{}, errNoServingConfig, ""
		g.warn(now, "config", "cascade seat guard: %v; while a vLLM seat is loaded every other model is treated as evicting it", errNoServingConfig)
		return g.res, g.resErr
	}
	fi, err := g.statConfig()
	if err != nil {
		g.res, g.resErr, g.resStamp = Residency{}, fmt.Errorf("serving config %s could not be read: %w", g.configPath, err), ""
		g.warn(now, "config", "cascade seat guard: %v; while a vLLM seat is loaded every other model is treated as evicting it", g.resErr)
		return g.res, g.resErr
	}
	stamp := fmt.Sprintf("%d/%d", fi.ModTime().UnixNano(), fi.Size())
	if stamp == g.resStamp {
		return g.res, g.resErr
	}
	g.resStamp = stamp
	b, err := g.readConfig()
	if err == nil {
		g.res, err = ParseResidency(b)
	}
	if err != nil {
		g.res, g.resErr = Residency{}, fmt.Errorf("serving config %s: %w", g.configPath, err)
		g.warn(now, "config", "cascade seat guard: %v; while a vLLM seat is loaded every other model is treated as evicting it", g.resErr)
		return g.res, g.resErr
	}
	g.resErr = nil
	return g.res, nil
}

// seatHit is one declared vLLM seat found holding the cards.
type seatHit struct {
	declared  string // as vllm_seats spells it
	canonical string // as /running lists it
}

// loadedSeats returns the declared vLLM seats the rows show holding (or
// loading onto) the cards. A seat that is stopping is already leaving, so
// protecting it would buy nothing; every other listed state counts, including
// one this build does not know (fail toward the seat).
func (g *Guard) loadedSeats(rows []seatload.Occupant, res Residency) []seatHit {
	var out []seatHit
	for _, row := range rows {
		switch strings.ToLower(strings.TrimSpace(row.State)) {
		case "stopped", "shutdown", "stopping":
			continue
		}
		id := res.Canonical(row.Model)
		for _, s := range g.seats {
			if strings.EqualFold(res.Canonical(s), id) {
				out = append(out, seatHit{declared: strings.TrimSpace(s), canonical: row.Model})
				break
			}
		}
	}
	return out
}

// judge is the verdict over one set of rows. staleNote is "" for a fresh
// reading, else the sentence saying how old the reading is.
func (g *Guard) judge(model string, rows []seatload.Occupant, res Residency, resErr error, staleNote string) Verdict {
	loaded := g.loadedSeats(rows, res)
	if len(loaded) == 0 {
		return Verdict{}
	}
	stale := staleNote != ""
	req := res.Canonical(model)
	for _, s := range loaded {
		if strings.EqualFold(s.canonical, req) || strings.EqualFold(s.declared, model) {
			return Verdict{} // asking for the seat itself evicts nothing
		}
	}
	if resErr != nil {
		declared, canonical := seatNames(loaded)
		return Verdict{Protect: true, Seat: declared[0], Seats: declared, Stale: stale,
			Reason: fmt.Sprintf("model=%s may evict=[%s]: %v, so co-residency is unknown and treated as evicting%s", model, strings.Join(canonical, " "), resErr, staleNote)}
	}
	var running []string
	for _, row := range rows {
		switch strings.ToLower(strings.TrimSpace(row.State)) {
		case "stopped", "shutdown":
			continue
		}
		running = append(running, row.Model)
	}
	ev := res.Evicts(model, running)
	if ev.Alone {
		g.warnAlone(model)
	}
	var hit []seatHit
	for _, s := range loaded {
		for _, e := range ev.Evicted {
			if strings.EqualFold(e, s.canonical) {
				hit = append(hit, s)
				break
			}
		}
	}
	if len(hit) == 0 {
		return Verdict{}
	}
	declared, _ := seatNames(hit)
	return Verdict{Protect: true, Seat: declared[0], Seats: declared, Stale: stale, Reason: ev.Describe(model) + staleNote}
}

// seatNames lists seats as vllm_seats declares them and as /running lists them.
func seatNames(seats []seatHit) (declared, canonical []string) {
	for _, s := range seats {
		declared = append(declared, s.declared)
		canonical = append(canonical, s.canonical)
	}
	return declared, canonical
}

// warnAlone reports, once per model per guard, a model that appears in no
// matrix set of the serving config. llama-swap runs such a model ALONE — it
// evicts everything loaded, the residents included — which for a cascade rung
// is almost always a config gap rather than an intent.
func (g *Guard) warnAlone(model string) {
	if g.alone == nil {
		g.alone = map[string]bool{}
	}
	if g.alone[model] {
		return
	}
	g.alone[model] = true
	log.Printf("cascade seat guard: %s appears in no matrix set of %s, so llama-swap runs it alone and it evicts every loaded model; add it to a set if it should run beside anything", model, g.configPath)
}

// warn logs a guard-side failure at most once per warnEvery per cause AND
// text: a box whose llama-swap stays down says so once a minute, not on every
// cascade call, while a cause whose failure CHANGES (connection refused, then
// a 503) is logged at once — a rate limit keyed on the cause alone hid the
// second fault for a minute. One entry per cause, so a text that keeps
// changing cannot grow the memo.
func (g *Guard) warn(now time.Time, cause, format string, args ...any) {
	if g.warned == nil {
		g.warned = map[string]warnMemo{}
	}
	text := fmt.Sprintf(format, args...)
	if last, ok := g.warned[cause]; ok && last.text == text && now.Sub(last.at) < warnEvery {
		return
	}
	g.warned[cause] = warnMemo{at: now, text: text}
	log.Print(text)
}
