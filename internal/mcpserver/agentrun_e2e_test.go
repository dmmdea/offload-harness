package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestAgentRunMCPLive is the P5a live proof that drive mode "a" (the MCP front
// door) works end-to-end: it builds the harness binary, spawns it as an MCP
// server, and — exactly as Claude would — calls the agent_run tool over the real
// MCP stdio transport. The handler runs the LOCAL agent loop in-process (planner
// on :11436 + read tools + the offload cascade) and returns a final answer.
//
// Gated by OFFLOAD_AGENT_E2E (needs the local planner model on :11436), so it is
// skipped in a normal `go test`. Ledger-pristine is guaranteed structurally — the
// handler's offload pipeline is built with a nil ledger — so it is asserted by
// construction + inherited from the P0 record=false proof, not re-measured here.
func TestAgentRunMCPLive(t *testing.T) {
	if os.Getenv("OFFLOAD_AGENT_E2E") == "" {
		t.Skip("set OFFLOAD_AGENT_E2E=1 to run the live agent_run MCP e2e (needs the local planner on :11436)")
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "harness"+exeSuffix())
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = repoRoot
	if out, berr := build.CombinedOutput(); berr != nil {
		t.Fatalf("build harness: %v\n%s", berr, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "agent-run-e2e", Version: "1"}, nil)
	sess, err := client.Connect(ctx, &mcp.CommandTransport{Command: exec.Command(bin, "mcp")}, nil)
	if err != nil {
		t.Fatalf("connect to spawned MCP server: %v", err)
	}
	defer sess.Close()

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: "agent_run",
		Arguments: map[string]any{
			"goal":      "List the .go source files directly inside the internal/agent directory and state exactly how many there are.",
			"read_root": repoRoot,
			"max_steps": 8,
		},
	})
	if err != nil {
		t.Fatalf("call agent_run over MCP: %v", err)
	}
	text := textOf(res)
	var out struct {
		Output     string `json:"output"`
		Steps      int    `json:"steps"`
		StopReason string `json:"stop_reason"`
		Tools      int    `json:"tools"`
		Deferred   bool   `json:"deferred"`
		Reason     string `json:"reason"`
	}
	if jerr := json.Unmarshal([]byte(text), &out); jerr != nil {
		t.Fatalf("parse agent_run result %q: %v", text, jerr)
	}
	if out.Deferred {
		t.Fatalf("agent_run deferred (the loop did not run): %s", out.Reason)
	}
	if strings.TrimSpace(out.Output) == "" {
		t.Errorf("agent_run should return a final answer; got %q", text)
	}
	if out.Steps <= 0 {
		t.Errorf("agent_run should report >=1 step; got %d (%q)", out.Steps, text)
	}
	t.Logf("agent_run via MCP: steps=%d stop=%s tools=%d output=%q", out.Steps, out.StopReason, out.Tools, out.Output)
}

func textOf(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
