package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// TestHelperSleepMs is a helper process: a no-op unless the parent asks for it, then
// it stays alive for LO_HELPER_SLEEP_MS so a wrapper has something to hold the card
// around. Exit 0 either way, so `gpu reserve -- <this>` reports success.
func TestHelperSleepMs(t *testing.T) {
	ms, _ := strconv.Atoi(os.Getenv("LO_HELPER_SLEEP_MS"))
	if ms > 0 {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
}

// leaseFixture writes a config whose state_dir is a fresh temp root, so the CLI under
// test and the in-test holder arbitrate the SAME lease directory and nothing touches
// the machine's real %ProgramData% lease.
func leaseFixture(t *testing.T) (cfgPath string, m *gpulease.Manager) {
	t.Helper()
	root := t.TempDir()
	cfgPath = filepath.Join(root, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"state_dir": `+strconv.Quote(root)+`}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatalf("open lease at %s: %v", root, err)
	}
	return cfgPath, m
}

func helperCmd() []string {
	return []string{"--", os.Args[0], "-test.run=TestHelperSleepMs", "-test.timeout=30s"}
}

// THE DEFECT: a held card was an ERROR, and every session that hit it read "busy" as
// "refuse the work". Failing fast is now something the caller has to ASK for, and the
// refusal names the flag that would have queued instead.
func TestGPUReserveFailsFastOnlyWhenAskedTo(t *testing.T) {
	cfg, m := leaseFixture(t)
	holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "other session's bench", TTL: time.Hour})
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}
	defer func() { _ = holder.Release() }()

	args := append([]string{"--config", cfg, "--wait", "0"}, helperCmd()...)
	err = runGPUReserve(args)
	if err == nil {
		t.Fatal("--wait 0 against a held card must fail fast")
	}
	for _, want := range []string{"other session's bench", "pass --wait", "declared until"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("fail-fast error must carry %q; got: %v", want, err)
		}
	}

	// A short wait against a holder whose DECLARED window outlasts it returns at
	// once (gpulease.Acquire's short-circuit) — and says how to queue anyway.
	start := time.Now()
	args = append([]string{"--config", cfg, "--wait", "300ms"}, helperCmd()...)
	err = runGPUReserve(args)
	if err == nil {
		t.Fatal("a 300ms wait against a 1h text hold must come back held")
	}
	if !strings.Contains(err.Error(), "not free within --wait 300ms") || !strings.Contains(err.Error(), "longer than the holder's declared window") {
		t.Errorf("the refusal must name the wait and the fix; got: %v", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("short-circuit took %s; a declared window past the wait must not be polled", el)
	}
}

// THE FIX: with the default wait the wrapper QUEUES behind the holder, prints one
// line, and runs the command the moment the card frees. The holder is media on purpose
// — a media window is a ceiling, not a promise, so it is waited out rather than
// short-circuited (the same reason TestAcquireWaitsOutAHolderThenTakesTheCard uses it).
func TestGPUReserveQueuesBehindAHolderThenRunsTheCommand(t *testing.T) {
	cfg, m := leaseFixture(t)
	holder, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render", TTL: time.Hour})
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(400 * time.Millisecond)
		_ = holder.Release()
		close(released)
	}()

	t.Setenv("LO_HELPER_SLEEP_MS", "0")
	args := append([]string{"--config", cfg, "--wait", "10s", "--reason", "queued bench"}, helperCmd()...)
	start := time.Now()
	if err := runGPUReserve(args); err != nil {
		t.Fatalf("reserve should have queued and then run: %v", err)
	}
	<-released
	if el := time.Since(start); el < 400*time.Millisecond {
		t.Errorf("the command ran after %s, before the holder released", el)
	}
	if info := m.Inspect(); info.Held {
		t.Errorf("the wrapper must release on exit; still held: %+v", info)
	}
}

// --unload-seat means the holder CLEARED the cards, and a cleared card the next text
// call refills is not cleared: the lease is stamped exclusive so the load gate holds
// models off it. Pinned through the wrapper's own lease record while the wrapped
// command runs — the flag wiring, not the drain (which needs a llama-swap and has its
// own tests in gpu_drain_test.go).
func TestGPUReserveExclusiveIsStampedOnTheLease(t *testing.T) {
	cfg, m := leaseFixture(t)
	t.Setenv("LO_HELPER_SLEEP_MS", "1500")
	done := make(chan error, 1)
	go func() {
		done <- runGPUReserve(append([]string{"--config", cfg, "--exclusive", "--reason", "5070 bench"}, helperCmd()...))
	}()
	deadline := time.Now().Add(10 * time.Second)
	var seen gpulease.Info
	for time.Now().Before(deadline) {
		if seen = m.Inspect(); seen.Held {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !seen.Held {
		t.Fatal("the wrapper never took the lease")
	}
	if seen.Class != gpulease.ClassText || !seen.Exclusive || seen.Reason != "5070 bench" {
		t.Errorf("lease record = %+v, want an exclusive text hold with the reason", seen)
	}
	if err := <-done; err != nil {
		t.Fatalf("wrapper: %v", err)
	}
}
