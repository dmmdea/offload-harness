package main

// `gpu reserve --drain [--unload-seat]` and `gpu release --warm-seat`: the
// maintenance half of the lease (0.113.16; rebuilt 0.117.0, register D-93).
//
// Taking a TEXT lease makes this node a non-target — the delegator skips it
// (health "lease"), the node refuses new dispatches (503, re-placeable) — but
// the seat can still be mid-work for what was placed before. --drain waits for
// that work to finish before the holder touches the cards, and "work" is read
// from BOTH sources that own a piece of it:
//
//   - the seat's own counters through llama-swap (internal/seatload): requests
//     running or waiting on the engine, or a load in progress;
//   - the run registry (internal/gpuactivity): the agent runs in flight on this
//     box. A run is a multi-step loop and the engine's gauge reads ZERO between
//     its steps — on 2026-09-14 the drain read that gap as idle, unloaded the
//     27B the instant a first step returned, and the run's second step then sat
//     behind the exclusive fence until its 600 s wall.
//
// Three things changed in 0.117.0, each from that day's log:
//
//  1. The drain's deadline is the QUEUE budget, not a fixed two minutes. The
//     lease was free both times, so `--wait 8h` was over in an instant, and the
//     drain then failed at 2m0s under a single legitimate 27B step (3m27s at
//     the seat's measured 23.6 tok/s). No value of --wait could outlast it. Now
//     --drain-timeout defaults to the rest of --wait (floor drainFloor) — a
//     reservation queues behind in-flight work exactly as it queues behind a
//     holder — and an explicit --drain-timeout still wins.
//  2. The lease is stamped DRAINING during the drain and EXCLUSIVE only after
//     it. --unload-seat implies exclusive, and stamping that at acquire made the
//     admission gate block the very run the drain was waiting for (deadlock by
//     design, resolved only by the run's wall). Draining cordons the seat — no
//     NEW run is admitted (modelaffinity.BlocksNewRun) — while the requests of
//     runs already in flight keep flowing.
//  3. Progress is printed on CHANGE (count, load state, a run's step), with a
//     reminder every five minutes, and every line names what is in flight and
//     the seat's own turn arithmetic — never "1 in flight" sixty-one times.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/seatload"
	"github.com/dmmdea/offload-harness/internal/seatrate"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// drainProbe is what one drain reads and where it reports.
type drainProbe struct {
	client   *http.Client
	endpoint string
	model    string
	// runs lists the registered runs on the named seats (id and alias); nil =
	// the engine's gauge alone, as before the registry existed.
	runs  func(now time.Time, names ...string) []gpuactivity.Run
	every time.Duration
	out   io.Writer
	// hint is the seat's own turn arithmetic, carried on the first progress
	// line and the deadline error so a caller knows what a longer wait buys.
	hint string
	// stuckAfter (register C-50, S-32): how long the seat may show the SAME
	// busy state — same in-flight count, same runs at the same step and phase
	// — before the drain gives up on it as stuck, whatever the overall
	// deadline says. Zero disables it. The overall deadline is the queue
	// budget (hours, ADR 0041) so a legitimate 12-step run is never cut; this
	// is the bound on a run that heartbeats but makes no progress, which the
	// 8 h budget otherwise holds the card for.
	stuckAfter time.Duration
	// skipSeat (plan P5): the seat sits on cards the lease does not hold, so its own gauge is
	// not what the drain waits for: only the runs p.runs returns (the ones on the leased cards).
	skipSeat bool
}

// drainSeat is the gauge-only drain: the historical signature, kept for the
// callers and tests that have no registry.
func drainSeat(ctx context.Context, client *http.Client, endpoint, model string, timeout, every time.Duration, out io.Writer) error {
	return drainUntil(ctx, drainProbe{client: client, endpoint: endpoint, model: model, every: every, out: out}, time.Now().Add(timeout))
}

// reminderEvery bounds how often an UNCHANGED drain state is re-printed.
const reminderEvery = 5 * time.Minute

// drainSpan prints a duration for a drain message: whole seconds, or milliseconds
// below one second, where seconds would read "0s" for a real span (register C-84).
func drainSpan(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(time.Second)
}

