package main

import (
	"os"
	"strings"
	"testing"
)

// TestRKNPUUpgradePathIsDocumented (0.153.0): the binary does not ship
// rkllm_server.py and install.sh never refreshes RKNPU_HOME or an existing
// config.json, so an installed RK3588 node that re-renders a 0.153.0
// llama-swap.yaml (--repeat-penalty 1.1) against its old server never starts the
// seat. The change's changelog, its ADR amendment and the accelerators page must
// each say to refresh RKNPU_HOME first and to reseed the config by hand.
func TestRKNPUUpgradePathIsDocumented(t *testing.T) {
	for _, path := range []string{
		"CHANGELOG.md",
		"docs/architecture/decisions/0062-rk3588-soc-tier-serves-from-the-npu-on-a-unified-memory-budget.md",
		"docs/systems/accelerators.md",
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if path == "CHANGELOG.md" {
			i := strings.Index(s, "## [0.153.0]")
			j := strings.Index(s, "## [0.151.1]")
			if i < 0 || j < i {
				t.Fatalf("%s: no 0.153.0 section", path)
			}
			s = s[i:j]
		}
		for _, want := range []string{"RKNPU_HOME", "rkllm_server.py", "--repeat-penalty", "config.json", "vision_tasks", "SEED-ONLY"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s: upgrade note missing %q", path, want)
			}
		}
	}
}
