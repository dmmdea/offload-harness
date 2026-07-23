package pipeline

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/contextbudget"
	"github.com/dmmdea/offload-harness/internal/gcf"
)

// bigJSONArray builds an eligible JSON array comfortably over maxChars.
func bigJSONArray(rows int) string {
	parts := make([]string, rows)
	for i := range parts {
		parts[i] = fmt.Sprintf(`{"path":"internal/pkg/file_%d.go","line":%d,"match":"func Fn%d() error","kind":"function"}`, i, i*13, i)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// TestCompactForBudgetRescuesOversizeJSON: an over-budget JSON-array input
// must be GCF-compacted when the flag is on — and when the compact form fits
// the budget, the subsequent Trim becomes a no-op (nothing is cut).
func TestCompactForBudgetRescuesOversizeJSON(t *testing.T) {
	in := bigJSONArray(60)
	max := len(in) * 2 / 3 // over budget as JSON, comfortably under as GCF
	out := compactForBudget(in, max, true)
	if !gcf.IsCompacted(out) {
		t.Fatalf("over-budget JSON input not compacted (len=%d max=%d)", len(in), max)
	}
	if len(out) > max {
		t.Fatalf("compact form still over budget: %d > %d", len(out), max)
	}
	trimmed, cut := contextbudget.Trim(out, max)
	if cut || trimmed != out {
		t.Errorf("Trim still cut a compact form that fits — lossless rescue failed")
	}
}

// TestCompactForBudgetLeavesEverythingElseAlone: flag off, in-budget inputs,
// non-JSON inputs, and already-compacted inputs must pass through byte-identical.
func TestCompactForBudgetLeavesEverythingElseAlone(t *testing.T) {
	in := bigJSONArray(60)
	if out := compactForBudget(in, len(in)/2, false); out != in {
		t.Errorf("flag off: input modified")
	}
	if out := compactForBudget(in, len(in)+1, true); out != in {
		t.Errorf("in-budget input modified — happy-path bytes must be stable")
	}
	prose := strings.Repeat("plain prose line\n", 400)
	if out := compactForBudget(prose, 100, true); out != prose {
		t.Errorf("non-JSON input modified")
	}
	once := compactForBudget(in, len(in)/2, true)
	if again := compactForBudget(once, 10, true); again != once {
		t.Errorf("already-compacted input re-processed")
	}
}
