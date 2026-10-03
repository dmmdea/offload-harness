package main

import (
	"flag"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/comfyinst"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// The holder of a lease stops the ComfyUI instances kept under it, by epoch, and only while the
// lease is still its own. One that lost it (released from outside, reclaimed after a suspend)
// is a straggler: the card may be another lease's now, and the instance on it may already be that
// lease's job (a lease that reuses a kept instance re-stamps its marker, but the window before is
// the straggler's to get wrong). Lease.Release already leaves the current holder's claim alone for
// the same reason; the instance stop follows it.

func TestStopInstancesOfALeaseStopsThemWhileTheLeaseIsOurs(t *testing.T) {
	cfgPath, comfyDir, m := scopedComfyFixture(t)
	calls := recordStops(t, m, []comfyinst.Outcome{{Key: "gaaaa1111", PID: 4242, Port: 8189, Stopped: true}})
	l := cardLease(t, m, "ours", "gpu-test-0")
	var sb strings.Builder
	stopInstancesOfLease(cfgOf(t, cfgPath), l, &sb)
	got := calls()
	if len(got) != 1 || got[0].epoch != l.Epoch() || got[0].dir != comfyDir || !got[0].held {
		t.Fatalf("stop calls = %+v, want one for epoch %d while it is still held", got, l.Epoch())
	}
	if !strings.Contains(sb.String(), "stopped the ComfyUI instance") {
		t.Errorf("output = %q, want what was stopped", sb.String())
	}
}

func TestStopInstancesOfALeaseWeNoLongerHoldStopsNothingAndSaysWhy(t *testing.T) {
	cfgPath, _, m := scopedComfyFixture(t)
	calls := recordStops(t, m, []comfyinst.Outcome{{Key: "gaaaa1111", PID: 4242, Port: 8189, Stopped: true}})
	l := cardLease(t, m, "taken", "gpu-test-0")
	if released, err := m.ReleaseByEpoch(l.Epoch()); err != nil || !released {
		t.Fatalf("release from outside: %v %v", released, err)
	}
	var sb strings.Builder
	stopInstancesOfLease(cfgOf(t, cfgPath), l, &sb)
	if got := calls(); len(got) != 0 {
		t.Fatalf("a holder that lost its lease stopped instances: %+v", got)
	}
	out := sb.String()
	if !strings.Contains(out, "epoch "+strconv.FormatUint(l.Epoch(), 10)) || !strings.Contains(out, "no longer ours") || !strings.Contains(out, "leaving the ComfyUI instances") || !strings.Contains(out, "running") {
		t.Errorf("the operator must be told which instances were left and why: %q", out)
	}
}

// The detached holder (`gpu hold`) is a holder like any other: when its lease is taken away it
// exits without reaching for the instances.
func TestADetachedHolderWhoseLeaseWasTakenAwayStopsNoInstances(t *testing.T) {
	cfgPath, _, m := scopedComfyFixture(t)
	calls := recordStops(t, m, nil)
	args := holdArgs("media", 20*time.Second, 0, gpulease.Options{Reason: "kept", Devices: []string{"gpu-test-0"}}, nil, cfgPath)
	done := make(chan error, 1)
	go func() { done <- runGPUHold(args[2:]) }()
	ls := waitForLeases(t, m, 1)
	if released, err := m.ReleaseByEpoch(ls[0].Epoch); err != nil || !released {
		t.Fatalf("release from outside: %v %v", released, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the holder never noticed it had lost its lease")
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("a detached holder that lost its lease stopped instances: %+v", got)
	}
}

// ... and the one that ends at its window, still holding the lease, does stop them.
func TestADetachedHolderThatEndsAtItsWindowStillStopsItsInstances(t *testing.T) {
	cfgPath, comfyDir, m := scopedComfyFixture(t)
	calls := recordStops(t, m, nil)
	args := holdArgs("media", 1200*time.Millisecond, 0, gpulease.Options{Reason: "kept", Devices: []string{"gpu-test-0"}}, nil, cfgPath)
	done := make(chan error, 1)
	go func() { done <- runGPUHold(args[2:]) }()
	ls := waitForLeases(t, m, 1)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 1 || got[0].epoch != ls[0].Epoch || got[0].dir != comfyDir || !got[0].held {
		t.Fatalf("stop calls = %+v, want one for epoch %d while it was still held", got, ls[0].Epoch)
	}
}

func cfgOf(t *testing.T, cfgPath string) config.Config {
	t.Helper()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("config", cfgPath, "")
	return loadCfg(fs)
}

// The wrapper form cannot be driven in-process (it ends in os.Exit when its command fails), so the
// wiring is pinned at the source: gpu_cmd.go stops instances through stopInstancesOfLease wherever
// the process holds a Lease object, and calls stopKeptInstances directly only in the release verb,
// which acts for an operator on an epoch it has just resolved.
func TestOnlyTheReleaseVerbStopsInstancesWithoutALeaseObject(t *testing.T) {
	src, err := os.ReadFile("gpu_cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	var direct, viaLease int
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(line, "stopKeptInstances(") {
			direct++
			if !strings.Contains(line, "target") {
				t.Errorf("gpu_cmd.go stops instances for something other than the epoch the release verb resolved:\n\t%s", trimmed)
			}
		}
		if strings.Contains(line, "stopInstancesOfLease(") {
			viaLease++
		}
	}
	if direct != 1 {
		t.Errorf("stopKeptInstances is called %d times in gpu_cmd.go, want exactly the release verb's one", direct)
	}
	if viaLease != 2 {
		t.Errorf("stopInstancesOfLease is called %d times in gpu_cmd.go, want the wrapper form's and the detached holder's", viaLease)
	}
}
