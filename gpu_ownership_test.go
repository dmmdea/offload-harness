package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// envOf is a getenv over a fixed map, so no test reads the real CLAUDE_CODE_SESSION_ID.
func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func parseOwnership(t *testing.T, args ...string) (*ownershipFlags, *flag.FlagSet) {
	t.Helper()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	of := addOwnershipFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return of, fs
}

// The owner a reserve records is what the caller said, else what the environment says.
func TestReserveOwnerFromFlags(t *testing.T) {
	of, _ := parseOwnership(t, "--owner-session", "sess-flag", "--owner-pid", "4321", "--owner-start-ms", "99")
	var opts gpulease.Options
	of.apply(&opts, envOf(map[string]string{"CLAUDE_CODE_SESSION_ID": "sess-env"}))
	if opts.Owner.Session != "sess-flag" || opts.Owner.PID != 4321 || opts.Owner.StartMs != 99 || opts.Owner.Remote {
		t.Fatalf("flags must win over the environment: %+v", opts.Owner)
	}
	of, _ = parseOwnership(t, "--owner-remote")
	opts = gpulease.Options{}
	of.apply(&opts, envOf(map[string]string{"CLAUDE_CODE_SESSION_ID": "sess-env"}))
	if !opts.Owner.Remote || opts.Owner.Session != "" {
		t.Fatalf("a remote owner is not named by this host's session: %+v", opts.Owner)
	}
}

func TestReserveOwnerFromEnvSessionID(t *testing.T) {
	of, _ := parseOwnership(t)
	var opts gpulease.Options
	of.apply(&opts, envOf(map[string]string{"CLAUDE_CODE_SESSION_ID": "sess-claude"}))
	if opts.Owner.Session != "sess-claude" || opts.Owner.PID != 0 {
		t.Fatalf("the Claude session id is the owner session, with no pid: %+v", opts.Owner)
	}
	// An explicit origin label (a non-Claude caller naming itself) wins, the ledger's rule.
	opts = gpulease.Options{}
	of.apply(&opts, envOf(map[string]string{"LOCAL_OFFLOAD_ORIGIN": "opencode-7", "CLAUDE_CODE_SESSION_ID": "sess-claude"}))
	if opts.Owner.Session != "opencode-7" {
		t.Fatalf("LOCAL_OFFLOAD_ORIGIN wins: %+v", opts.Owner)
	}
	// Nothing in the environment: an unknown owner, never an invented one.
	opts = gpulease.Options{}
	of.apply(&opts, envOf(nil))
	if !opts.Owner.IsZero() {
		t.Fatalf("no environment, no owner: %+v", opts.Owner)
	}
}

// --unattended is the bounded-claim contract: a declared window and a progress contract.
func TestUnattendedReserveNeedsADeclaredWindowAndAProgressContract(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		forGiven bool
		want     string
	}{
		{"no window", []string{"--unattended", "--progress-file", "log.jsonl", "--stall", "2h"}, false, "--for"},
		{"no progress file", []string{"--unattended", "--stall", "2h"}, true, "--progress-file"},
		{"no stall window", []string{"--unattended", "--progress-file", "log.jsonl"}, true, "--stall"},
		{"file without stall", []string{"--progress-file", "log.jsonl"}, false, "--stall"},
		{"stall without file", []string{"--stall", "2h"}, false, "--progress-file"},
		{"yield grace without unattended is fine", []string{"--yield-grace", "5m"}, false, ""},
		{"complete", []string{"--unattended", "--progress-file", "log.jsonl", "--stall", "2h", "--yield-grace", "5m", "--on-yield", "touch STOP"}, true, ""},
		{"attended with a contract", []string{"--progress-file", "log.jsonl", "--stall", "2h"}, false, ""},
	}
	for _, c := range cases {
		of, _ := parseOwnership(t, c.args...)
		err := of.validate(c.forGiven)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: want an error naming %q, got %v", c.name, c.want, err)
		}
	}
	of, _ := parseOwnership(t, "--unattended", "--progress-file", "log.jsonl", "--stall", "2h", "--yield-grace", "5m", "--on-yield", "touch STOP")
	var opts gpulease.Options
	of.apply(&opts, envOf(nil))
	wantProg, _ := filepath.Abs("log.jsonl") // recorded absolute: every reader must find the same file
	if !opts.Unattended || opts.ProgressFile != wantProg || opts.Stall != 2*time.Hour || opts.YieldGrace != 5*time.Minute || opts.OnYield != "touch STOP" {
		t.Fatalf("contract not carried onto the options: %+v", opts)
	}
}

