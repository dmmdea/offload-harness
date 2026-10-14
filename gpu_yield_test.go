package main

// The holders' side of the yield request (plan P11, ADR 0086): the wrapper form and the detached
// holder ask an overdue lease's holder to yield when someone waits for its cards, run the job's own
// recorded --on-yield once, and never stop the command, release the lease or kill anything. Every
// test holds a lease on a scratch state root (leaseFixture); the on_yield runner is replaced by a
// stub unless the test is about the shell itself; no test touches a real lease directory, a GPU or
// nvidia-smi. The tests that wait for a deadline are timing-sensitive: a 1 s window and a 150 ms
// tick, bounded by generous ceilings, never by sleeps alone.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// yieldStub stands in for the shell: it records what the holder ran, with the yield sidecar as it
// was at that instant, and answers a chosen exit status.
type yieldStub struct {
	mu    sync.Mutex
	calls []yieldStubCall
	exit  int
}

type yieldStubCall struct {
	line    string
	env     []string
	sidecar string
}

func (s *yieldStub) run(line string, env []string) (int, error) {
	call := yieldStubCall{line: line, env: append([]string(nil), env...)}
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, gpulease.YieldFileEnv+"="); ok {
			if b, err := os.ReadFile(v); err == nil {
				call.sidecar = string(b)
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call)
	return s.exit, nil
}

func (s *yieldStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *yieldStub) first(t *testing.T) yieldStubCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		t.Fatal("the holder ran no on_yield")
	}
	return s.calls[0]
}

func stubYieldShell(t *testing.T) *yieldStub {
	t.Helper()
	s := &yieldStub{}
	old := yieldShell
	yieldShell = s.run
	t.Cleanup(func() { yieldShell = old })
	return s
}

func yieldEnvValue(env []string, name string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v
		}
	}
	return ""
}

// yieldRootRig sets the process up for a holder that asks: the policy installed, a fast tick, a
// session-less owner. The waiter is planted separately (plantRootWaiter), once the lease is held:
// a waiter registered before the wrapper asks for the card stands in front of it in the line.
func yieldRootRig(t *testing.T, policy string) (cfg string, m *gpulease.Manager) {
	t.Helper()
	termFixedUtil(t, false)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("LOCAL_OFFLOAD_ORIGIN", "")
	old := wrapperTickEvery
	wrapperTickEvery = 150 * time.Millisecond
	t.Cleanup(func() { wrapperTickEvery = old })
	// A holder asks only about a label its term check has looked at again, and that look is a minute away
	// by default: a test that watches a lease being asked moves it within the window of the test.
	oldRecheck := termRecheckEvery
	termRecheckEvery = 250 * time.Millisecond
	t.Cleanup(func() { termRecheckEvery = oldRecheck })
	cfg, m = leaseFixture(t)
	// The policy is the config's: every gpu verb loads the config and Load installs what it says, so a
	// policy installed by hand would be replaced by the unset default the moment the verb ran.
	body := `{"state_dir": ` + strconv.Quote(filepath.Dir(cfg)) + `, "gpu_on_overdue": ` + strconv.Quote(policy) + `}`
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gpulease.SetDefaultOnOverdue("") })
	return cfg, m
}

// afterCommandStarts runs fn once the wrapped command has started, when the lease is held.
func afterCommandStarts(t *testing.T, fn func()) {
	t.Helper()
	old := wrapperCommandStarted
	wrapperCommandStarted = fn
	t.Cleanup(func() { wrapperCommandStarted = old })
}

// plantRootWaiter queues one live waiter on the whole node: a process of its own (the parent of the
// test binary), because the holder's own pid never counts as demand.
func plantRootWaiter(t *testing.T, m *gpulease.Manager) {
	t.Helper()
	dir := filepath.Join(m.Root(), "gpu", "waiters")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	w := gpulease.Waiter{PID: os.Getppid(), Class: gpulease.ClassText, Reason: "kv bench", SinceMs: time.Now().UnixMilli()}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.%d.json", w.PID, w.SinceMs)), b, 0o666); err != nil {
		t.Fatal(err)
	}
}

