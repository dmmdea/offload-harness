package main

// The wrapper's process tree (plan P10): recorded in the lease record, stopped as a whole when
// the lease is lost, and left running (and claimed) when the wrapper itself dies.
//
// These tests run a REAL wrapper: the test binary re-executed as `gpu reserve`, so a lease loss
// ends in the wrapper's own os.Exit and a kill of the wrapper is a kill of a process. Every lease
// lives in a scratch state root; nothing here touches the machine's real lease directory, a
// card or a ComfyUI instance.

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// TestHelperRunWrapper is the wrapper process of these tests: `gpu reserve` with the arguments
// the parent passes, on a fast tick. A no-op unless the parent names them.
func TestHelperRunWrapper(t *testing.T) {
	raw := os.Getenv("LO_WRAPPER_ARGS")
	if raw == "" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		os.Exit(90)
	}
	wrapperTickEvery = 150 * time.Millisecond
	if ms, err := strconv.Atoi(os.Getenv("LO_WRAPPER_TICK_MS")); err == nil && ms > 0 {
		wrapperTickEvery = time.Duration(ms) * time.Millisecond
	}
	if err := runGPUReserve(args); err != nil {
		os.Stderr.WriteString("wrapper error: " + err.Error() + "\n")
		os.Exit(91)
	}
}

// TestHelperSpawnGrandchild is the wrapped command: it starts a grandchild, reports both pids and
// sleeps. A no-op unless the parent names the pid files.
func TestHelperSpawnGrandchild(t *testing.T) {
	cmdFile, grandFile := os.Getenv("LO_CMD_PIDFILE"), os.Getenv("LO_GRAND_PIDFILE")
	if cmdFile == "" {
		return
	}
	grand := exec.Command(os.Args[0], "-test.run=^TestHelperLongSleep$", "-test.timeout=200s")
	grand.Env = append(os.Environ(), "LO_LONG_SLEEP=1")
	if err := grand.Start(); err != nil {
		os.Exit(92)
	}
	_ = os.WriteFile(grandFile, []byte(strconv.Itoa(grand.Process.Pid)), 0o644)
	_ = os.WriteFile(cmdFile, []byte(strconv.Itoa(os.Getpid())), 0o644)
	time.Sleep(120 * time.Second)
}

func TestHelperLongSleep(t *testing.T) {
	if os.Getenv("LO_LONG_SLEEP") == "" {
		return
	}
	time.Sleep(150 * time.Second)
}

// treeRun is one real wrapper and the processes it started.
type treeRun struct {
	t       *testing.T
	cfg     string
	m       *gpulease.Manager
	wrapper *exec.Cmd
	exited  chan error
	cmdPID  int
	grand   int
	// stderrPath is where the wrapper's stderr goes: a FILE, because the command and its
	// grandchild inherit the wrapper's stderr, and a pipe would keep the parent's Wait open until
	// the last of them exits.
	stderrPath string
}

