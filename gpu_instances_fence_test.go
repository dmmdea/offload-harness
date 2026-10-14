package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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

// ... and the one that ends at its window, still holding the lease, does stop them. Since the
// bounded terms (P9) a detached holder ends at its window only with --release-at-expiry; without
// it the lease stays and reads overdue, so this case asks for the release.
func TestADetachedHolderThatEndsAtItsWindowStillStopsItsInstances(t *testing.T) {
	cfgPath, comfyDir, m := scopedComfyFixture(t)
	calls := recordStops(t, m, nil)
	args := append(holdArgs("media", 1200*time.Millisecond, 0, gpulease.Options{Reason: "kept", Devices: []string{"gpu-test-0"}}, nil, cfgPath), "--release-at-expiry")
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

// The wrapper form cannot be driven in-process (it ends in os.Exit when its command fails), and the detached parent's release needs
// a process of its own, so the wiring is pinned at the source. The invariant: the ComfyUI instances kept under a lease are stopped
// only by a caller that has FENCED THE EPOCH it stops them for. The test names each allowed caller by the function that holds the
// call, in every non-test file of the package (it used to count lines in gpu_cmd.go alone, so a call added in another file, and a
// third call added in this one, passed or failed for the wrong reason). Two kinds of caller qualify:
//
//   - stopInstancesOfLease, for a process that holds a *Lease: it checks the fence itself (Lease.Check) before it stops anything.
//     Allowed callers are the wrapper form (runGPUReserve), the detached holder (runGPUHold) and its release at the end of a
//     host-RAM wait (releaseHolderAtWaitEnd). The holder's release stops the instances AND its deferred function does too: the
//     first is needed because the instances hold VRAM the seat's warm-back wants and the warm-back comes before the release; the
//     deferred one finds no marker of an instance the first one stopped, so it is redundant and harmless, and it stays because it
//     is the only stop on every other way the holder ends.
//   - stopKeptInstances, directly, for a process that holds NO Lease object and acts for an epoch it has resolved or fenced first:
//     the release verb (runGPURelease, which resolves its target with ReleaseTarget before it stops anything and stops for that
//     target), and the detached reserve's release after a host-RAM wait (releaseAfterRAMWait, which returns before it stops
//     anything unless the record still holds its epoch). The detached parent never holds the lease (its hidden holder does), so it
//     has no object to check; an epoch is never reused and each instance's marker names the lease that launched it, so a stop by
//     epoch leaves an instance that a later lease took over (comfyinst.stopInstance re-reads the marker at the moment of the stop).
//
// Anything else is a stop nobody has examined.

type stopSite struct {
	file, fn, callee string
	arg              string // the epoch (or lease) argument, as written
}

// packageStopCalls parses every non-test .go file of the package and returns each call of the stop functions with the function
// that encloses it, any use of those names that is not a call (a function value would dodge a list of call sites), and the body of
// every function by name.
func packageStopCalls(t *testing.T) (calls []stopSite, nonCalls []string, bodies map[string]string) {
	t.Helper()
	names := map[string]bool{"stopKeptInstances": true, "stopInstancesOfLease": true, "comfyStopFn": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	bodies = map[string]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text := func(a, b token.Pos) string { return string(src[fset.Position(a).Offset:fset.Position(b).Offset]) }
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			bodies[fd.Name.Name] = text(fd.Body.Pos(), fd.Body.End())
			called := map[*ast.Ident]bool{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := c.Fun.(*ast.Ident); ok && names[id.Name] {
					called[id] = true
					arg := ""
					if len(c.Args) > 1 {
						arg = text(c.Args[1].Pos(), c.Args[1].End())
					}
					calls = append(calls, stopSite{file: name, fn: fd.Name.Name, callee: id.Name, arg: arg})
				}
				return true
			})
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && names[id.Name] && !called[id] {
					nonCalls = append(nonCalls, fmt.Sprintf("%s: %s in %s", name, id.Name, fd.Name.Name))
				}
				return true
			})
		}
	}
	return calls, nonCalls, bodies
}

func TestInstancesAreStoppedOnlyByCallersThatFencedTheirEpoch(t *testing.T) {
	calls, nonCalls, bodies := packageStopCalls(t)
	// callee -> the functions that may call it
	allowed := map[string][]string{
		"stopInstancesOfLease": {"runGPUReserve", "runGPUHold", "releaseHolderAtWaitEnd"},
		"stopKeptInstances":    {"stopInstancesOfLease", "runGPURelease", "releaseAfterRAMWait"},
		"comfyStopFn":          {"stopKeptInstances"},
	}
	has := func(list []string, s string) bool {
		for _, x := range list {
			if x == s {
				return true
			}
		}
		return false
	}
	seen := map[string]bool{}
	for _, c := range calls {
		seen[c.callee+" in "+c.fn] = true
		if !has(allowed[c.callee], c.fn) {
			t.Errorf("%s: %s calls %s, and nothing here shows it fenced the epoch first. Go through stopInstancesOfLease, or add the caller to this test WITH the reason it is safe", c.file, c.fn, c.callee)
		}
	}
	for _, n := range nonCalls {
		t.Errorf("a stop function used as a value cannot be pinned by its call sites: %s", n)
	}
	for callee, fns := range allowed {
		for _, fn := range fns {
			if !seen[callee+" in "+fn] {
				t.Errorf("this test allows %s to call %s but it does not: the list is stale, and would pass a call added back there", fn, callee)
			}
		}
	}

	// What makes each caller safe is part of the pin: the epoch it stops for, and the fence that precedes the stop.
	for _, c := range calls {
		switch {
		case c.callee == "stopInstancesOfLease" && c.arg != "lease":
			t.Errorf("%s stops instances for %q, not for the lease object it holds", c.fn, c.arg)
		case c.callee == "stopKeptInstances" && c.fn == "runGPURelease" && c.arg != "target":
			t.Errorf("the release verb stops instances for %q, not for the epoch it resolved (target)", c.arg)
		case c.callee == "stopKeptInstances" && c.fn == "releaseAfterRAMWait" && c.arg != "epoch":
			t.Errorf("the detached reserve's release stops instances for %q, not for the epoch it fenced", c.arg)
		}
	}
	fencedBefore := func(fn, fence, callee string) {
		body := bodies[fn]
		f, k := strings.Index(body, fence), strings.Index(body, callee+"(")
		if f < 0 || k < 0 || f > k {
			t.Errorf("%s must fence the epoch (%s) before it calls %s", fn, fence, callee)
		}
	}
	fencedBefore("runGPURelease", "ReleaseTarget(", "stopKeptInstances")
	fencedBefore("releaseAfterRAMWait", "HoldsEpoch(epoch)", "stopKeptInstances")
	fencedBefore("stopInstancesOfLease", "lease.Check()", "stopKeptInstances")
}
