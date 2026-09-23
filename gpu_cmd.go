package main

// `local-offload gpu` — the operator-facing half of the machine-wide GPU lease.
//
// This is the verb that makes the lease usable by work the harness does not own. The
// incident it prevents: a benchmark was running against llama-swap when a media job
// arrived through a different ingress, unloaded every GPU-resident model, and killed
// the measurement. Text work never took the lock, so nothing could have stopped it.
// `gpu reserve --class text` gives that work a way to hold the card, and a render then
// WAITS instead of tearing the tier down.
//
//	local-offload gpu status [--json]
//	local-offload gpu reserve --class text --for 45m --reason "kv bench" -- <command...>
//	local-offload gpu reserve --class text --for 45m --reason "kv bench" --detach
//	local-offload gpu release [--epoch N]
//
// The WRAPPER form is the one to prefer: the lease lives exactly as long as the
// command and cannot be leaked by forgetting to release, the way `timeout` or `nice`
// compose. --detach exists for an interactive session that wants the card held across
// several commands, and it is the weaker form by design.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

func runGPU(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: local-offload gpu <status|reserve|release> [flags]")
	}
	switch args[0] {
	case "status":
		return runGPUStatus(args[1:])
	case "reserve":
		return runGPUReserve(args[1:])
	case "release":
		return runGPURelease(args[1:])
	case "hold":
		return runGPUHold(args[1:])
	default:
		return fmt.Errorf("unknown gpu subcommand %q (want status, reserve or release)", args[0])
	}
}

// openLease resolves the state root the same way every consumer does, so the CLI can
// never end up arbitrating a different lease than the render path.
func openLease(fs *flag.FlagSet) (*gpulease.Manager, error) {
	cfg := loadCfg(fs)
	// gpu_lock_path MUST be honoured here. Ignoring it while the render path and the
	// vision gate obeyed it put the reservation verb on a different directory from the
	// renders it is supposed to arbitrate against — they never contended, which is the
	// original incident with no warning.
	return gpulease.OpenAt(cfg.GPULockPath, cfg.StateDir)
}