// The detached holder is a NEW process whose parent exits; it cannot find its owner by
// looking at its own parent, so the parent passes the owner (and the contract) explicitly.
func TestHoldChildCapturesParentOwner(t *testing.T) {
	opts := gpulease.Options{
		Reason: "film", Origin: "me", Owner: gpulease.Owner{Session: "sess-parent", PID: 4321, StartMs: 99},
		Unattended: true, ProgressFile: filepath.Join(t.TempDir(), "work", "log.jsonl"), Stall: 2 * time.Hour, YieldGrace: 5 * time.Minute, OnYield: "touch work/STOP",
	}
	args := holdChildArgs("media", 20*time.Hour, 0, opts, "")
	if args[0] != "gpu" || args[1] != "hold" {
		t.Fatalf("the child is spawned as gpu hold: %q", args)
	}
	// The child parses its own flags with the SAME ownership flag set `gpu hold` registers.
	fs := flag.NewFlagSet("gpu hold", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.String("config", "", "")
	fs.String("class", "text", "")
	fs.Duration("for", 0, "")
	fs.Duration("wait", 0, "")
	fs.String("reason", "", "")
	fs.String("origin", "", "")
	fs.Bool("exclusive", false, "")
	fs.Bool("draining", false, "")
	of := addOwnershipFlags(fs)
	if err := fs.Parse(args[2:]); err != nil {
		t.Fatalf("the hold child cannot parse what its parent passed: %v\nargs: %q", err, args)
	}
	var got gpulease.Options
	// The child's environment names ANOTHER session: the parent's owner must win.
	of.apply(&got, envOf(map[string]string{"CLAUDE_CODE_SESSION_ID": "sess-child-env"}))
	if got.Owner != opts.Owner || !got.Unattended || got.ProgressFile != opts.ProgressFile || got.Stall != opts.Stall ||
		got.YieldGrace != opts.YieldGrace || got.OnYield != opts.OnYield {
		t.Fatalf("the child must hold the lease for the parent's owner and contract:\nparent %+v\nchild  %+v", opts, got)
	}
	// A parent with no owner passes none (the child does not invent one from its own parent).
	args = holdChildArgs("text", time.Hour, 0, gpulease.Options{Reason: "x"}, "")
	if strings.Contains(strings.Join(args, " "), "--owner") {
		t.Fatalf("no owner, no owner flags: %q", args)
	}
}

// `gpu owner-flags` prints the flags for the calling tree, ready to splice into a launcher
// that detaches (a process spawned through WMI has no parent to ask).
func TestOwnerFlagsPrintsTheFlagsForTheCallingTree(t *testing.T) {
	line := ownerFlagsLine(envOf(map[string]string{"CLAUDE_CODE_SESSION_ID": "sess-claude"}), 321, func(pid int) (int64, bool) { return 777, pid == 321 })
	if line != "--owner-session=sess-claude --owner-pid=321 --owner-start-ms=777" {
		t.Fatalf("got %q", line)
	}
	// No session in the environment: the pid alone.
	if line := ownerFlagsLine(envOf(nil), 321, func(int) (int64, bool) { return 0, false }); line != "--owner-pid=321" {
		t.Fatalf("got %q", line)
	}
	// A session id with whitespace cannot ride as one token: it is left out, never split.
	if line := ownerFlagsLine(envOf(map[string]string{"LOCAL_OFFLOAD_ORIGIN": "has space"}), 5, func(int) (int64, bool) { return 0, false }); strings.Contains(line, "has") {
		t.Fatalf("an unquotable session must be omitted: %q", line)
	}
	// What it prints parses back to the owner the launcher meant.
	of, _ := parseOwnership(t, strings.Fields(ownerFlagsLine(envOf(map[string]string{"CLAUDE_CODE_SESSION_ID": "s1"}), 9, func(int) (int64, bool) { return 5, true }))...)
	var opts gpulease.Options
	of.apply(&opts, envOf(nil))
	if opts.Owner != (gpulease.Owner{Session: "s1", PID: 9, StartMs: 5}) {
		t.Fatalf("round trip: %+v", opts.Owner)
	}
}

// End to end: the wrapper stamps its owner and contract on the record a reader sees.
func TestReserveStampsOwnerAndContractOnTheRecord(t *testing.T) {
	cfg, m := leaseFixture(t)
	t.Setenv("LO_HELPER_SLEEP_MS", "400")
	prog := filepath.Join(t.TempDir(), "log.jsonl")
	done := make(chan error, 1)
	go func() {
		done <- runGPUReserve(append([]string{"--config", cfg, "--wait", "0", "--reason", "film", "--for", "20h",
			"--owner-session", "sess-e2e", "--owner-pid", strconv.Itoa(os.Getpid()),
			"--unattended", "--progress-file", prog, "--stall", "2h", "--yield-grace", "5m", "--on-yield", "touch STOP"}, helperCmd()...))
	}()
	var info gpulease.Info
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if info = m.Inspect(); info.Held {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !info.Held || info.Owner == nil || info.Owner.Session != "sess-e2e" || info.Owner.PID != os.Getpid() || !info.Unattended ||
		info.Progress == nil || info.Progress.File != prog || info.Progress.StallMs != (2*time.Hour).Milliseconds() ||
		info.YieldGraceMs != (5*time.Minute).Milliseconds() || info.OnYield != "touch STOP" {
		t.Fatalf("the record must carry the owner and the contract: %+v", info)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// `gpu status` says who owns the lease, whether they are there, and what the progress
// file says; --json carries the same, so a session reading it can branch.
func TestGPUStatusShowsOwnerStateAndProgress(t *testing.T) {
	cfg, m := leaseFixture(t)
	leaseDir := filepath.Join(m.Root(), "gpu", "lease")
	prog := filepath.Join(t.TempDir(), "log.jsonl")
	if err := os.WriteFile(prog, []byte("{\"detail\":\"clip 4 of 17\"}\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	_ = os.Chtimes(prog, old, old)
	now := time.Now()
	rec := map[string]any{
		"epoch": 41, "class": "media", "reason": "film clips (synthetic)", "command": "python film.py",
		"holder":         map[string]any{"pid": os.Getpid(), "start_time_ms": 0},
		"acquired_at_ms": now.Add(-5 * time.Hour).UnixMilli(), "expires_at_ms": now.Add(15 * time.Hour).UnixMilli(), "renewed_at_ms": now.UnixMilli(),
		"owner":      map[string]any{"session": "sess-gone", "pid": 2000000000, "tracked": true},
		"unattended": true,
		"progress":   map[string]any{"file": prog, "stall_ms": (2 * time.Hour).Milliseconds()},
	}
	b, _ := json.Marshal(rec)
	if err := os.MkdirAll(leaseDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leaseDir, "meta.json"), b, 0o666); err != nil {
		t.Fatal(err)
	}
	text := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"HELD-STALLED", "owner: session sess-gone", "gone for", ", since ", "unattended", "progress: log.jsonl", "clip 4 of 17", "STALLED", "gpu takeover --epoch 41"} {
		if !strings.Contains(text, want) {
			t.Errorf("status text lacks %q:\n%s", want, text)
		}
	}
	js := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg, "--json"}); err != nil {
			t.Fatal(err)
		}
	})
	var out map[string]any
	if err := json.Unmarshal([]byte(js), &out); err != nil {
		t.Fatalf("status --json is not JSON: %v\n%s", err, js)
	}
	if out["verdict"] != "held-stalled" {
		t.Fatalf("verdict = %v", out["verdict"])
	}
	owner, _ := out["owner"].(map[string]any)
	if owner["session"] != "sess-gone" || out["unattended"] != true {
		t.Fatalf("owner/unattended: %v", out)
	}
	holder, _ := out["activity"].(map[string]any)["holder"].(map[string]any)
	prg, _ := holder["progress"].(map[string]any)
	if holder["owner_state"] != "gone" || holder["stalled"] != true || prg["state"] != "stalled" || prg["detail"] != "clip 4 of 17" {
		t.Fatalf("activity.holder: %v", holder)
	}
}

