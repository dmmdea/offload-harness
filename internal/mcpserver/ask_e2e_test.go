package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestAskMCPLive is the live proof that the one-call ask lane works end to end: it builds
// the harness binary, spawns it as an MCP server, and — exactly as Claude would — calls
// offload_ask over the real MCP stdio transport with nothing but a question and a path. The
// handler authors the contract, inlines the file, and runs it on the local agent seat.
//
// Gated by OFFLOAD_AGENT_E2E (needs the local planner on :11436), so it is skipped in a
// normal `go test`. What it asserts is deliberately narrow: the call must return a TYPED
// result, either an answer or a named defer. A defer is a valid outcome — the seat honestly
// reporting it could not do the work is this lane working as designed — and `verified` is
// logged rather than asserted, because whether a given seat's phrasing satisfies the
// harness-mined anchor is a property of the model, not of this code.
func TestAskMCPLive(t *testing.T) {
	if os.Getenv("OFFLOAD_AGENT_E2E") == "" {
		t.Skip("set OFFLOAD_AGENT_E2E=1 to run the live offload_ask MCP e2e (needs the local planner on :11436)")
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

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "ask-e2e", Version: "1"}, nil)
	sess, err := client.Connect(ctx, &mcp.CommandTransport{Command: exec.Command(bin, "mcp")}, nil)
	if err != nil {
		t.Fatalf("connect to spawned MCP server: %v", err)
	}
	defer sess.Close()

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: "offload_ask",
		Arguments: map[string]any{
			"question":  "Which constant holds the compiled-in version, and what value is it set to?",
			"paths":     []string{"internal/buildinfo/buildinfo.go"},
			"read_root": repoRoot,
		},
	})
	if err != nil {
		t.Fatalf("call offload_ask over MCP: %v", err)
	}
	text := textOf(res)
	var out struct {
		Answer             string   `json:"answer"`
		Evidence           string   `json:"evidence"`
		Verified           bool     `json:"verified"`
		Acceptance         []string `json:"acceptance"`
		AcceptanceFailures []string `json:"acceptance_failures"`
		Seat               string   `json:"seat"`
		Steps              int      `json:"steps"`
		StopReason         string   `json:"stop_reason"`
		Deferred           bool     `json:"deferred"`
		Reason             string   `json:"reason"`
		DeferClass         string   `json:"defer_class"`
	}
	if jerr := json.Unmarshal([]byte(text), &out); jerr != nil {
		t.Fatalf("parse offload_ask result %q: %v", text, jerr)
	}
	if out.Deferred {
		// Named and typed: the honest outcome. Loud in the log, not a failure.
		t.Logf("offload_ask DEFERRED: class=%s reason=%s", out.DeferClass, out.Reason)
		if strings.TrimSpace(out.Reason) == "" {
			t.Error("a defer must carry a reason — an unexplained defer is unactionable")
		}
		return
	}
	if strings.TrimSpace(out.Answer) == "" {
		t.Errorf("a non-deferred offload_ask must return an answer; got %q", text)
	}
	if len(out.Acceptance) == 0 {
		t.Errorf("the harness-authored acceptance must ride the response; got %q", text)
	}
	t.Logf("offload_ask via MCP: seat=%s steps=%d stop=%s verified=%v acceptance=%v failures=%v\nanswer=%q\nevidence=%q",
		out.Seat, out.Steps, out.StopReason, out.Verified, out.Acceptance, out.AcceptanceFailures, out.Answer, out.Evidence)
}