// drainUntil polls until the seat is idle on two consecutive reads — no request
// running or waiting on the engine, no load in progress, and no registered run
// on the seat — or the deadline passes. It returns nil when drained; an error
// names what it last saw, the seat's turn arithmetic, and how to wait longer.
func drainUntil(ctx context.Context, p drainProbe, deadline time.Time) error {
	start := time.Now()
	timeout := deadline.Sub(start).Round(time.Second)
	zeros := 0
	last, printed := "", ""
	var lastPrint time.Time
	// Progress tracking for stuckAfter: the busy-state key last seen and when
	// it last changed.
	stuckKey, stuckSince := "", start
	for {
		now := time.Now()
		var rd seatload.Reading
		var err error
		if !p.skipSeat {
			rd, err = seatload.Inflight(ctx, p.client, p.endpoint, p.model)
		}
		var runs []gpuactivity.Run
		if p.runs != nil {
			runs = p.runs(now, p.model, rd.Canonical)
		}
		key := ""
		switch {
		case err != nil:
			last, key = err.Error(), "err:"+err.Error()
			zeros = 0
		case !rd.Loaded && rd.Ambiguous:
			// The bare name is not in /running, the roster could not say what
			// id the seat is listed under, and /running is not empty: the seat
			// may be one of those entries mid-request. "Could not tell" is not
			// "idle" — keep polling, and name the cause at the deadline.
			last = fmt.Sprintf("cannot tell whether %s is loaded: roster unreadable (%v) and /running lists %d other model(s)", p.model, rd.RosterErr, rd.RunningOthers)
			key = "ambiguous"
			zeros = 0
		case rd.Stopping:
			// Leaving, not loading (its ttl ran out): no request waits on it, but
			// the drain still lets the unload finish before it counts idle zeros.
			last = "seat stopping (an unload is in progress; waiting for it to finish)" + runsClause(runs, now)
			key = "stopping:" + runsKey(runs)
			zeros = 0
		case rd.Starting:
			// A load in progress IS work in flight: the request that triggered
			// it is waiting on the engine. seatload never touched the upstream
			// (llama-swap would hold that read for the whole load), so this
			// costs one small GET per tick (register D-92).
			last = fmt.Sprintf("seat %s (a load is in progress; waiting for it to become ready)", strings.TrimPrefix(rd.Source, "running-state:")) + runsClause(runs, now)
			key = "starting:" + runsKey(runs)
			zeros = 0
		case (rd.Loaded && rd.Inflight > 0) || len(runs) > 0:
			last = fmt.Sprintf("%d in flight", rd.Inflight) + runsClause(runs, now)
			key = fmt.Sprintf("busy:%d:%s", rd.Inflight, runsKey(runs))
			zeros = 0
		case !rd.Loaded:
			return nil // not loaded and no run registered: nothing to drain
		default:
			zeros++
			if zeros >= 2 {
				return nil
			}
			last, key = "0 in flight (confirming)", "confirming"
		}
		if p.out != nil && key != "" && key != "confirming" {
			changed := key != printed
			if changed || now.Sub(lastPrint) >= reminderEvery {
				line := fmt.Sprintf("gpu reserve: draining %s: %s", p.model, last)
				if changed && p.hint != "" {
					line += " — " + p.hint
				}
				if !changed {
					line += fmt.Sprintf(" (still waiting after %s; deadline in %s)", now.Sub(start).Round(time.Second), time.Until(deadline).Round(time.Second))
				}
				fmt.Fprintln(p.out, line)
				printed, lastPrint = key, now
			}
		}
		if p.stuckAfter > 0 && key != "" && key != "confirming" {
			if key != stuckKey {
				stuckKey, stuckSince = key, now
			} else if now.Sub(stuckSince) >= p.stuckAfter {
				msg := fmt.Sprintf("drain of %s gave up after %s without progress (last: %s): the seat's state has not changed for %s, longer than the seat's own turn arithmetic allows", p.model, drainSpan(now.Sub(start)), last, drainSpan(now.Sub(stuckSince)))
				if p.hint != "" {
					msg += "; " + p.hint
				}
				msg += "; a run that heartbeats but makes no progress does not hold the card for the whole --wait; pass --drain-timeout to wait a fixed time instead; work in flight was not interrupted"
				return errors.New(msg)
			}
		}
		if !now.Before(deadline) {
			msg := fmt.Sprintf("drain of %s did not finish within %s (last: %s)", p.model, timeout, last)
			if p.hint != "" {
				msg += "; " + p.hint
			}
			msg += "; the drain is bounded by the queue budget (--wait) unless --drain-timeout is given — pass a longer one to keep waiting; work in flight was not interrupted"
			return errors.New(msg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.every):
		}
	}
}

// runsClause renders the registered runs for a progress line.
func runsClause(runs []gpuactivity.Run, now time.Time) string {
	if len(runs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(runs))
	for _, r := range runs {
		parts = append(parts, r.Summary(now))
	}
	return fmt.Sprintf("; %d run(s) registered on the seat: %s", len(runs), strings.Join(parts, " | "))
}

// runsKey is the part of the runs' state whose change is worth a new line: the
// set of runs and each one's step and phase — not its age, which changes every tick.
func runsKey(runs []gpuactivity.Run) string {
	parts := make([]string, 0, len(runs))
	for _, r := range runs {
		parts = append(parts, fmt.Sprintf("%d:%d:%s", r.PID, r.Step, r.Phase))
	}
	return strings.Join(parts, ",")
}

