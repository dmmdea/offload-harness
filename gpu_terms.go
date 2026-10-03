package main

// Terms at the holders that tick (plan P9, ADR 0070). A lease's term ends in a renewal or a
// label, never in a release; this file is the holder's side of that: the wrapper form's 15 s
// tick and the detached holder's loop call termTicker.tick, the warning `gpu reserve` prints
// for a window above the cap on a term, and the argv the hidden holder is spawned with.
//
// Nothing here releases a lease, stops a command or kills a process. The only things that can
// happen to a lease at the end of its term are that its declared end moves one term forward
// (the owner is alive and the job is progressing, or the cards are working) or that the lease
// is labelled expired (it stays held, heartbeating, and reads held-overdue).

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// termUtilWorkingFn is how the term check asks whether a lease's cards are working: one
// nvidia-smi sample over the lease's cards (the whole node when it names none), the display
// card excluded. A variable so a test never reaches the driver. A sample that fails reads as
// "not working": an unknown is never work, so a box without nvidia-smi renews nothing on this
// evidence alone.
var termUtilWorkingFn = func(devices []string) bool {
	gpus, err := gpuactivity.SampleGPUs(context.Background())
	if err != nil {
		return false
	}
	return gpuactivity.UtilWorking(gpus, devices)
}

// termRecheckEvery is how often a lease that is already labelled expired is asked again whether
// its term has become renewable (its owner came back, its progress resumed). The first label is
// stamped on the tick that finds the term ended; only the re-asking is spaced, so a long-expired
// lease does not sample the cards every 15 s.
var termRecheckEvery = time.Minute

// termTicker runs the term check on a holder's tick and says, once per change, what it did.
type termTicker struct {
	lease *gpulease.Lease
	// expiredAt is when the lease was last found labelled; zero while it is not.
	expiredAt time.Time
	warned    bool
}

func newTermTicker(lease *gpulease.Lease) *termTicker { return &termTicker{lease: lease} }

// tick asks about the term. It is safe to call every heartbeat: inside the term it reads one
// record and writes nothing.
func (t *termTicker) tick() {
	if !t.expiredAt.IsZero() && time.Since(t.expiredAt) < termRecheckEvery {
		return
	}
	devices := t.lease.Devices()
	res, err := t.lease.AdvanceTerm(gpulease.TermSignals{UtilWorking: func() bool { return termUtilWorkingFn(devices) }})
	if err != nil {
		// A tick that cannot be answered is told once; the next heartbeat tries again. It is never a
		// lost lease (Renew just proved it is ours) and never a reason to touch the command.
		if !t.warned {
			t.warned = true
			fmt.Fprintf(os.Stderr, "gpu reserve: could not check the lease's term (%v); the lease is still held and the next heartbeat tries again\n", err)
		}
		return
	}
	t.warned = false
	switch res.Outcome {
	case gpulease.TermExtended:
		t.expiredAt = time.Time{}
		fmt.Fprintf(os.Stderr, "gpu reserve: the lease's term ended and was renewed to %s (%s)\n", res.End.Local().Format("15:04:05"), res.Why)
	case gpulease.TermExpired:
		t.expiredAt = time.Now()
		fmt.Fprintf(os.Stderr, "gpu reserve: the lease's term ended and was not renewed: %s. It is labelled expired: still held and heartbeating, reading held-overdue and open to a takeover; nothing is released or killed\n", res.Why)
	case gpulease.TermStillExpired:
		t.expiredAt = time.Now()
	case gpulease.TermNotDue:
		t.expiredAt = time.Time{} // renewed by someone else, or never due
	}
}

// reserveTermWarning is the sentence `gpu reserve` prints when the window it was asked for is
// above a limit on terms; "" when it is not. The window is accepted whole either way.
func reserveTermWarning(window time.Duration, opts gpulease.Options) string {
	p := gpulease.PlanTerm(window, opts.ProgressFile != "", opts.MaxTerm, opts.MaxTotal)
	if p.Warning == "" {
		return ""
	}
	return "gpu reserve: warning: " + p.Warning
}

// detachHoldArgs is the argv of the hidden holder a --detach reserve spawns: the holder's own
// flags (holdArgs) plus --release-at-expiry when the caller asked for the pre-terms ending. The
// flag is read off the reserve's flag set, the way the config path is, so the holder is told
// exactly what its parent was.
func detachHoldArgs(fs *flag.FlagSet, class string, dur, wait time.Duration, opts gpulease.Options, auto *reserveDeviceFlags, cfgPath string) []string {
	args := holdArgs(class, dur, wait, opts, auto, cfgPath)
	if f := fs.Lookup("release-at-expiry"); f != nil && f.Value.String() == "true" {
		args = append(args, "--release-at-expiry")
	}
	return args
}
