package pipeline

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
)

// The wire trace carries a bounded note for calls that did NOT commit (the
// rigger's evidence for rewrite_error), and never for committed ones (the
// corpus keeps facts, not result bytes).
func TestTraceFromEffectsCarriesABoundedNoteForNonCommittedCallsOnly(t *testing.T) {
	long := strings.Repeat("no such file or directory ", 20)
	effects := []agent.EffectRecord{
		{Step: 1, Tool: "read_file", Status: agent.EffectCommitted, ObsChars: 50, Note: "must never appear"},
		{Step: 2, Tool: "read_file", Status: agent.EffectFailed, ObsChars: 30, Note: "error: open x.md:\n" + long},
		{Step: 3, Tool: "run_shell", Status: agent.EffectNone, ObsChars: 20, Note: "NOT executed: capped"},
	}
	tr := TraceFromEffects(effects)
	if len(tr) != 3 {
		t.Fatalf("trace = %+v", tr)
	}
	if tr[0].Note != "" {
		t.Fatalf("a committed call must carry no note: %q", tr[0].Note)
	}
	if tr[1].Note == "" || len(tr[1].Note) != core.AgentTraceNoteMax || strings.Contains(tr[1].Note, "\n") || !strings.HasPrefix(tr[1].Note, "error: open x.md:") {
		t.Fatalf("failed call note must be the clipped, one-line text: %q (len %d)", tr[1].Note, len(tr[1].Note))
	}
	if tr[2].Note != "NOT executed: capped" {
		t.Fatalf("none call note = %q", tr[2].Note)
	}
}