// seatTurn is the seat's own arithmetic for one completion, from the store the
// agent runs write (internal/seatrate): the seconds one more step costs at the
// measured rate, the cold load on top when the seat is loading, and the
// completion size the estimate assumes. ok is false when the seat has no
// sample yet.
func seatTurn(cfg config.Config, seat string) (turnSec, coldSec float64, final int, toks float64, ok bool) {
	root, err := gpulease.ResolveStateRoot(cfg.StateDir)
	if err != nil {
		return 0, 0, 0, 0, false
	}
	st, lerr := seatrate.Load(seatrate.Path(root))
	if lerr != nil || st == nil {
		return 0, 0, 0, 0, false
	}
	s := st.Get(seat)
	if s.TokS <= 0 {
		return 0, 0, 0, 0, false
	}
	final = cfg.AgentMaxTokens * 4
	if final < 1024 {
		final = 1024
	}
	if final > 8192 {
		final = 8192
	}
	return float64(final) / s.TokS, s.ColdLoadSec, final, s.TokS, true
}

// seatTurnHint renders seatTurn for a progress line: what one more step costs
// and so what a longer wait buys. Empty when the seat has no sample yet.
func seatTurnHint(cfg config.Config, seat string) string {
	turn, cold, final, toks, ok := seatTurn(cfg, seat)
	if !ok {
		return ""
	}
	hint := fmt.Sprintf("one seat turn is ~%.0f s at the seat's measured %.1f tok/s (a %d-token completion)", turn, toks, final)
	if cold > 0 {
		hint += fmt.Sprintf(", plus ~%.0f s if it is loading", cold)
	}
	return hint
}

// stuckMultiple is how many seat turns of an UNCHANGED busy state the drain
// tolerates before calling the run stuck: a turn is the longest legitimate
// silence (one completion at the seat's rate); two of them with no step
// advance, no in-flight change and no load finishing is a run that heartbeats
// without progressing.
const stuckMultiple = 2

// seatStuckAfter derives the drain's no-progress bound from the seat's own
// turn arithmetic (register C-50): stuckMultiple turns plus the cold load,
// never under drainFloor. Zero (disabled) when the seat has no rate sample —
// the drain then has only the overall deadline, as before.
func seatStuckAfter(cfg config.Config, seat string) time.Duration {
	turn, cold, _, _, ok := seatTurn(cfg, seat)
	if !ok {
		return 0
	}
	d := time.Duration((stuckMultiple*turn + cold) * float64(time.Second))
	if d < drainFloor {
		d = drainFloor
	}
	return d
}

// unloadSeat frees the seat through llama-swap: the current API first, the
// legacy GET as a fallback for an older llama-swap. That GET is TOTAL (llama-swap
// ignores ?model=; see tools/llamaswap's unload-all), so it is refused while any
// model in protect (the resident mem0 stack) would go down with the seat; the
// caller then fails loud instead (register C-87).
func unloadSeat(ctx context.Context, client *http.Client, endpoint, model string, protect []string) error {
	base := strings.TrimRight(endpoint, "/")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/models/unload/"+url.PathEscape(model), nil)
	resp, err := client.Do(req)
	why := ""
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode < 300 {
			return nil
		}
		why = fmt.Sprintf("status %d", resp.StatusCode)
	} else {
		why = err.Error()
	}
	if len(protect) > 0 {
		return fmt.Errorf("unload %s: the per-model route failed (%s), and the only other route, GET /unload, unloads everything: refused while the memory stack or a seat that must stay (%s) may be resident", model, why, strings.Join(protect, ", "))
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, base+"/unload?model="+url.QueryEscape(model), nil)
	resp, err = client.Do(req)
	if err != nil {
		return fmt.Errorf("unload %s: %w", model, err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("unload %s: status %d", model, resp.StatusCode)
	}
	return nil
}

// warmSeat loads the seat back through llama-swap's on-demand path (/upstream)
// by asking the upstream for its health; llama-swap loads the model to answer.
// The client's timeout bounds the load (a 27B tp2 seat takes ~3 min).
//
// This is the lease HOLDER's own load — the warm-back the reservation was taken
// to schedule — so it builds its URL with modelaffinity.HolderUpstreamURL, the
// one unfenced builder: this process holds the lease the fence would make it
// wait on (only its child carries GPU_LEASE_EPOCH).
func warmSeat(ctx context.Context, client *http.Client, endpoint, model string) error {
	wu, uerr := modelaffinity.HolderUpstreamURL(endpoint, model, "/health")
	if uerr != nil {
		return fmt.Errorf("warm %s: %w", model, uerr)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, wu, nil)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("warm %s: %w", model, err)
	}
	resp.Body.Close()
	if resp.StatusCode < 500 {
		return nil
	}
	// A 5xx is llama-swap giving up on the health wait, not the load failing:
	// the 3-card seat's cold load outlasts its healthCheckTimeout, and on
	// 2026-09-18 19:5x the wrapper reported "status 500" while the engine kept
	// loading and came up minutes later, untracked by the lease that owed the
	// warm. Watch the seat's own state instead of trusting the status: a load
	// in progress is waited out, a ready seat is a warm that succeeded, a seat
	// that never started is the failure.
	deadline := time.Now().Add(warmWatch)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	for {
		// State only (/running): the watch needs loaded/starting, never the
		// gauge, so it reads nothing at the seat.
		rd, rerr := seatload.Running(ctx, client, endpoint, model)
		switch {
		case rerr == nil && rd.Loaded && !rd.Starting:
			return nil
		case rerr == nil && !rd.Loaded && !rd.Starting && !rd.Ambiguous:
			return fmt.Errorf("warm %s: status %d and the seat is not loading", model, resp.StatusCode)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("warm %s: status %d and the seat did not become ready within %s", model, resp.StatusCode, warmWatch)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("warm %s: %w", model, ctx.Err())
		case <-time.After(warmWatchEvery):
		}
	}
}

