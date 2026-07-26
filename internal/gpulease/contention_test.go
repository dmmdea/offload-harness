package gpulease

// Cross-language contention tests: a Go acquirer and the Node acquirer
// (render/gpu-lock.mjs) against ONE lease directory.
//
// This file exists because its absence hid several defects. Both sides were tested in
// isolation and both suites were green while, between them, TWO PARTIES COULD HOLD
// THE LEASE AT ONCE: Go's atomic token was `os.Mkdir(leaseDir)` with the meta written
// several operations later, while Node's token was an O_EXCL write of meta.json over
// a non-exclusive mkdir. Each was internally consistent; together they were not.
//
// THE HOLDER MUST STAY ALIVE. A helper that acquires and exits leaves a lease whose
// holder pid is dead, which is *correctly* reclaimable — asserting mutual exclusion
// against it tests nothing and fails for the wrong reason. Every test below keeps the
// contending process running for the duration of the assertion.

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// nodeHelper drives render/gpu-lock.mjs from a child node process. It prints ONE JSON
// line describing the outcome, then — in hold mode — stays alive so its pid remains a
// live holder while the Go side makes its assertions.
const nodeHelper = `
import { acquireGpuLock, isStale } from %MJS%;
import { readFileSync } from "node:fs";
import { join } from "node:path";
const lockPath = process.argv[2];
const mode = process.argv[3];
const out = (o) => { process.stdout.write(JSON.stringify(o) + "\n"); };
try {
  if (mode === "inspect") {
    let meta = null;
    try { meta = JSON.parse(readFileSync(join(lockPath, "meta.json"), "utf8")); } catch {}
    out({ ok: true, meta, stale: isStale(meta) });
  } else {
    const lock = await acquireGpuLock({ lockPath, waitMs: 0, reason: "node-contender", ttlMs: 600000 });
    out({ ok: true, acquired: lock !== null, epoch: lock ? lock.epoch : null });
    if (mode === "hold" && lock) {
      // Stay alive holding the lease. The parent kills us when it is done.
      await new Promise((r) => setTimeout(r, 120000));
    }
  }
} catch (e) {
  out({ ok: false, error: String((e && e.code) || e) });
}
`

type nodeResult struct {
	OK       bool            `json:"ok"`
	Acquired bool            `json:"acquired"`
	Epoch    *int64          `json:"epoch"`
	Meta     json.RawMessage `json:"meta"`
	Stale    bool            `json:"stale"`
	Error    string          `json:"error"`
}

func nodeScript(t *testing.T) (exe string, script string) {
	t.Helper()
	nodeExe, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cross-language contention cannot be verified here")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	mjs := filepath.Join(repoRoot, "render", "gpu-lock.mjs")
	if _, err := os.Stat(mjs); err != nil {
		t.Fatalf("render/gpu-lock.mjs not found at %s: %v", mjs, err)
	}
	mjsURL := `"file:///` + strings.ReplaceAll(filepath.ToSlash(mjs), " ", "%20") + `"`

	dir := t.TempDir()
	path := filepath.Join(dir, "helper.mjs")
	if err := os.WriteFile(path, []byte(strings.Replace(nodeHelper, "%MJS%", mjsURL, 1)), 0o666); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	return nodeExe, path
}

// startNodeHolder spawns the helper in hold mode, waits for its result line, and
// returns it along with a cleanup that terminates the still-running holder.
func startNodeHolder(t *testing.T, lockPath string) (nodeResult, func()) {
	t.Helper()
	exe, script := nodeScript(t)
	cmd := exec.Command(exe, script, lockPath, "hold")
	cmd.Env = append(os.Environ(), "GPU_LOCK="+lockPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start node holder: %v", err)
	}
	stop := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}

	type lineOrErr struct {
		res nodeResult
		err error
	}
	ch := make(chan lineOrErr, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "{") {
				continue
			}
			var r nodeResult
			if json.Unmarshal([]byte(line), &r) == nil {
				ch <- lineOrErr{res: r}
				return
			}
		}
		ch <- lineOrErr{err: sc.Err()}
	}()

	select {
	case v := <-ch:
		if v.err != nil {
			stop()
			t.Fatalf("reading node holder output: %v", v.err)
		}
		return v.res, stop
	case <-time.After(30 * time.Second):
		stop()
		t.Fatal("node holder produced no result within 30s")
	}
	return nodeResult{}, stop
}