// A waiter queued behind an unhealthy lease is told which lease, what is wrong, and how to free it.
func TestQueuedWaiterIsToldTheLeaseVerdictAndTheTakeoverCommand(t *testing.T) {
	cfg, m := leaseFixture(t)
	leaseDir := filepath.Join(m.Root(), "gpu", "lease")
	now := time.Now()
	rec := map[string]any{
		"epoch": 41, "class": "media", "reason": "film", "command": "python film.py",
		"holder":         map[string]any{"pid": os.Getpid(), "start_time_ms": 0},
		"acquired_at_ms": now.Add(-9 * time.Hour).UnixMilli(), "expires_at_ms": now.Add(-5 * time.Hour).UnixMilli(), "renewed_at_ms": now.UnixMilli(),
	}
	b, _ := json.Marshal(rec)
	_ = os.MkdirAll(leaseDir, 0o777)
	if err := os.WriteFile(filepath.Join(leaseDir, "meta.json"), b, 0o666); err != nil {
		t.Fatal(err)
	}
	var runErr error
	stderr := captureStderr(t, func() {
		runErr = runGPUReserve(append([]string{"--config", cfg, "--wait", "0", "--reason", "next"}, helperCmd()...))
	})
	if runErr == nil {
		t.Fatal("the card is held: a fail-fast reserve must be refused")
	}
	msg := runErr.Error() + "\n" + stderr
	for _, want := range []string{"held-overdue", "epoch 41", "python film.py", "local-offload gpu takeover --epoch 41"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal lacks %q:\n%s", want, msg)
		}
	}
	// And the queueing form prints it once, as it takes its place in the line.
	stderr = captureStderr(t, func() {
		go func() {
			time.Sleep(300 * time.Millisecond)
			_, _ = m.ReleaseByEpoch(41)
		}()
		t.Setenv("LO_HELPER_SLEEP_MS", "10")
		_ = runGPUReserve(append([]string{"--config", cfg, "--wait", "30s", "--reason", "next"}, helperCmd()...))
	})
	if !strings.Contains(stderr, "held-overdue") || !strings.Contains(stderr, "gpu takeover --epoch 41") {
		t.Fatalf("the queued waiter must be told the verdict and the command:\n%s", stderr)
	}
}

