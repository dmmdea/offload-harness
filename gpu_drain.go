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
	"github.com/dmmdea/offload-harness/internal/seatload"
	"github.com/dmmdea/offload-harness/internal/seatrate"
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
}

// drainSeat is the gauge-only drain: the historical signature, kept for the
// callers and tests that have no registry.
func drainSeat(ctx context.Context, client *http.Client, endpoint, model string, timeout, every time.Duration, out io.Writer) error {
	return drainUntil(ctx, drainProbe{client: client, endpoint: endpoint, model: model, every: every, out: out}, time.Now().Add(timeout))
}

// reminderEvery bounds how often an UNCHANGED drain state is re-printed.
const reminderEvery = 5 * time.Minute

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
		rd, err := seatload.Inflight(ctx, p.client, p.endpoint, p.model)
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
				msg := fmt.Sprintf("drain of %s gave up after %s without progress (last: %s): the seat's state has not changed for %s, longer than the seat's own turn arithmetic allows", p.model, now.Sub(start).Round(time.Second), last, now.Sub(stuckSince).Round(time.Second))
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
// legacy GET as a fallback for an older llama-swap.
func unloadSeat(ctx context.Context, client *http.Client, endpoint, model string) error {
	base := strings.TrimRight(endpoint, "/")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/models/unload/"+url.PathEscape(model), nil)
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode < 300 {
			return nil
		}
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
func warmSeat(ctx context.Context, client *http.Client, endpoint, model string) error {
	base := strings.TrimRight(endpoint, "/")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/upstream/"+url.PathEscape(model)+"/health", nil)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("warm %s: %w", model, err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("warm %s: status %d", model, resp.StatusCode)
	}
	return nil
}

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

// maintainSeat runs the --drain / --unload-seat steps against the config's
// seat: drain until deadline, then turn the DRAINING stamp into EXCLUSIVE when
// the window asked for it (or just clear it), then unload. Errors are returned
// as-is; the caller decides what to do with the lease. owed, when non-nil, is
// told the seat's name after a successful unload: the warm-owed marker the
// LAST releasing holder pays (register D-124).
func maintainSeat(cfg config.Config, restamp restamper, drain bool, deadline time.Time, unload, exclusive bool, owed func(seat string)) error {
	endpoint, model, err := seatTarget(cfg)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if drain {
		p := drainProbe{client: maintenanceClient, endpoint: endpoint, model: model, every: 2 * time.Second, out: os.Stderr, hint: seatTurnHint(cfg, model), stuckAfter: seatStuckAfter(cfg, model)}
		if reg, rerr := gpuactivity.Open(cfg.GPULockPath, cfg.StateDir); rerr == nil {
			p.runs = reg.OnSeat
		} else {
			fmt.Fprintf(os.Stderr, "gpu reserve: run registry unavailable (%v): draining on the seat's gauge alone\n", rerr)
		}
		if err := drainUntil(ctx, p, deadline); err != nil {
			// A failed drain must not leave the DRAINING stamp on a lease that
			// stays held (the detach form keeps it): the stamp cordons the seat
			// — no new run admitted — for the rest of the window, which turned
			// one failed drain into a box refused for 8 h (register C-50,
			// S-31). The lease stays held and non-exclusive; the caller says
			// what to do with it.
			if restamp != nil {
				if serr := restamp(func(m *gpulease.Meta) { m.Draining = false }); serr != nil {
					fmt.Fprintf(os.Stderr, "gpu reserve: could not clear the draining stamp after the failed drain: %v\n", serr)
				}
			}
			return err
		}
		fmt.Fprintf(os.Stderr, "gpu reserve: %s drained (no request in flight, no run registered)\n", model)
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
		if err := unloadSeat(ctx, maintenanceClient, endpoint, model); err != nil {
			return err
		}
		if owed != nil {
			owed(model)
		}
		fmt.Fprintf(os.Stderr, "gpu reserve: %s unloaded\n", model)
	}
	return nil
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopRenew := func() {}
	if g.renew != nil {
		stopRenew = renewUntilLost(g.renew, drainRenewEvery, func(lerr error) {
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
