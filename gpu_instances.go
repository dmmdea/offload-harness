package main

// Who stops a kept ComfyUI instance (plan P13). A runner told to keep the ComfyUI it launched
// (render/comfy-lifecycle.mjs, keep:true) leaves it running after the runner exits, so a batch
// of items under one lease loads its models once. The instance lives no longer than the lease
// it was launched under (its launch marker records the epoch), and the HOLDER of that lease
// stops it when it lets go: the wrapper form when its command ends, the detached holder when
// it exits, and `gpu release` when an operator ends a lease from outside. The pipeline does the
// same for the media leases it takes (internal/pipeline). What is stopped, and the proof
// required first, is internal/comfyinst's.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/comfyinst"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// comfyStopFn stops the instances kept under a lease epoch. A variable so no test reaches a
// real ComfyUI directory.
var comfyStopFn = func(ctx context.Context, comfyDir string, epoch uint64) []comfyinst.Outcome {
	return comfyinst.StopForLease(ctx, comfyDir, epoch, comfyinst.RealDeps())
}

// stopInstancesOfLease is what a lease's HOLDER does when it lets go: stop the instances kept
// under the lease, but only while the lease is still its own. A holder that lost it (released from
// outside, or reclaimed after a suspend) is a straggler: the card may be another lease's now and
// the instance on it already that lease's job, so it leaves them running and says which, the same
// rule Lease.Release follows for the claim (a fenced-out holder leaks rather than destroys). A
// kept instance a later lease reuses is re-stamped to it, and one nobody reuses is stopped by hand
// or by the next lease on its card.
func stopInstancesOfLease(cfg config.Config, lease *gpulease.Lease, out io.Writer) {
	if err := lease.Check(); err != nil {
		if strings.TrimSpace(cfg.ComfyDir) != "" {
			fmt.Fprintf(out, "gpu: leaving the ComfyUI instances kept under lease epoch %d running: the lease is no longer ours (%v), so the card, and any instance on it, may be another lease's now; stop them by hand if they are not\n", lease.Epoch(), err)
		}
		return
	}
	stopKeptInstances(cfg, lease.Epoch(), out)
}

// stopKeptInstances stops the ComfyUI instances kept under lease epoch `epoch` and says what it
// did. It runs BEFORE the lease is released, so the next holder never finds a stale instance
// on its card, and a failure to stop one is reported and never blocks the release (a leaked
// lease costs every caller; a leftover instance costs one card, and is named).
func stopKeptInstances(cfg config.Config, epoch uint64, out io.Writer) {
	if strings.TrimSpace(cfg.ComfyDir) == "" || epoch == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, o := range comfyStopFn(ctx, cfg.ComfyDir, epoch) {
		switch {
		case o.Stopped:
			fmt.Fprintf(out, "gpu: stopped the ComfyUI instance %s (pid %d, port %d) kept under lease epoch %d\n", o.Key, o.PID, o.Port, epoch)
		case o.Why != "":
			fmt.Fprintf(out, "gpu: the ComfyUI instance %s (pid %d, port %d) kept under lease epoch %d was not stopped: %s\n", o.Key, o.PID, o.Port, epoch, o.Why)
		}
	}
}