func (r *treeRun) stderrText() string {
	b, _ := os.ReadFile(r.stderrPath)
	return string(b)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if n, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && n > 0 {
				return n
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
	return 0
}

// writeTreeConfig writes the config a real wrapper runs under over leaseFixture's, and returns the
// ComfyUI directory it names. A test that gives no config gets a scratch state root AND a scratch
// comfy_dir: a config with no comfy_dir resolves, on Windows, to the machine's real ComfyUI
// install, whose launch markers the wrapper would then read.
func writeTreeConfig(t *testing.T, cfgPath string, m *gpulease.Manager, cfgJSON string) string {
	t.Helper()
	if cfgJSON == "" {
		cfgJSON = `{"state_dir": STATE, "comfy_dir": ` + strconv.Quote(t.TempDir()) + `}`
	}
	body := strings.ReplaceAll(cfgJSON, "STATE", strconv.Quote(m.Root()))
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		ComfyDir string `json:"comfy_dir"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("the wrapper's config is not JSON: %v\n%s", err, body)
	}
	return parsed.ComfyDir
}

// machineStateRedirects are the environment variables that point the lease directory or the
// configuration at somewhere other than the scratch root a test gives its wrapper.
var machineStateRedirects = []string{"GPU_LOCK", "LOCAL_OFFLOAD_STATE_DIR", "LOCAL_OFFLOAD_CONFIG"}

// isolateFromMachineState clears them for the test, so the parent's lease Manager and the
// wrapper both resolve the scratch root: a Manager that honoured GPU_LOCK would let the test's
// ReleaseByEpoch act on the machine's real lease. Call it before leaseFixture.
func isolateFromMachineState(t *testing.T) {
	t.Helper()
	for _, name := range machineStateRedirects {
		t.Setenv(name, "")
	}
}

// treeEnv is the environment of a real wrapper: the parent's without the redirects, plus extra.
func treeEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		redirect := false
		for _, bad := range machineStateRedirects {
			redirect = redirect || strings.EqualFold(name, bad)
		}
		if !redirect {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// startTreeRun starts a wrapper whose command starts a grandchild, and returns once both exist.
// extra are more `gpu reserve` flags; cfgJSON replaces the scratch config's body when not empty.
func startTreeRun(t *testing.T, cfgJSON string, extra ...string) *treeRun {
	t.Helper()
	isolateFromMachineState(t)
	cfg, m := leaseFixture(t)
	comfy := writeTreeConfig(t, cfg, m, cfgJSON)
	// The parent reads launch markers from the same scratch directory its wrapper does.
	prevComfy := gpulease.ComfyDir()
	gpulease.SetComfyDir(comfy)
	t.Cleanup(func() { gpulease.SetComfyDir(prevComfy) })
	dir := t.TempDir()
	cmdFile, grandFile := filepath.Join(dir, "cmd.pid"), filepath.Join(dir, "grand.pid")
	args := append([]string{"--config", cfg, "--class", "media", "--for", "1h", "--wait", "0", "--reason", "tree test"}, extra...)
	args = append(args, "--", os.Args[0], "-test.run=^TestHelperSpawnGrandchild$", "-test.timeout=200s")
	b, _ := json.Marshal(args)
	run := &treeRun{t: t, cfg: cfg, m: m, exited: make(chan error, 1), stderrPath: filepath.Join(dir, "wrapper.stderr")}
	errFile, ferr := os.Create(run.stderrPath)
	if ferr != nil {
		t.Fatal(ferr)
	}
	t.Cleanup(func() { _ = errFile.Close() })
	run.wrapper = exec.Command(os.Args[0], "-test.run=^TestHelperRunWrapper$", "-test.timeout=200s")
	run.wrapper.Env = treeEnv("LO_WRAPPER_ARGS="+string(b), "LO_CMD_PIDFILE="+cmdFile, "LO_GRAND_PIDFILE="+grandFile)
	run.wrapper.Stderr = errFile
	if err := run.wrapper.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { run.exited <- run.wrapper.Wait() }()
	t.Cleanup(func() {
		_ = run.wrapper.Process.Kill()
		for _, pid := range []int{run.cmdPID, run.grand} {
			if pid > 0 {
				killProcess(pid)
			}
		}
	})
	run.cmdPID = readPID(t, cmdFile)
	run.grand = readPID(t, grandFile)
	waitForLeases(t, m, 1)
	return run
}

func killProcess(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

func waitDead(t *testing.T, what string, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !gpulease.PIDAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s (pid %d) is still running", what, pid)
}

// recordedTree waits until the lease record carries the tree of the wrapper's command, and
// returns it.
func (r *treeRun) recordedTree() []gpulease.TreeMember {
	r.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, meta, _ := gpulease.InspectDirDetail(r.m.Dir()); meta != nil {
			have := map[int]bool{}
			for _, mem := range meta.Tree {
				have[mem.PID] = true
			}
			if have[r.cmdPID] && have[r.grand] {
				return meta.Tree
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, meta, _ := gpulease.InspectDirDetail(r.m.Dir())
	r.t.Fatalf("the lease record never carried the command (%d) and its grandchild (%d): %+v", r.cmdPID, r.grand, meta)
	return nil
}

// THE WRAPPER RECORDS WHAT IT STARTED: the command and every descendant, each with the start
// identity a reader compares, so a recycled pid is never mistaken for a member.
func TestWrapperRecordsItsTreeInTheLeaseRecord(t *testing.T) {
	run := startTreeRun(t, "")
	tree := run.recordedTree()
	for _, mem := range tree {
		start, ok := gpulease.ProcessStart(mem.PID)
		if !ok || start != mem.StartMs {
			t.Errorf("member %+v: start identity now %d (known %v); the record must carry the one a reader reads", mem, start, ok)
		}
		if mem.PID == os.Getpid() || mem.PID == run.wrapper.Process.Pid {
			t.Errorf("the wrapper itself is not a member of the tree it started: %+v", mem)
		}
	}
}

// The tree is written right after the command starts, not at the first tick: a wrapper that dies
// in its first seconds must still have its command on record. The tick here is an hour long, so
// only the initial write can explain a record.
func TestWrapperRecordsItsCommandBeforeTheFirstTick(t *testing.T) {
	t.Setenv("LO_WRAPPER_TICK_MS", "3600000")
	run := startTreeRun(t, "")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, meta, _ := gpulease.InspectDirDetail(run.m.Dir()); meta != nil {
			for _, mem := range meta.Tree {
				if mem.PID == run.cmdPID {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, meta, _ := gpulease.InspectDirDetail(run.m.Dir())
	t.Fatalf("the command (pid %d) was not on record before the first tick: %+v", run.cmdPID, meta)
}

// THE FIX FOR THE DOUBLE-BOOKING: a lease lost while its command runs ends the command AND what
// it started. Killing the direct child only left the grandchild on a card the next job was about
// to be given.
func TestWrapperKillsGrandchildOnLeaseLost(t *testing.T) {
	run := startTreeRun(t, "")
	epoch := run.m.Inspect().Epoch
	if released, err := run.m.ReleaseByEpoch(epoch); err != nil || !released {
		t.Fatalf("could not take the lease away: released=%v err=%v", released, err)
	}
	waitDead(t, "the grandchild", run.grand, 30*time.Second)
	waitDead(t, "the wrapped command", run.cmdPID, 30*time.Second)
	select {
	case <-run.exited:
	case <-time.After(30 * time.Second):
		t.Fatal("the wrapper never exited after killing its command")
	}
	if !strings.Contains(run.stderrText(), "LEASE LOST") {
		t.Errorf("losing the lease is loud:\n%s", run.stderrText())
	}
}

// THE ACCEPTANCE (plan P10): a wrapper that dies while its job runs leaves the card claimed as a
// tree-orphan, nobody else is handed the card, and the claim frees itself the moment the job is
// gone. A real wrapper is killed; nothing is mocked.
func TestKilledWrapperLiveGrandchildLeavesCardClaimedAsTreeOrphan(t *testing.T) {
	run := startTreeRun(t, "")
	run.recordedTree()
	epoch := run.m.Inspect().Epoch

	if err := run.wrapper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-run.exited
	waitDead(t, "the wrapper", run.wrapper.Process.Pid, 15*time.Second)
	if !gpulease.PIDAlive(run.grand) || !gpulease.PIDAlive(run.cmdPID) {
		t.Fatal("killing the wrapper must not kill its job (that is opt-in: --die-with-wrapper)")
	}

	info := run.m.Inspect()
	if !info.Held || info.Epoch != epoch || !info.TreeOrphan {
		t.Fatalf("the card stays claimed as a tree-orphan: %+v", info)
	}
	if !strings.Contains(info.TreeWhy, "recorded") {
		t.Fatalf("a wrapper of this build leaves the tree it recorded as the evidence, not a guess from the process table: %q", info.TreeWhy)
	}
	_, err := run.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "next job", TTL: time.Hour})
	var held *gpulease.ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("a waiter must not be handed the card while the job runs: %v", err)
	}
	if !strings.Contains(held.Error(), "tree-orphan") || !strings.Contains(held.Error(), "gpu release --epoch "+strconv.FormatUint(epoch, 10)) {
		t.Errorf("the waiter is told what holds the card and the command that frees it: %v", held)
	}

	killProcess(run.grand)
	killProcess(run.cmdPID)
	waitDead(t, "the grandchild", run.grand, 15*time.Second)
	waitDead(t, "the command", run.cmdPID, 15*time.Second)
	got, err := run.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "next job", TTL: time.Hour})
	if err != nil {
		t.Fatalf("the claim frees itself once the job is gone: %v", err)
	}
	_ = got.Release()
}

// KILL_ON_JOB_CLOSE is opt-in. Where the platform has a handle whose closing ends a tree, the
// flag makes a dead wrapper take its job with it; where it has none, the flag is refused up
// front, before the reserve waits in a queue for hours for a guarantee it cannot give.
func TestDieWithWrapperIsOptIn(t *testing.T) {
	if runtime.GOOS != "windows" {
		cfg, _ := leaseFixture(t)
		err := runGPUReserve(append([]string{"--config", cfg, "--wait", "0", "--die-with-wrapper"}, helperCmd()...))
		if err == nil || !strings.Contains(err.Error(), "--die-with-wrapper") {
			t.Fatalf("a platform that cannot honour --die-with-wrapper refuses it, got %v", err)
		}
		return
	}
	run := startTreeRun(t, "", "--die-with-wrapper")
	run.recordedTree()
	if err := run.wrapper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-run.exited
	waitDead(t, "the wrapped command", run.cmdPID, 30*time.Second)
	waitDead(t, "the grandchild", run.grand, 30*time.Second)
}

// A kept ComfyUI instance can change hands: another lease reuses it and re-stamps its launch
// marker. The wrapper that LOST its lease must not take that instance down with its own tree: it
// is another lease's job now. Everything else of its tree goes.
func TestLeaseLostSparesAnInstanceAnotherLeaseTookOver(t *testing.T) {
	leaseLostSparesAnInstance(t)
}

// The same with --die-with-wrapper, where the job object is created kill-on-close. Sparing the instance at the lease-lost
// tick is not enough there: the wrapper's finish() lets go of the job, and closing the last handle of a kill-on-close
// job ends every process still in it, the spared instance included. The wrapper must disarm the kill before it lets go.
func TestLeaseLostSparesAnInstanceAnotherLeaseTookOverEvenWithDieWithWrapper(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("--die-with-wrapper is a Windows job-object guarantee; other platforms refuse it (TestDieWithWrapperIsOptIn)")
	}
	leaseLostSparesAnInstance(t, "--die-with-wrapper")
}

// The instance can change hands after the wrapper's last look at the lease, and the wrapper then ends the ordinary way,
// its command exiting with the lease still its own. The wrapper's finish() lets go of a kill-on-close job all the same,
// so the sparing has to happen there too and not only on the tick that finds the lease gone.
func TestCommandEndsWhileAnotherLeaseHoldsAnInstanceInTheDieWithWrapperJob(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("--die-with-wrapper is a Windows job-object guarantee; other platforms refuse it (TestDieWithWrapperIsOptIn)")
	}
	comfy := t.TempDir()
	run := startTreeRun(t, `{"state_dir": STATE, "comfy_dir": `+strconv.Quote(comfy)+`}`, "--die-with-wrapper")
	epoch := run.m.Inspect().Epoch
	marker, _ := json.Marshal(map[string]any{
		"startedAt": time.Now().Add(-time.Hour).UnixMilli(), "pid": run.grand, "ownerPid": run.cmdPID,
		"args": []string{"main.py"}, "key": "p1", "port": 1, "leaseEpoch": epoch + 100,
	})
	if err := os.WriteFile(filepath.Join(comfy, ".offload-launch-p1.json"), marker, 0o644); err != nil {
		t.Fatal(err)
	}
	killProcess(run.cmdPID) // the command ends; the lease is still this wrapper's
	select {
	case <-run.exited:
	case <-time.After(30 * time.Second):
		t.Fatal("the wrapper never exited after its command ended")
	}
	if !gpulease.PIDAlive(run.grand) {
		t.Fatalf("the instance another lease took over died when the wrapper let go of its kill-on-close job:\n%s", run.stderrText())
	}
	if !strings.Contains(run.stderrText(), "--die-with-wrapper no longer applies") {
		t.Errorf("the wrapper says why the guarantee it was asked for stopped at that instance:\n%s", run.stderrText())
	}
}

func leaseLostSparesAnInstance(t *testing.T, extra ...string) {
	t.Helper()
	comfy := t.TempDir()
	run := startTreeRun(t, `{"state_dir": STATE, "comfy_dir": `+strconv.Quote(comfy)+`}`, extra...)
	epoch := run.m.Inspect().Epoch
	marker, _ := json.Marshal(map[string]any{
		"startedAt": time.Now().Add(-time.Hour).UnixMilli(), "pid": run.grand, "ownerPid": run.cmdPID,
		"args": []string{"main.py"}, "key": "p1", "port": 1, "leaseEpoch": epoch + 100,
	})
	if err := os.WriteFile(filepath.Join(comfy, ".offload-launch-p1.json"), marker, 0o644); err != nil {
		t.Fatal(err)
	}
	if released, err := run.m.ReleaseByEpoch(epoch); err != nil || !released {
		t.Fatalf("could not take the lease away: released=%v err=%v", released, err)
	}
	waitDead(t, "the wrapped command", run.cmdPID, 30*time.Second)
	select {
	case <-run.exited:
	case <-time.After(30 * time.Second):
		t.Fatal("the wrapper never exited")
	}
	if !gpulease.PIDAlive(run.grand) {
		t.Fatal("the instance another lease took over was killed with the wrapper's tree")
	}
	if !strings.Contains(run.stderrText(), "left running") {
		t.Errorf("the wrapper says what it spared and why:\n%s", run.stderrText())
	}
}
