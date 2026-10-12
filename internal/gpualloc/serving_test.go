package gpualloc

import (
	"strings"
	"testing"
)

// The reader of the serving config, and its tests (TestServingPinsReadsOnlyTheEnvPin and the rest),
// moved to internal/servingcfg with the code (register C-86a): the load fence reads the same file
// through the same parser, and config cannot import this package.

func TestExplainGroupsByReasonInOrder(t *testing.T) {
	lines := Explain([]Verdict{
		{Model: "a", Unload: true, Why: "placement unknown (x)"},
		{Model: "b", Why: "placed elsewhere by layers"},
		{Model: "c", Unload: true, Why: "placement unknown (x)"},
	})
	want := []string{"unloaded, placement unknown (x): a, c", "left resident, placed elsewhere by layers: b"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Errorf("lines = %q, want %q", lines, want)
	}
}