// warmWatch bounds how long a warm-back watches a load that outlasted
// llama-swap's own health wait; warmWatchEvery is the poll cadence. Variables
// so a test can shorten them.
var (
	warmWatch      = 15 * time.Minute
	warmWatchEvery = 5 * time.Second
)

// seatTarget resolves the seat the maintenance verbs act on: the config's
// llama-swap endpoint and agent seat alias.
func seatTarget(cfg config.Config) (endpoint, model string, err error) {
	endpoint = strings.TrimSpace(cfg.Endpoint)
	model = strings.TrimSpace(cfg.AgentPlannerModel(""))
	if endpoint == "" || model == "" {
		return "", "", errors.New("drain/unload need the config's endpoint and agent seat (agent_model)")
	}
	return endpoint, model, nil
}

// maintenanceClient is the HTTP client the drain/unload/warm verbs share:
// loopback llama-swap, generous timeout so a warm-back (a model load) fits.
var maintenanceClient = &http.Client{Timeout: 15 * time.Minute}

// restamper rewrites the holder's own lease record (Lease.Restamp for the
// wrapper form; Manager.Restamp by epoch for the detached child's lease).
type restamper func(fn func(*gpulease.Meta)) error

// leaseScope says which seats sit on the cards a lease holds (plan P5, register C-86): `--drain`
// and `--unload-seat` clear THOSE cards, not the node. A whole-node lease (no ids) reaches every
// seat. A seat whose pin is unknown, cannot be placed on a card, or is read while the card
// table cannot be, is on the leased cards: the direction of every doubt is today's behaviour.
type leaseScope struct {
	cfg     config.Config
	ids     []string
	cards   []gpuprobe.Card
	cardsOK bool
}

func newLeaseScope(ctx context.Context, cfg config.Config, ids []string) leaseScope {
	s := leaseScope{cfg: cfg, ids: ids}
	if len(ids) == 0 {
		return s
	}
	cards, _, err := cardTable(ctx, cfg)
	s.cards, s.cardsOK = cards, err == nil && len(cards) > 0
	if !s.cardsOK {
		// The fallback is right (every doubt fences) and silent, until now: the operator would
		// believe a seat on another card was spared.
		why := "no card was listed"
		if err != nil {
			why = err.Error()
		}
		fmt.Fprintf(os.Stderr, "gpu reserve: the card table could not be read (%s): every seat is treated as sitting on the leased cards, so the drain and unload act as for the whole node\n", why)
	}
	return s
}

func (s leaseScope) bounded() bool { return len(s.ids) > 0 }

func (s leaseScope) touchesPins(pins []string) bool {
	if !s.bounded() || !s.cardsOK {
		return true
	}
	rids, ok := gpulease.ResolvePins(pins, s.cards)
	if !ok {
		return true
	}
	return gpulease.Info{Held: true, Devices: s.ids}.Touches(rids)
}

// touches reports whether the seat serving model sits on the leased cards.
func (s leaseScope) touches(model string) bool {
	if !s.bounded() {
		return true
	}
	pins, ok := s.cfg.ModelPins(model)
	if !ok {
		return true
	}
	return s.touchesPins(pins)
}

// touchesRun reports whether a registered run is on the leased cards: by the pins it recorded,
// else by the seat it runs on.
func (s leaseScope) touchesRun(r gpuactivity.Run) bool {
	if len(r.Devices) > 0 {
		return s.touchesPins(r.Devices)
	}
	return s.touches(r.Seat)
}

// split divides models into those on the leased cards and those that are not.
func (s leaseScope) split(models []string) (on, off []string) {
	for _, m := range models {
		if s.touches(m) {
			on = append(on, m)
		} else {
			off = append(off, m)
		}
	}
	return on, off
}

// unloadModelsFor is the model-id list the render lane's freeLlamaSwap unloads under a lease
// that holds leaseIDs: the llama-swap roster, minus the memory stack, minus every seat pinned to
// cards the lease does not hold. A model nobody declared a pin for stays on the list (it could
// be anywhere). ok is false for a whole-node lease (nothing to narrow: the render lane keeps its
// own rule) and when the roster is empty.
func unloadModelsFor(ctx context.Context, cfg config.Config, roster []string, leaseIDs []string) ([]string, bool) {
	if len(leaseIDs) == 0 || len(roster) == 0 {
		return nil, false
	}
	scope := newLeaseScope(ctx, cfg, leaseIDs)
	keep := map[string]bool{}
	for _, m := range effectiveMemoryStack(cfg) {
		keep[strings.ToLower(strings.TrimSpace(m))] = true
	}
	var out []string
	for _, id := range roster {
		if keep[strings.ToLower(strings.TrimSpace(id))] {
			continue
		}
		if scope.touches(id) {
			out = append(out, id)
		}
	}
	return out, true
}

