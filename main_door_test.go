package main

import (
	"os"
	"strings"
	"testing"
)

// Register A-102: every CLI command that runs an offload task stamps its door
// ("cli:<subcommand>") on the request, so its ledger row names the surface
// that admitted the call. The guard is a SOURCE lint for the same reason the
// MCP one is: the value of the door column is that it is total, and one new
// subcommand shipping without a door puts door-less cascade rows back in the
// ledger with nothing failing.
func TestEveryCLIRequestCarriesADoor(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	lines := strings.Split(string(src), "\n")
	found := 0
	for i, ln := range lines {
		if !strings.Contains(ln, "core.Request{") {
			continue
		}
		found++
		window := strings.Join(lines[i:min(i+9, len(lines))], "\n")
		if !strings.Contains(window, `Door:`) {
			t.Errorf("main.go:%d builds a core.Request with no cli: Door: %s", i+1, strings.TrimSpace(ln))
			continue
		}
		// The door must be a CLI door — the generic text-task handler derives
		// it from the subcommand the user typed, which is the same vocabulary.
		if !strings.Contains(window, `Door: "cli:`) && !strings.Contains(window, `Door:  "cli:`) &&
			!strings.Contains(window, `Door:   "cli:`) && !strings.Contains(window, `Door: "cli:" +`) {
			t.Errorf("main.go:%d stamps a door that is not a cli: door: %s", i+1, strings.TrimSpace(ln))
		}
	}
	if found == 0 {
		t.Fatal("no core.Request literal found in main.go — the guard stopped guarding anything")
	}
}
