package gpulease

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Who wrote a whole-node record decides whether its silence can be read (plan P4, review fix).
// A record from a binary that predates card-scoped leases could not name cards, so what it
// actually runs is evidence of what it holds. A whole-node record from a binary that COULD have
// named cards (an explicit --whole-node, a reserve with the writer flag off, the pipeline's own
// media lease) is the whole node by its writer's word, and no command line narrows it.

func TestRecordsCarryTheFormatOfTheirWriter(t *testing.T) {
	m, _ := newTestManager(t)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "render"})
	if err != nil {
		t.Fatal(err)
	}
	if info := m.Inspect(); !info.Held || info.Legacy {
		t.Fatalf("a record this binary wrote is not legacy, got Held=%v Legacy=%v", info.Held, info.Legacy)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}

	m.EmulateLegacyWriter()
	l, err = m.TryAcquire(ClassMedia, Options{Reason: "render"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if info := m.Inspect(); !info.Held || !info.Legacy {
		t.Fatalf("a record without the stamp is what an older binary wrote, got Held=%v Legacy=%v", info.Held, info.Legacy)
	}
}

func TestDeviceLeasesAreNeverLegacy(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetCardScoped(true)
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "render", Devices: []string{idCard0}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	for _, i := range m.Leases() {
		if i.Legacy {
			t.Fatalf("a lease that declares its cards is not legacy: %+v", i)
		}
	}
}

// scopeOver runs a Scoper whose process tree names card 2 over what the manager reports, and
// says whether the tree was read at all.
func scopeOver(t *testing.T, m *Manager) (Info, int64) {
	t.Helper()
	h := newInferHarness(t)
	h.dir = m.leaseDir()
	h.sc = h.newScoper()
	h.tree = tree(TreeProc{PID: m.pid, PPID: 1, Cmdline: "python main.py --cuda-device 2"})
	return h.sc.Scope(m.Inspect()), h.treeCalls.Load()
}

func TestExplicitWholeNodeLeaseIsNeverInferred(t *testing.T) {
	m, _ := newTestManager(t)
	// `gpu reserve --whole-node`: no devices, written by this binary, with a command that names
	// a card of its own.
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "everything", Command: "python main.py --cuda-device 2", WrapperVersion: "0.161.0"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	got, treeReads := scopeOver(t, m)
	if got.ScopeKind() != ScopeWholeNode || len(got.Inferred) != 0 {
		t.Fatalf("an explicit whole-node lease means the whole node, got %v (%s)", got.Inferred, got.ScopeWhy)
	}
	if treeReads != 0 {
		t.Fatalf("a lease that is not legacy needs no process-tree read, got %d", treeReads)
	}
	if !got.Touches([]string{idCard0}) {
		t.Fatal("card 0 stays fenced by a whole-node lease that said so")
	}
}

func TestFlagOffWholeNodeFromThisBinaryIsNotInferred(t *testing.T) {
	m, _ := newTestManager(t)
	if m.CardScoped() {
		t.Fatal("the fixture must be a host with the writer flag off")
	}
	// What the pipeline's own media lease looks like: no wrapper version, no devices.
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "render", Command: "python main.py --cuda-device 2"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	got, treeReads := scopeOver(t, m)
	if got.ScopeKind() != ScopeWholeNode || treeReads != 0 {
		t.Fatalf("a flag-off whole-node lease from this binary is whole-node byte for byte as before, got %s tree reads=%d", got.ScopeKind(), treeReads)
	}
	if !strings.Contains(got.ScopeWhy, "writer") {
		t.Errorf("the reason names why it is not inferred: %q", got.ScopeWhy)
	}
}

func TestWholeNodeRecordOfAnOlderBinaryIsStillInferred(t *testing.T) {
	m, _ := newTestManager(t)
	m.EmulateLegacyWriter()
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film", Command: "python run.py"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	got, _ := scopeOver(t, m)
	if got.ScopeKind() != ScopeInferred || !reflect.DeepEqual(got.Inferred, []string{idCard2}) {
		t.Fatalf("the record the older binary wrote is the one the rule exists for, got %v (%s)", got.Inferred, got.ScopeWhy)
	}
}

// The older binary releases its own lease, and it knows nothing of the sidecar, so the sidecar
// is debris that the next acquirer removes (a release by this binary removes it itself).
func TestNextAcquirerSweepsASidecarThatAnOlderBinaryLeftBehind(t *testing.T) {
	m, _ := newTestManager(t)
	m.EmulateLegacyWriter()
	l, err := m.TryAcquire(ClassMedia, Options{Reason: "film"})
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(m.leaseDir(), "seen."+strconv.FormatUint(l.Epoch(), 10))
	if err := os.WriteFile(old, []byte(`{"epoch":1}`), 0o666); err != nil {
		t.Fatal(err)
	}
	// The older binary's release: the claim is removed and nothing else.
	if err := os.Remove(m.metaPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("the fixture's sidecar must outlive the older binary's release")
	}

	next, err := m.TryAcquire(ClassMedia, Options{Reason: "next"})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Release()
	if _, err := os.Stat(old); err == nil {
		t.Fatal("the next acquirer must sweep a sidecar whose lease is gone")
	}
}

func TestDeviceGrantSweepsASidecarWhoseLeaseIsGone(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetCardScoped(true)
	first, err := m.TryAcquire(ClassMedia, Options{Reason: "first", Devices: []string{idCard0}})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	// A sidecar for an epoch that no lease holds, beside a live device lease.
	dead := filepath.Join(m.leaseDir(), "seen.9999")
	live := filepath.Join(m.leaseDir(), "seen."+strconv.FormatUint(first.Epoch(), 10))
	for _, p := range []string{dead, live} {
		if err := os.WriteFile(p, []byte(`{}`), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	second, err := m.TryAcquire(ClassMedia, Options{Reason: "second", Devices: []string{idCard2}})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if _, err := os.Stat(dead); err == nil {
		t.Fatal("a sidecar for an epoch nobody holds is debris")
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatal("a live lease's own file is never swept")
	}
}