// unloadModelsEnv renders the list as the environment variable the render lane reads
// (GPU_LEASE_UNLOAD_MODELS): a comma list, `-` for "unload nothing", and no variable at all
// when the list is unknown, which leaves the lane on its own rule.
func unloadModelsEnv(models []string, known bool) string {
	if !known {
		return ""
	}
	if len(models) == 0 {
		return "GPU_LEASE_UNLOAD_MODELS=-"
	}
	return "GPU_LEASE_UNLOAD_MODELS=" + strings.Join(models, ",")
}

// leaseDevicesOf is the cards the live lease with this epoch holds (lease ids), nil when the epoch
// is not live or holds the whole node.
func leaseDevicesOf(m *gpulease.Manager, epoch uint64) []string {
	for _, l := range m.Leases() {
		if l.Epoch == epoch {
			return l.Devices
		}
	}
	fmt.Fprintf(os.Stderr, "gpu hold: lease epoch %d is not live in this lease directory, so its cards cannot be read: the drain and unload act as for the whole node\n", epoch)
	return nil
}

// detachMaintain is the detached holder's maintain step (`gpu reserve --detach --drain
// --unload-seat`): the drain and unload of the lease the hidden child holds, scoped to that
// lease's cards by epoch.
func detachMaintain(m *gpulease.Manager, epoch uint64, cfg config.Config, drain bool, deadline time.Time, unload, exclusive bool) error {
	return maintainSeatScoped(context.Background(), cfg, func(fn func(*gpulease.Meta)) error { return m.Restamp(epoch, fn) },
		drain, deadline, unload, exclusive, markWarmOwed(m), leaseDevicesOf(m, epoch))
}

// rosterIDsFn reads the llama-swap roster ids; a variable so a test needs no llama-swap.
var rosterIDsFn = func(ctx context.Context, endpoint string) ([]string, error) {
	r, err := swapclient.FetchRoster(ctx, endpoint, 5*time.Second)
	if err != nil {
		return nil, err
	}
	return r.IDs(), nil
}

// wrapperUnloadEnv is the GPU_LEASE_UNLOAD_MODELS entry a device lease's wrapped command is
// handed (plan P5): the render lane's freeLlamaSwap unloads exactly that list instead of every
// model off the memory stack. "" for a whole-node lease, and whenever the roster cannot be read
// (the render lane then keeps its own rule, which only ever unloads more, never less).
func wrapperUnloadEnv(ctx context.Context, cfg config.Config, leaseIDs []string) string {
	if len(leaseIDs) == 0 || strings.TrimSpace(cfg.Endpoint) == "" {
		return ""
	}
	roster, err := rosterIDsFn(ctx, cfg.Endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gpu reserve: the llama-swap roster could not be read (%v): the render lane keeps its own rule and unloads every model off the memory stack, not only the seats on the leased cards\n", err)
		return ""
	}
	models, known := unloadModelsFor(ctx, cfg, roster, leaseIDs)
	return unloadModelsEnv(models, known)
}

// maintainSeat is maintainSeatCtx for a caller that nothing cancels.
func maintainSeat(cfg config.Config, restamp restamper, drain bool, deadline time.Time, unload, exclusive bool, owed func(seat string)) error {
	return maintainSeatCtx(context.Background(), cfg, restamp, drain, deadline, unload, exclusive, owed)
}

// maintainSeatCtx runs the --drain / --unload-seat steps against the config's
// seat: drain until deadline, then turn the DRAINING stamp into EXCLUSIVE when
// the window asked for it (or just clear it), then unload. Errors are returned
// as-is; the caller decides what to do with the lease. owed, when non-nil, is
// told the seat's name after a successful unload: the warm-owed marker the
// LAST releasing holder pays (register D-124).
//
// ctx ends the drain and the unload early. The wrapper form cancels it the
// moment its lease is lost (register C-59): a drain that goes on waiting for a
// card another holder has is the failure arriving hours late. The restamp
// comes BEFORE the unload, so a lease that is already gone never gets as far
// as clearing the seat under whoever holds the card now.
func maintainSeatCtx(ctx context.Context, cfg config.Config, restamp restamper, drain bool, deadline time.Time, unload, exclusive bool, owed func(seat string)) error {
	return maintainSeatScoped(ctx, cfg, restamp, drain, deadline, unload, exclusive, owed, nil)
}

