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

// TestReviewDiffMCPLive is the live proof that the clean-context review lane works end to
// end against a REAL diff: it builds the harness binary, spawns it as an MCP server, takes
// `git diff HEAD~1` from this checkout, and — exactly as a lead agent would — calls
// offload_review_diff over the real MCP stdio transport with nothing but that diff and a
// task statement.
//
// Gated by OFFLOAD_AGENT_E2E (needs the local planner on :11436), so it is skipped in a
// normal `go test`. What it asserts is deliberately narrow: the call must return a TYPED
// result — findings (possibly none) or a NAMED defer. A defer is a valid outcome; a crash
// or an MCP error is not. Nothing here asserts WHAT the seat found: whether a given local
// model spots a given defect is a property of the model, and this lane is advisory by
// design, so pinning its findings would pin the wrong thing.
func TestReviewDiffMCPLive(t *testing.T) {
	if os.Getenv("OFFLOAD_AGENT_E2E") == "" {
		t.Skip("set OFFLOAD_AGENT_E2E=1 to run the live offload_review_diff MCP e2e (needs the local planner on :11436)")
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	git := exec.Command("git", "diff", "HEAD~1")
	git.Dir = repoRoot
	diff, gerr := git.Output()
	if gerr != nil || len(strings.TrimSpace(string(diff))) == 0 {
		t.Skipf("no `git diff HEAD~1` available in %s: %v", repoRoot, gerr)
	}

	bin := filepath.Join(t.TempDir(), "harness"+exeSuffix())
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = repoRoot
	if out, berr := build.CombinedOutput(); berr != nil {
		t.Fatalf("build harness: %v\n%s", berr, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "review-e2e", Version: "1"}, nil)
	sess, err := client.Connect(ctx, &mcp.CommandTransport{Command: exec.Command(bin, "mcp")}, nil)
	if err != nil {
		t.Fatalf("connect to spawned MCP server: %v", err)
	}
	defer sess.Close()

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: "offload_review_diff",
		Arguments: map[string]any{
			"diff": string(diff),
			"task": "the change described in the last commit message",
		},
	})
	if err != nil {
		t.Fatalf("call offload_review_diff over MCP: %v", err)
	}
	text := textOf(res)
	var out struct {
		Findings []struct {
			Severity string `json:"severity"`
			File     string `json:"file"`
			Line     int    `json:"line"`
			Claim    string `json:"claim"`
			Why      string `json:"why"`
		} `json:"findings"`
		ReviewedBytes     int    `json:"reviewed_bytes"`
		DroppedUngrounded int    `json:"dropped_ungrounded"`
		Seat              string `json:"seat"`
		Steps             int    `json:"steps"`
		StopReason        string `json:"stop_reason"`
		Note              string `json:"note"`
		Deferred          bool   `json:"deferred"`
		Reason            string `json:"reason"`
		DeferClass        string `json:"defer_class"`
	}
	if jerr := json.Unmarshal([]byte(text), &out); jerr != nil {
		t.Fatalf("parse offload_review_diff result %q: %v", text, jerr)
	}
	if out.Deferred {
		// Named and typed: the honest outcome. Loud in the log, not a failure.
		t.Logf("offload_review_diff DEFERRED: class=%s reason=%s", out.DeferClass, out.Reason)
		if strings.TrimSpace(out.Reason) == "" {
			t.Error("a defer must carry a reason — an unexplained defer is unactionable")
		}
		return
	}
	if out.ReviewedBytes != len(diff) {
		t.Errorf("reviewed_bytes = %d, want the diff's own %d — the caller must know what was judged", out.ReviewedBytes, len(diff))
	}
	if len(out.Findings) == 0 && strings.TrimSpace(out.Note) == "" {
		t.Error("an empty findings list must carry the note saying it is not a verification")
	}
	t.Logf("offload_review_diff via MCP: seat=%s steps=%d stop=%s reviewed=%dB findings=%d dropped_ungrounded=%d note=%q",
		out.Seat, out.Steps, out.StopReason, out.ReviewedBytes, len(out.Findings), out.DroppedUngrounded, out.Note)
	for i, f := range out.Findings {
		t.Logf("  [%d] %s %s:%d — %s (%s)", i, f.Severity, f.File, f.Line, f.Claim, f.Why)
	}
}
