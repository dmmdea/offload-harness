package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/comfyinst"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// A kept ComfyUI instance lives no longer than the lease it was launched under, and the
// HOLDER of the lease stops it when it releases (plan P13; internal/comfyinst). No test in
// this package may reach a real ComfyUI directory, so the production stopper is replaced by
// a no-op for the whole package; the tests below install a recording one.
func init() {
	comfyStopFn = func(context.Context, string, uint64) []comfyinst.Outcome { return nil }
}

type stopCall struct {
	dir   string
	epoch uint64
	held  bool // the lease was still held when the stop ran
}

func recordStops(t *testing.T, m *gpulease.Manager, outcomes []comfyinst.Outcome) func() []stopCall {
	t.Helper()
	var mu sync.Mutex
	var calls []stopCall
	old := comfyStopFn
	comfyStopFn = func(_ context.Context, dir string, epoch uint64) []comfyinst.Outcome {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, stopCall{dir: dir, epoch: epoch, held: m.Inspect().Held})
		return outcomes
	}
	t.Cleanup(func() { comfyStopFn = old })
	return func() []stopCall { mu.Lock(); defer mu.Unlock(); return append([]stopCall(nil), calls...) }
}

func comfyFixture(t *testing.T) (cfgPath, comfyDir string, m *gpulease.Manager) {
	t.Helper()
	root := t.TempDir()
	comfyDir = filepath.Join(root, "comfy")
	if err := os.MkdirAll(comfyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath = filepath.Join(root, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"state_dir": `+strconv.Quote(root)+`, "comfy_dir": `+strconv.Quote(comfyDir)+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	return cfgPath, comfyDir, m
}

// TestInstanceStoppedWhenTheReserveWrapperReleases: the wrapped command ends, and before the
// lease is released the wrapper stops the instances kept under that lease's epoch.
func TestInstanceStoppedWhenTheReserveWrapperReleases(t *testing.T) {
	cfgPath, comfyDir, m := comfyFixture(t)
	calls := recordStops(t, m, []comfyinst.Outcome{{Key: "gaaaa1111", PID: 4242, Port: 8189, Stopped: true}})
	t.Setenv("LO_HELPER_SLEEP_MS", "0")

	args := append([]string{"--config", cfgPath, "--wait", "10s", "--reason", "kept"}, helperCmd()...)
	if err := runGPUReserve(args); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	got := calls()
	if len(got) != 1 {
		t.Fatalf("the wrapper stopped instances %d times, want once: %+v", len(got), got)
	}
	if got[0].dir != comfyDir {
		t.Errorf("stopped instances in %q, want the configured comfy_dir %q", got[0].dir, comfyDir)
	}
	if got[0].epoch == 0 {
		t.Error("the stop must name the lease's epoch")
	}
	if !got[0].held {
		t.Error("the instances must be stopped BEFORE the lease is released, or the next holder can find them on its card")
	}
	if m.Inspect().Held {
		t.Error("the lease must be released afterwards")
	}
}

func TestStopKeptInstancesIsASilentNoOpWithoutAComfyDir(t *testing.T) {
	called := false
	old := comfyStopFn
	comfyStopFn = func(context.Context, string, uint64) []comfyinst.Outcome { called = true; return nil }
	t.Cleanup(func() { comfyStopFn = old })
	stopKeptInstances(config.Config{}, 7, os.Stderr)
	if called {
		t.Fatal("an unbound comfy_dir has no instances to stop")
	}
}

// What the operator reads: a stopped instance, and one that was left running with the reason.
func TestStopKeptInstancesReportsWhatItDid(t *testing.T) {
	old := comfyStopFn
	comfyStopFn = func(context.Context, string, uint64) []comfyinst.Outcome {
		return []comfyinst.Outcome{
			{Key: "gaaaa1111", PID: 4242, Port: 8189, Stopped: true},
			{Key: "gbbbb2222", PID: 4343, Port: 8190, Why: "the instance on port 8190 did not answer /system_stats"},
		}
	}
	t.Cleanup(func() { comfyStopFn = old })
	var sb strings.Builder
	stopKeptInstances(config.Config{ComfyDir: "somewhere"}, 7, &sb)
	out := sb.String()
	for _, want := range []string{"stopped", "gaaaa1111", "4242", "epoch 7", "not stopped", "gbbbb2222", "did not answer"} {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
			t.Errorf("output lacks %q: %s", want, out)
		}
	}
}

// `gpu release` ends a lease from outside (a detached holder's, or an operator's): the
// instances kept under the epoch it releases go with it.
func TestGPUReleaseStopsTheInstancesOfTheEpochItReleases(t *testing.T) {
	cfgPath, comfyDir, m := comfyFixture(t)
	calls := recordStops(t, m, nil)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "kept", TTL: 0})
	if err != nil {
		t.Fatal(err)
	}
	epoch := l.Epoch()

	if err := runGPURelease([]string{"--config", cfgPath, "--epoch", strconv.FormatUint(epoch, 10)}); err != nil {
		t.Fatalf("release: %v", err)
	}
	got := calls()
	if len(got) != 1 || got[0].epoch != epoch || got[0].dir != comfyDir {
		t.Fatalf("stop calls = %+v, want one for epoch %d in %s", got, epoch, comfyDir)
	}
	if m.Inspect().Held {
		t.Error("the lease must be released")
	}
}

// Nothing held, nothing to stop: a `gpu release` of a card nobody holds must not go looking
// in the ComfyUI directory for instances of some epoch it guessed.
func TestGPUReleaseOfAFreeCardStopsNothing(t *testing.T) {
	cfgPath, _, m := comfyFixture(t)
	calls := recordStops(t, m, nil)
	if err := runGPURelease([]string{"--config", cfgPath}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("stop calls = %+v, want none when no lease was held", got)
	}
}

// The detached holder is the lease's holder: when it exits at its window (or because the lease
// was taken away) the instances kept under its epoch go with it.
func TestDetachedHolderStopsItsKeptInstancesWhenItExits(t *testing.T) {
	cfgPath, comfyDir, m := comfyFixture(t)
	calls := recordStops(t, m, nil)
	args := holdArgs("media", 1500*time.Millisecond, 0, gpulease.Options{Reason: "kept"}, nil, cfgPath)
	done := make(chan error, 1)
	go func() { done <- runGPUHold(args[2:]) }()
	ls := waitForLeases(t, m, 1)
	epoch := ls[0].Epoch
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 1 || got[0].epoch != epoch || got[0].dir != comfyDir || !got[0].held {
		t.Fatalf("stop calls = %+v, want one for epoch %d in %s while the lease was still held", got, epoch, comfyDir)
	}
}
