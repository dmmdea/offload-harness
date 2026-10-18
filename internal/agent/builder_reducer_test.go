package agent

import (
	"context"
	"strings"
	"testing"
)

// TestBuildInstallsTheEvidenceReducerOnlyWhenAskedAndOffloaded: the reducer rides the
// offload seam Build is handed. It is off unless EvidenceReducer is set, it cannot be
// installed without a seam (and Build says so), and the seam it uses is the one given.
func TestBuildInstallsTheEvidenceReducerOnlyWhenAskedAndOffloaded(t *testing.T) {
	seamCalls := 0
	seam := func(context.Context, string, string, map[string]any) (string, error) {
		seamCalls++
		return "", nil
	}
	cases := []struct {
		name      string
		offload   OffloadFunc
		asked     bool
		installed bool
		note      string
	}{
		{"off by default", seam, false, false, ""},
		{"asked and offloaded", seam, true, true, ""},
		{"asked, no seam", nil, true, false, "agent_evidence_reducer"},
		{"not asked, no seam", nil, false, false, ""},
	}
	for _, c := range cases {
		cfg := baseBuild()
		cfg.Offload, cfg.EvidenceReducer = c.offload, c.asked
		res, err := Build(cfg)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := res.Loop.reducer != nil; got != c.installed {
			t.Errorf("%s: reducer installed = %v, want %v", c.name, got, c.installed)
		}
		joined := strings.Join(res.Notes, "\n")
		if c.note != "" && !strings.Contains(joined, c.note) {
			t.Errorf("%s: Build's notes do not name %q: %q", c.name, c.note, joined)
		}
		if c.note == "" && strings.Contains(joined, "agent_evidence_reducer") {
			t.Errorf("%s: an unexpected reducer note: %q", c.name, joined)
		}
		if c.installed && res.Loop.reducer != nil {
			before := seamCalls
			_, _ = res.Loop.reducer.offload(context.Background(), "extract", "x", nil)
			if seamCalls != before+1 {
				t.Errorf("%s: the reducer is not on the seam Build was handed", c.name)
			}
		}
	}
}
