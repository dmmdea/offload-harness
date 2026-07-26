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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"

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
	if *asJSON {
		b, _ := json.MarshalIndent(map[string]any{
			"held": info.Held, "class": info.Class, "epoch": info.Epoch, "pid": info.PID,
			"age_s": int(info.Age.Seconds()), "reason": info.Reason, "origin": info.Origin,
			"job_id": info.JobID, "expires_at": info.ExpiresAt.Format(time.RFC3339),
			"state_root": m.Root(),
		}, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	if !info.Held {
		// Say the card is UNRESERVED explicitly. The lease only binds code paths that
		// take it, so an unreserved card is exactly when a bench is exposed — that
		// should be visible, not inferred from silence.
		fmt.Printf("GPU: free (unreserved)  state root: %s\n", m.Root())
		return nil
	}
	fmt.Printf("GPU: held by %s  pid %d  epoch %d  for %s  expires %s\n  reason: %s\n",
		info.Class, info.PID, info.Epoch, info.Age.Round(time.Second),
		info.ExpiresAt.Format(time.Kitchen), info.Reason)
	return nil
}

func runGPUReserve(args []string) error {
	fs := flag.NewFlagSet("gpu reserve", flag.ExitOnError)
	fs.String("config", "", "config file path")
	class := fs.String("class", "text", "lease class: text (a measurement/bench) or media (a generation job)")
	dur := fs.Duration("for", 45*time.Minute, "how long the card is needed; stamped as the lease's declared window")
	reason := fs.String("reason", "", "why the card is held (shown to whoever is waiting)")
	origin := fs.String("origin", "", "who asked for it (session/host)")
	detach := fs.Bool("detach", false, "hold the lease in a hidden background process instead of wrapping a command")
	asJSON := fs.Bool("json", false, "emit JSON")
	_ = fs.Parse(args)

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
	opts := gpulease.Options{Reason: *reason, Origin: *origin, TTL: *dur}

	if *detach {
		return detachHolder(fs, *class, *dur, *reason, *origin, *asJSON, m)
	}

	lease, err := m.TryAcquire(gpulease.Class(*class), opts)
	if err != nil {
		return err // *ErrHeld already reports who holds it and for how long
	}
	// Release on the way out no matter how we leave, including Ctrl-C: a leaked text
	// reservation blocks every render until it expires.
	defer func() { _ = lease.Release() }()
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
				_ = lease.Release()
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

// detachHolder spawns a HIDDEN child that owns the lease, so the lease's holder pid is
// a real, observable process rather than this short-lived CLI invocation.
func detachHolder(fs *flag.FlagSet, class string, dur time.Duration, reason, origin string, asJSON bool, m *gpulease.Manager) error {
	if info := m.Inspect(); info.Held {
		return &gpulease.ErrHeld{Info: info}
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	child := exec.Command(self, "gpu", "hold",
		"--class", class, "--for", dur.String(), "--reason", reason, "--origin", origin)
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
		return fmt.Errorf("spawning detached holder: %w", err)
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
	_ = child.Process.Release()

	// Wait for the child to actually take the lease before reporting success —
	// otherwise a failed hold reads as a successful reservation.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		info := m.Inspect()
		// THE HOLDER MUST BE OURS. The pre-flight Inspect is a TOCTOU: a racer can take
		// the card between the check and the spawn. Without comparing pids, the losing
		// parent reported someone else's reservation as its own and printed a release
		// command that would kill the winner's hold.
		if info.Held && info.PID != childPID {
			return fmt.Errorf("another holder took the GPU first: %s (pid %d, reason %q)",
				info.Class, info.PID, info.Reason)
		}
		if info.Held {
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
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("detached holder (pid %d) did not take the lease within 10s; its output is at %s", childPID, errLog)
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
	_ = fs.Parse(args)

	m, err := openLease(fs)
	if err != nil {
		return err
	}
	lease, err := m.TryAcquire(gpulease.Class(*class), gpulease.Options{
		Reason: *reason, Origin: *origin, TTL: *dur,
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
	_ = fs.Parse(args)
	m, err := openLease(fs)
	if err != nil {
		return err
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
	return nil
}
