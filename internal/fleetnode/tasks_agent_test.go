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
	"fmt"
	"net/http"
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
	_, _, err := BuildRequest(context.Background(), fullCfg(), true, "agent", json.RawMessage(`{}`))
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
	_, cleanup, err := BuildRequest(context.Background(), agentNodeCfg(t), true, "agent", json.RawMessage(payload))
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
			_, cleanup, err := BuildRequest(context.Background(), agentNodeCfg(t), true, "agent", json.RawMessage(c.payload))
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
	req, cleanup, err := BuildRequest(context.Background(), cfg, true, "agent", json.RawMessage(payload))
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
			req, cleanup, err := BuildRequest(context.Background(), agentNodeCfg(t), true, "agent", json.RawMessage(payload))
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

// TestAgentLaneAdvertisementMatchesAdmission is the ADVERTISE==ADMIT pin.
// Health's four agent_* fields are the ONLY thing delegate/nodeview.go reads,
// so they alone decide whether a delegator will place work here; the ack-time
// guard decides whether that work is then accepted. When those two disagree,
// the delegator does everything right — reads health, passes the gate, mints a
// job, dispatches — and collects a 403 into Summary.Failed. It fails CLOSED,
// but it is precisely the mis-routing the design says it prevents.
//
// The disagreement was structural: health keyed on cfg.FleetAgentEnabled ALONE
// while the ack keyed on enabled + seat + (loopback-or-token), and the two
// read DIFFERENT notions of "loopback" (cfg.FleetListen vs the RESOLVED
// Options.LoopbackListener). One shared predicate over the resolved listener
// is what makes them undriftable, so this test asserts equality rather than
// two hardcoded expectations — a future change that moves only one side fails
// here whichever side it moves.
func TestAgentLaneAdvertisementMatchesAdmission(t *testing.T) {
	contract := `{"schema_version":1,"goal":"g","output_schema":` + agentSchemaJSON + `}`
	body := `{"job_id":"agd-parity","task_type":"agent","payload":` + contract + `}`
	cases := []struct {
		name     string
		loopback bool
		token    string
		// fleetListen is the CONFIG's bind. Left empty it agrees with every
		// resolved listener (ConfigLoopbackListen reads "" as the loopback
		// default), which is exactly why the four original rows could not see
		// the admission path keying on the config instead of the resolution.
		fleetListen string
		want        bool // the lane is usable at all in this configuration
	}{
		{"loopback listener, tokenless (open by locality)", true, "", "", true},
		{"loopback listener, token set", true, "tok", "", true},
		{"non-loopback listener, token set", false, "tok", "", true},
		// The row the drift produced: advertised a placeable lane, 403'd the
		// dispatch that followed.
		{"non-loopback listener, tokenless (RCE-class surface, refused)", false, "", "", false},
		// The row the FIX for that drift still missed: a --listen flag that
		// diverges from fleet_listen in the loopback dimension. Health asked
		// the predicate about the RESOLVED listener (loopback ⇒ advertise),
		// while BuildRequest's ack-time admission asked it about the CONFIG
		// (0.0.0.0 ⇒ not loopback, tokenless ⇒ refuse), so health advertised
		// agent and dispatch answered 400 `unsupported task_type "agent"`.
		// The inverse direction is not a hole: config-loopback with a resolved
		// non-loopback listener and no token is 403'd by the auth guard before
		// BuildRequest ever runs.
		{"config bind non-loopback, RESOLVED listener loopback, tokenless", true, "", "0.0.0.0:18811", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{
				Home:              t.TempDir(), // BaseDir() → any materialized job dir stays test-owned
				FleetAgentEnabled: true,
				AgentModel:        "agent-seat",
				AgentCtxTokens:    16384,
				FleetAuthToken:    tc.token,
				FleetListen:       tc.fleetListen,
			}
			s, _ := newTestServer(t, cfg, &fakeRunner{}, authOpts(tc.loopback))

			m := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
			advertisedFields := m["agent_enabled"] == true
			advertisedTask := false
			tasks, _ := m["supported_task_types"].([]any)
			for _, task := range tasks {
				if task == "agent" {
					advertisedTask = true
				}
			}

			var hdr map[string]string
			if tc.token != "" {
				hdr = map[string]string{"Authorization": "Bearer " + tc.token}
			}
			rec := do(t, s, http.MethodPost, "/fleet/dispatch", body, hdr)
			admitted := rec.Code == http.StatusAccepted

			if admitted != tc.want {
				t.Fatalf("dispatch admitted = %v (status %d), want %v (body %s)", admitted, rec.Code, tc.want, rec.Body.String())
			}
			if advertisedFields != admitted {
				t.Errorf("health agent_enabled = %v but dispatch admitted = %v — the delegator reads ONLY these fields, so an advertisement dispatch will refuse is a mis-route by construction (health %s)",
					advertisedFields, admitted, m)
			}
			if advertisedTask != admitted {
				t.Errorf("supported_task_types lists agent = %v but dispatch admitted = %v", advertisedTask, admitted)
			}
			if !tc.want {
				wantErrorShape(t, rec, http.StatusForbidden, agentLaneTokenRequired)
			}
		})
	}
}