// The node publishes what its lease is doing: main wires the standing reader to the same
// lease directory every acquirer contends on, with the operator's grace.
func TestFleetLeaseStandingReadsTheNodesLease(t *testing.T) {
	cfgPath, m := leaseFixture(t)
	leaseDir := filepath.Join(m.Root(), "gpu", "lease")
	now := time.Now()
	rec := map[string]any{
		"epoch": 41, "class": "media", "reason": "film",
		"holder":         map[string]any{"pid": os.Getpid(), "start_time_ms": 0},
		"acquired_at_ms": now.Add(-5 * time.Hour).UnixMilli(), "expires_at_ms": now.Add(5 * time.Hour).UnixMilli(), "renewed_at_ms": now.UnixMilli(),
		"owner": map[string]any{"session": "sess-gone", "pid": 2000000000, "tracked": true},
	}
	b, _ := json.Marshal(rec)
	_ = os.MkdirAll(leaseDir, 0o777)
	if err := os.WriteFile(filepath.Join(leaseDir, "meta.json"), b, 0o666); err != nil {
		t.Fatal(err)
	}
	mark, _ := json.Marshal(map[string]any{"since_ms": now.Add(-40 * time.Minute).UnixMilli(), "by_pid": 1})
	_ = os.WriteFile(filepath.Join(leaseDir, "orphan.41"), mark, 0o666)

	cfg := loadCfgPath(cfgPath)
	standing := fleetLeaseStanding(cfg)
	info := m.Inspect()
	if st := standing(info); !st.Orphaned || st.Escalation() != "held-orphaned" {
		t.Fatalf("40 minutes gone is past the 15 minute default: %+v", st)
	}
	// The operator's grace applies: an hour of grace makes 40 minutes inside it.
	cfg.GPUOrphanGraceMin = 60
	if st := fleetLeaseStanding(cfg)(info); st.Orphaned {
		t.Fatalf("gpu_orphan_grace_min must reach the node's standing: %+v", st)
	}
}

// A --detach reserve builds its refusal itself (the holder's pid is the one it must compare
// against), so it must say what is wrong with an unhealthy lease like every other waiter.
func TestDetachedReserveBehindAnUnhealthyLeaseIsTold(t *testing.T) {
	cfg, m := leaseFixture(t)
	leaseDir := filepath.Join(m.Root(), "gpu", "lease")
	now := time.Now()
	rec := map[string]any{
		"epoch": 41, "class": "media", "reason": "film", "command": "python film.py",
		"holder":         map[string]any{"pid": os.Getpid(), "start_time_ms": 0},
		"acquired_at_ms": now.Add(-9 * time.Hour).UnixMilli(), "expires_at_ms": now.Add(-5 * time.Hour).UnixMilli(), "renewed_at_ms": now.UnixMilli(),
	}
	b, _ := json.Marshal(rec)
	_ = os.MkdirAll(leaseDir, 0o777)
	if err := os.WriteFile(filepath.Join(leaseDir, "meta.json"), b, 0o666); err != nil {
		t.Fatal(err)
	}
	err := runGPUReserve([]string{"--config", cfg, "--detach", "--for", "1h", "--wait", "0", "--reason", "next"})
	if err == nil {
		t.Fatal("the card is held: a fail-fast detached reserve must be refused")
	}
	for _, want := range []string{"held-overdue", "epoch 41", "local-offload gpu takeover --epoch 41"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the detached refusal lacks %q: %v", want, err)
		}
	}
}

