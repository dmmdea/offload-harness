// Task 4 (multi-node agent delegation): the fleet "agent" task's advertisement
// gate and BuildRequest translation. The advertisement tests pin the additive-
// off contract (a node that never opted in advertises byte-identically to
// 0.62.x); the BuildRequest tests pin the strict-path contract handling —
// decode, the output_schema requirement (roast delta 3), the receiver-derived
// depth (delta 2), the MaxSteps cap assertion (delta 5, clamped in
// core.DecodeAgentContract — asserted here, never re-clamped), and the
// materialize-then-cleanup discipline shared with buildPipelineJob.
package fleetnode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// agentNodeCfg is a minimal opted-in agent node: enabled + a resolvable seat +
// a token (so the listener posture never matters for the BuildRequest tests).
func agentNodeCfg(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		Home:              t.TempDir(), // BaseDir() → the job dir lands in a test-owned tree
		FleetAgentEnabled: true,
		AgentModel:        "agent-seat",
		FleetAuthToken:    "tok",
	}
}

func supportsAgent(cfg config.Config) bool {
	for _, task := range SupportedTasks(cfg) {
		if task == "agent" {
			return true
		}
	}
	return false
}

// TestAgentTaskAdvertisementGate: "agent" is advertised ONLY when the operator
// opted in AND a seat resolves AND the lane is reachable safely (loopback
// listener, or a token for anything beyond it). Default-off is the additive
// contract: absent keys ⇒ the advertisement is byte-identical to before.
func TestAgentTaskAdvertisementGate(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Config
		want bool
	}{
		{"zero config", config.Config{}, false},
		{"disabled despite seat+token (default-off pin)",
			config.Config{AgentModel: "seat", FleetAuthToken: "tok"}, false},
		{"enabled + seat + token",
			config.Config{FleetAgentEnabled: true, AgentModel: "seat", FleetAuthToken: "tok"}, true},
		{"enabled + seat + tokenless + default listen (loopback default)",
			config.Config{FleetAgentEnabled: true, AgentModel: "seat"}, true},
		{"enabled + seat + tokenless + explicit loopback listen",
			config.Config{FleetAgentEnabled: true, AgentModel: "seat", FleetListen: "127.0.0.1:18811"}, true},
		{"enabled + seat + tokenless + tailscale listen (refused: RCE-class surface)",
			config.Config{FleetAgentEnabled: true, AgentModel: "seat", FleetListen: "100.64.0.5:18811"}, false},
		{"enabled + seat + token + tailscale listen",
			config.Config{FleetAgentEnabled: true, AgentModel: "seat", FleetAuthToken: "tok", FleetListen: "100.64.0.5:18811"}, true},
		{"enabled + token but NO resolvable seat",
			config.Config{FleetAgentEnabled: true, FleetAuthToken: "tok"}, false},
		{"enabled + token + seat via workhorse fallback (agent_model unset)",
			config.Config{FleetAgentEnabled: true, Model: "workhorse", FleetAuthToken: "tok"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := supportsAgent(c.cfg); got != c.want {
				t.Fatalf("supportsAgent = %v, want %v (tasks: %v)", got, c.want, SupportedTasks(c.cfg))
			}
		})
	}
}

// TestAgentTaskUnconfiguredIsUnsupported: a media-only box 400s an agent
// dispatch exactly like any other unconfigured task_type.
func TestAgentTaskUnconfiguredIsUnsupported(t *testing.T) {
	_, _, err := BuildRequest(context.Background(), fullCfg(), "agent", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "unsupported task_type") {
		t.Fatalf("err = %v, want unsupported task_type", err)
	}
}

const agentSchemaJSON = `{"properties":{"answer":{"type":"string"}},"required":["answer"]}`

// TestBuildRequestAgentRequiresOutputSchema (roast delta 3): remote execution
// REQUIRES OutputSchema — a contract without one is refused at BuildRequest
// (the server maps this to a 400 at ack), never accepted and deferred later
// after the network round-trip and loop budget were already spent.
func TestBuildRequestAgentRequiresOutputSchema(t *testing.T) {
	payload := `{"schema_version":1,"goal":"summarize the docs"}`
	_, cleanup, err := BuildRequest(context.Background(), agentNodeCfg(t), "agent", json.RawMessage(payload))
	if cleanup != nil {
		cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), "output_schema") {
		t.Fatalf("err = %v, want output_schema-required", err)
	}
}