// TestHelperYieldAware is the wrapped command of these tests (a no-op unless the parent names its
// output file): it reports the yield path it was handed and whether the file already existed, polls
// for the file for up to LO_HELPER_SLEEP_MS, reports whether it appeared, and exits 0.
func TestHelperYieldAware(t *testing.T) {
	out := os.Getenv("LO_HELPER_YIELD_OUT")
	if out == "" {
		return
	}
	p := os.Getenv(gpulease.YieldFileEnv)
	_, statErr := os.Stat(p)
	lines := []string{"path=" + p, fmt.Sprintf("existed-at-start=%v", p != "" && statErr == nil)}
	ms, _ := strconv.Atoi(os.Getenv("LO_HELPER_SLEEP_MS"))
	deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
	saw := false
	for p != "" && time.Now().Before(deadline) {
		if _, err := os.Stat(p); err == nil {
			saw = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	lines = append(lines, fmt.Sprintf("saw-yield=%v", saw))
	_ = os.WriteFile(out, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func yieldReserveArgs(cfg string, extra []string, helper string) []string {
	return append(append([]string{"--config", cfg, "--class", "media", "--for", "1s", "--wait", "0", "--reason", "wrapped"}, extra...), helperCmdFor(helper)...)
}

// ---------------------------------------------------------------------------
// The wrapper form
// ---------------------------------------------------------------------------

// THE WHOLE ROUND, with a job that cooperates: the lease outlives its second, somebody waits, the
// holder is asked once, runs the on_yield the job recorded (and nothing else), the job sees the
// file it was told about, finishes by itself, and the wrapper's own end releases the lease.
func TestWrapperRunsItsOwnOnYieldOnceAndTheJobSeesTheYieldFile(t *testing.T) {
	cfg, m := yieldRootRig(t, "request")
	stub := stubYieldShell(t)
	afterCommandStarts(t, func() { plantRootWaiter(t, m) })
	probe := filepath.Join(t.TempDir(), "probe.txt")
	t.Setenv("LO_HELPER_YIELD_OUT", probe)
	t.Setenv("LO_HELPER_SLEEP_MS", "20000")
	var err error
	stderr := captureStderr(t, func() {
		err = runGPUReserve(yieldReserveArgs(cfg, []string{"--on-yield", "touch work/STOP", "--yield-grace", "5m"}, "TestHelperYieldAware"))
	})
	if err != nil {
		t.Fatalf("the wrapper: %v\n%s", err, stderr)
	}
	// The helper runs in the holder's goroutine; the job may end before it has returned.
	termWaitFor(t, "the holder's on_yield to have run", 10*time.Second, func() bool { return stub.count() >= 1 })
	time.Sleep(200 * time.Millisecond) // and a second run, if there were one, would have started by now
	if stub.count() != 1 {
		t.Fatalf("on_yield ran %d times, want exactly once\n%s", stub.count(), stderr)
	}
	call := stub.first(t)
	if call.line != "touch work/STOP" {
		t.Errorf("ran %q", call.line)
	}
	epoch := yieldEnvValue(call.env, "GPU_LEASE_EPOCH")
	if epoch == "" || yieldEnvValue(call.env, "GPU_LEASE_DIR") != m.Dir() {
		t.Errorf("the on_yield environment names the lease: %v", call.env)
	}
	n, _ := strconv.ParseUint(epoch, 10, 64)
	want := gpulease.YieldFilePath(m.Dir(), n)
	if yieldEnvValue(call.env, gpulease.YieldFileEnv) != want {
		t.Errorf("GPU_LEASE_YIELD_FILE = %q, want %q", yieldEnvValue(call.env, gpulease.YieldFileEnv), want)
	}
	var req gpulease.YieldRequest
	if err := json.Unmarshal([]byte(call.sidecar), &req); err != nil || req.Epoch != n || req.Policy != "request" || len(req.Waiters) != 1 || req.GraceMs != (5*time.Minute).Milliseconds() {
		t.Errorf("the sidecar when on_yield started = %q (%v)", call.sidecar, err)
	}
	got, _ := os.ReadFile(probe)
	for _, line := range []string{"path=" + want, "existed-at-start=false", "saw-yield=true"} {
		if !strings.Contains(string(got), line) {
			t.Errorf("the job's report lacks %q:\n%s", line, got)
		}
	}
	if c := strings.Count(stderr, "asked to yield"); c != 1 {
		t.Errorf("the wrapper says it was asked to yield once, got %d:\n%s", c, stderr)
	}
	for _, bad := range []string{"LEASE LOST", "killing"} {
		if strings.Contains(stderr, bad) {
			t.Errorf("a yield request is not a lost lease (%q):\n%s", bad, stderr)
		}
	}
	// The command ended by itself and the wrapper released the lease: the card is free.
	if info := m.Inspect(); info.Held {
		t.Errorf("the lease is still held after the command ended: %+v", info)
	}
}

// The record is shared and anyone who can write the lease directory can rewrite it; the holder runs
// the value it was started with, never what the record says now.
func TestWrapperNeverRunsTheRecordsOnYield(t *testing.T) {
	cfg, m := yieldRootRig(t, "request")
	stub := stubYieldShell(t)
	probe := filepath.Join(t.TempDir(), "probe.txt")
	t.Setenv("LO_HELPER_YIELD_OUT", probe)
	t.Setenv("LO_HELPER_SLEEP_MS", "20000")
	afterCommandStarts(t, func() {
		plantRootWaiter(t, m)
		info := m.Inspect()
		if err := m.Restamp(info.Epoch, func(meta *gpulease.Meta) { meta.OnYield = "echo tampered" }); err != nil {
			t.Errorf("restamp: %v", err)
		}
	})
	var err error
	stderr := captureStderr(t, func() {
		err = runGPUReserve(yieldReserveArgs(cfg, []string{"--on-yield", "touch work/STOP"}, "TestHelperYieldAware"))
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	termWaitFor(t, "the holder's on_yield to have run", 10*time.Second, func() bool { return stub.count() >= 1 })
	if got := stub.first(t).line; got != "touch work/STOP" {
		t.Fatalf("the holder ran %q: it must run its own in-memory value, never the shared record's", got)
	}
}

// The ask cannot be taken back, so the wrapper does not make it on a label its own term check has not looked
// at again. With that look an hour away, a lease that is overdue and labelled, with a waiter on its cards for
// the whole of the command, is not asked; the tests above are the control (the same run with the look 250 ms
// away asks within the second).
func TestWrapperDoesNotAskALabelThatHasNotSurvivedARecheck(t *testing.T) {
	cfg, m := yieldRootRig(t, "request")
	termRecheckEvery = time.Hour
	stub := stubYieldShell(t)
	afterCommandStarts(t, func() { plantRootWaiter(t, m) })
	t.Setenv("LO_HELPER_SLEEP_MS", "3500")
	var err error
	stderr := captureStderr(t, func() {
		err = runGPUReserve(yieldReserveArgs(cfg, []string{"--on-yield", "touch work/STOP"}, "TestHelperSleepMs"))
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "labelled expired") {
		t.Fatalf("setup: the lease was never labelled expired, so the test proves nothing:\n%s", stderr)
	}
	if strings.Contains(stderr, "asked to yield") || stub.count() != 0 {
		t.Errorf("the holder was asked about a label that had not survived a recheck (%d runs):\n%s", stub.count(), stderr)
	}
}

func TestWrapperWithoutOnYieldSaysSoAndKeepsHolding(t *testing.T) {
	cfg, m := yieldRootRig(t, "request")
	stub := stubYieldShell(t)
	afterCommandStarts(t, func() { plantRootWaiter(t, m) })
	t.Setenv("LO_HELPER_SLEEP_MS", "4000")
	// ended is closed once the wrapper has returned and its stderr is in hand.
	ended := make(chan struct{})
	var runErr error
	var stderr string
	go func() {
		defer close(ended)
		stderr = captureStderr(t, func() { runErr = runGPUReserve(yieldReserveArgs(cfg, nil, "TestHelperSleepMs")) })
	}()
	termWaitFor(t, "the holder to be asked", 20*time.Second, func() bool { return yieldSidecar(m) != "" })
	if in := m.Inspect(); !in.Held || in.Yields != gpulease.YieldsWrapper {
		t.Fatalf("the lease is held by a wrapper that stamped its form: %+v", in)
	}
	time.Sleep(600 * time.Millisecond) // the lease and the command go on past the ask
	select {
	case <-ended:
		t.Fatal("the ask ended the command")
	default:
	}
	if !m.Inspect().Held {
		t.Fatal("the ask released the lease")
	}
	select {
	case <-ended:
	case <-time.After(40 * time.Second):
		t.Fatal("the command never finished")
	}
	if runErr != nil {
		t.Fatalf("the command completes normally: %v", runErr)
	}
	if stub.count() != 0 {
		t.Errorf("something was run although no --on-yield was recorded")
	}
	for _, want := range []string{"no --on-yield was recorded", gpulease.YieldFileEnv, "nothing is released or killed"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

// yieldSidecar returns the content of the first yield.<epoch> file in the lease directory, or "".
func yieldSidecar(m *gpulease.Manager) string {
	entries, err := os.ReadDir(m.Dir())
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "yield.") && !strings.HasSuffix(e.Name(), ".tmp") {
			if b, err := os.ReadFile(filepath.Join(m.Dir(), e.Name())); err == nil {
				return string(b)
			}
		}
	}
	return ""
}

// The command may ignore the request and the on_yield may fail: the lease stays held, the command
// keeps running to its own end, nothing is stopped.
func TestNoYieldPathStopsTheCommand(t *testing.T) {
	cfg, m := yieldRootRig(t, "request")
	stub := stubYieldShell(t)
	stub.exit = 3
	afterCommandStarts(t, func() { plantRootWaiter(t, m) })
	t.Setenv("LO_HELPER_SLEEP_MS", "4500")
	var err error
	start := time.Now()
	stderr := captureStderr(t, func() {
		err = runGPUReserve(yieldReserveArgs(cfg, []string{"--on-yield", "stop.sh"}, "TestHelperSleepMs"))
	})
	if err != nil {
		t.Fatalf("a command that ignored the request ends by itself, normally: %v\n%s", err, stderr)
	}
	if el := time.Since(start); el < 4*time.Second {
		t.Fatalf("the command ended after %s: it was cut short (it sleeps 4.5s)", el)
	}
	if stub.count() != 1 {
		t.Errorf("on_yield ran %d times", stub.count())
	}
	for _, want := range []string{"asked to yield", "failed", "exit 3"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	for _, bad := range []string{"LEASE LOST", "killing"} {
		if strings.Contains(stderr, bad) {
			t.Errorf("stderr says %q: the command was stopped", bad)
		}
	}
	if m.Inspect().Held {
		t.Error("the lease is not released when the wrapper ends")
	}
}

// A one-shot verb goes on after a config load error, and the policy it runs under is the one that asks
// nobody: a mistyped gpu_on_overdue is a warning and a hold, never a request.
func TestGPUReserveUnderAnInvalidPolicyStillRunsAndAsksNobody(t *testing.T) {
	cfg, m := yieldRootRig(t, "kill")
	stub := stubYieldShell(t)
	afterCommandStarts(t, func() { plantRootWaiter(t, m) })
	t.Setenv("LO_HELPER_SLEEP_MS", "3500")
	var err error
	stderr := captureStderr(t, func() {
		err = runGPUReserve(yieldReserveArgs(cfg, []string{"--on-yield", "touch work/STOP"}, "TestHelperSleepMs"))
	})
	if err != nil {
		t.Fatalf("a refused gpu_on_overdue is a warning to a one-shot verb, not a refusal to run: %v\n%s", err, stderr)
	}
	for _, want := range []string{"gpu_on_overdue", "request", "hold", "no request is made"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "asked to yield") || stub.count() != 0 {
		t.Errorf("an unrecognised policy asked (%d runs):\n%s", stub.count(), stderr)
	}
}

// ---------------------------------------------------------------------------
// The detached holder
// ---------------------------------------------------------------------------

func TestDetachHolderAsksButNeverReleasesOnYield(t *testing.T) {
	termFastHolder(t)
	cfg, m := yieldRootRig(t, "request")
	stub := stubYieldShell(t)
	done := termHoldFor(t, cfg, "1s", "--on-yield", "stop-the-job", "--yield-grace", "1m")
	l := waitForLeases(t, m, 1)[0]
	plantRootWaiter(t, m)
	if l.Yields != gpulease.YieldsDetached {
		t.Fatalf("a detached holder stamps its form: %q", l.Yields)
	}
	termWaitFor(t, "the holder to be asked and run its on_yield", 20*time.Second, func() bool { return stub.count() == 1 })
	if got := stub.first(t).line; got != "stop-the-job" {
		t.Errorf("ran %q", got)
	}
	// The job is a separate process: the holder keeps the card whatever the request did, until
	// somebody ends the lease. 600 ms is five of its renewals.
	time.Sleep(600 * time.Millisecond)
	if err, gone := termExited(done); gone {
		t.Fatalf("the detached holder returned after the request (%v): it must never release on a yield", err)
	}
	in := m.Inspect()
	if !in.Held || in.Epoch != l.Epoch {
		t.Fatalf("the detached holder let the lease go: %+v", in)
	}
	if stub.count() != 1 {
		t.Errorf("on_yield ran %d times", stub.count())
	}
	if _, err := m.ReleaseByEpoch(l.Epoch); err != nil {
		t.Fatal(err)
	}
	termWaitFor(t, "the holder to see its lease released and exit", 10*time.Second, func() bool { _, ok := termExited(done); return ok })
}

// --release-at-expiry is the pre-terms ending: the holder releases at --for and so never ticks the
// request, and does not claim to.
func TestDetachHolderThatReleasesAtExpiryRunsNoRequest(t *testing.T) {
	termFastHolder(t)
	cfg, m := yieldRootRig(t, "request")
	stub := stubYieldShell(t)
	done := termHoldFor(t, cfg, "1s", "--release-at-expiry", "--on-yield", "stop-the-job")
	l := waitForLeases(t, m, 1)[0]
	plantRootWaiter(t, m)
	if l.Yields != "" {
		t.Errorf("a holder that releases at its deadline never asks, so it stamps no form: %q", l.Yields)
	}
	termWaitFor(t, "the holder to release at its deadline", 15*time.Second, func() bool { _, ok := termExited(done); return ok })
	if stub.count() != 0 {
		t.Error("on_yield ran for a holder that releases at expiry")
	}
}

// What `gpu reserve --detach` prints: the yield file the job can watch, since its environment does
// not carry the path (it is not a child of the holder).
func TestDetachReportNamesTheYieldFile(t *testing.T) {
	dir := t.TempDir()
	info := gpulease.Info{Held: true, Class: gpulease.ClassMedia, Epoch: 9, PID: 4242, ExpiresAt: time.Now().Add(time.Hour), Yields: gpulease.YieldsDetached, OnYield: "touch work/STOP"}
	want := gpulease.YieldFilePath(dir, 9)
	t.Cleanup(func() { gpulease.SetDefaultOnOverdue("") })

	// With the request policy the holder asks, and the report says who runs the command.
	gpulease.SetDefaultOnOverdue("request")
	text := detachedReport(info, dir, false)
	for _, s := range []string{"reserved: media", "epoch 9", "release with: local-offload gpu release --epoch 9", want, "only once the holder is asked", "runs the --on-yield it was given"} {
		if !strings.Contains(text, s) {
			t.Errorf("the report lacks %q:\n%s", s, text)
		}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(detachedReport(info, dir, true)), &out); err != nil {
		t.Fatalf("--json prints one JSON object: %v", err)
	}
	if out["yield_file"] != want || out["epoch"] != float64(9) || out["held"] != true {
		t.Errorf("json = %v", out)
	}
	// A job that recorded no command is told there is nothing to run, not that the holder runs one.
	bare := info
	bare.OnYield = ""
	text = detachedReport(bare, dir, false)
	if !strings.Contains(text, want) || !strings.Contains(text, "no --on-yield was recorded") || strings.Contains(text, "runs the --on-yield it was given") {
		t.Errorf("a lease with no --on-yield: the holder runs nothing and the report says so:\n%s", text)
	}
	// Under the default policy (and hold, and an unrecognised value) nobody is asked: the report promises
	// nothing and is what it was before the request existed.
	for _, policy := range []string{"", "hold", "kill"} {
		gpulease.SetDefaultOnOverdue(policy)
		if strings.Contains(detachedReport(info, dir, false), "yield") || strings.Contains(detachedReport(info, dir, true), "yield") {
			t.Errorf("gpu_on_overdue = %q asks nobody, and the report still talks of a yield request", policy)
		}
	}
	// A holder that cannot be asked has no file to name, and the report is what it was.
	gpulease.SetDefaultOnOverdue("request")
	info.Yields = ""
	if strings.Contains(detachedReport(info, dir, false), "yield") || strings.Contains(detachedReport(info, dir, true), "yield") {
		t.Error("a holder that never asks reports no yield file")
	}
}

// ---------------------------------------------------------------------------
// The shell, for real
// ---------------------------------------------------------------------------

// The line runs through the platform shell in the holder's own directory, with the environment the
// job was told about, and its exit status comes back; a quoted path inside it survives the shell.
func TestYieldShellRunsTheLineWithTheEnvironmentInTheHoldersDirectory(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker dir", "ran.txt") // a space: the quoting must survive
	if err := os.MkdirAll(filepath.Dir(marker), 0o777); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LO_YIELD_MARKER", marker)
	line := `echo "$GPU_LEASE_EPOCH" "$GPU_LEASE_YIELD_FILE" > "$LO_YIELD_MARKER"; pwd >> "$LO_YIELD_MARKER"; exit 3`
	if runtime.GOOS == "windows" {
		line = `(echo %GPU_LEASE_EPOCH% %GPU_LEASE_YIELD_FILE%& cd) > "%LO_YIELD_MARKER%" & exit 3`
	}
	exit, err := runYieldShell(line, []string{"GPU_LEASE_EPOCH=12", "GPU_LEASE_DIR=/x", "GPU_LEASE_YIELD_FILE=/x/yield.12"})
	if err != nil || exit != 3 {
		t.Fatalf("exit %d, err %v: want the line's own status 3 and no error", exit, err)
	}
	b, rerr := os.ReadFile(marker)
	if rerr != nil {
		t.Fatalf("the line did not run: %v", rerr)
	}
	got := strings.ReplaceAll(string(b), "\r", "")
	if !strings.Contains(got, "12 /x/yield.12") {
		t.Errorf("the environment did not reach the line:\n%s", got)
	}
	wd, _ := os.Getwd()
	if !strings.Contains(strings.ToLower(filepath.ToSlash(got)), strings.ToLower(filepath.ToSlash(wd))) && runtime.GOOS != "windows" {
		t.Errorf("the line ran in %q, want the holder's directory %q", got, wd)
	}
	if runtime.GOOS == "windows" && !strings.Contains(strings.ToLower(got), strings.ToLower(filepath.Base(wd))) {
		t.Errorf("the line ran in %q, want the holder's directory %q", got, wd)
	}
}

// A wrapper's stdout is the job's: whatever the helper prints must not land in it (or in the stderr the
// session reads as notifications). The line is run for real, with the process's own streams pointed at files,
// and a marker the line writes proves it ran.
func TestYieldShellDiscardsTheLinesOutput(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran.txt")
	t.Setenv("LO_YIELD_RAN", ran)
	line := `echo helper-chatter-out; echo helper-chatter-err 1>&2; echo ran > "$LO_YIELD_RAN"`
	if runtime.GOOS == "windows" {
		line = `echo helper-chatter-out& echo helper-chatter-err 1>&2& echo ran > "%LO_YIELD_RAN%"`
	}
	out, err := os.Create(filepath.Join(dir, "stdout.txt"))
	if err != nil {
		t.Fatal(err)
	}
	errf, err := os.Create(filepath.Join(dir, "stderr.txt"))
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = out, errf
	exit, runErr := runYieldShell(line, nil)
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = out.Close()
	_ = errf.Close()
	if runErr != nil || exit != 0 {
		t.Fatalf("exit %d, err %v", exit, runErr)
	}
	if _, serr := os.Stat(ran); serr != nil {
		t.Fatalf("the line did not run, so the test proves nothing: %v", serr)
	}
	for _, f := range []string{"stdout.txt", "stderr.txt"} {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		if strings.Contains(string(b), "helper-chatter") {
			t.Errorf("the line's output reached the holder's %s: %q", f, b)
		}
	}
}

// A shell that cannot be started is a failure to start, not an exit status of the line: the holder records it
// with its reason. The shell is made unreachable by pointing PATH at an empty directory.
func TestYieldShellThatCannotBeStartedIsAnErrorNotAStatus(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	exit, err := runYieldShell("true", nil)
	if err == nil || exit != -1 {
		t.Fatalf("exit %d, err %v: a shell that cannot start is exit -1 and an error", exit, err)
	}
}

// ---------------------------------------------------------------------------
// Words
// ---------------------------------------------------------------------------

func TestOwnershipFlagHelpSaysWhoRunsOnYield(t *testing.T) {
	fs := flag.NewFlagSet("gpu reserve", flag.ContinueOnError)
	addOwnershipFlags(fs)
	for _, name := range []string{"on-yield", "yield-grace"} {
		f := fs.Lookup(name)
		if f == nil {
			t.Fatalf("no --%s", name)
		}
		for _, want := range []string{"gpu_on_overdue", "request"} {
			if !strings.Contains(f.Usage, want) {
				t.Errorf("--%s help lacks %q: %q", name, want, f.Usage)
			}
		}
		if strings.Contains(f.Usage, "takeover command") {
			t.Errorf("--%s help still says the takeover command reads it: %q", name, f.Usage)
		}
	}
}