// maintainSeatScoped is maintainSeatCtx for a lease that holds the cards in devices (lease ids):
// the drain waits only for runs on those cards, and the unload takes only the seats that sit on
// them (plan P5). nil devices is the whole node, exactly as before.
func maintainSeatScoped(ctx context.Context, cfg config.Config, restamp restamper, drain bool, deadline time.Time, unload, exclusive bool, owed func(seat string), devices []string) error {
	endpoint, model, err := seatTarget(cfg)
	if err != nil {
		return err
	}
	scope := newLeaseScope(ctx, cfg, devices)
	agentOn := scope.touches(model)
	if scope.bounded() && !agentOn {
		fmt.Fprintf(os.Stderr, "gpu reserve: %s sits on cards this lease does not hold; it is neither drained nor unloaded\n", model)
	}
	if drain {
		p := drainProbe{client: maintenanceClient, endpoint: endpoint, model: model, every: 2 * time.Second, out: os.Stderr, hint: seatTurnHint(cfg, model), stuckAfter: seatStuckAfter(cfg, model), skipSeat: !agentOn}
		if reg, rerr := gpuactivity.Open(cfg.GPULockPath, cfg.StateDir); rerr == nil {
			p.runs = reg.OnSeat
			if scope.bounded() {
				// The runs that matter are the ones on the leased cards, whatever seat they run on.
				p.runs = func(now time.Time, _ ...string) []gpuactivity.Run { return reg.Where(now, scope.touchesRun) }
			}
		} else {
			fmt.Fprintf(os.Stderr, "gpu reserve: run registry unavailable (%v): draining on the seat's gauge alone\n", rerr)
		}
		if err := drainUntil(ctx, p, deadline); err != nil {
			// A failed drain must not leave the DRAINING stamp on a lease that
			// stays held (the detach form keeps it): the stamp cordons the seat
			// — no new run admitted — for the rest of the window, which turned
			// one failed drain into a box refused for 8 h (register C-50,
			// S-31). The lease stays held and non-exclusive; the caller says
			// what to do with it. A drain the caller cancelled (the lease is
			// gone) has no stamp left to clear: the attempt could only fail.
			if restamp != nil && ctx.Err() == nil {
				if serr := restamp(func(m *gpulease.Meta) { m.Draining = false }); serr != nil {
					fmt.Fprintf(os.Stderr, "gpu reserve: could not clear the draining stamp after the failed drain: %v\n", serr)
				}
			}
			return err
		}
		if agentOn {
			fmt.Fprintf(os.Stderr, "gpu reserve: %s drained (no request in flight, no run registered)\n", model)
		} else {
			fmt.Fprintf(os.Stderr, "gpu reserve: the leased cards are drained (no run registered on them)\n")
		}
	}
	if restamp != nil && (drain || exclusive) {
		if err := restamp(func(m *gpulease.Meta) {
			m.Draining = false
			if exclusive {
				m.Exclusive = true
			}
		}); err != nil {
			return fmt.Errorf("stamping the lease after the drain: %w", err)
		}
		if exclusive {
			fmt.Fprintf(os.Stderr, "gpu reserve: lease stamped exclusive (text loads wait or route elsewhere for the window)\n")
		}
	}
	if unload {
		// Read the seat BEFORE unloading: only a seat that was resident is owed a
		// warm-back. Marking it owed unconditionally made a lease over a cold
		// seat LOAD it at release — on 2026-09-23 <node-b>'s 3-card 27B came up
		// on all three cards after a video render, with nothing asking for it,
		// and sat there until its ttl. A reading that fails or is ambiguous keeps
		// the old behaviour (owed): "could not tell" must not cost a warm seat.
		wasLoaded := agentOn && seatWasResident(ctx, endpoint, model)
		// OTHER RESIDENTS, read before either unload so the record reflects
		// what was ACTUALLY there (register D-1xx-3, 2026-09-23; R2/R3
		// measured on <node-e>): `--unload-seat` cleared only the
		// configured agent seat, so a DIFFERENT client's own load — the
		// vision seat `qwen3.5-9b-vl`, loaded by another session — stayed
		// resident through an entire media lease on an 8 GB card and only
		// aged out at its own ttl. gpulease has no per-card model (one lease
		// fences the whole box — see waiters.go/gpulease.go), so on a
		// single-card box "the cards the lease fences" is every card, and
		// every OTHER model llama-swap currently holds is unloaded too, not
		// only the agent seat.
		stack := effectiveMemoryStack(cfg)
		others, kept, readable := otherResidentModels(ctx, endpoint, model, stack)
		if len(kept) > 0 {
			fmt.Fprintf(os.Stderr, "gpu reserve: kept the memory stack resident (mem0 never yields to a lease): %s\n", strings.Join(kept, ", "))
		}
		// A lease that holds some cards takes the seats on those cards and leaves the rest
		// resident (plan P5). A seat that stays is also protected from the total unload route:
		// GET /unload ignores ?model=, so it would take the card-0 seat down with the card-2 one.
		others, leftAlone := scope.split(others)
		if len(leftAlone) > 0 {
			fmt.Fprintf(os.Stderr, "gpu reserve: left resident (not on the leased cards): %s\n", strings.Join(leftAlone, ", "))
		}
		// The legacy GET /unload is total, so it may run only when no stack member
		// is resident; an unreadable /running cannot show that, so it protects the
		// whole configured stack.
		protect := append(append([]string(nil), kept...), leftAlone...)
		if !readable {
			protect = stack
		}
		if agentOn {
			if err := unloadSeat(ctx, maintenanceClient, endpoint, model, protect); err != nil {
				return err
			}
		}
		unloadedOthers := unloadOthers(ctx, endpoint, others, protect)
		if len(unloadedOthers) > 0 {
			fmt.Fprintf(os.Stderr, "gpu reserve: also unloaded %d other resident model(s) so the leased cards are actually clear: %s\n",
				len(unloadedOthers), strings.Join(unloadedOthers, ", "))
		}
		if !wasLoaded {
			// A seat that sits on other cards was never this lease's to unload, so it is owed no
			// warm-back and there is nothing to say about it here (it was announced above).
			if agentOn {
				fmt.Fprintf(os.Stderr, "gpu reserve: %s was not loaded; nothing to warm back after the window\n", model)
			}
			return nil
		}
		if owed != nil {
			owed(model)
		}
		fmt.Fprintf(os.Stderr, "gpu reserve: %s unloaded\n", model)
	}
	return nil
}