func runGPUStatus(args []string) error {
	fs := flag.NewFlagSet("gpu status", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "emit JSON")
	_ = fs.Parse(args)
	m, err := openLease(fs)
	if err != nil {
		return err
	}
	info := m.Inspect()
	// What the cards are DOING (0.117.0, register D-93): the seat's in-flight
	// count, the registered runs, a utilization sample and one verdict.
	act := gpuactivity.Snapshot(context.Background(), activityOptions(loadCfg(fs)))
	// Who is queued (register D-124): the line behind the holder, and whether
	// the agent seat is owed a warm-back by the last of them.
	waiters := m.Waiters()
	warmOwed := m.SeatWarmOwed()
	if *asJSON {
		queued := make([]map[string]any, 0, len(waiters))
		for _, w := range waiters {
			queued = append(queued, map[string]any{"pid": w.PID, "class": w.Class, "reason": w.Reason, "since": w.Since().Format(time.RFC3339)})
		}
		b, _ := json.MarshalIndent(map[string]any{
			"held": info.Held, "class": info.Class, "epoch": info.Epoch, "pid": info.PID,
			"age_s": int(info.Age.Seconds()), "reason": info.Reason, "origin": info.Origin,
			"job_id": info.JobID, "expires_at": info.ExpiresAt.Format(time.RFC3339),
			"exclusive": info.Exclusive, "draining": info.Draining, "command": info.Command, "state_root": m.Root(),
			"queued": queued, "seat_warm_owed": warmOwed,
			"verdict": act.Verdict, "activity": act.Map(),
			// The next step, spelled out: a session reading "held" used to conclude
			// "refuse the work"; the honest answer is "queue behind it".
			"queue_with": queueHint,
		}, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	// The line and the owed warm are printed held or free: a warm can be owed
	// while the card is free (a fenced-out holder pays nothing; the marker
	// waits for the next last holder).
	queueLines := func() {
		if len(waiters) > 0 {
			parts := make([]string, 0, len(waiters))
			for _, w := range waiters {
				parts = append(parts, fmt.Sprintf("pid %d (%s, %s in line)", w.PID, w.Class, time.Since(w.Since()).Round(time.Second)))
			}
			fmt.Printf("  queued: %d — %s\n", len(waiters), strings.Join(parts, ", "))
		}
		if warmOwed != "" {
			fmt.Printf("  seat warm-back owed: %s (paid by the last holder to release)\n", warmOwed)
		}
	}
	if !info.Held {
		// Say the card is UNRESERVED explicitly. The lease only binds code paths that
		// take it, so an unreserved card is exactly when a bench is exposed — that
		// should be visible, not inferred from silence.
		fmt.Printf("GPU: free (unreserved)  state root: %s\n", m.Root())
		queueLines()
		printActivity(act)
		return nil
	}
	excl := ""
	if info.Exclusive {
		excl = "  (exclusive: text loads wait or route elsewhere)"
	}
	if info.Draining {
		excl += "  (draining: new runs wait, in-flight runs complete)"
	}
	if info.Command != "" {
		excl += "\n  running: " + info.Command
	}
	fmt.Printf("GPU: held by %s  pid %d  epoch %d  for %s  expires %s%s\n  reason: %s\n  queue behind it: %s\n",
		info.Class, info.PID, info.Epoch, info.Age.Round(time.Second),
		info.ExpiresAt.Format(time.Kitchen), excl, info.Reason, queueHint)
	queueLines()
	printActivity(act)
	return nil
}

// queueHint: one sentence, shared with offload_status (gpulease.QueueHint) so the
// two surfaces a session reads cannot disagree about what to do next.
const queueHint = gpulease.QueueHint

// defaultReserveWait is how long `gpu reserve` queues behind a holder when the caller
// says nothing. Eight hours, not 90 s: the media pipeline's ceiling protects ONE tool
// call from hanging, but a reservation is taken by a session that has a job to run and
// would otherwise write the job off. The whole window is waited out (Options.WaitOut):
// a text holder's declared window is printed, not trusted, because holders release
// before it as a rule.
const defaultReserveWait = 8 * time.Hour

func runGPUReserve(args []string) error {
	fs := flag.NewFlagSet("gpu reserve", flag.ExitOnError)
	fs.String("config", "", "config file path")
	class := fs.String("class", "text", "lease class: text (a measurement/bench) or media (a generation job)")
	dur := fs.Duration("for", 45*time.Minute, "how long the card is needed; stamped as the lease's declared window")
	reason := fs.String("reason", "", "why the card is held (shown to whoever is waiting)")
	origin := fs.String("origin", "", "who asked for it (session/host)")
	detach := fs.Bool("detach", false, "hold the lease in a hidden background process instead of wrapping a command")
	drain := fs.Bool("drain", false, "after taking the lease, wait until the agent seat is IDLE — no request running or waiting on the engine, no load in progress, no registered agent run — before continuing; the lease is stamped DRAINING meanwhile (new runs wait, in-flight runs complete) and exclusive only once idle; a drain that misses its deadline releases the lease and exits non-zero")
	drainTimeout := fs.Duration("drain-timeout", 0, "how long --drain waits for in-flight work; 0 (the default) = the rest of the --wait queue budget, never under 2m — one 27B step runs 3-4 min, which is why a fixed 2m window failed twice on 2026-09-14")
	unload := fs.Bool("unload-seat", false, "after the drain, unload the agent seat through llama-swap so the cards are free; the wrapper form warms it back when the command ends (detach: use `gpu release --warm-seat`); implies --exclusive")
	wait := fs.Duration("wait", defaultReserveWait, "how long to QUEUE behind a current holder before giving up (0 = fail fast); the holder's declared window is reported, not trusted — the wait runs its full length")
	exclusive := fs.Bool("exclusive", false, "stamp a text lease exclusive: the harness's text-load gate then keeps models off these cards for the lease's length (loads ride a cascade remote lane or wait their own budget); implied by --unload-seat")
	asJSON := fs.Bool("json", false, "emit JSON")
	_ = fs.Parse(args)
	if *unload {
		*exclusive = true // a cleared card that the next text call refills is not cleared
	}
	// A DETACHED holder exits at --for and releases, whether or not the work is
	// still running (the loop below). With the 45-minute default that silently
	// frees the card mid-job: the next render then claims it and unloads the
	// seat on top of the running work, and the node is left advertising a seat
	// that is not there (2026-09-07 audit). The wrapper form ties the hold to a
	// process and needs no window, so the requirement lands only on --detach.
	forGiven := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "for" {
			forGiven = true
		}
	})
	if *detach && !forGiven {
		return errors.New("--detach requires an explicit --for: a detached holder releases the card at that deadline whether or not the work has finished, and the default window would free it mid-job. Declare the real window (e.g. --for 8h), or use the wrapper form `gpu reserve ... -- <command>`, which holds the lease exactly as long as the command runs")
	}
	if *unload && !*drain {
		return errors.New("--unload-seat requires --drain: never unload a seat with a request in flight")
	}

	cmdArgs := fs.Args() // everything after `--`
	if len(cmdArgs) == 0 && !*detach {
		return errors.New("give a command to wrap (`gpu reserve ... -- <command>`) or pass --detach")
	}
	if len(cmdArgs) > 0 && *detach {
		return errors.New("--detach and a wrapped command are mutually exclusive")
	}

	m, err := openLease(fs)
	if err != nil {
		return err
	}
	// Exclusive is stamped AT ACQUIRE only when no drain is requested. With
	// --drain (and --unload-seat, which implies exclusive) the record is stamped
	// DRAINING first — new runs wait, requests of runs already in flight keep
	// flowing — and turned exclusive by maintainSeat once the seat is idle. The
	// old order (exclusive at acquire) made the drain block the very run it
	// was waiting for (2026-09-14, register D-93).
	exclusiveAfter := (*exclusive || *unload) && *drain
	opts := gpulease.Options{Reason: *reason, Origin: *origin, TTL: *dur,
		Exclusive: *exclusive && !*drain, Draining: *drain, Command: strings.Join(cmdArgs, " ")}
	queuedAt := time.Now()

	if *detach {
		epoch, err := detachHolder(fs, *class, *dur, *wait, opts, *asJSON, m)
		if err != nil {
			return err
		}
		// The lease is held by the hidden child FIRST (so no new work is placed
		// here), then the seat is drained and unloaded. A failed drain leaves the
		// lease held on purpose — the card stays reserved, work keeps routing
		// elsewhere — and the exit code tells the caller not to start.
		if *drain || *unload {
			if err := maintainSeat(loadCfg(fs), func(fn func(*gpulease.Meta)) error { return m.Restamp(epoch, fn) }, *drain, drainDeadline(*drainTimeout, queuedAt, *wait), *unload, exclusiveAfter, markWarmOwed(m)); err != nil {
				return fmt.Errorf("%w (the lease is still held; `gpu release` frees it)", err)
			}
		}
		return nil
	}

	lease, err := acquireQueued(m, gpulease.Class(*class), opts, *wait)
	if err != nil {
		return err
	}
	// Release on the way out no matter how we leave, including Ctrl-C: a leaked text
	// reservation blocks every render until it expires. When the seat was unloaded
	// for this window it is warmed back BEFORE the release, so the first contract
	// placed here again finds a loaded seat — but ONLY while the card is still
	// ours and nobody is queued behind us (register D-124: an unordered warm
	// after a cut command loaded the seat onto the next lease's exclusive card,
	// and a warm ahead of a queued --unload-seat is a load bought for nothing).
	// The warm is heartbeat for its length; the lease outlives the command by
	// exactly the warm.
	cfg := loadCfg(fs)
	finish := func() {
		if *unload {
			warmBackGuarded(cfg, leaseWarmGuard(m, lease), os.Stderr)
		}
		_ = lease.Release()
	}
	defer finish()
	if *drain || *unload {
		// The drain now runs for the queue budget (hours) and the renewal loop
		// below starts only once the wrapped command does; the reclaim rule needs
		// a stale heartbeat AND an expired --for window, so a drain longer than
		// --for with no renewal handed the card to the next acquirer mid-wait
		// (reviewer finding, 0.117.0). Heartbeat for the drain's whole length.
		stopRenew := renewWhile(lease, drainRenewEvery)
		merr := maintainSeat(cfg, lease.Restamp, *drain, drainDeadline(*drainTimeout, queuedAt, *wait), *unload, exclusiveAfter, markWarmOwed(m))
		stopRenew()
		if merr != nil {
			return merr // deferred finish releases the lease (and warms back if it got that far)
		}
	}
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt)
	defer signal.Stop(sigc)

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The child inherits the lease so any render it triggers skips its own acquire and
	// its own unload — that is what hoists freeLlamaSwap from per-job to per-lease.
	cmd.Env = append(os.Environ(),
		"GPU_LEASE_DIR="+lease.Dir(),
		fmt.Sprintf("GPU_LEASE_EPOCH=%d", lease.Epoch()),
		// The class travels with the lease so an inheriting render knows what kind of
		// hold it is running under; the unload is elected per lease inside the render
		// path (claimLeaseUnload), so exactly one job per lease tears the tier down.
		"GPU_LEASE_CLASS="+string(lease.Class()),
	)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting wrapped command: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Heartbeat while the command runs. The reclaim rule needs BOTH a stale heartbeat
	// and an expired window, so a missed tick inside the declared window is harmless.
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				finish()               // os.Exit skips defers: warm back + release explicitly
				os.Exit(ee.ExitCode()) // propagate so shell loops branch correctly
			}
			return err
		case <-sigc:
			_ = cmd.Process.Kill()
		case <-tick.C:
			// LOSING THE LEASE MUST BE LOUD. Discarding this error left the wrapped
			// benchmark running with no reservation while a render tore its tier down
			// — the failure the wrapper exists to prevent, now invisible. Renew()
			// checks the epoch first, so this fires on an operator `gpu release` or on
			// being fenced out.
			if err := lease.Renew(); err != nil {
				fmt.Fprintf(os.Stderr,
					"gpu reserve: LEASE LOST (%v) — the GPU is no longer reserved for this command; killing it\n", err)
				_ = cmd.Process.Kill()
			}
		}
	}
}