// runNodeOnce runs the helper to completion (inspect mode).
func runNodeOnce(t *testing.T, lockPath, mode string) nodeResult {
	t.Helper()
	exe, script := nodeScript(t)
	cmd := exec.Command(exe, script, lockPath, mode)
	cmd.Env = append(os.Environ(), "GPU_LOCK="+lockPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node helper failed: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var r nodeResult
		if json.Unmarshal([]byte(line), &r) == nil {
			return r
		}
	}
	t.Fatalf("no JSON line from node helper; got:\n%s", out)
	return nodeResult{}
}

func testManagerAt(t *testing.T, root string) *Manager {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "gpu"), 0o777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return &Manager{
		root:         root,
		heartbeatTTL: DefaultHeartbeatTTL,
		now:          time.Now,
		procStart:    processStart,
		pid:          os.Getpid(),
	}
}

// While GO holds the lease, a LIVE Node acquirer must be refused.
func TestNodeCannotAcquireWhileGoHolds(t *testing.T) {
	root := t.TempDir()
	m := testManagerAt(t, root)
	lease, err := m.TryAcquire(ClassText, Options{Reason: "go bench", TTL: 10 * time.Minute})
	if err != nil {
		t.Fatalf("go acquire: %v", err)
	}
	defer func() { _ = lease.Release() }()

	res := runNodeOnce(t, m.leaseDir(), "acquire")
	if !res.OK {
		t.Fatalf("node helper errored: %s", res.Error)
	}
	if res.Acquired {
		t.Fatal("NODE ACQUIRED THE LEASE WHILE GO HELD IT — two holders on one GPU")
	}
}

// And the reverse: while a LIVE Node process holds it, Go must be refused.
func TestGoCannotAcquireWhileNodeHolds(t *testing.T) {
	root := t.TempDir()
	m := testManagerAt(t, root)

	res, stop := startNodeHolder(t, m.leaseDir())
	defer stop()
	if !res.OK || !res.Acquired {
		t.Fatalf("node could not acquire a free lease: %+v", res)
	}
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "go render"}); err == nil {
		t.Fatal("GO ACQUIRED THE LEASE WHILE NODE HELD IT — two holders on one GPU")
	}
}

// THE C1 RACE, made deterministic: an EMPTY lease directory is the transient state of
// an in-progress acquisition. It must not be a claim by itself — meta.json is the only
// token — and a live Node claim inside that directory must still block Go.
func TestEmptyLeaseDirIsNotAClaimAndNodeStillBlocksGo(t *testing.T) {
	root := t.TempDir()
	m := testManagerAt(t, root)
	// Simulate the window: the directory exists, nobody has claimed it.
	if err := os.MkdirAll(m.leaseDir(), 0o777); err != nil {
		t.Fatalf("pre-create lease dir: %v", err)
	}
	if info := m.Inspect(); info.Held {
		t.Fatal("an empty lease directory reported as HELD")
	}
	res, stop := startNodeHolder(t, m.leaseDir())
	defer stop()
	if !res.OK || !res.Acquired {
		t.Fatalf("node could not claim inside an existing empty lease dir: %+v", res)
	}
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "go render"}); err == nil {
		t.Fatal("Go claimed over a live Node claim in a pre-existing lease dir — the C1 race")
	}
}

