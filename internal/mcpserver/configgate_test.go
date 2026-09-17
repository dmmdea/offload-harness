package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// badConfig is the refusal text a real Load produces for the headline case, kept
// verbatim so the tests assert on what an operator would actually read.
var badConfig = errors.New(`endpoint: "http://127.0.0.1:9" dials port :9 — port 9 is the IANA discard port, the shape of an endpoint whose value was never substituted; set the real port or remove the key`)

// callOverTransport runs one tools/call against the BUILT server over an
// in-memory transport — the same path a real client takes, so anything the
// server installs in front of its handlers is exercised rather than bypassed.
func callOverTransport(t *testing.T, s *Server, name string, args string) (map[string]any, string) {
	t.Helper()
	srv := s.buildServer("test")
	clientT, serverT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "gate", Version: "1"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("tools/call %s: %v", name, err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("tools/call %s returned no content", name)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("tools/call %s returned %T, want text", name, res.Content[0])
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(text.Text), &m); err != nil {
		t.Fatalf("tools/call %s result is not a JSON object: %v\n%s", name, err, text.Text)
	}
	return m, text.Text
}

// TestMCPServerReportsAnInvalidConfigInsteadOfVanishing (review of #361,
// blocker 2b): the MCP server must NOT refuse to start on a config that failed
// validation. A server that exits removes every offload_* tool from every
// session with no message anywhere the operator will see — the trap this repo
// has already been bitten by. So it starts, and says so on every surface:
// offload_status carries config_error, and every other tool defers by name
// rather than running work against a config the loader refused.
func TestMCPServerReportsAnInvalidConfigInsteadOfVanishing(t *testing.T) {
	statusSamplesGPU = false
	t.Cleanup(func() { statusSamplesGPU = true })

	s := New(pipeline.New(config.Default(), nil, nil, nil)).WithConfigError(badConfig)

	status, raw := callOverTransport(t, s, "offload_status", `{}`)
	// FIRST key, not merely present: this is the one tool still answering while
	// every other one defers, and a Go map marshals in sorted key order, which
	// would bury the reason among a dozen capability sections.
	if !strings.HasPrefix(raw, `{"config_error":`) {
		t.Errorf("config_error must be the FIRST key of the status object; got %.120s", raw)
	}
	got, _ := status["config_error"].(string)
	if got == "" {
		t.Fatalf("offload_status must carry config_error; got keys %v", keysOf(status))
	}
	for _, want := range []string{"endpoint", "127.0.0.1:9", ":9"} {
		if !strings.Contains(got, want) {
			t.Errorf("config_error %q must name %q", got, want)
		}
	}

	run, _ := callOverTransport(t, s, "agent_run", `{"goal":"anything"}`)
	if run["deferred"] != true {
		t.Fatalf("agent_run must defer while the config is invalid; got %v", run)
	}
	reason, _ := run["reason"].(string)
	if !strings.Contains(reason, "config invalid") || !strings.Contains(reason, "endpoint") {
		t.Errorf("agent_run reason %q must say the config is invalid and name the key", reason)
	}
}

// TestMCPServerIsUnchangedOnAValidConfig: the gate must be inert when there is
// nothing to report — no config_error key, no defer — so a healthy box's tool
// surface is byte-identical to a build without this gate.
func TestMCPServerIsUnchangedOnAValidConfig(t *testing.T) {
	statusSamplesGPU = false
	t.Cleanup(func() { statusSamplesGPU = true })

	s := New(pipeline.New(config.Default(), nil, nil, nil))
	status, _ := callOverTransport(t, s, "offload_status", `{}`)
	if _, present := status["config_error"]; present {
		t.Fatalf("config_error must be absent on a valid config; got %v", status["config_error"])
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