// effectiveMemoryStack is the set every path that clears or reclaims the cards keeps
// resident: the configured memory_stack, or the default when it is empty. It is one
// reading shared by `gpu reserve --unload-seat` and fleet reclaim (register C-94),
// and it matches the render side: the pipeline exports MEMORY_STACK only when the
// list is non-empty, so render/gpu-lock.mjs then keeps its own default.
func effectiveMemoryStack(cfg config.Config) []string {
	if len(cfg.MemoryStack) == 0 {
		return config.Default().MemoryStack
	}
	return cfg.MemoryStack
}

// otherResidentModels lists every model llama-swap's /running reports besides
// the agent seat (skipping states already leaving/gone: stopped, shutdown),
// read ONCE before anything is unloaded. The config's memory stack is never in
// the list: it is returned as kept instead (register C-87, 2026-10-01: a media
// lease unloaded the reference box's mem0 embedder from the utility card for nothing the
// render on another card could use; the operator's rule is that mem0 never
// yields, and render/gpu-lock.mjs always kept the stack). Best-effort: an
// unreadable /running must never block the agent seat's own unload, which is
// why this returns no error — see the caller.
func otherResidentModels(ctx context.Context, endpoint, agentSeat string, memoryStack []string) (others, kept []string, readable bool) {
	rows, err := seatload.Occupants(ctx, maintenanceClient, endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gpu reserve: could not read /running to find other resident models (%v); only the agent seat will be unloaded\n", err)
		return nil, nil, false
	}
	keep := map[string]bool{}
	for _, m := range memoryStack {
		if m = strings.ToLower(strings.TrimSpace(m)); m != "" {
			keep[m] = true
		}
	}
	for _, row := range rows {
		if strings.EqualFold(row.Model, agentSeat) {
			continue
		}
		switch strings.ToLower(row.State) {
		case "stopped", "shutdown":
			continue
		}
		if keep[strings.ToLower(row.Model)] {
			kept = append(kept, row.Model)
			continue
		}
		others = append(others, row.Model)
	}
	return others, kept, true
}

// unloadOthers unloads every model in others, skipping (and reporting, never
// failing the lease over) one that will not go. It returns the ones it
// actually unloaded — the record the reservation prints. These are FOREIGN
// residents: the PR #464 rule ("warm back only the seat that was loaded, and
// only if it was loaded") stays exactly as it is for the configured agent
// seat, and none of these is ever warmed back automatically — that would
// mean the harness deciding another client's model belongs back on a card it
// no longer controls.
func unloadOthers(ctx context.Context, endpoint string, others, protect []string) []string {
	var done []string
	for _, m := range others {
		if err := unloadSeat(ctx, maintenanceClient, endpoint, m, protect); err != nil {
			fmt.Fprintf(os.Stderr, "gpu reserve: could not unload foreign resident %s: %v\n", m, err)
			continue
		}
		done = append(done, m)
	}
	return done
}

// seatWasResident reports whether the seat is loaded and staying: loaded or
// loading counts, a seat already STOPPING (its ttl ran out) does not. Read
// state only (/running), so the reading never resets the seat's idle timer.
func seatWasResident(ctx context.Context, endpoint, model string) bool {
	rd, err := seatload.Running(ctx, maintenanceClient, endpoint, model)
	if err != nil || rd.Ambiguous {
		return true
	}
	return rd.Loaded && !rd.Stopping
}

// warmGuard is what a warm-back must hold to be allowed to touch the card
// (register D-124): a way to prove the card is still ours (held), the list of
// leases queued behind us (waiters), and the warm-owed marker (owed, clear).
// A warm that cannot prove ownership, or that has a successor queued, does
// not run — the successor unloads the seat again anyway, and an unordered
// warm lands a seat on a card another exclusive lease holds.
type warmGuard struct {
	// held returns nil while the card is still ours; it is checked before the
	// warm and, when renew is set, on every heartbeat during it.
	held func() error
	// renew heartbeats the lease during the warm (a 27B load is minutes, the
	// heartbeat TTL is two): nil for a caller whose lease is heartbeat elsewhere.
	renew   func() error
	waiters func() []gpulease.Waiter
	owed    func() string
	clear   func()
	// onlyIfOwed skips the warm when no warm-back is owed (the seat was not
	// resident when a lease unloaded it). The wrapper's automatic warm sets it;
	// an explicit `gpu release --warm-seat` is the operator asking, and loads.
	onlyIfOwed bool
	// others reports a live lease, other than the one warming, that sits on the seat's
	// cards. With card-scoped leases several holders share a box, and the warm loads the
	// seat on all its cards: the first to finish must not load it over cards the others
	// still use. The warm stays owed and the last lease on those cards pays it.
	others func(model string) (busy bool, why string)
}

