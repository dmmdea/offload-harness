package leakgate

import (
	"sort"
	"testing"
)

// TestScanTreeCoversTheOpenSpecTree pins that the spec tree OpenSpec writes
// (openspec/: the project config, the specs, change proposals and their
// archive) is scanned like every other tracked path, bodies and names, YAML and
// Markdown alike. Those files are prose a session writes about this repository,
// the likeliest place for a machine name to reach the public tree (security.md,
// "Admitted third-party software"). A path or extension filter added to
// ScanTree later turns this red.
func TestScanTreeCoversTheOpenSpecTree(t *testing.T) {
	m := plainMatcher(t)
	fsys := memFS{
		"openspec/config.yaml":                             file("schema: spec-driven\ncontext: |\n  the gate runs on zorblax\n"),
		"openspec/specs/gate/spec.md":                      file("## Requirements\n\nthe quuxel box keeps its seat\n"),
		"openspec/changes/add-gate/proposal.md":            file("## Why\n\n" + CanaryText() + "\n"),
		"openspec/changes/archive/2026-10-14-clean/tasks.md": file("- [x] a clean task\n"),
		"openspec/changes/gribble-rename/design.md":        file("a clean body\n"),
	}
	r := scanOf(t, m, fsys, nil, nil, ScanOptions{})

	got := map[string]bool{}
	for _, f := range r.Findings {
		key := f.Path
		if f.Name {
			key += " (name)"
		}
		got[key] = true
	}
	want := []string{
		"openspec/changes/add-gate/proposal.md",
		"openspec/changes/gribble-rename/design.md (name)",
		"openspec/config.yaml",
		"openspec/specs/gate/spec.md",
	}
	var gotKeys []string
	for k := range got {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)
	if len(gotKeys) != len(want) {
		t.Fatalf("openspec findings\n got %q\nwant %q", gotKeys, want)
	}
	for i := range want {
		if gotKeys[i] != want[i] {
			t.Fatalf("openspec findings\n got %q\nwant %q", gotKeys, want)
		}
	}
	if len(r.Fatals) > 0 {
		t.Fatalf("unexpected fatals: %v", fatalReasons(r))
	}
}
