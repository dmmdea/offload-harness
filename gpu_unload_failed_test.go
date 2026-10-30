package main

// Review finding (silent failure): unloadOutcome.freedNothing counted successes only, and unloadOthers logs a failed unload of
// another resident model and goes on. A lease whose seat was cold and whose other resident would not go was therefore told
// "the unload freed nothing on these cards ... the shortage is not the seat's", a claim the unload's own failure contradicts:
// something resident may still hold the memory. The outcome now counts failed unloads, and the wait line and the bound's
// failure say that N unloads failed instead of claiming nothing was there.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// unloadWith runs the unload step against a llama-swap stand-in that lists the given residents (the agent seat "agent-pool" is
// not among them unless named) and returns what the unload reported.
func unloadWith(t *testing.T, f *multiSeatSwap) unloadOutcome {
	t.Helper()
	// The roster route is served too: without it the stand-in cannot tell a cold seat from one it could not read.
	ids := []string{"agent-pool"}
	for m := range f.loaded {
		ids = append(ids, m)
	}
	srv := swapWithRoster(f, ids...)
	t.Cleanup(srv.Close)
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.json")
	cfg := `{"state_dir": ` + strconv.Quote(root) + `, "endpoint": ` + strconv.Quote(srv.URL) + `, "agent_model": "agent-pool"}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	var out unloadOutcome
	if err := maintainSeat(context.Background(), loadCfgPath(cfgPath), maintainOpts{unload: true, out: &out}); err != nil {
		t.Fatalf("a foreign resident that will not go does not fail the unload (it is logged and the lease goes on): %v", err)
	}
	return out
}

func TestAFailedUnloadOfAnotherResidentIsCountedAndNeverReadAsFreedNothing(t *testing.T) {
	// The agent seat is cold; another client's vision seat is resident and will not unload on either route.
	f := newMultiSeatSwap("qwen3.5-9b-vl")
	f.failUnload = map[string]bool{"qwen3.5-9b-vl": true}
	f.failBulk = true
	out := unloadWith(t, f)
	if out.failed != 1 || out.others != 0 || out.seat {
		t.Fatalf("one unload failed, nothing was taken down: %+v", out)
	}
	if out.freedNothing() {
		t.Errorf("an unload that FAILED did not find nothing resident: it must never be read as freed-nothing: %+v", out)
	}

	// Control: with nothing else resident the same cold seat really did free nothing.
	clean := unloadWith(t, newMultiSeatSwap())
	if !clean.freedNothing() || clean.failed != 0 {
		t.Errorf("a cold seat with nothing else resident freed nothing, and nothing failed: %+v", clean)
	}

	// And a mixed unload (one went, one did not) records both.
	g := newMultiSeatSwap("qwen3.5-9b-vl", "whisper-stt")
	g.failUnload = map[string]bool{"qwen3.5-9b-vl": true}
	g.failBulk = true
	mixed := unloadWith(t, g)
	if mixed.others != 1 || mixed.failed != 1 || mixed.freedNothing() {
		t.Errorf("one unloaded and one failed: %+v", mixed)
	}
}

// What the operator reads: the waiting line and the bound's failure say that unloads failed, and neither claims the seat was
// not the cause.
func TestTheWaitLineAndTheBoundSayWhenAnUnloadFailedInsteadOfClaimingNothingWasThere(t *testing.T) {
	short := &gpulease.ErrHostRAM{}
	short.NeedGiB = 30
	short.Why = "waiting for host RAM: needs 30.0 GiB of commit, committed 70.0 of 100.0 GiB commit limit, 8.0 GiB headroom"
	failed := unloadOutcome{ran: true, failed: 2, why: "the seat was not loaded"}
	var out bytes.Buffer
	await := func(o gpulease.AwaitHostRAMOptions) error {
		o.OnWait(short)
		return short
	}
	err := awaitHostRAMBounded(context.Background(), await, time.Now().Add(time.Hour), time.Hour, hostRAMHold{max: time.Second, heldSince: time.Now(), freed: failed}, &out)
	var bound *errHostRAMHoldBound
	if !errors.As(err, &bound) {
		t.Fatalf("the bound ended the wait: %v", err)
	}
	for name, text := range map[string]string{"the waiting line": out.String(), "the failure": err.Error()} {
		if strings.Contains(text, "freed nothing") || strings.Contains(text, "not the seat's") {
			t.Errorf("%s claims the unload found nothing although 2 unloads failed:\n%s", name, text)
		}
		if !strings.Contains(text, "2 unload") || !strings.Contains(text, "failed") {
			t.Errorf("%s must say that 2 unloads failed:\n%s", name, text)
		}
	}
}
