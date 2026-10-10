package main

// F8 follow-up (2026-10-10): the long renders of this workspace are launched as `cmd /c render-both.cmd` and
// `bash render-card-v3.sh`, with the helper's path inside the script, so a wrapper that recognised only a
// helper named as an argument left exactly those leases labelled "expired ... open to a takeover" at the end
// of their first term. The wrapper now reads the script it was asked to run (gpulease.HeartbeatHelperBehind),
// and `--progress-file auto` asks for the minted file for a command that cannot be read. The scripts here
// carry the lines those two recipes carry, with neutral paths.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

const batchRecipe = "@echo off\r\nrem Runs inside the GPU lease.\r\nset \"R=C:\\tools\\render\\comfy-generate.mjs\"\r\nset \"P=D:\\work\\round2\"\r\nnode \"%R%\" --batch \"%P%\\jobs.jsonl\" --family qwen-image\r\n"

const shellRecipe = "#!/usr/bin/env bash\nset -u\nGEN=\"/opt/tools/render/comfy-generate.mjs\"\nnode \"$GEN\" --batch \"$1\" --family qwen-image\n"

func writeRecipe(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAutoProgressReadsTheScriptACommandRunsAndHonoursAnExplicitAsk(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "progress")
	scripts := t.TempDir()
	batch := writeRecipe(t, scripts, "render-both.cmd", batchRecipe)
	shell := writeRecipe(t, scripts, "render-card.sh", shellRecipe)
	bench := writeRecipe(t, scripts, "bench.sh", "#!/bin/sh\necho bench\n")
	now := time.Unix(1_791_000_000, 5)
	cases := []struct {
		name  string
		opts  gpulease.Options
		cmd   []string
		asked bool
		want  string // "" = nothing declared
	}{
		{"a batch file that runs a helper", gpulease.Options{}, []string{"cmd", "/c", batch}, false, "comfy-generate.mjs (named in render-both.cmd)"},
		{"a shell script that runs a helper", gpulease.Options{}, []string{"bash", shell, "jobs.jsonl"}, false, "comfy-generate.mjs (named in render-card.sh)"},
		{"an inline command line", gpulease.Options{}, []string{"cmd", "/c", `node C:\x\render\comfy-video.mjs out.mp4`}, false, "comfy-video.mjs (named in the inline command)"},
		{"a script that runs no helper", gpulease.Options{}, []string{"bash", bench}, false, ""},
		{"asked for, a command nothing can be read from", gpulease.Options{}, []string{"bash", bench}, true, "the wrapped command"},
		{"asked for, a script that does run a helper", gpulease.Options{}, []string{"cmd", "/c", batch}, true, "comfy-generate.mjs (named in render-both.cmd)"},
		{"the caller's own file wins over the ask", gpulease.Options{ProgressFile: "/abs/log.jsonl", Stall: time.Hour}, []string{"bash", bench}, true, ""},
	}
	for _, c := range cases {
		path, what, ok := autoProgressFile(c.opts, c.cmd, dir, 77, now, c.asked)
		if c.want == "" {
			if ok || path != "" {
				t.Errorf("%s: declared %q (%s)", c.name, path, what)
			}
			continue
		}
		if !ok || what != c.want {
			t.Errorf("%s: ok=%v what=%q, want %q", c.name, ok, what, c.want)
		}
	}
}

// THE RECIPE FORM, END TO END: the helper is named inside the script the wrapper was asked to run, so the
// wrapper reads the script, declares the contract and hands the file down, exactly as for the direct form.
func TestWrapperDeclaresTheContractForAScriptThatRunsAHelper(t *testing.T) {
	cfg, m := progressTestFixture(t, 15*time.Minute)
	recipe := writeRecipe(t, t.TempDir(), "render-both.cmd", batchRecipe)
	envOut := filepath.Join(t.TempDir(), "env.txt")
	t.Setenv("LO_HELPER_ENV_OUT", envOut)
	t.Setenv("LO_HELPER_RUN_MS", "500")
	t.Setenv("LO_HELPER_BEAT_MS", "100")
	done := startReserve(t, cfg, append([]string{"--for", "1h"}, helperBeatCmd(recipe)...)...)
	waitForLeases(t, m, 1)
	in := m.Inspect()
	if in.Progress == nil || in.Progress.StallMs != autoProgressStall.Milliseconds() {
		t.Fatalf("a script that runs a helper must be handed the contract: %+v", in.Progress)
	}
	minted := in.Progress.File
	r := waitReserve(t, done)
	if r.err != nil {
		t.Fatal(r.err, r.stderr)
	}
	if !strings.Contains(r.stderr, "declaring a progress contract for comfy-generate.mjs (named in render-both.cmd)") {
		t.Errorf("the wrapper must say which helper it found and where:\n%s", r.stderr)
	}
	if body, _ := os.ReadFile(envOut); string(body) != "set:"+minted {
		t.Errorf("the script was handed %q, want the lease's own file %q", body, minted)
	}
	if _, err := os.Stat(minted); !os.IsNotExist(err) {
		t.Errorf("the minted file must go with the command (stat err %v)", err)
	}
}

// A script that runs no helper is judged exactly as before.
func TestWrapperDeclaresNothingForAScriptThatRunsNoHelper(t *testing.T) {
	cfg, m := progressTestFixture(t, 15*time.Minute)
	recipe := writeRecipe(t, t.TempDir(), "bench.sh", "#!/bin/sh\n# renders with comfy-generate.mjs elsewhere\necho bench\n")
	t.Setenv("LO_HELPER_RUN_MS", "400")
	t.Setenv("LO_HELPER_BEAT_MS", "0")
	done := startReserve(t, cfg, append([]string{"--for", "1h"}, helperBeatCmd(recipe)...)...)
	waitForLeases(t, m, 1)
	if in := m.Inspect(); in.Progress != nil {
		t.Errorf("a bench whose comment mentions a helper must not be handed a contract: %+v", in.Progress)
	}
	if r := waitReserve(t, done); r.err != nil || strings.Contains(r.stderr, "progress contract") {
		t.Errorf("err %v, stderr:\n%s", r.err, r.stderr)
	}
}