// acquireQueued takes the card, QUEUEING behind a current holder for up to wait.
//
// THE DEFECT THIS REPLACES: the CLI called TryAcquire, so a held card was an error,
// and every session that hit it read the error as "the machine is busy, refuse the
// work" — while the pipeline underneath had queued renders behind each other since
// ADR 0018. A reservation is a place in line, and the line is printed ONCE on entry
// (who holds it, why, until when) and once on exit; a poll line per second would be
// a notification loop in the session that wrapped this.
func acquireQueued(m *gpulease.Manager, class gpulease.Class, opts gpulease.Options, wait time.Duration) (*gpulease.Lease, error) {
	lease, err := m.TryAcquire(class, opts)
	var held *gpulease.ErrHeld
	if err == nil || wait <= 0 || !errors.As(err, &held) {
		return lease, heldHint(err, wait)
	}
	fmt.Fprintf(os.Stderr, "gpu reserve: queued behind %v — waiting up to %s\n", held, wait)
	start := time.Now()
	// WaitOut: the holder's declared window is printed above as information, never
	// treated as a verdict — holders release before it as a rule, and a waiter that
	// left the line on the declaration was the refusal this verb exists to end.
	opts.Wait, opts.WaitOut = wait, true
	lease, err = m.Acquire(class, opts)
	if err != nil {
		return nil, heldHint(err, wait)
	}
	fmt.Fprintf(os.Stderr, "gpu reserve: acquired after %s in the queue\n", time.Since(start).Round(time.Second))
	return lease, nil
}