// TestBuildRequestAgentContractValidation: decode/validation failures are
// ack-time 400s with the core decoder's reasons intact.
func TestBuildRequestAgentContractValidation(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantSub string
	}{
		{"empty payload (schema_version 0)", `{}`, "schema_version"},
		{"wrong schema_version", `{"schema_version":2,"goal":"g","output_schema":` + agentSchemaJSON + `}`, "schema_version"},
		{"missing goal", `{"schema_version":1,"output_schema":` + agentSchemaJSON + `}`, "goal"},
		{"traversal doc name", `{"schema_version":1,"goal":"g","output_schema":` + agentSchemaJSON + `,"context":[{"name":"../evil","text":"x"}]}`, "flat filename"},
		{"malformed json", `{not json`, "agent contract"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, cleanup, err := BuildRequest(context.Background(), agentNodeCfg(t), "agent", json.RawMessage(c.payload))
			if cleanup != nil {
				cleanup()
			}
			if err == nil || !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("err = %v, want mention of %q", err, c.wantSub)
			}
		})
	}
}

// TestBuildRequestAgentHappyPath: a valid contract materializes its context
// docs under BaseDir()/pipeline-jobs/<job_id>/context/, hands the pipeline the
// DECODED contract, and the cleanup closure removes the whole job dir (the
// buildPipelineJob discipline: docs live exactly as long as the job).
func TestBuildRequestAgentHappyPath(t *testing.T) {
	cfg := agentNodeCfg(t)
	payload := `{"schema_version":1,"goal":"summarize the docs","output_schema":` + agentSchemaJSON + `,
		"context":[{"name":"a.md","text":"alpha"},{"name":"b.md","text":"beta"}],
		"max_steps":99,"timeout_sec":30}`
	req, cleanup, err := BuildRequest(context.Background(), cfg, "agent", json.RawMessage(payload))
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if req.Task != core.TaskAgentRun || req.Input != "summarize the docs" {
		t.Fatalf("req = %+v", req)
	}

	contract, ok := req.Params["contract"].(core.AgentContract)
	if !ok {
		t.Fatalf("params.contract is %T, want core.AgentContract", req.Params["contract"])
	}
	// Roast delta 5 ASSERTION (not a re-clamp): core.DecodeAgentContract owns
	// the ceiling; a payload asking 99 steps must arrive here already at the cap.
	if contract.MaxSteps != core.AgentMaxStepsCap {
		t.Fatalf("MaxSteps = %d, want the decode-time cap %d", contract.MaxSteps, core.AgentMaxStepsCap)
	}

	ctxDir, _ := req.Params["context_dir"].(string)
	jobID, _ := req.Params["job_id"].(string)
	if ctxDir == "" || jobID == "" {
		t.Fatalf("params missing context_dir/job_id: %#v", req.Params)
	}
	wantDir := filepath.Join(cfg.BaseDir(), "pipeline-jobs", jobID, "context")
	if ctxDir != wantDir {
		t.Fatalf("context_dir = %q, want %q", ctxDir, wantDir)
	}
	for name, want := range map[string]string{"a.md": "alpha", "b.md": "beta"} {
		b, rerr := os.ReadFile(filepath.Join(ctxDir, name))
		if rerr != nil || string(b) != want {
			t.Fatalf("doc %s = %q (%v), want %q", name, b, rerr, want)
		}
	}

	cleanup()
	if _, serr := os.Stat(filepath.Join(cfg.BaseDir(), "pipeline-jobs", jobID)); !os.IsNotExist(serr) {
		t.Fatalf("cleanup must remove the whole job dir (stat err = %v)", serr)
	}
}

// TestBuildRequestAgentDepthDerived (roast delta 2): the wire depth is
// ADVISORY — the receiving node executes at max(1, wireDepth), so a contract
// claiming depth 0 can never present itself as an origin on this node.
func TestBuildRequestAgentDepthDerived(t *testing.T) {
	cases := []struct {
		name      string
		wireDepth int
		want      int
	}{
		{"origin claim is lifted to 1", 0, 1},
		{"depth 1 stays 1", 1, 1},
		{"deeper wire depth is kept", 3, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload := `{"schema_version":1,"goal":"g","output_schema":` + agentSchemaJSON + `,
				"depth":` + jsonInt(c.wireDepth) + `}`
			req, cleanup, err := BuildRequest(context.Background(), agentNodeCfg(t), "agent", json.RawMessage(payload))
			if err != nil {
				t.Fatalf("BuildRequest: %v", err)
			}
			defer cleanup()
			contract := req.Params["contract"].(core.AgentContract)
			if contract.Depth != c.want {
				t.Fatalf("derived depth = %d, want %d", contract.Depth, c.want)
			}
		})
	}
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