// TestAgentDispatchAdversarialBodies drives the malformed/oversize contract
// shapes THROUGH THE MUX with the lane ON. The unit tests around BuildRequest
// prove the decoder refuses these; only an end-to-end dispatch proves the
// refusal survives the handler sequence — that it arrives as the taxonomy's
// 400 JSON envelope rather than a panic, a 500, or a silent accept, and that
// nothing was materialized on the way.
//
// The >256KiB row is the spec's Task-4 boundary demand: the cap existed only as
// a core unit test, while the wire path it bounds — a body that sails under the
// 1 MiB maxDispatchBody and is refused by the CONTRACT's own cap — was never
// exercised at the boundary.
func TestAgentDispatchAdversarialBodies(t *testing.T) {
	oversize := strings.Repeat("x", core.AgentContextMaxBytes+1)
	docJSON, err := json.Marshal(core.ContextDoc{Name: "big.md", Text: oversize})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		payload string
		wantSub string
		// materializes marks a row that fails AFTER buildAgentRun has created
		// the job dir. Every other row dies in DecodeAgentContract or the
		// output_schema check — i.e. BEFORE os.MkdirAll(jobsRoot) — so jobsRoot
		// never exists, ReadDir always errors, and the leftover-dir assertion
		// below never runs a single comparison: the os.RemoveAll calls it is
		// supposed to protect could be deleted and this test stayed green.
		materializes bool
	}{
		// A truncated contract also truncates the ENVELOPE it is embedded in, so
		// the strict envelope decoder catches it first — pinned here because the
		// caller-facing outcome (a 400 in the taxonomy's JSON shape) is what
		// matters, and "which decoder said no" must not change it.
		{"malformed contract JSON", `{"schema_version":1,"goal":`, "malformed dispatch body", false},
		{"contract is a JSON array, not an object", `[{"goal":"g"}]`, "agent contract", false},
		{"contract is a bare string", `"just a goal"`, "agent contract", false},
		{"unsupported schema_version", `{"schema_version":99,"goal":"g","output_schema":` + agentSchemaJSON + `}`, "schema_version", false},
		{"no goal", `{"schema_version":1,"output_schema":` + agentSchemaJSON + `}`, "goal", false},
		{"no output_schema (remote execution requires one)", `{"schema_version":1,"goal":"g"}`, "output_schema", false},
		{"context over the 256 KiB cap", `{"schema_version":1,"goal":"g","output_schema":` + agentSchemaJSON +
			`,"context":[` + string(docJSON) + `]}`, "cap", false},
		{"reserved device name as a context doc", `{"schema_version":1,"goal":"g","output_schema":` + agentSchemaJSON +
			`,"context":[{"name":"NUL","text":"vanishes"}]}`, "reserved Windows device name", false},
		{"traversal doc name", `{"schema_version":1,"goal":"g","output_schema":` + agentSchemaJSON +
			`,"context":[{"name":"../escape.md","text":"x"}]}`, "flat filename", false},
		// The row that actually exercises the cleanup: a doc name that is a
		// perfectly legal flat filename by the contract's rules but exceeds
		// every mainstream filesystem's 255-byte component limit, so the
		// failure lands in os.WriteFile — after the job dir and its context/
		// subdir are on disk. This is the only row that can catch a missing
		// os.RemoveAll.
		{"context doc name no filesystem can hold", `{"schema_version":1,"goal":"g","output_schema":` + agentSchemaJSON +
			`,"context":[{"name":"` + strings.Repeat("n", 400) + `.md","text":"x"}]}`, "writing context doc", true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := agentNodeCfg(t)
			s, _ := newTestServer(t, cfg, &fakeRunner{}, authOpts(true))
			body := fmt.Sprintf(`{"job_id":"agd-bad-%d","task_type":"agent","payload":%s}`, i, tc.payload)
			rec := do(t, s, http.MethodPost, "/fleet/dispatch", body,
				map[string]string{"Authorization": "Bearer " + cfg.FleetAuthToken})
			wantErrorShape(t, rec, http.StatusBadRequest, tc.wantSub)

			// A refused dispatch must leave nothing behind: buildAgentRun mints
			// its job dir before writing docs, so a mid-validation bail that
			// forgot to clean up would strand a directory per bad request.
			jobsRoot := filepath.Join(cfg.BaseDir(), "pipeline-jobs")
			entries, rerr := os.ReadDir(jobsRoot)
			switch {
			case tc.materializes:
				// The dir MUST exist here — otherwise this row died early too
				// and proves nothing about cleanup.
				if rerr != nil {
					t.Fatalf("reading %s: %v — this row is supposed to fail AFTER materialization", jobsRoot, rerr)
				}
			case rerr != nil && !os.IsNotExist(rerr):
				t.Fatalf("reading %s: %v", jobsRoot, rerr)
			}
			if len(entries) != 0 {
				t.Fatalf("refused dispatch left %d job dir(s) under %s", len(entries), jobsRoot)
			}
		})
	}
}
