package main

import (
	"os"
	"strings"
	"testing"
)

// TestAgentStateDefaultsFollowTheInstallRoot is a wiring lint: the agent's audit
// trail, ask queue and traces are data, so their defaults come from agent.DefaultStateFile
// (agent.StateFile, plus a note when an earlier release left a trail behind) under cfg.BaseDir() (config `home`), never from a hand-built ~/.local-offload path.
// A literal one put them on the OS drive of a node whose install root had moved to a
// data drive (register C-92), and nothing but this reading of the source can see it:
// the defaults are resolved inline in main().
func TestAgentStateDefaultsFollowTheInstallRoot(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	for _, name := range []string{"agent-audit.jsonl", "agent-asks.jsonl", "agent-traces"} {
		want := `agent.DefaultStateFile(cfg.BaseDir(), "` + name + `", os.Stderr)`
		if !strings.Contains(text, want) {
			t.Errorf("main.go must default %s through %s", name, want)
		}
	}
	if strings.Contains(text, `filepath.Join(home, ".local-offload"`) {
		t.Errorf("main.go builds a ~/.local-offload path by hand; use agent.DefaultStateFile(cfg.BaseDir(), ...)")
	}
}