// otherLeaseOnSeat is warmGuard.others for the lease with epoch self: a held lease on the
// seat's cards that is not self. Epoch 0 is `gpu release`'s "whatever is held", which the
// release itself only accepts when exactly one lease is live, so one live lease on the
// seat's cards is that one. A seat that declares no cards is on every card.
func otherLeaseOnSeat(m *gpulease.Manager, self uint64) func(model string) (bool, string) {
	return func(model string) (bool, string) {
		var held []gpulease.Info
		for _, l := range modelaffinity.ScopeToModel(m.Inspect(), model).Each() {
			if l.Held {
				held = append(held, l)
			}
		}
		for _, l := range held {
			if l.Epoch == self || (self == 0 && len(held) == 1) {
				continue
			}
			cards := "the whole node"
			if eff := l.EffectiveDevices(); len(eff) > 0 {
				cards = "cards " + strings.Join(eff, ",")
			}
			return true, fmt.Sprintf("%s lease epoch %d on %s", l.Class, l.Epoch, cards)
		}
		return false, ""
	}
}

// warmBackGuarded reloads the config's seat when the guard allows it and
// reports the decision on out. It never returns an error: a skipped or failed
// warm-back is loud but not fatal — the lease release must still run (a leaked
// lease costs every caller, a cold seat costs one on-demand load).
func warmBackGuarded(cfg config.Config, g warmGuard, out io.Writer) {
	endpoint, model, err := seatTarget(cfg)
	if err != nil {
		return
	}
	if g.onlyIfOwed && g.owed != nil && g.owed() == "" {
		fmt.Fprintf(out, "gpu: not warming %s back: it was not loaded when the lease took the card\n", model)
		return
	}
	if g.held != nil {
		if herr := g.held(); herr != nil {
			fmt.Fprintf(out, "gpu: NOT warming %s back: the card is no longer ours (%v); the seat reloads on demand under whoever holds it\n", model, herr)
			return
		}
	}
	if g.waiters != nil {
		if ws := g.waiters(); len(ws) > 0 {
			names := make([]string, 0, len(ws))
			for _, w := range ws {
				names = append(names, fmt.Sprintf("pid %d (%s, queued %s)", w.PID, w.Class, time.Since(w.Since()).Round(time.Second)))
			}
			fmt.Fprintf(out, "gpu: NOT warming %s back: %d lease(s) queued behind this one — %s; the warm belongs to the last holder\n", model, len(ws), strings.Join(names, ", "))
			return
		}
	}
	if g.others != nil {
		if busy, why := g.others(model); busy {
			fmt.Fprintf(out, "gpu: NOT warming %s back yet: %s still sits on its cards; the warm stays owed and the last lease on them pays it\n", model, why)
			return
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopRenew := func() {}
	if g.renew != nil {
		renew := g.renew
		if g.held != nil {
			// A heartbeat that fails is a LOSS only when the card is no longer
			// ours. A write that fails with the lease still held is reported once
			// and retried on the next tick: abandoning the warm on it left the
			// seat cold with the warm still owed (register C-59, the drain's
			// heartbeat had the same defect).
			warned := false
			renew = func() error {
				err := g.renew()
				if err == nil {
					warned = false
					return nil
				}
				if g.held() != nil {
					return err
				}
				if !warned {
					warned = true
					fmt.Fprintf(out, "gpu: could not write the lease heartbeat while warming %s back (%v); the lease is still ours, retrying\n", model, err)
				}
				return nil
			}
		}
		stopRenew = renewUntilLost(renew, drainRenewEvery, func(lerr error) {
			fmt.Fprintf(out, "gpu: LEASE LOST while warming %s back (%v); abandoning the warm\n", model, lerr)
			cancel()
		})
	}
	werr := warmSeat(ctx, maintenanceClient, endpoint, model)
	stopRenew()
	if werr != nil {
		fmt.Fprintf(out, "gpu: warm-back of %s failed: %v\n", model, werr)
		return
	}
	if g.clear != nil {
		g.clear()
	}
	fmt.Fprintf(out, "gpu: %s warmed back\n", model)
}

// renewUntilLost calls renew on a ticker until stop is called; the first
// failure is reported through lost and ends the loop. stop returns only once
// the loop has exited, so no renew lands after the caller has moved on to
// releasing (a late heartbeat file for a released epoch would be debris).
func renewUntilLost(renew func() error, every time.Duration, lost func(error)) (stop func()) {
	done := make(chan struct{})
	exited := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(exited)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := renew(); err != nil {
					lost(err)
					return
				}
			}
		}
	}()
	return func() {
		once.Do(func() { close(done) })
		<-exited
	}
}
