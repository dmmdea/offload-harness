package main

// `gpu release` is an operator's command to end a lease, and it does three things in a fixed
// order: stop the ComfyUI instances kept under the lease, warm the seat back, release. The first
// is destructive (a Wan-class clip is 70-80 minutes of work), so it may only run once the command
// has settled WHICH lease it is ending: with several card leases held and no --epoch the release
// is refused, and nothing the refusal does not own may have been touched. These tests pin that
// order against the shapes of lease directory a card-scoped host produces.

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/comfyinst"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// scopedComfyFixture is a card-scoped host (green reader audit, switch on) with a ComfyUI
// directory configured, so the release verb has instances to look for.
func scopedComfyFixture(t *testing.T) (cfgPath, comfyDir string, m *gpulease.Manager) {
	t.Helper()
	root := t.TempDir()
	comfyDir = filepath.Join(root, "comfy")
	if err := os.MkdirAll(comfyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "gpu"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "gpu", "reader-audit.json"), []byte(`{"result":"green"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath = filepath.Join(root, "config.json")
	cfg := `{"state_dir": ` + strconv.Quote(root) + `, "comfy_dir": ` + strconv.Quote(comfyDir) + `, "gpu_card_scoped_leases": true}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("config", cfgPath, "")
	m, err := openLease(fs)
	if err != nil {
		t.Fatal(err)
	}
	return cfgPath, comfyDir, m
}

func cardLease(t *testing.T, m *gpulease.Manager, reason, card string) *gpulease.Lease {
	t.Helper()
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: reason, Devices: []string{card}, TTL: time.Hour})
	if err != nil {
		t.Fatalf("acquire %s: %v", card, err)
	}
	return l
}

// The operator slip: two card leases are held and `gpu release` is run with no --epoch. The
// release is refused (which lease is meant is the operator's to say) and the refusal must not
// have stopped the instance of either: before this the verb stopped the LOWEST epoch's kept
// ComfyUI first, killing a live job's instance, and then refused.
func TestGPUReleaseWithoutAnEpochAndSeveralLeasesStopsNothing(t *testing.T) {
	cfgPath, _, m := scopedComfyFixture(t)
	calls := recordStops(t, m, []comfyinst.Outcome{{Key: "gaaaa1111", PID: 4242, Port: 8189, Stopped: true}})
	l1 := cardLease(t, m, "first", "gpu-test-0")
	l2 := cardLease(t, m, "second", "gpu-test-1")

	err := runGPURelease([]string{"--config", cfgPath})
	if err == nil || !strings.Contains(err.Error(), "pass --epoch N") {
		t.Fatalf("want the refusal that asks for --epoch, got %v", err)
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("a refused release stopped kept instances: %+v", got)
	}
	info := m.Inspect()
	if !info.HoldsEpoch(l1.Epoch()) || !info.HoldsEpoch(l2.Epoch()) {
		t.Fatalf("a refused release must leave both leases held, held now: %v", info.Epochs)
	}
}

// The refusal must come before the warm-back as well: a seat loaded onto cards two live jobs
// still hold is the incident the warm guard exists for.
func TestGPUReleaseRefusalDoesNotWarmTheSeat(t *testing.T) {
	f := &warmOrderSwap{drainSwap: &drainSwap{}}
	cfgPath, m := warmOrderFixture(t, f)
	cfgPath = scopeWarmFixture(t, cfgPath, m)
	calls := recordStops(t, m, nil)
	cardLease(t, m, "first", "gpu-test-0")
	cardLease(t, m, "second", "gpu-test-1")
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}

	if err := runGPURelease([]string{"--config", cfgPath, "--warm-seat"}); err == nil {
		t.Fatal("two card leases and no --epoch must be refused")
	}
	if f.warms.Load() != 0 || indexOf(f.snapshot(), "warm", 0) >= 0 {
		t.Fatalf("a refused release warmed the seat: %v", eventNames(f.snapshot()))
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("a refused release stopped kept instances: %+v", got)
	}
}

// With exactly one lease held, "whatever is held" is unambiguous and its instances go with it.
func TestGPUReleaseWithoutAnEpochStopsTheOnlyLeasesInstances(t *testing.T) {
	cfgPath, comfyDir, m := scopedComfyFixture(t)
	calls := recordStops(t, m, nil)
	l := cardLease(t, m, "only", "gpu-test-0")

	if err := runGPURelease([]string{"--config", cfgPath}); err != nil {
		t.Fatalf("release: %v", err)
	}
	got := calls()
	if len(got) != 1 || got[0].epoch != l.Epoch() || got[0].dir != comfyDir || !got[0].held {
		t.Fatalf("stop calls = %+v, want one for epoch %d in %s while it was still held", got, l.Epoch(), comfyDir)
	}
	if m.Inspect().Held {
		t.Error("the lease must be released")
	}
}

