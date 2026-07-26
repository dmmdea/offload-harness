package gpulease

// Cross-language tests: what still spans the Go/Node boundary now that acquisition has
// COLLAPSED into this package.
//
// The original version of this file tested mutual exclusion between a Go acquirer and a
// Node acquirer, because both implemented acquisition and they disagreed — different
// atomic tokens, different liveness rules, a non-atomic epoch counter. Node no longer
// acquires, so that entire class of test is moot by construction, which was the point of
// the collapse.
//
// What remains genuinely cross-language, and is tested here:
//   - Node must READ a Go-written lease record correctly (the fence compares epochs, and
//     a record Node cannot parse reads as "fenced out", which would block every render).
//   - Node's fence must agree with Go about which epoch is current.
//   - A render must REFUSE to run without an inherited lease, rather than reintroducing
//     acquisition.
//
// These do NOT skip when node is missing. A suite whose whole purpose is catching what
// the single-language suites cannot see must not silently evaporate; set
// GPULEASE_ALLOW_NO_NODE=1 to downgrade that to a skip on a machine without node.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// nodeHelper reads the shared record through render/gpu-lock.mjs and reports what Node
// makes of it. Read-only: Node has no acquisition API left to call.
const nodeHelper = `
import { readLease, checkInheritedLease, inheritedLease } from %MJS%;
const dir = process.argv[2];
const epoch = Number(process.argv[3]);
const out = (o) => { process.stdout.write(JSON.stringify(o) + "\n"); };
try {
  const meta = readLease(dir);
  out({
    ok: true,
    parsed: meta !== null,
    epoch: meta ? meta.epoch : null,
    holderPid: meta && meta.holder ? meta.holder.pid : null,
    klass: meta ? meta.class : null,
    fencePasses: checkInheritedLease({ dir, epoch }),
    inherits: inheritedLease({ GPU_LEASE_DIR: dir, GPU_LEASE_EPOCH: String(epoch), GPU_LEASE_CLASS: "media" }) !== null,
  });
} catch (e) {
  out({ ok: false, error: String((e && e.code) || e) });
}
`

type nodeResult struct {
	OK          bool   `json:"ok"`
	Parsed      bool   `json:"parsed"`
	Epoch       *int64 `json:"epoch"`
	HolderPID   *int   `json:"holderPid"`
	Class       string `json:"klass"`
	FencePasses bool   `json:"fencePasses"`
	Inherits    bool   `json:"inherits"`
	Error       string `json:"error"`
}

// runNode executes the read-only helper against a lease directory.
func runNode(t *testing.T, leaseDir string, epoch uint64) nodeResult {
	t.Helper()
	nodeExe, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("GPULEASE_ALLOW_NO_NODE") != "" {
			t.Skip("node not on PATH and GPULEASE_ALLOW_NO_NODE is set")
		}
		t.Fatal("node is not on PATH. These are the only tests that check Go and Node agree " +
			"about the lease record; silently skipping them is how the two drifted before. " +
			"Install node, or set GPULEASE_ALLOW_NO_NODE=1 to accept the gap.")
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
	script := filepath.Join(dir, "helper.mjs")
	if err := os.WriteFile(script, []byte(strings.Replace(nodeHelper, "%MJS%", mjsURL, 1)), 0o666); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	out, err := exec.Command(nodeExe, script, leaseDir, itoa(epoch)).CombinedOutput()
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

func itoa(v uint64) string { return strconv.FormatUint(v, 10) }

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

// Node must parse a Go-written record, including the NESTED holder.pid. When the two
// schemas diverged, the reader found no holder and treated a live lease as ownerless.
func TestNodeReadsGoWrittenLease(t *testing.T) {
	m := testManagerAt(t, t.TempDir())
	lease, err := m.TryAcquire(ClassMedia, Options{Reason: "go render", TTL: 10 * time.Minute})
	if err != nil {
		t.Fatalf("go acquire: %v", err)
	}
	defer func() { _ = lease.Release() }()

	res := runNode(t, m.leaseDir(), lease.Epoch())
	if !res.OK {
		t.Fatalf("node helper errored: %s", res.Error)
	}
	if !res.Parsed {
		t.Fatal("Node could not parse a Go-written lease record — the fence would read this as 'fenced out' and block every render")
	}
	if res.HolderPID == nil || *res.HolderPID != os.Getpid() {
		t.Fatalf("Node read holder.pid = %v, want %d — schema drift", res.HolderPID, os.Getpid())
	}
	if res.Class != string(ClassMedia) {
		t.Fatalf("Node read class = %q, want %q; the class gate depends on this", res.Class, ClassMedia)
	}
}

// The fence must agree across the boundary: the epoch Go issued is the epoch Node sees.
func TestNodeFenceAgreesWithGoEpoch(t *testing.T) {
	m := testManagerAt(t, t.TempDir())
	lease, err := m.TryAcquire(ClassMedia, Options{Reason: "go render", TTL: 10 * time.Minute})
	if err != nil {
		t.Fatalf("go acquire: %v", err)
	}
	defer func() { _ = lease.Release() }()

	if res := runNode(t, m.leaseDir(), lease.Epoch()); !res.FencePasses {
		t.Fatal("Node's fence rejected the CURRENT epoch — every inherited render would refuse to run")
	}
	// And a stale epoch must be rejected: this is the closing-lid case.
	if res := runNode(t, m.leaseDir(), lease.Epoch()-1); res.FencePasses {
		t.Fatal("Node's fence accepted a STALE epoch — a suspended process would act on top of the current holder")
	}
}

// After Go releases, Node must see no lease at all — a released lease that still reads as
// present would fence out the next legitimate holder.
func TestNodeSeesNothingAfterGoReleases(t *testing.T) {
	m := testManagerAt(t, t.TempDir())
	lease, err := m.TryAcquire(ClassMedia, Options{Reason: "go render"})
	if err != nil {
		t.Fatalf("go acquire: %v", err)
	}
	epoch := lease.Epoch()
	if err := lease.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	res := runNode(t, m.leaseDir(), epoch)
	if res.Parsed {
		t.Fatal("Node still sees a lease record after Go released it")
	}
	if res.FencePasses {
		t.Fatal("Node's fence passed against a released lease")
	}
}

// The unload markers the render path elects with must not survive the lease: a marker
// outliving its lease suppresses a needed unload for the next one.
func TestReleaseClearsUnloadMarkers(t *testing.T) {
	m := testManagerAt(t, t.TempDir())
	lease, err := m.TryAcquire(ClassMedia, Options{Reason: "go render"})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	marker := filepath.Join(m.leaseDir(), "unloaded."+itoa(lease.Epoch()))
	if err := os.WriteFile(marker, []byte("1"), 0o666); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("an unload marker survived its lease; the next lease would skip a needed unload")
	}
}