// `--progress-file auto` is the caller saying "mint it": for a command the scan cannot read, with the stall
// window the caller gave. It is the wrapper form's: a detached hold has no command to hand it to.
func TestProgressFileAutoMintsForAnyWrappedCommand(t *testing.T) {
	cfg, m := progressTestFixture(t, 15*time.Minute)
	envOut := filepath.Join(t.TempDir(), "env.txt")
	t.Setenv("LO_HELPER_ENV_OUT", envOut)
	t.Setenv("LO_HELPER_RUN_MS", "400")
	t.Setenv("LO_HELPER_BEAT_MS", "0")
	done := startReserve(t, cfg, append([]string{"--for", "1h", "--progress-file", "auto", "--stall", "20m"}, helperBeatCmd("bench.sh")...)...)
	waitForLeases(t, m, 1)
	in := m.Inspect()
	if in.Progress == nil || in.Progress.StallMs != (20*time.Minute).Milliseconds() || filepath.Dir(in.Progress.File) != gpulease.ProgressDir(m.Root()) {
		t.Fatalf("--progress-file auto must mint a file under the state root with the caller's window: %+v", in.Progress)
	}
	minted := in.Progress.File
	if strings.EqualFold(filepath.Base(minted), "auto") {
		t.Errorf("the word auto was recorded as the path")
	}
	r := waitReserve(t, done)
	if r.err != nil {
		t.Fatal(r.err, r.stderr)
	}
	if !strings.Contains(r.stderr, "--progress-file auto: minted") {
		t.Errorf("the wrapper says what it minted:\n%s", r.stderr)
	}
	if body, _ := os.ReadFile(envOut); string(body) != "set:"+minted {
		t.Errorf("the command was handed %q, want %q", body, minted)
	}
	if _, err := os.Stat(minted); !os.IsNotExist(err) {
		t.Errorf("a minted file goes with its command (stat err %v)", err)
	}
}

// Without --stall the minted file takes the default window.
func TestProgressFileAutoWithoutStallTakesTheDefaultWindow(t *testing.T) {
	cfg, m := progressTestFixture(t, 15*time.Minute)
	t.Setenv("LO_HELPER_RUN_MS", "300")
	t.Setenv("LO_HELPER_BEAT_MS", "0")
	done := startReserve(t, cfg, append([]string{"--for", "1h", "--progress-file", "auto"}, helperBeatCmd("bench.sh")...)...)
	waitForLeases(t, m, 1)
	if in := m.Inspect(); in.Progress == nil || in.Progress.StallMs != autoProgressStall.Milliseconds() {
		t.Errorf("--progress-file auto with no --stall must use the default window %s: %+v", autoProgressStall, in.Progress)
	}
	if r := waitReserve(t, done); r.err != nil {
		t.Fatal(r.err, r.stderr)
	}
}

func TestProgressFileAutoIsRefusedWhereItMeansNothing(t *testing.T) {
	cfg, _ := leaseFixture(t)
	// A reserve that did NOT refuse would start a hidden holder and wait for it; the timeout turns that into a
	// failure of this test instead of a hang (the refusal comes before any lease is touched).
	refused := make(chan error, 1)
	go func() {
		refused <- runGPUReserve([]string{"--config", cfg, "--class", "media", "--for", "1h", "--detach", "--progress-file", "auto"})
	}()
	select {
	case err := <-refused:
		if err == nil || !strings.Contains(err.Error(), "--detach") {
			t.Errorf("--detach has no command to hand a minted file to: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("--detach --progress-file auto was not refused: the reserve went on to start a hold")
	}
	err := runGPUReserve([]string{"--config", cfg, "--class", "media", "--for", "1h", "--unattended", "--progress-file", "auto", "--stall", "1h", "--", os.Args[0], "-test.run=^$"})
	if err == nil || !strings.Contains(err.Error(), "--unattended") {
		t.Errorf("an unattended lease must name the file it is judged by, not ask for one: %v", err)
	}
}

// A progress directory that cannot be made must not turn into a refusal: the caller's --stall with no file is
// refused by the lease (a window with nothing to watch), so the wrapper drops the window with the file, says so,
// and the command runs as it always did.
func TestProgressFileAutoWithAnUnmakeableDirectoryRunsTheCommandUnjudged(t *testing.T) {
	cfg, m := progressTestFixture(t, 15*time.Minute)
	if err := os.WriteFile(gpulease.ProgressDir(m.Root()), []byte("a file where the directory should be"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LO_HELPER_RUN_MS", "300")
	t.Setenv("LO_HELPER_BEAT_MS", "0")
	done := startReserve(t, cfg, append([]string{"--for", "1h", "--progress-file", "auto", "--stall", "20m"}, helperBeatCmd("bench.sh")...)...)
	waitForLeases(t, m, 1)
	if in := m.Inspect(); in.Progress != nil {
		t.Errorf("no file could be made, so no contract may be recorded: %+v", in.Progress)
	}
	r := waitReserve(t, done)
	if r.err != nil {
		t.Fatalf("a full disk must not refuse the job: %v\n%s", r.err, r.stderr)
	}
	if !strings.Contains(r.stderr, "could not create") {
		t.Errorf("the wrapper says it could not make the directory:\n%s", r.stderr)
	}
}
