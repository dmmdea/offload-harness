package delegate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// The delegator reads the local lease to ROUTE a contract, and offload_status reads it to
// REPORT. Neither is the load gate, so neither remembers what it sees: the scope of a legacy
// lease is reported from the evidence, and no sidecar or ledger line is written for it.
func TestLocalLeaseIsAnInspectionAndWritesNothing(t *testing.T) {
	b := newFlagshipLeaseBox(t)
	// A legacy record (an older binary wrote it), a ComfyUI launch marker whose pid is the
	// wrapper's own, and a box that declared ComfyUI's device order.
	b.m.EmulateLegacyWriter()
	b.hold(gpulease.ClassMedia)

	comfy := t.TempDir()
	marker, err := json.Marshal(map[string]any{"pid": os.Getpid(), "ownerPid": 0, "startedAt": 1, "args": []string{"main.py", "--cuda-device", "2"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(comfy, ".offload-launch.json"), marker, 0o644); err != nil {
		t.Fatal(err)
	}
	modelaffinity.SetComfyDir(comfy)
	modelaffinity.SetLegacyInference(true)
	t.Cleanup(func() { modelaffinity.SetComfyDir(""); modelaffinity.SetLegacyInference(false) })
	t.Cleanup(modelaffinity.SetCardTableReader(func(context.Context, string) ([]gpuprobe.Card, string, error) {
		return []gpuprobe.Card{
			{UUID: "GPU-aaaa0000", NvidiaIndex: 0, ComfyOrder: 1},
			{UUID: "GPU-bbbb0000", NvidiaIndex: 1, Display: true, ComfyOrder: 0},
			{UUID: "GPU-cccc0000", NvidiaIndex: 2, ComfyOrder: 2},
		}, "", nil
	}))

	got := LocalLease(b.cfg.GPULockPath, b.cfg.StateDir)
	if got.ScopeKind() != gpulease.ScopeInferred {
		t.Fatalf("the delegator reads the scope the evidence implies, got %s (%s)", got.ScopeKind(), got.ScopeWhy)
	}
	entries, err := os.ReadDir(b.m.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "seen.") {
			t.Fatalf("a delegator read wrote %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(b.m.Dir()), "scope-ledger.jsonl")); err == nil {
		t.Fatal("a delegator read wrote the scope ledger")
	}

	// The load gate, by contrast, remembers: the control that makes the checks above mean something.
	modelaffinity.InspectLease(b.m.Dir())
	if _, err := os.Stat(filepath.Join(b.m.Dir(), "seen."+strconv.FormatUint(got.Epoch, 10))); err != nil {
		t.Fatalf("the observing read persists the sticky set: %v", err)
	}
}
