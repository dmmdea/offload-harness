package main

// F8 follow-up (2026-10-10): the wrapper creates its minted progress file just before the command starts, not
// at acquisition, and removes it when the command ends. The claim is the reason a long `--drain` (an hour is
// ordinary: `--drain --drain-timeout 90m`) cannot read stalled: a file that existed through the drain would go
// stale 15 minutes in, while the wrapper is only waiting. No test drained, so moving the creation to the
// acquisition would have turned nothing red; this one drains.

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

func TestTheMintedFileDoesNotExistThroughTheDrainAndExistsWhileTheCommandRuns(t *testing.T) {
	f := &drainSwap{}
	f.loaded.Store(true)
	f.inflight.Store(1) // a request in flight: the drain waits
	srv := httptest.NewServer(f.handler("seat"))
	defer srv.Close()
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.json")
	cfg := `{"state_dir": ` + strconv.Quote(root) + `, "endpoint": ` + strconv.Quote(srv.URL) + `, "agent_model": "seat"}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("LOCAL_OFFLOAD_ORIGIN", "")
	termFixedUtil(t, false)
	recipe := writeRecipe(t, t.TempDir(), "render-both.cmd", batchRecipe)
	t.Setenv("LO_HELPER_RUN_MS", "1500")
	t.Setenv("LO_HELPER_BEAT_MS", "100")

	done := make(chan reserveRun, 1)
	go func() {
		var rerr error
		stderr := captureStderr(t, func() {
			rerr = runGPUReserve(append([]string{"--config", cfgPath, "--class", "media", "--wait", "30s", "--drain", "--reason", "render", "--for", "1h"}, helperBeatCmd(recipe)...))
		})
		done <- reserveRun{rerr, stderr}
	}()

	// The drain: the lease is held and declares the contract, and the file it names does not exist yet.
	var declared string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if in := m.Inspect(); in.Held && in.Progress != nil {
			declared = in.Progress.File
			break
		}
	}
	if declared == "" {
		t.Fatal("the lease never declared the progress contract")
	}
	time.Sleep(600 * time.Millisecond) // a drain in progress, not a moment
	if _, err := os.Stat(declared); !os.IsNotExist(err) {
		t.Fatalf("the minted file exists through the drain (stat err %v): it would go stale while the wrapper only waits", err)
	}
	if !m.Inspect().Draining {
		t.Fatal("the scenario must be a lease still draining")
	}

	// The seat goes idle, the command starts, and only then does the file exist.
	f.inflight.Store(0)
	appeared := false
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(declared); err == nil {
			appeared = true
			break
		}
	}
	if !appeared {
		t.Error("the minted file never appeared once the command started")
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("%v\n%s", r.err, r.stderr)
		}
		if !strings.Contains(r.stderr, "declaring a progress contract for comfy-generate.mjs") {
			t.Errorf("the wrapper says what it declared:\n%s", r.stderr)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the wrapper never finished")
	}
	if _, err := os.Stat(declared); !os.IsNotExist(err) {
		t.Errorf("the minted file must be gone when the command ended (stat err %v)", err)
	}
}