// THE C1 RACE ITSELF, made deterministic.
//
// A sequential test cannot reach this: Go's acquire fails fast on an existing lease
// directory, so it never sits inside its own Mkdir→record window. The seam puts a live
// Node acquirer INSIDE that window. The invariant is simple and absolute — after
// TryAcquire returns, at most one party may believe it holds the lease.
//
// This FAILS when the two sides use different atomic tokens (Go claiming by creating
// the directory, Node claiming by exclusively creating meta.json): Node's claim lands
// in the window and Go's record then overwrites it, leaving two holders.
func TestGoDoesNotClobberANodeClaimMadeInsideItsAcquireWindow(t *testing.T) {
	root := t.TempDir()
	m := testManagerAt(t, root)

	var stop func()
	var nodeRes nodeResult
	var fired bool
	orig := hookLeaseDirCreated
	hookLeaseDirCreated = func() {
		if fired {
			return // only race the first attempt
		}
		fired = true
		nodeRes, stop = startNodeHolder(t, m.leaseDir())
	}
	t.Cleanup(func() {
		hookLeaseDirCreated = orig
		if stop != nil {
			stop()
		}
	})

	lease, err := m.TryAcquire(ClassMedia, Options{Reason: "go render", TTL: 10 * time.Minute})

	if !fired {
		t.Fatal("seam never fired; the race was not exercised")
	}
	if !nodeRes.OK {
		t.Fatalf("node helper errored inside the window: %s", nodeRes.Error)
	}

	// Node claimed inside the window. Go must therefore NOT come away holding it.
	if nodeRes.Acquired && err == nil {
		_ = lease.Release()
		t.Fatal("BOTH Go and Node hold the lease: Node claimed inside Go's acquire window " +
			"and Go's record overwrote it. One GPU, two holders.")
	}
	// The complementary direction is also acceptable and safe: Node was refused
	// because Go had already claimed. Exactly one of them must hold it.
	if !nodeRes.Acquired && err != nil {
		t.Fatalf("NEITHER side holds the lease (node refused, go errored: %v) — the card is stranded", err)
	}
	if err == nil {
		_ = lease.Release()
	}
}

// Go must be able to READ a Node-written lease: same path is not enough, the record
// SCHEMA must match. If Go cannot see holder.pid it treats the lease as ownerless.
func TestGoReadsLiveNodeWrittenLease(t *testing.T) {
	root := t.TempDir()
	m := testManagerAt(t, root)

	res, stop := startNodeHolder(t, m.leaseDir())
	defer stop()
	if !res.OK || !res.Acquired {
		t.Fatalf("node acquire failed: %+v", res)
	}
	info := m.Inspect()
	if !info.Held {
		t.Fatal("Go reports a LIVE Node-held lease as FREE — schema mismatch; Go would reclaim it")
	}
	if info.PID <= 0 {
		t.Fatalf("Go could not read the holder pid from a Node-written lease: %+v", info)
	}
}

// Node must be able to READ a Go-written lease and agree it is NOT stale. A
// disagreement means one side waits while the other reclaims.
func TestNodeReadsGoWrittenLeaseAndAgreesItIsHeld(t *testing.T) {
	root := t.TempDir()
	m := testManagerAt(t, root)
	lease, err := m.TryAcquire(ClassText, Options{Reason: "go bench", TTL: 10 * time.Minute})
	if err != nil {
		t.Fatalf("go acquire: %v", err)
	}
	defer func() { _ = lease.Release() }()

	res := runNodeOnce(t, m.leaseDir(), "inspect")
	if !res.OK {
		t.Fatalf("node helper errored: %s", res.Error)
	}
	if len(res.Meta) == 0 || string(res.Meta) == "null" {
		t.Fatal("Node could not parse a Go-written lease record")
	}
	if res.Stale {
		t.Fatal("Node considers a live Go-held lease STALE — it would reclaim the GPU from under it")
	}
}

// A lease whose holder has EXITED is legitimately reclaimable across the boundary —
// this is the counterpart to the tests above and pins that we did not "fix" mutual
// exclusion by making dead holders un-reclaimable.
func TestGoReclaimsLeaseFromExitedNodeHolder(t *testing.T) {
	root := t.TempDir()
	m := testManagerAt(t, root)

	res := runNodeOnce(t, m.leaseDir(), "acquire") // acquires, then EXITS
	if !res.OK || !res.Acquired {
		t.Fatalf("node acquire failed: %+v", res)
	}
	if _, err := m.TryAcquire(ClassMedia, Options{Reason: "go render"}); err != nil {
		t.Fatalf("Go could not reclaim a lease whose Node holder exited: %v", err)
	}
}

// After Go releases, Node must be able to take the lease.
func TestNodeAcquiresAfterGoReleases(t *testing.T) {
	root := t.TempDir()
	m := testManagerAt(t, root)
	lease, err := m.TryAcquire(ClassText, Options{Reason: "go bench"})
	if err != nil {
		t.Fatalf("go acquire: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	res := runNodeOnce(t, m.leaseDir(), "acquire")
	if !res.OK || !res.Acquired {
		t.Fatalf("node could not acquire after Go released: %+v", res)
	}
}