// Naming an epoch ends that lease and no other: its instances go, the sibling's stay.
func TestGPUReleaseOfANamedEpochStopsOnlyThatLeasesInstances(t *testing.T) {
	cfgPath, _, m := scopedComfyFixture(t)
	calls := recordStops(t, m, nil)
	l1 := cardLease(t, m, "first", "gpu-test-0")
	l2 := cardLease(t, m, "second", "gpu-test-1")

	if err := runGPURelease([]string{"--config", cfgPath, "--epoch", strconv.FormatUint(l2.Epoch(), 10)}); err != nil {
		t.Fatalf("release: %v", err)
	}
	got := calls()
	if len(got) != 1 || got[0].epoch != l2.Epoch() {
		t.Fatalf("stop calls = %+v, want one, for epoch %d", got, l2.Epoch())
	}
	if !m.Inspect().HoldsEpoch(l1.Epoch()) || m.Inspect().HoldsEpoch(l2.Epoch()) {
		t.Errorf("only epoch %d may be released; held now %v", l2.Epoch(), m.Inspect().Epochs)
	}
}

// An epoch nobody holds names no lease, so there is nothing of its to stop.
func TestGPUReleaseOfAnEpochNobodyHoldsStopsNothing(t *testing.T) {
	cfgPath, _, m := scopedComfyFixture(t)
	calls := recordStops(t, m, nil)
	l := cardLease(t, m, "only", "gpu-test-0")

	if err := runGPURelease([]string{"--config", cfgPath, "--epoch", "9999"}); err != nil {
		t.Fatalf("release of an epoch nobody holds: %v", err)
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("stop calls = %+v, want none for an epoch no lease holds", got)
	}
	if !m.Inspect().HoldsEpoch(l.Epoch()) {
		t.Error("the lease that was not named must still be held")
	}
}

// The order the docs promise: the kept instances go before the seat is warmed back (both want
// the VRAM), and both before the release.
func TestGPUReleaseStopsKeptInstancesBeforeItWarmsTheSeatBack(t *testing.T) {
	f := &warmOrderSwap{drainSwap: &drainSwap{}, warmHold: 30 * time.Millisecond}
	cfgPath, m := warmOrderFixture(t, f)
	cfgPath = scopeWarmFixture(t, cfgPath, m)
	old := comfyStopFn
	comfyStopFn = func(context.Context, string, uint64) []comfyinst.Outcome { f.note("stop"); return nil }
	t.Cleanup(func() { comfyStopFn = old })
	holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "detached", TTL: time.Hour, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkSeatWarmOwed("seat"); err != nil {
		t.Fatal(err)
	}

	if err := runGPURelease([]string{"--config", cfgPath, "--warm-seat", "--epoch", strconv.FormatUint(holder.Epoch(), 10)}); err != nil {
		t.Fatalf("release: %v", err)
	}
	ev := f.snapshot()
	stop, warm := indexOf(ev, "stop", 0), indexOf(ev, "warm", 0)
	if stop < 0 || warm < 0 {
		t.Fatalf("want both a stop and a warm, saw %v", eventNames(ev))
	}
	if stop > warm {
		t.Errorf("the seat was warmed back before the kept instances were stopped: %v", eventNames(ev))
	}
	if ev[stop].holder == "" {
		t.Errorf("the instances must be stopped while the lease is still held: %v", eventNames(ev))
	}
}

// A release that is refused AFTER the stop (the epoch lock never cleared) cannot take the stop
// back, so it says so: the operator must not be left thinking the instances are still up.
func TestGPUReleaseThatFailsAfterTheStopSaysTheInstancesWereStopped(t *testing.T) {
	cfgPath, _, m := scopedComfyFixture(t)
	calls := recordStops(t, m, nil)
	l := cardLease(t, m, "only", "gpu-test-0")
	lock := filepath.Join(filepath.Dir(cfgPath), "gpu", "epoch.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil { // fresh, so it reads as contention, not debris
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(lock) })

	err := runGPURelease([]string{"--config", cfgPath, "--epoch", strconv.FormatUint(l.Epoch(), 10)})
	if err == nil {
		t.Fatal("the epoch lock never cleared, so the release must fail")
	}
	if got := calls(); len(got) != 1 {
		t.Fatalf("stop calls = %+v, want the one the operator asked for", got)
	}
	if !strings.Contains(err.Error(), "already stopped") {
		t.Errorf("the failure must say the kept instances were already stopped: %v", err)
	}
	if !m.Inspect().HoldsEpoch(l.Epoch()) {
		t.Error("the lease is still held and the operator is told so")
	}
}

// scopeWarmFixture rewrites a warm fixture's config so the host is card-scoped and has a ComfyUI
// directory, keeping its llama-swap endpoint and seat. Returns the config path.
func scopeWarmFixture(t *testing.T, cfgPath string, m *gpulease.Manager) string {
	t.Helper()
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(cfgPath)
	comfyDir := filepath.Join(root, "comfy")
	if err := os.MkdirAll(comfyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "gpu"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "gpu", "reader-audit.json"), []byte(`{"result":"green"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := strings.TrimSpace(string(b))
	s = strings.TrimSuffix(s, "}") + `, "comfy_dir": ` + strconv.Quote(comfyDir) + `, "gpu_card_scoped_leases": true}`
	if err := os.WriteFile(cfgPath, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	return cfgPath
}