// heldHint makes a refusal actionable: the holder's declared window is already in the
// message, so the only missing fact is which flag turns the refusal into a wait.
func heldHint(err error, wait time.Duration) error {
	var held *gpulease.ErrHeld
	if err == nil || !errors.As(err, &held) {
		return err
	}
	if wait <= 0 {
		return fmt.Errorf("%w; pass --wait <duration> (default %s) to queue behind it instead of failing", err, defaultReserveWait)
	}
	return fmt.Errorf("%w; not free within --wait %s — pass a longer --wait to keep queueing", err, wait)
}

// detachHolder spawns a HIDDEN child that owns the lease, so the lease's holder pid is
// a real, observable process rather than this short-lived CLI invocation. With a
// positive wait the CHILD queues (gpu hold acquires with the same --wait) and this
// parent waits for it to win the card, so "reserved" is printed only once it is true.
func detachHolder(fs *flag.FlagSet, class string, dur, wait time.Duration, opts gpulease.Options, asJSON bool, m *gpulease.Manager) (uint64, error) {
	if info := m.Inspect(); info.Held && wait <= 0 {
		return 0, heldHint(&gpulease.ErrHeld{Info: info}, wait)
	}
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	child := exec.Command(self, "gpu", "hold",
		"--class", class, "--for", dur.String(), "--wait", wait.String(), "--reason", opts.Reason, "--origin", opts.Origin)
	if opts.Exclusive {
		child.Args = append(child.Args, "--exclusive")
	}
	if opts.Draining {
		child.Args = append(child.Args, "--draining")
	}
	if cfgPath := fs.Lookup("config").Value.String(); cfgPath != "" {
		child.Args = append(child.Args, "--config", cfgPath)
	}
	hideWindow(child) // no console window may ever appear; one gets closed and the hold dies

	// A hidden child with nil Stderr writes to NUL, so a holder that fails to start
	// produced ZERO diagnostics and the parent could only report "did not take the
	// lease". Capture it.
	errLog := filepath.Join(os.TempDir(), fmt.Sprintf("local-offload-gpu-hold-%d.log", os.Getpid()))
	if f, ferr := os.Create(errLog); ferr == nil {
		child.Stderr = f
		defer func() { _ = f.Close() }()
	}
	if err := child.Start(); err != nil {
		return 0, fmt.Errorf("spawning detached holder: %w", err)
	}
	childPID := child.Process.Pid
	childProc := child.Process
	// If we do not end up reporting success, the child must not survive. Both failure
	// paths below used to return while leaving it running: it would then acquire as soon
	// as the winner released and hold the card for its full --for window, while the
	// operator had been told the reservation FAILED. This file warns about exactly that
	// hazard a few lines up ("a leaked text reservation blocks every render until it
	// expires").
	reported := false
	defer func() {
		if !reported && childProc != nil {
			_ = childProc.Kill()
		}
	}()
	// NO Process.Release() HERE. Releasing invalidates the handle, so the Kill above
	// returns EINVAL and the orphan survives both failure paths — the guard read as
	// present while doing nothing. Holding the handle costs nothing: this CLI
	// invocation returns moments later and exits, at which point the OS drops the
	// handle and the child carries on as an independent process either way.

	// The child is the one that queues; if it gives up (its --wait ran out, or the
	// holder's window outlasts it) it exits non-zero and THAT is the answer, read
	// here rather than inferred from a card that is still someone else's.
	childDone := make(chan error, 1)
	go func() { childDone <- child.Wait() }()

	// Wait for the child to actually take the lease before reporting success —
	// otherwise a failed hold reads as a successful reservation. The child's own
	// queue window bounds this; the 10 s is the spawn-and-claim allowance on top.
	deadline := time.Now().Add(10*time.Second + wait)
	queuedBehind := 0
	for time.Now().Before(deadline) {
		select {
		case werr := <-childDone:
			childProc = nil // nothing left to reap
			return 0, fmt.Errorf("detached holder (pid %d) gave up before taking the lease: %v; its output is at %s", childPID, werr, errLog)
		default:
		}
		info := m.Inspect()
		// THE HOLDER MUST BE OURS. Without comparing pids, a parent reported someone
		// else's reservation as its own and printed a release command that would kill
		// the winner's hold. With a queue window a foreign holder is not a racer but
		// the line we are standing in: say so once, keep waiting, and let the child's
		// exit (above) be the verdict when the line does not move in time.
		if info.Held && info.PID != childPID {
			if wait <= 0 {
				return 0, fmt.Errorf("another holder took the GPU first: %s (pid %d, reason %q)",
					info.Class, info.PID, info.Reason)
			}
			if queuedBehind != info.PID {
				queuedBehind = info.PID
				fmt.Fprintf(os.Stderr, "gpu reserve: queued behind %v — the holder (pid %d) takes the card when it frees; waiting up to %s\n",
					&gpulease.ErrHeld{Info: info}, childPID, wait)
			}
		}
		if info.Held && info.PID == childPID {
			if asJSON {
				b, _ := json.Marshal(map[string]any{
					"held": true, "class": info.Class, "epoch": info.Epoch,
					"pid": info.PID, "expires_at": info.ExpiresAt.Format(time.RFC3339),
				})
				fmt.Println(string(b))
			} else {
				fmt.Printf("reserved: %s epoch %d (pid %d) until %s\n  release with: local-offload gpu release --epoch %d\n",
					info.Class, info.Epoch, info.PID, info.ExpiresAt.Format(time.Kitchen), info.Epoch)
			}
			reported = true // the holder is ours and reported; leave it running
			return info.Epoch, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return 0, fmt.Errorf("detached holder (pid %d) did not take the lease within %s; its output is at %s", childPID, (10*time.Second + wait).Round(time.Second), errLog)
}

// runGPUHold is the detached holder itself (internal; spawned by --detach). It holds
// the lease and exits as soon as the lease stops being ours — which is how
// `gpu release` frees it without ever killing a process by pid.
func runGPUHold(args []string) error {
	fs := flag.NewFlagSet("gpu hold", flag.ExitOnError)
	fs.String("config", "", "config file path")
	class := fs.String("class", "text", "lease class")
	dur := fs.Duration("for", 45*time.Minute, "declared window")
	reason := fs.String("reason", "", "why")
	origin := fs.String("origin", "", "who")
	wait := fs.Duration("wait", 0, "queue behind a current holder for up to this long (the parent passes its --wait)")
	exclusive := fs.Bool("exclusive", false, "stamp the text lease exclusive (the parent passes its --exclusive)")
	draining := fs.Bool("draining", false, "stamp the text lease draining (the parent passes its --drain; it restamps exclusive when the drain completes)")
	_ = fs.Parse(args)

	m, err := openLease(fs)
	if err != nil {
		return err
	}
	// The queue lives HERE, in the process that will hold the card, so the lease is
	// taken by the pid the parent reports and a parent that dies mid-wait leaves
	// nothing behind but a holder that will release at its own deadline.
	lease, err := m.Acquire(gpulease.Class(*class), gpulease.Options{
		Reason: *reason, Origin: *origin, TTL: *dur, Wait: *wait, WaitOut: true, Exclusive: *exclusive, Draining: *draining,
	})
	if err != nil {
		return err
	}
	defer func() { _ = lease.Release() }()

	// Poll FAST but renew slowly. These are two different clocks and conflating them
	// was wrong: a single 15s ticker meant `gpu release` left the holder alive for up
	// to 15s afterwards, so `gpu status` could report the card free while a holder
	// process was still sitting there. Renewal only has to beat the heartbeat TTL
	// (120s), while noticing we have been released should feel immediate.
	const (
		pollEvery  = 1 * time.Second
		renewEvery = 15 * time.Second
	)
	deadline := time.Now().Add(*dur)
	lastRenew := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(pollEvery)
		if err := lease.Check(); err != nil {
			return nil // released or fenced out — exit quietly, the lease is not ours
		}
		if time.Since(lastRenew) >= renewEvery {
			_ = lease.Renew()
			lastRenew = time.Now()
		}
	}
	return nil
}

func runGPURelease(args []string) error {
	fs := flag.NewFlagSet("gpu release", flag.ExitOnError)
	fs.String("config", "", "config file path")
	epoch := fs.Uint64("epoch", 0, "release only if this is still the current lease (0 = release whatever is held)")
	warm := fs.Bool("warm-seat", false, "after releasing, load the agent seat back through llama-swap (the counterpart of `gpu reserve --detach --drain --unload-seat`)")
	_ = fs.Parse(args)
	m, err := openLease(fs)
	if err != nil {
		return err
	}
	// Warm BEFORE the release so the seat is loaded by the time delegators see
	// the card free again; a failed warm-back is reported and never blocks the
	// release (a leaked lease costs every caller, a cold seat costs one load).
	// The warm runs only when the lease being released is the current one and
	// nobody is queued behind it (register D-124): a successor unloads the seat
	// again, and a warm against someone else's exclusive lease is the incident.
	if *warm {
		warmBackGuarded(loadCfg(fs), releaseWarmGuard(m, *epoch), os.Stderr)
	}
	released, err := m.ReleaseByEpoch(*epoch)
	if err != nil {
		return err
	}
	if !released {
		fmt.Println("no lease is held")
		return nil
	}
	fmt.Println("released")
	if !*warm {
		if seat := m.SeatWarmOwed(); seat != "" {
			fmt.Printf("note: %s is owed a warm-back (unloaded for a lease); `gpu release --warm-seat` or the next request loads it\n", seat)
		}
	}
	return nil
}

// markWarmOwed is the maintainSeat callback that stamps the warm-owed marker
// after an unload.
func markWarmOwed(m *gpulease.Manager) func(seat string) {
	return func(seat string) {
		if err := m.MarkSeatWarmOwed(seat); err != nil {
			fmt.Fprintf(os.Stderr, "gpu reserve: could not record that %s is owed a warm-back: %v\n", seat, err)
		}
	}
}

// leaseWarmGuard is the wrapper form's guard: the lease object proves and
// heartbeats ownership; waiters and the marker come from the manager.
func leaseWarmGuard(m *gpulease.Manager, l *gpulease.Lease) warmGuard {
	return warmGuard{
		held:       l.Check,
		renew:      l.Renew,
		waiters:    m.Waiters,
		owed:       m.SeatWarmOwed,
		clear:      m.ClearSeatWarmOwed,
		onlyIfOwed: true,
	}
}

// releaseWarmGuard is `gpu release --warm-seat`'s guard: the caller does not
// hold a Lease object, so ownership is "the record is the epoch I was told to
// release" (epoch 0 = whatever is held, the operator's override — the warm
// then runs when the card is free or held by the record being released).
func releaseWarmGuard(m *gpulease.Manager, epoch uint64) warmGuard {
	held := func() error {
		info := m.Inspect()
		if epoch != 0 && info.Held && info.Epoch != epoch {
			return fmt.Errorf("the lease has moved on (asked to release epoch %d, current is %d)", epoch, info.Epoch)
		}
		return nil
	}
	return warmGuard{
		held: held,
		// No Lease object to heartbeat here (a detached holder's child does
		// that), but the ownership check re-runs on the same cadence for the
		// warm's whole length, so a card that moves on mid-load cancels it.
		renew:   held,
		waiters: m.Waiters,
		owed:    m.SeatWarmOwed,
		clear:   m.ClearSeatWarmOwed,
	}
}

// drainFloor is the least a drain waits, whatever the queue budget says: a
// caller that asked to fail fast on the LEASE (`--wait 0`) still gets a real
// window for in-flight work — the old fixed default, kept as the floor.
const drainFloor = 2 * time.Minute

// drainDeadline: an explicit --drain-timeout wins; otherwise the drain runs
// inside the same queue budget as the lease (--wait, measured from when the
// reservation started queueing), never under drainFloor.
func drainDeadline(explicit time.Duration, queuedAt time.Time, wait time.Duration) time.Time {
	if explicit > 0 {
		return time.Now().Add(explicit)
	}
	d := queuedAt.Add(wait)
	if floor := time.Now().Add(drainFloor); d.Before(floor) {
		d = floor
	}
	return d
}

// activityOptions is the activity read the gpu verbs make: this box's lease
// root, its llama-swap and its agent seat, with a utilization sample.
func activityOptions(cfg config.Config) gpuactivity.Options {
	return gpuactivity.Options{LockOverride: cfg.GPULockPath, StateDir: cfg.StateDir, Endpoint: cfg.Endpoint, Seat: cfg.AgentPlannerModel(""), SampleGPU: true}
}

func printActivity(v gpuactivity.View) {
	for _, ln := range v.Lines() {
		fmt.Println("  " + ln)
	}
}

// drainRenewEvery is the heartbeat cadence while the wrapper form drains — the
// same 15 s the run loop uses, well inside the 120 s heartbeat TTL. A variable
// so a test can watch the heartbeat move within its own window.
var drainRenewEvery = 15 * time.Second

// renewWhile heartbeats the lease on a ticker until stop is called. Losing the
// lease mid-drain is reported once and ends the loop; the drain's own Restamp
// or the wrapped command's Renew then fails loudly on the fenced epoch.
func renewWhile(l *gpulease.Lease, every time.Duration) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := l.Renew(); err != nil {
					fmt.Fprintf(os.Stderr, "gpu reserve: LEASE LOST while draining (%v)\n", err)
					return
				}
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}