// THE DEFECT: `--progress-file work/log.jsonl` was recorded as typed, and every reader that
// did not share the reserving shell's working directory (offload_status, the fleet node,
// another session's `gpu status`) stat'ed it against its own and read "unknown": a job that
// stalled hours ago was never held-stalled. The reserve resolves the path against ITS
// directory before it is recorded.
func TestRelativeProgressFileSurvivesACwdChange(t *testing.T) {
	cfg, m := leaseFixture(t)
	jobDir, elsewhere := t.TempDir(), t.TempDir()
	prog := filepath.Join(jobDir, "prog.jsonl")
	if err := os.WriteFile(prog, []byte("{\"detail\":\"clip 4 of 17\"}\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(prog, old, old)

	t.Chdir(jobDir)
	of, _ := parseOwnership(t, "--unattended", "--progress-file", "prog.jsonl", "--stall", "10ms")
	if err := of.validate(true); err != nil {
		t.Fatal(err)
	}
	opts := gpulease.Options{Reason: "film", TTL: time.Hour}
	of.apply(&opts, envOf(nil))
	if !filepath.IsAbs(opts.ProgressFile) {
		t.Errorf("the recorded progress file must be absolute, got %q", opts.ProgressFile)
	}
	l, err := m.TryAcquire(gpulease.ClassMedia, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	time.Sleep(60 * time.Millisecond) // the stall window counts from the lease's start at the earliest

	// A reader in another directory still finds the file and reads the stall.
	t.Chdir(elsewhere)
	text := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"HELD-STALLED", "progress: prog.jsonl last activity", "clip 4 of 17", "STALLED"} {
		if !strings.Contains(text, want) {
			t.Errorf("status from another directory lacks %q (a relative path was recorded verbatim?):\n%s", want, text)
		}
	}
	if strings.Contains(text, "unknown") && strings.Contains(text, "progress: prog.jsonl unknown") {
		t.Errorf("the progress file must not read unknown from another directory:\n%s", text)
	}
}

// The detached holder is handed the SAME absolute path its parent recorded, not the typed one.
func TestHoldChildIsHandedTheAbsoluteProgressFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	of, _ := parseOwnership(t, "--unattended", "--progress-file", "prog.jsonl", "--stall", "2h")
	var opts gpulease.Options
	of.apply(&opts, envOf(nil))
	args := holdChildArgs("media", 20*time.Hour, 0, opts, "")
	for i, a := range args {
		if a == "--progress-file" {
			if !filepath.IsAbs(args[i+1]) {
				t.Fatalf("the hold child must get an absolute progress file, got %q", args[i+1])
			}
			return
		}
	}
	t.Fatalf("no --progress-file passed to the hold child: %q", args)
}

// --on-yield is recorded verbatim for the takeover to run, so one that cannot be recorded
// faithfully is refused by the flags, before anything is acquired.
func TestOnYieldTooLongIsRefusedByTheFlags(t *testing.T) {
	of, _ := parseOwnership(t, "--on-yield", "touch "+strings.Repeat("a", gpulease.MaxOnYieldBytes))
	err := of.validate(false)
	if err == nil || !strings.Contains(err.Error(), "--on-yield") {
		t.Fatalf("an over-long --on-yield must be refused, naming the flag: %v", err)
	}
	of, _ = parseOwnership(t, "--on-yield", "touch work/STOP")
	if err := of.validate(false); err != nil {
		t.Fatalf("an ordinary --on-yield is fine: %v", err)
	}
}

// A progress file whose directory does not exist is the typo that makes the contract inert
// for the whole window: say so at reserve time (a warning: the job may create the directory).
func TestProgressFileWithMissingParentWarns(t *testing.T) {
	good := filepath.Join(t.TempDir(), "log.jsonl")
	if w := progressFileWarning(good); w != "" {
		t.Fatalf("an existing directory needs no warning: %q", w)
	}
	bad := filepath.Join(t.TempDir(), "no-such-dir", "log.jsonl")
	w := progressFileWarning(bad)
	if !strings.Contains(w, "does not exist") || !strings.Contains(w, "no-such-dir") {
		t.Fatalf("a missing directory must be named: %q", w)
	}
	if w := progressFileWarning(""); w != "" {
		t.Fatalf("no progress file, no warning: %q", w)
	}
}

// "no owner recorded" is true only when NO owner is recorded. A lease whose record names a
// session that could not be tracked (its MCP server predates the registry: the common case
// today) says what is true: the owner is recorded, but cannot be told from here.
func TestOwnerStatusLineSaysWhyAnOwnerIsUnknown(t *testing.T) {
	line := func(h *gpuactivity.Holder) string {
		return strings.Join(ownershipStatusLines(h, gpulease.Info{Held: true, Epoch: h.Epoch}), "\n")
	}
	none := line(&gpuactivity.Holder{Epoch: 7, OwnerState: "unknown"})
	if !strings.Contains(none, "no owner recorded") || !strings.Contains(none, "owner: unknown") {
		t.Errorf("a lease with no owner says so:\n%s", none)
	}
	untracked := line(&gpuactivity.Holder{Epoch: 7, OwnerState: "unknown", OwnerSession: "sess-untracked",
		OwnerNote: "the session was not in the session registry when the lease was taken, so whether it is still there cannot be told"})
	if strings.Contains(untracked, "no owner recorded") {
		t.Errorf("an owner IS recorded here; 'no owner recorded' contradicts the record:\n%s", untracked)
	}
	for _, want := range []string{"session sess-untracked", "not in the session registry", "never orphaned", "declared window only"} {
		if !strings.Contains(untracked, want) {
			t.Errorf("unknown-owner line lacks %q:\n%s", want, untracked)
		}
	}
	unreadable := line(&gpuactivity.Holder{Epoch: 7, OwnerState: "unknown", OwnerSession: "s", OwnerNote: "the session registry could not be read (open owners: Access is denied), so whether the owner is still there cannot be told"})
	if !strings.Contains(unreadable, "registry could not be read") || strings.Contains(unreadable, "no owner recorded") {
		t.Errorf("a registry failure is named:\n%s", unreadable)
	}
	// A marker that could not be recorded is a warning line, whatever the owner state.
	gone := line(&gpuactivity.Holder{Epoch: 7, OwnerState: "gone", OwnerSession: "s", OrphanGraceS: 900,
		OrphanMarkErr: "orphan marker could not be recorded: the lease directory is not writable from here: read-only file system"})
	if !strings.Contains(gone, "orphan marker could not be recorded") || !strings.Contains(gone, "read-only file system") {
		t.Errorf("an unwritable marker is reported:\n%s", gone)
	}
}

// A progress file that never appeared says how long it has been missing once past the stall
// window, and a stat failure that is not "not found" is named.
func TestProgressStatusLineSaysWhyAFileIsUnknown(t *testing.T) {
	line := func(p *gpuactivity.ProgressState) string {
		h := &gpuactivity.Holder{Epoch: 7, OwnerState: "unknown", Progress: p}
		return strings.Join(ownershipStatusLines(h, gpulease.Info{Held: true, Epoch: 7}), "\n")
	}
	early := line(&gpuactivity.ProgressState{File: "/w/log.jsonl", State: "unknown", Problem: "does not exist", AgeSec: 600, StallSec: 7200})
	if !strings.Contains(early, "progress: log.jsonl unknown") || !strings.Contains(early, "does not exist yet") {
		t.Errorf("inside the window:\n%s", early)
	}
	late := line(&gpuactivity.ProgressState{File: "/w/log.jsonl", State: "unknown", Problem: "does not exist", AgeSec: 3 * 3600, StallSec: 3600})
	for _, want := range []string{"has not appeared in 3h0m0s", "1h0m0s", "path is wrong"} {
		if !strings.Contains(late, want) {
			t.Errorf("past the window the line lacks %q:\n%s", want, late)
		}
	}
	denied := line(&gpuactivity.ProgressState{File: "/w/log.jsonl", State: "unknown", Problem: "cannot be read: Access is denied", AgeSec: 600, StallSec: 7200})
	if !strings.Contains(denied, "cannot be read: Access is denied") || strings.Contains(denied, "does not exist") {
		t.Errorf("a permission failure is named:\n%s", denied)
	}
}

// With several live leases the owner and progress lines of `gpu status` describe the lease
// the verdict is about (the most escalated), labelled with its epoch, not the owner of the
// lowest epoch beside the progress of another.
func TestGPUStatusOwnerLinesDescribeTheEscalatedLease(t *testing.T) {
	cfg, m := leaseFixture(t)
	m.SetCardScoped(true)
	healthy, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "healthy render", Devices: []string{"GPU-TEST-0"}, TTL: time.Hour,
		Owner: gpulease.Owner{Session: "sess-healthy", PID: os.Getpid()}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = healthy.Release() }()
	prog := filepath.Join(t.TempDir(), "log.jsonl")
	if err := os.WriteFile(prog, []byte("{\"detail\":\"clip 4 of 17\"}\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	_ = os.Chtimes(prog, old, old)
	stalled, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", Devices: []string{"GPU-TEST-1"}, TTL: time.Hour,
		Unattended: true, ProgressFile: prog, Stall: 10 * time.Millisecond, Owner: gpulease.Owner{Session: "sess-film", PID: os.Getpid()}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stalled.Release() }()
	time.Sleep(60 * time.Millisecond)

	text := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"standing of lease epoch " + strconv.FormatUint(stalled.Epoch(), 10), "owner: session sess-film", "progress: log.jsonl", "STALLED"} {
		if !strings.Contains(text, want) {
			t.Errorf("status lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "owner: session sess-healthy") {
		t.Errorf("the owner line describes the lowest-epoch lease beside the stalled lease's progress:\n%s", text)
	}

	// The JSON is the same lease's reading: owner, progress and epoch are the stalled lease's,
	// beside the activity.holder standing that is about the same lease, and epochs[] still
	// lists every live lease.
	js := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg, "--json"}); err != nil {
			t.Fatal(err)
		}
	})
	var out map[string]any
	if err := json.Unmarshal([]byte(js), &out); err != nil {
		t.Fatalf("status --json is not JSON: %v\n%s", err, js)
	}
	owner, _ := out["owner"].(map[string]any)
	prg, _ := out["progress"].(map[string]any)
	if owner["session"] != "sess-film" || prg["file"] != prog || out["unattended"] != true || out["epoch"] != float64(stalled.Epoch()) {
		t.Errorf("--json describes the lowest-epoch lease beside the stalled lease's standing: owner=%v progress=%v unattended=%v epoch=%v (stalled epoch %d)",
			out["owner"], out["progress"], out["unattended"], out["epoch"], stalled.Epoch())
	}
	holder, _ := out["activity"].(map[string]any)["holder"].(map[string]any)
	if holder["epoch"] != out["epoch"] {
		t.Errorf("activity.holder and the top-level lease must be the same lease: %v vs %v", holder["epoch"], out["epoch"])
	}
	if eps, _ := out["epochs"].([]any); len(eps) != 2 {
		t.Errorf("epochs[] still lists every live lease: %v", out["epochs"])
	}
}

