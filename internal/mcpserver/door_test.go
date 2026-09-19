// Register A-102: the MCP tool handlers stamp the door — the registered tool
// name — on the request they hand the pipeline, so the ledger row this call
// records names the surface that admitted it instead of being one of the 558
// door-less cascade rows.
//
// This runs the REAL handler over a REAL pipeline against one fake seat, so it
// proves the stamp survives the whole handler → Run → result path rather than
// re-asserting a literal in the handler's source.
package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// doorSeatServer answers any completion with a valid summary.
func doorSeatServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"summary\":\"a summary\",\"bullets\":[\"one\"]}"},` +
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":8}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSummarizeToolStampsItsDoor(t *testing.T) {
	srv := doorSeatServer(t)

	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.Model = "fake-workhorse"
	cfg.TriageModel = "fake-workhorse"
	cfg.EscalationModel = ""
	cfg.ReasoningModel = ""
	cfg.MaxRetries = 0
	cfg.ThresholdsPath = ""
	cfg.RouterWeightsPath = ""
	cfg.TierOverridesPath = ""
	cfg.ConfHeadLabelsPath = ""
	cfg.CachePath = ""
	cfg.LedgerPath = ""

	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	s := New(pipeline.New(cfg, client, nil, nil))

	res, err := s.handleSummarize(context.Background(), callReq(`{"text":"The quarterly report covers revenue, churn, hiring plans and the roadmap for the next two product cycles across all three regional teams."}`))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] == true {
		t.Fatalf("summarize deferred: %v", m["reason"])
	}
	meta, ok := m["meta"].(map[string]any)
	if !ok {
		b, _ := json.Marshal(m)
		t.Fatalf("result carries no meta object: %s", b)
	}
	// The door must be the tool name AS REGISTERED in tools/list — a reader
	// groups ledger rows by it, so a paraphrase is as useless as no door.
	if meta["door"] != "offload_summarize" {
		t.Fatalf("meta.door = %v, want offload_summarize (the registered tool name)", meta["door"])
	}
}

// TestEveryToolHandlerRequestCarriesADoor is the DRIFT guard: the value of the
// door column is that it is total. One new tool handler that builds a
// core.Request without a door puts door-less cascade rows back in the ledger,
// and nothing about that failure is visible in a passing test suite — which is
// exactly how all 558 of them accumulated. So the source is the fixture.
func TestEveryToolHandlerRequestCarriesADoor(t *testing.T) {
	src, err := os.ReadFile("mcpserver.go")
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
		// A multi-line literal puts the fields on the following lines; a
		// one-liner puts them here. Either way the door must be within the
		// literal's first few lines.
		window := strings.Join(lines[i:min(i+9, len(lines))], "\n")
		if !strings.Contains(window, `Door: "offload_`) && !strings.Contains(window, `Door:  "offload_`) {
			t.Errorf("mcpserver.go:%d builds a core.Request with no MCP-tool Door: %s", i+1, strings.TrimSpace(ln))
		}
	}
	if found == 0 {
		t.Fatal("no core.Request literal found — the guard stopped guarding anything")
	}
}
