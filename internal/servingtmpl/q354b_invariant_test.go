package servingtmpl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seatBlockOf returns the rendered block for one llama-swap model id: from its key to
// the next top-level model key. A line scan rather than a regex because Go's RE2 has no
// lookahead, and the PowerShell suite's equivalent pattern relies on one.
func seatBlockOf(out, modelID string) string {
	var b strings.Builder
	in := false
	for _, ln := range strings.Split(out, "\n") {
		isTopKey := strings.HasPrefix(ln, "  ") && len(ln) > 2 && ln[2] != ' '
		if in && isTopKey {
			break
		}
		if isTopKey && strings.HasPrefix(strings.TrimSpace(ln), modelID+":") {
			in = true
		}
		if in {
			b.WriteString(ln)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// TestQ354BSeatKeepsItsMeasuredReasoningInvariant guards, on BOTH templates that ship
// the seat, the two facts the ampere-6 bake-off measured: the seat must NOT pin
// `--reasoning off`, and must NOT be folded into ${common} (which pins it). Measured at
// the deployed ctx with a fresh server per run, n=3 per arm and zero spread: 67/67/67%
// recall on the extraction shape with reasoning on, 28/28/28% with it off.
//
// Why this exists in Go and not only in setup/render.tests.ps1: that script renders for
// the HOST OS and CI pins it to windows-latest, so every existing assertion covers
// llama-swap.win-cuda.yaml ONLY. Adding `--reasoning off` to the LINUX template passed
// the entire suite green — and the ampere-6 reference box is a Linux server, so the
// unguarded template was the likelier deployment target. This test is OS-independent.
//
// The two checks are a non-redundant PAIR, deliberately. Folding the seat into ${common}
// does NOT put the literal "--reasoning off" into the rendered block, because ${common}
// is a llama-swap runtime macro this renderer never expands — so only the second check
// catches that edit, which is exactly the "obvious cleanup" a future reader will attempt.
// Do not delete either as redundant.
func TestQ354BSeatKeepsItsMeasuredReasoningInvariant(t *testing.T) {
	for _, tc := range []struct{ name, file string }{
		{"linux-cuda", "llama-swap.linux-cuda.yaml"},
		{"win-cuda", "llama-swap.win-cuda.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			p := params()
			p.IncludeQ354B = true
			out, err := Render(string(b), p)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			block := seatBlockOf(out, "qwen3.5-4b-agent")
			if strings.TrimSpace(block) == "" {
				t.Fatal("no qwen3.5-4b-agent block in the rendered config — the seat vanished, or was renamed without updating this guard")
			}
			if strings.Contains(block, "--reasoning off") {
				t.Errorf("seat pins --reasoning off; MEASURED 28%% vs 67%% recall. Block:\n%s", block)
			}
			if strings.Contains(block, "${common}") {
				t.Errorf("seat uses ${common}, which pins --reasoning off; MEASURED 28%% vs 67%% recall. Block:\n%s", block)
			}
		})
	}
}