// A lease with NO owner can still be labelled with who asked for it (--origin). `gpu status`
// called such a lease "owner: unknown — no owner recorded" and never printed the origin, so a
// bench launched from a unit with no session in its environment was identified by `reason:`
// alone, and two sessions mixed up who held the card (F9, 2026-10-07). The label is shown in the
// owner line; it is NOT an owner, so it never displaces a recorded one and the lease stays
// never-orphaned, judged by its window.
func TestOwnerStatusLineFallsBackToTheOriginWhenNoOwnerIsRecorded(t *testing.T) {
	const origin = "claude bench-run-3 gpu-tuning"
	line := func(h *gpuactivity.Holder) string {
		return strings.Join(ownershipStatusLines(h, gpulease.Info{Held: true, Epoch: h.Epoch}), "\n")
	}
	got := line(&gpuactivity.Holder{Epoch: 7, OwnerState: "unknown", Origin: origin})
	for _, want := range []string{`owner: none recorded`, `origin "` + origin + `"`, "never orphaned", "declared window only"} {
		if !strings.Contains(got, want) {
			t.Errorf("a lease with an origin and no owner lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"owner: unknown", "no owner recorded"} {
		if strings.Contains(got, bad) {
			t.Errorf("the origin is known, so the line must not say %q:\n%s", bad, got)
		}
	}

	// The origin is a label, never a replacement for a recorded owner.
	for name, h := range map[string]*gpuactivity.Holder{
		"session": {Epoch: 7, OwnerState: "alive", OwnerSession: "sess-real", Origin: origin},
		"process": {Epoch: 7, OwnerState: "alive", OwnerPID: 4242, Origin: origin},
		"remote":  {Epoch: 7, OwnerState: "remote", Origin: origin},
		"untracked session": {Epoch: 7, OwnerState: "unknown", OwnerSession: "sess-untracked", Origin: origin,
			OwnerNote: "the session was not in the session registry when the lease was taken, so whether it is still there cannot be told"},
	} {
		if text := line(h); strings.Contains(text, "none recorded") || strings.Contains(text, origin) {
			t.Errorf("%s: a recorded owner is named as the owner, not displaced by the origin:\n%s", name, text)
		}
	}

	// No origin, or one that is only whitespace: the line is byte-for-byte what it was.
	for _, blank := range []string{"", "  \t "} {
		text := line(&gpuactivity.Holder{Epoch: 7, OwnerState: "unknown", Origin: blank})
		if !strings.Contains(text, "owner: unknown — no owner recorded: never orphaned, judged by its declared window only") {
			t.Errorf("origin %q: the line without a label changed:\n%s", blank, text)
		}
	}

	// An origin is free text from a flag: it prints quoted, on one line.
	text := line(&gpuactivity.Holder{Epoch: 7, OwnerState: "unknown", Origin: "a \"b\"\nc"})
	if !strings.Contains(text, `origin "a \"b\"\nc"`) || strings.Count(text, "\n") != 0 {
		t.Errorf("a hostile origin must print as one quoted line:\n%q", text)
	}
}

// useRealLeaseReadNoGPU keeps the REAL lease read behind `gpu status` (the holder's standing,
// which useQuietStatus's synthetic view leaves out, so no owner line prints under it) and drops
// every live read: no nvidia-smi, no process table, no llama-swap, no card table.
func useRealLeaseReadNoGPU(t *testing.T) {
	t.Helper()
	oldCards, oldAct, oldForeign := cardTableFn, statusActivityFn, statusForeignFn
	cardTableFn = func(context.Context, config.Config) ([]gpuprobe.Card, string, error) {
		return nil, "", errors.New("synthetic: no card table")
	}
	statusActivityFn = func(ctx context.Context, o gpuactivity.Options) gpuactivity.View {
		o.SampleGPU, o.Endpoint, o.Seat = false, "", ""
		return gpuactivity.Snapshot(ctx, o)
	}
	statusForeignFn = func(context.Context, config.Config) []ForeignGPUHolder { return nil }
	t.Cleanup(func() { cardTableFn, statusActivityFn, statusForeignFn = oldCards, oldAct, oldForeign })
}

// End to end through the verb, on a record shaped like the F9 incident's: a text bench taken
// with --origin and no session, so no owner is recorded. The text names the origin in the owner
// line; the JSON carries it where it already was (origin, activity.holder.origin) and the lease
// is still unknown-owner and not orphaned.
func TestGPUStatusNamesTheOriginWhenTheLeaseRecordsNoOwner(t *testing.T) {
	const origin = "claude bench-run-3 gpu-tuning"
	cfg, m := leaseFixture(t)
	useRealLeaseReadNoGPU(t)
	now := time.Now()
	rec := map[string]any{
		"epoch": 41, "class": "text", "reason": "kv bench (synthetic)", "origin": origin, "command": "python bench.py",
		"holder":         map[string]any{"pid": os.Getpid(), "start_time_ms": 0},
		"acquired_at_ms": now.Add(-5 * time.Minute).UnixMilli(), "expires_at_ms": now.Add(2 * time.Hour).UnixMilli(), "renewed_at_ms": now.UnixMilli(),
	}
	b, _ := json.Marshal(rec)
	leaseDir := filepath.Join(m.Root(), "gpu", "lease")
	if err := os.MkdirAll(leaseDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leaseDir, "meta.json"), b, 0o666); err != nil {
		t.Fatal(err)
	}

	text := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"GPU: held by a text-class lease", "reason: kv bench (synthetic)", `owner: none recorded — origin "` + origin + `"`, "never orphaned"} {
		if !strings.Contains(text, want) {
			t.Errorf("status text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "owner: unknown") {
		t.Errorf("the origin is on the record, so the owner line must not read unknown:\n%s", text)
	}

	js := captureStdout(t, func() {
		if err := runGPUStatus([]string{"--config", cfg, "--json"}); err != nil {
			t.Fatal(err)
		}
	})
	var out map[string]any
	if err := json.Unmarshal([]byte(js), &out); err != nil {
		t.Fatalf("status --json is not JSON: %v\n%s", err, js)
	}
	holder, _ := out["activity"].(map[string]any)["holder"].(map[string]any)
	if out["origin"] != origin || holder["origin"] != origin {
		t.Errorf("the origin must be on the wire at origin and activity.holder.origin: %v / %v", out["origin"], holder["origin"])
	}
	if _, ok := out["owner"]; ok || holder["owner_state"] != "unknown" || holder["orphaned"] == true {
		t.Errorf("an origin is a label, not an owner: owner=%v owner_state=%v orphaned=%v", out["owner"], holder["owner_state"], holder["orphaned"])
	}
	if v, _ := out["verdict"].(string); v == "held-orphaned" {
		t.Errorf("a lease with no owner is never orphaned, whatever its origin: verdict %v", v)
	}
}
