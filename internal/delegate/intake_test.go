// Delegator-side contract intake (roast delta 6): PrepareContract turns a
// surface's SubtaskSpec — wire-contract fields + context_paths — into a
// validated, self-contained core.AgentContract. The load-bearing pins:
// context_paths are read DELEGATOR-side under os.Root confinement (the calling
// session's context never pays, and the wire contract stays inline-docs), the
// per-file/total caps hold, and the delegator MINTS schema_version/depth —
// caller claims are overwritten, never trusted.

package delegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// specWithSchema returns a minimal remote-eligible spec (goal + schema).
func specWithSchema() SubtaskSpec {
	return SubtaskSpec{AgentContract: core.AgentContract{
		Goal:         "summarize the docs",
		OutputSchema: []byte(`{"properties":{"answer":{"type":"string"}}}`),
	}}
}

func TestPrepareContractInlinesContextPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("alpha doc"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.md"), []byte("beta doc"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := specWithSchema()
	// One absolute path, one root-relative: both forms must resolve and confine.
	spec.ContextPaths = []string{filepath.Join(root, "a.md"), filepath.Join("sub", "b.md")}
	c, err := PrepareContract(spec, root)
	if err != nil {
		t.Fatalf("PrepareContract: %v", err)
	}
	if len(c.Context) != 2 {
		t.Fatalf("context docs = %d, want 2 (inlined from context_paths)", len(c.Context))
	}
	if c.Context[0].Name != "a.md" || c.Context[0].Text != "alpha doc" {
		t.Errorf("doc 0 = %+v, want base name a.md with the file's text", c.Context[0])
	}
	if c.Context[1].Name != "b.md" || c.Context[1].Text != "beta doc" {
		t.Errorf("doc 1 = %+v, want base name b.md with the file's text", c.Context[1])
	}
}

// TestPrepareContractMintsVersionDepthAndClamps: the delegator is the ORIGIN —
// schema_version and depth are minted here (a caller-supplied value is
// overwritten, not validated), and the step/timeout ceilings mirror
// DecodeAgentContract exactly so a locally-run contract obeys the same rules
// as a dispatched one.
func TestPrepareContractMintsVersionDepthAndClamps(t *testing.T) {
	spec := specWithSchema()
	spec.SchemaVersion = 99
	spec.Depth = 3
	spec.MaxSteps = 999
	spec.TimeoutSec = 99999
	c, err := PrepareContract(spec, t.TempDir())
	if err != nil {
		t.Fatalf("PrepareContract: %v", err)
	}
	if c.SchemaVersion != core.AgentWireSchemaVersion {
		t.Errorf("schema_version = %d, want minted %d", c.SchemaVersion, core.AgentWireSchemaVersion)
	}
	if c.Depth != 0 {
		t.Errorf("depth = %d, want minted 0 (the surface IS the origin)", c.Depth)
	}
	if c.MaxSteps != core.AgentMaxStepsCap {
		t.Errorf("max_steps = %d, want clamped to %d", c.MaxSteps, core.AgentMaxStepsCap)
	}
	if c.TimeoutSec != core.AgentTimeoutSecCap {
		t.Errorf("timeout_sec = %d, want clamped to %d", c.TimeoutSec, core.AgentTimeoutSecCap)
	}

	// Zero values take the wire defaults.
	spec2 := specWithSchema()
	c2, err := PrepareContract(spec2, t.TempDir())
	if err != nil {
		t.Fatalf("PrepareContract: %v", err)
	}
	if c2.MaxSteps != core.AgentMaxStepsDefault || c2.TimeoutSec != core.AgentTimeoutSecDefault {
		t.Errorf("defaults = %d/%d, want %d/%d", c2.MaxSteps, c2.TimeoutSec, core.AgentMaxStepsDefault, core.AgentTimeoutSecDefault)
	}
}

func TestPrepareContractRejectsPathOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("leak"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{outside, filepath.Join("..", "escape.txt")} {
		spec := specWithSchema()
		spec.ContextPaths = []string{p}
		if _, err := PrepareContract(spec, root); err == nil {
			t.Errorf("PrepareContract(%q) succeeded; want a confinement error", p)
		}
	}
}

func TestPrepareContractRejectsOversizeFile(t *testing.T) {
	root := t.TempDir()
	big := make([]byte, contextPathMaxBytes+1)
	if err := os.WriteFile(filepath.Join(root, "big.txt"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	spec := specWithSchema()
	spec.ContextPaths = []string{"big.txt"}
	_, err := PrepareContract(spec, root)
	if err == nil || !strings.Contains(err.Error(), "big.txt") {
		t.Fatalf("err = %v, want a per-file size rejection naming big.txt", err)
	}
}

// TestPrepareContractRejectsDuplicateBaseNames: two paths whose BASE names
// collide would silently shadow each other at node-side materialization —
// Validate's duplicate-name check must catch the collision post-inline.
func TestPrepareContractRejectsDuplicateBaseNames(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"x", "y"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, d, "same.md"), []byte(d), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	spec := specWithSchema()
	spec.ContextPaths = []string{filepath.Join("x", "same.md"), filepath.Join("y", "same.md")}
	if _, err := PrepareContract(spec, root); err == nil {
		t.Fatal("duplicate base names must be rejected, not silently shadowed")
	}
}

// TestPrepareContractMissingGoalFails: Validate runs as usual after inlining.
func TestPrepareContractMissingGoalFails(t *testing.T) {
	if _, err := PrepareContract(SubtaskSpec{}, t.TempDir()); err == nil {
		t.Fatal("a goal-less spec must fail Validate")
	}
}
