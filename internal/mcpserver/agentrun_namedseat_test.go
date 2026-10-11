package mcpserver

// Register D-125: agent_run(model: <seat>) validated the named planner against THIS box's llama-swap roster only, so a
// seat served by another fleet node could not be asked for by name from a box that does not serve it, even though the
// fleet's own views (health agent_seat, served_models) say exactly who runs it. And a routed agent_run (register C-46)
// built its contract without an output_schema, which the delegator refuses on every node as `noschema`.
//
// These tests drive the real door at a real local roster (an httptest llama-swap) and real scripted fleet nodes
// (httptest, the wire shapes a node speaks: GET /fleet/health, POST /fleet/dispatch, GET /fleet/jobs/{id}), through the
// real delegator engine: nothing between the door and the wire is faked, except where a test injects the dispatch seam
// (s.reviewFleet) to read the exact arguments the door hands the engine.
//
// HERMETIC. The delegator appends a row to cfg.LedgerPath for every contract it places. The stale build of this item
// appended 1,194 fixture rows to an operator's real ledger because its fixture set Home and StateDir after
// config.Default() had already fixed LedgerPath from the real home. This fixture sets the ledger and every state path to
// temp explicitly (the package's TestMain also points LOCAL_OFFLOAD_HOME at a throwaway root), proves the configuration
// is temp (requireHermetic), and proves the real ledger took no row of the run (guardRealLedger).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	placetable "github.com/dmmdea/offload-harness/internal/placement"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
)

const (
	nsLocalSeat = wallGateSeat      // what this box's llama-swap serves, and the box's own agent seat
	nsFastSeat  = "fast-agent-seat" // an agent seat only fleet nodes serve
	nsBigSeat   = "big-agent-seat"  // another node's agent seat
	nsAnswer    = "digest from the fleet"
)

// nsNode is a scripted fleet node. It advertises its agent seat and the roster it serves, validates every dispatched
// contract the way a real node's ack does (the shape, and the output_schema a remote run requires), remembers each one,
// and answers every job as finished on the seat it is scripted to report.
type nsNode struct {
	id        string
	agentSeat string
	served    []string
	ran       string // the seat the job's result says it came from; "" = the agent seat
	layers    []placetable.LayerRow
	agentOff  bool // the node does not advertise the agent lane
	url       string

	healths    atomic.Int64
	dispatches atomic.Int64
	mu         sync.Mutex
	contracts  []core.AgentContract
}

func (n *nsNode) received() []core.AgentContract {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]core.AgentContract(nil), n.contracts...)
}

func newNSNode(t *testing.T, id, agentSeat string, served ...string) *nsNode {
	t.Helper()
	n := &nsNode{id: id, agentSeat: agentSeat, served: served}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, _ *http.Request) {
		n.healths.Add(1)
		h := map[string]any{
			"node_id": n.id, "queue_depth": 0, "agent_seat": n.agentSeat, "agent_ctx_tokens": 32768,
			"agent_seat_resident": true, "agent_enabled": !n.agentOff, "jobs_queued": 0, "jobs_running": 0,
			"served_models": n.served,
		}
		if len(n.layers) > 0 {
			h["layers"] = n.layers
		}
		_ = json.NewEncoder(w).Encode(h)
	})
	mux.HandleFunc("POST /fleet/dispatch", func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			JobID   string          `json:"job_id"`
			Payload json.RawMessage `json:"payload"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		c, err := core.DecodeAgentContract(bytes.NewReader(env.Payload))
		if err == nil && len(c.OutputSchema) == 0 {
			err = fmt.Errorf("agent contract: output_schema required for remote execution (the delegator must have a mechanical check before merging)")
		}
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": err.Error()})
			return
		}
		n.dispatches.Add(1)
		n.mu.Lock()
		n.contracts = append(n.contracts, c)
		n.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": env.JobID, "status": "accepted"})
	})
	mux.HandleFunc("GET /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		seat := n.ran
		if seat == "" {
			seat = n.agentSeat
		}
		wire, _ := json.Marshal(core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion, NodeID: n.id, Seat: seat, Output: nsAnswer,
			Structured: json.RawMessage(`{"answer":"` + nsAnswer + `"}`), Steps: 3, StopReason: "done",
		})
		_, _ = io.WriteString(w, fmt.Sprintf(`{"job_id":%q,"state":"done","data":%s}`, r.PathValue("id"), wire))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	n.url = srv.URL
	return n
}

// deadNodeURL is the base of a fleet node that is down: a port that was served and is closed.
func deadNodeURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}

// nsDoor is the agent_run door over a local llama-swap that serves localModels and a fleet of the given node bases.
// localHits counts EVERY request the local seat receives, so "this box was never asked" is a measurement; the
// delegator's local runner fails the test if it runs (a named fleet seat must never fall back to this box).
func nsDoor(t *testing.T, localModels []string, remotes []string, mutate func(*config.Config)) (s *Server, localHits *atomic.Int64) {
	t.Helper()
	rosterprobe.Default.Reset()
	t.Cleanup(rosterprobe.Default.Reset)
	var hits atomic.Int64
	local := wallGateServerSeats(t, &hits, localModels...)
	root := t.TempDir() // everything this test's door may write lives under this one directory
	s = wallGateServerFor(t, local.URL, 0, func(c *config.Config) {
		c.Home = filepath.Join(root, "home")
		c.StateDir = filepath.Join(root, "state")
		c.LedgerPath = filepath.Join(c.Home, "ledger.jsonl")
		for _, d := range []string{c.Home, c.StateDir} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		c.DelegateRemotes = remotes
		c.AgentPlacementWaitSec = -1 // a refusal is the answer here, never a two-minute wait for room
		if mutate != nil {
			mutate(c)
		}
	})
	s.localAgent = func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		t.Error("the delegator's local runner ran: a named fleet seat must never fall back to this box")
		return core.AgentWireResult{}, nil
	}
	requireHermetic(t, s.p.Cfg(), root)
	return s, &hits
}

// requireHermetic fails unless the configuration the door runs under keeps every path the delegator writes (the home, the
// state root, the ledger) inside root, the one directory the test owns.
func requireHermetic(t *testing.T, cfg config.Config, root string) {
	t.Helper()
	root = filepath.Clean(root)
	for name, p := range map[string]string{"Home": cfg.Home, "StateDir": cfg.StateDir, "LedgerPath": cfg.LedgerPath} {
		if p == "" {
			t.Fatalf("fixture config %s is unset: the delegator would resolve it from the real home", name)
		}
		if rel, err := filepath.Rel(root, filepath.Clean(p)); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("fixture config %s = %q is outside the test's own directory %q: a run would write a real file", name, p, root)
		}
	}
}

// guardRealLedger fails the test when a row carrying marker was appended to the ledger the operator's real home would
// name (DefaultBase without LOCAL_OFFLOAD_HOME). The ledger is append-only, so what matters is the bytes added after the
// test began, and unrelated writers (a live harness on the same machine) cannot trip it: only the marker can.
func guardRealLedger(t *testing.T, marker string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	path := filepath.Join(home, ".local-offload", "ledger.jsonl")
	var before int64
	if st, serr := os.Stat(path); serr == nil {
		before = st.Size()
	}
	t.Cleanup(func() {
		f, oerr := os.Open(path)
		if oerr != nil {
			return
		}
		defer f.Close()
		if _, serr := f.Seek(before, io.SeekStart); serr != nil {
			return
		}
		added, _ := io.ReadAll(f)
		if bytes.Contains(added, []byte(marker)) {
			t.Errorf("the real ledger %s took a row of this test (marker %q): the fixture is not hermetic", path, marker)
		}
	})
}

// runNS calls the agent_run door with args (a goal carrying marker is added when none is given) and decodes the answer.
func runNS(t *testing.T, s *Server, marker string, args map[string]any) map[string]any {
	t.Helper()
	guardRealLedger(t, marker)
	if _, has := args["goal"]; !has {
		args["goal"] = "digest the standing notes in one paragraph " + marker
	}
	b, _ := json.Marshal(args)
	res, err := s.handleAgentRun(context.Background(), callReq(string(b)))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	return decodeResult(t, res)
}

func nsMarker(t *testing.T) string { return "ns-marker-" + strings.ReplaceAll(t.Name(), "/", "-") }

func nsReason(m map[string]any) string { r, _ := m["reason"].(string); return r }

// TestAgentRunNamingASeatOnlyAFleetNodeServesRunsItThere is the cure: the local roster lacks the seat, two fleet nodes
// have it as their agent seat and a third has another. Exactly ONE contract reaches ONE of the two, none reaches the
// third or this box's runner, and the contract is dispatchable: an output_schema, the door, the call's own arguments.
func TestAgentRunNamingASeatOnlyAFleetNodeServesRunsItThere(t *testing.T) {
	a := newNSNode(t, "node-a", nsFastSeat, nsFastSeat)
	b := newNSNode(t, "node-b", nsFastSeat, nsFastSeat)
	c := newNSNode(t, "node-c", nsBigSeat, nsBigSeat)
	s, _ := nsDoor(t, []string{nsLocalSeat}, []string{a.url, b.url, c.url}, nil)

	m := runNS(t, s, nsMarker(t), map[string]any{"model": nsFastSeat, "max_steps": 4, "profile": "research"})

	if m["deferred"] == true {
		t.Fatalf("a seat a fleet node serves must run there, got a defer: %v", m)
	}
	if got := a.dispatches.Load() + b.dispatches.Load(); got != 1 {
		t.Fatalf("exactly one contract must reach the nodes that serve the seat, got %d (a=%d b=%d)", got, a.dispatches.Load(), b.dispatches.Load())
	}
	if n := c.dispatches.Load(); n != 0 {
		t.Fatalf("the node whose agent seat is another model received %d contract(s)", n)
	}
	ran := a
	if b.dispatches.Load() == 1 {
		ran = b
	}
	got := ran.received()
	if len(got) != 1 {
		t.Fatalf("the node must receive exactly one contract, got %d", len(got))
	}
	ct := got[0]
	if len(ct.OutputSchema) == 0 {
		t.Fatalf("a remote run requires an output_schema: the gate refuses a contract without one as noschema")
	}
	if ct.Door != "agent_run" || ct.MaxSteps != 4 || ct.Profile != "research" || !ct.TimeoutAuto || ct.Layer != "" || ct.Depth != 0 {
		t.Fatalf("the call's own arguments must ride the contract: %+v", ct)
	}
	for k, want := range map[string]string{"output": nsAnswer, "node": ran.id, "seat": nsFastSeat, "model": nsFastSeat, "route": "remote", "executed_on": "fleet"} {
		if m[k] != want {
			t.Errorf("response %q = %v, want %q (the response says where and on what it ran): %v", k, m[k], want, m)
		}
	}
	if p, _ := m["placement"].(string); !strings.Contains(p, ran.id) {
		t.Errorf("placement must name the node the run landed on, got %q", p)
	}
	if st, _ := m["structured"].(map[string]any); st["answer"] != nsAnswer {
		t.Errorf("the node's structured answer must be published, got %v", m["structured"])
	}
}

// TestAgentRunNamingASeatOnlyAFleetNodeServesHandsTheEngineBothBases pins the narrowing: the engine is called once, at
// route=remote, with the bases of the nodes whose agent seat is the name (in roster order) and none other, because a
// call's own remotes list REPLACES the configured roster (delegate.RunWith) and is checked against it (RosterOnly).
func TestAgentRunNamingASeatOnlyAFleetNodeServesHandsTheEngineBothBases(t *testing.T) {
	a := newNSNode(t, "node-a", nsFastSeat, nsFastSeat)
	c := newNSNode(t, "node-c", nsBigSeat, nsBigSeat)
	b := newNSNode(t, "node-b", "FAST-AGENT-SEAT", nsFastSeat) // the name is matched without regard to case
	s, _ := nsDoor(t, []string{nsLocalSeat}, []string{a.url, c.url, b.url}, nil)
	var calls, gotRemotes = 0, []string(nil)
	var gotRoute string
	var gotOpts *delegate.RunOptions
	var gotSubtasks []core.AgentContract
	s.reviewFleet = func(_ context.Context, _ config.Config, _ delegate.LocalRunner, subtasks []core.AgentContract, route string, remotes []string, opts *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error) {
		calls++
		gotRoute, gotRemotes, gotOpts, gotSubtasks = route, remotes, opts, subtasks
		return []delegate.PlacedResult{{Node: "node-a", Seat: nsFastSeat, PlacementReason: "route=remote forced → node-a",
			Result: core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: nsAnswer, Steps: 1, Seat: nsFastSeat, StopReason: "done"}}}, delegate.Summary{Succeeded: 1}, nil
	}

	m := runNS(t, s, nsMarker(t), map[string]any{"model": nsFastSeat})

	if m["deferred"] == true {
		t.Fatalf("want an answer, got %v", m)
	}
	if calls != 1 || gotRoute != "remote" || len(gotSubtasks) != 1 {
		t.Fatalf("the engine must be called once, at route=remote, with one contract: calls=%d route=%q subtasks=%d", calls, gotRoute, len(gotSubtasks))
	}
	if want := []string{a.url, b.url}; len(gotRemotes) != 2 || gotRemotes[0] != want[0] || gotRemotes[1] != want[1] {
		t.Fatalf("remotes = %v, want exactly the two nodes whose agent seat is %q, in roster order: %v", gotRemotes, nsFastSeat, want)
	}
	if gotOpts == nil || !gotOpts.RosterOnly {
		t.Errorf("the narrowed list must be checked against the roster (RosterOnly): %+v", gotOpts)
	}
}

// TestAgentRunNamingASeatServedNowhereIsRefusedWithTheRosterMessageAndWhereItLooked: a name no node serves keeps the
// established roster sentence, which every caller already reads, and says where the fleet looked, one clause per node.
func TestAgentRunNamingASeatServedNowhereIsRefusedWithTheRosterMessageAndWhereItLooked(t *testing.T) {
	a := newNSNode(t, "node-a", nsBigSeat, nsBigSeat)
	b := newNSNode(t, "node-b", "other-agent-seat", "other-agent-seat")
	s, _ := nsDoor(t, []string{nsLocalSeat}, []string{a.url, b.url}, nil)

	m := runNS(t, s, nsMarker(t), map[string]any{"model": nsFastSeat})

	if m["deferred"] != true {
		t.Fatalf("a seat nobody serves must still be refused, got %v", m)
	}
	reason := nsReason(m)
	for _, need := range []string{
		fmt.Sprintf("explicitly requested model %q is not in the endpoint's served roster", nsFastSeat), // the established sentence, unchanged
		"node-a", nsBigSeat, "node-b", "other-agent-seat", // where the fleet looked, and what it saw
	} {
		if !strings.Contains(reason, need) {
			t.Fatalf("the refusal must say %q, got %q", need, reason)
		}
	}
	if m["defer_class"] != core.DeferClassConfig {
		t.Errorf("defer_class = %v, want %q: every node answered, so the name is simply nobody's", m["defer_class"], core.DeferClassConfig)
	}
	if a.dispatches.Load()+b.dispatches.Load() != 0 {
		t.Errorf("nothing may be dispatched for a seat nobody serves")
	}
}

// TestAgentRunNamingAnUnservedSeatWithAnUnreachableNodeIsAnInfrastructureRefusal: when a node did not answer, "nobody
// serves it" is not established: the class says a box is broken, and the reason names the box that did not answer.
func TestAgentRunNamingAnUnservedSeatWithAnUnreachableNodeIsAnInfrastructureRefusal(t *testing.T) {
	a := newNSNode(t, "node-a", nsBigSeat, nsBigSeat)
	dead := deadNodeURL(t)
	s, _ := nsDoor(t, []string{nsLocalSeat}, []string{a.url, dead}, nil)

	m := runNS(t, s, nsMarker(t), map[string]any{"model": nsFastSeat})

	if m["deferred"] != true {
		t.Fatalf("want a refusal, got %v", m)
	}
	if m["defer_class"] != core.DeferClassInfrastructure {
		t.Errorf("defer_class = %v, want %q: a node that did not answer might have served the seat", m["defer_class"], core.DeferClassInfrastructure)
	}
	reason := nsReason(m)
	if !strings.Contains(reason, strings.TrimPrefix(dead, "http://")) {
		t.Errorf("the refusal must name the node that did not answer (%s), got %q", dead, reason)
	}
	if !strings.Contains(reason, "is not in the endpoint's served roster") || !strings.Contains(reason, "node-a") {
		t.Errorf("the refusal must keep the roster sentence and say what the node that answered has, got %q", reason)
	}
}

// TestAgentRunWithARouteRunsThroughTheRealGateWithASchema is the C-46 cure for every routed agent_run, named seat or not:
// the contract carries a flat output_schema, so the real gate no longer refuses it as `noschema` on every node.
func TestAgentRunWithARouteRunsThroughTheRealGateWithASchema(t *testing.T) {
	a := newNSNode(t, "node-a", nsBigSeat, nsBigSeat)
	b := newNSNode(t, "node-b", nsFastSeat, nsFastSeat)
	s, _ := nsDoor(t, []string{nsLocalSeat}, []string{a.url, b.url}, nil)

	m := runNS(t, s, nsMarker(t), map[string]any{"route": "remote", "max_steps": 3})

	if m["deferred"] == true {
		t.Fatalf("a routed agent_run must pass the real gate, got a defer: %v", m)
	}
	if strings.Contains(fmt.Sprint(m), "noschema") {
		t.Fatalf("noschema still reaches the caller: %v", m)
	}
	if a.dispatches.Load()+b.dispatches.Load() != 1 {
		t.Fatalf("exactly one node must take the contract (a=%d b=%d)", a.dispatches.Load(), b.dispatches.Load())
	}
	if m["output"] != nsAnswer || m["route"] != "remote" || m["executed_on"] != "fleet" || m["node"] == nil || m["placement"] == nil {
		t.Errorf("the response must name where it ran: %v", m)
	}
	if _, has := m["model"]; has {
		t.Errorf("a call that named no model must not publish one it did not ask for: %v", m)
	}
}

// TestAgentRunWithRouteRemoteAndAModelRunsOnlyNodesThatServeIt: route=remote with a model is a fleet call even when this
// box serves the name, because the caller asked for a remote seat, and only the nodes whose agent seat is the name are asked.
func TestAgentRunWithRouteRemoteAndAModelRunsOnlyNodesThatServeIt(t *testing.T) {
	a := newNSNode(t, "node-a", nsBigSeat, nsBigSeat)
	b := newNSNode(t, "node-b", nsFastSeat, nsFastSeat)
	c := newNSNode(t, "node-c", nsBigSeat, nsBigSeat)
	// the local roster serves the name too: route=remote must not shortcut to this box
	s, localHits := nsDoor(t, []string{nsLocalSeat, nsBigSeat}, []string{a.url, b.url, c.url}, nil)

	m := runNS(t, s, nsMarker(t), map[string]any{"route": "remote", "model": nsBigSeat})

	if m["deferred"] == true {
		t.Fatalf("want an answer from a node that serves %q, got %v", nsBigSeat, m)
	}
	if b.dispatches.Load() != 0 {
		t.Errorf("the node whose agent seat is another model received %d contract(s)", b.dispatches.Load())
	}
	if got := a.dispatches.Load() + c.dispatches.Load(); got != 1 {
		t.Errorf("exactly one of the two nodes that serve %q must run it, got %d", nsBigSeat, got)
	}
	if m["seat"] != nsBigSeat || m["model"] != nsBigSeat || m["executed_on"] != "fleet" || m["route"] != "remote" {
		t.Errorf("the response must name the seat the node ran: %v", m)
	}
	if n := localHits.Load(); n != 0 {
		t.Errorf("route=remote must not ask this box's roster: the local seat received %d request(s)", n)
	}
}

// TestAgentRunRefusesAModelOnARouteThatCannotHonourIt: auto, spread and queue place by capacity, not by seat name, so a
// model on them was silently ignored (3 real agent_run route rows in the ledger). It is refused, naming route=remote,
// before anything is asked of the fleet.
func TestAgentRunRefusesAModelOnARouteThatCannotHonourIt(t *testing.T) {
	for _, route := range []string{"auto", "spread", "queue"} {
		t.Run(route, func(t *testing.T) {
			a := newNSNode(t, "node-a", nsFastSeat, nsFastSeat)
			s, _ := nsDoor(t, []string{nsLocalSeat}, []string{a.url}, nil)
			dispatched := false
			s.reviewFleet = func(context.Context, config.Config, delegate.LocalRunner, []core.AgentContract, string, []string, *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error) {
				dispatched = true
				return nil, delegate.Summary{}, nil
			}

			m := runNS(t, s, nsMarker(t), map[string]any{"route": route, "model": nsFastSeat})

			if m["deferred"] != true {
				t.Fatalf("route=%s with a model must be refused, got %v", route, m)
			}
			if r := nsReason(m); !strings.Contains(r, "route=remote") || !strings.Contains(r, nsFastSeat) || !strings.Contains(r, route) {
				t.Errorf("the refusal must name the model, the route and route=remote: %q", r)
			}
			if m["defer_class"] != core.DeferClassContract {
				t.Errorf("defer_class = %v, want %q: the fix is in the call", m["defer_class"], core.DeferClassContract)
			}
			if steps, _ := m["steps"].(float64); steps != 0 {
				t.Errorf("steps = %v, want 0", m["steps"])
			}
			if dispatched || a.healths.Load() != 0 || a.dispatches.Load() != 0 {
				t.Errorf("a refused call must touch nothing: dispatched=%v healths=%d dispatches=%d", dispatched, a.healths.Load(), a.dispatches.Load())
			}
		})
	}
}

// TestAgentRunDoesNotPublishAnAnswerFromAnotherSeat: a node can change its agent seat between the probe and the dispatch
// (an operator edit). The answer is checked against the name before it is published: from another seat it is withheld.
func TestAgentRunDoesNotPublishAnAnswerFromAnotherSeat(t *testing.T) {
	a := newNSNode(t, "node-a", nsFastSeat, nsFastSeat)
	a.ran = "some-other-seat" // the job reports it ran elsewhere
	s, _ := nsDoor(t, []string{nsLocalSeat}, []string{a.url}, nil)

	m := runNS(t, s, nsMarker(t), map[string]any{"model": nsFastSeat})

	if m["deferred"] != true {
		t.Fatalf("an answer from another seat must be deferred, got %v", m)
	}
	if out, has := m["output"]; has {
		t.Errorf("the answer must be withheld, got output %v", out)
	}
	if r := nsReason(m); !strings.Contains(r, "some-other-seat") || !strings.Contains(r, nsFastSeat) {
		t.Errorf("the reason must name both seats: %q", r)
	}
	if a.dispatches.Load() != 1 {
		t.Errorf("test premise: the node ran the contract once, got %d", a.dispatches.Load())
	}
}

// TestAgentRunNamingAnAliasThatIsNotTheAgentSeatIsRefusedNamingTheAgentSeat: the node serves the alias (its roster
// lists it) but runs its agent seat, so the contract would not run the name. The door does not guess: it refuses and
// names the seat the node does run.
func TestAgentRunNamingAnAliasThatIsNotTheAgentSeatIsRefusedNamingTheAgentSeat(t *testing.T) {
	a := newNSNode(t, "node-a", nsBigSeat, nsBigSeat, "an-alias-the-node-serves")
	s, _ := nsDoor(t, []string{nsLocalSeat}, []string{a.url}, nil)

	m := runNS(t, s, nsMarker(t), map[string]any{"model": "an-alias-the-node-serves"})

	if m["deferred"] != true {
		t.Fatalf("an alias that is not an agent seat must be refused, got %v", m)
	}
	reason := nsReason(m)
	for _, need := range []string{"node-a", nsBigSeat, "an-alias-the-node-serves", "not as its agent seat"} {
		if !strings.Contains(reason, need) {
			t.Errorf("the refusal must say %q, got %q", need, reason)
		}
	}
	if a.dispatches.Load() != 0 {
		t.Errorf("nothing may be dispatched for a name that is not an agent seat")
	}
}

// TestAgentRunNamingALayersAgentSeatSetsTheContractLayer: a composite node serves a seat by layer. Its agent-role seat in
// a layer other than the default is reachable by naming the layer (register A-100), so the contract carries it and the
// engine is handed that node alone.
func TestAgentRunNamingALayersAgentSeatSetsTheContractLayer(t *testing.T) {
	plain := newNSNode(t, "node-plain", nsBigSeat, nsBigSeat)
	comp := newNSNode(t, "node-comp", "default-agent-seat", "default-agent-seat", nsFastSeat)
	comp.layers = []placetable.LayerRow{
		{Name: "base", Admissible: true, Reason: "ok", Seats: []placetable.SeatRow{{Role: "agent", Model: "default-agent-seat"}}},
		{Name: "wide", Admissible: true, Reason: "ok", Seats: []placetable.SeatRow{{Role: "long", Model: "wide-long-seat"}, {Role: "agent", Model: nsFastSeat}}},
	}
	s, _ := nsDoor(t, []string{nsLocalSeat}, []string{plain.url, comp.url}, nil)
	var gotRemotes []string
	var gotContract core.AgentContract
	s.reviewFleet = func(_ context.Context, _ config.Config, _ delegate.LocalRunner, subtasks []core.AgentContract, _ string, remotes []string, _ *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error) {
		gotRemotes, gotContract = remotes, subtasks[0]
		return []delegate.PlacedResult{{Node: "node-comp", Seat: nsFastSeat, PlacementReason: "route=remote forced → node-comp",
			Result: core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: nsAnswer, Seat: nsFastSeat}}}, delegate.Summary{Succeeded: 1}, nil
	}

	m := runNS(t, s, nsMarker(t), map[string]any{"model": nsFastSeat})

	if m["deferred"] == true {
		t.Fatalf("want an answer, got %v", m)
	}
	if gotContract.Layer != "wide" {
		t.Errorf("the contract must name the layer whose agent seat is %q, got %q", nsFastSeat, gotContract.Layer)
	}
	if len(gotRemotes) != 1 || gotRemotes[0] != comp.url {
		t.Errorf("the engine must be handed only the composite node, got %v", gotRemotes)
	}
}

// TestAgentRunNamingALayersAgentSeatPassesTheRealGate: the same composite node through the real delegator, to show the
// contract that names the layer is eligible there: the gate decides it FOR that layer from the node's own rows, and the
// node receives the layer in the contract it is dispatched.
func TestAgentRunNamingALayersAgentSeatPassesTheRealGate(t *testing.T) {
	comp := newNSNode(t, "node-comp", "default-agent-seat", "default-agent-seat", nsFastSeat)
	comp.layers = []placetable.LayerRow{
		{Name: "base", Admissible: true, Reason: "ok", Seats: []placetable.SeatRow{{Role: "agent", Model: "default-agent-seat", CtxTokens: 32768}}},
		{Name: "wide", Admissible: true, Reason: "ok", Seats: []placetable.SeatRow{{Role: "agent", Model: nsFastSeat, CtxTokens: 32768}}},
	}
	comp.ran = nsFastSeat
	s, _ := nsDoor(t, []string{nsLocalSeat}, []string{comp.url}, nil)

	m := runNS(t, s, nsMarker(t), map[string]any{"model": nsFastSeat})

	if m["deferred"] == true {
		t.Fatalf("a layer's agent seat must pass the real gate, got %v", m)
	}
	got := comp.received()
	if len(got) != 1 || got[0].Layer != "wide" {
		t.Fatalf("the node must receive one contract naming layer wide, got %+v", got)
	}
}

// TestAgentRunNamingALocallyServedSeatNeverAsksTheFleet and its forced-local twin are the guards on the fork: a name this
// box serves runs here, and route=local never looks at the fleet whatever the name is.
func TestAgentRunNamingALocallyServedSeatNeverAsksTheFleet(t *testing.T) {
	a := newNSNode(t, "node-a", nsFastSeat, nsFastSeat)
	s, _ := nsDoor(t, []string{nsLocalSeat, "second-local-seat"}, []string{a.url}, nil)

	m := runNS(t, s, nsMarker(t), map[string]any{"model": "second-local-seat"})

	if m["deferred"] == true {
		t.Fatalf("a locally served seat must run here, got %v", m)
	}
	if m["model"] != "second-local-seat" || m["executed_on"] != nil {
		t.Errorf("the run must be the local one: %v", m)
	}
	if a.healths.Load() != 0 || a.dispatches.Load() != 0 {
		t.Errorf("the fleet must not be asked for a seat this box serves: healths=%d dispatches=%d", a.healths.Load(), a.dispatches.Load())
	}
}

func TestAgentRunForcedLocalNeverAsksTheFleetForANamedSeat(t *testing.T) {
	a := newNSNode(t, "node-a", nsFastSeat, nsFastSeat)
	s, _ := nsDoor(t, []string{nsLocalSeat}, []string{a.url}, nil)

	m := runNS(t, s, nsMarker(t), map[string]any{"route": "local", "model": nsFastSeat})

	if m["deferred"] != true || !strings.Contains(nsReason(m), "is not in the endpoint's served roster") {
		t.Fatalf("route=local must keep the roster refusal, got %v", m)
	}
	if strings.Contains(nsReason(m), "node-a") {
		t.Errorf("route=local must not report a fleet it did not look at: %q", nsReason(m))
	}
	if _, has := m["defer_class"]; has {
		t.Errorf("the local roster refusal carries no class, as before: %v", m)
	}
	if a.healths.Load() != 0 || a.dispatches.Load() != 0 {
		t.Errorf("route=local must not ask the fleet: healths=%d dispatches=%d", a.healths.Load(), a.dispatches.Load())
	}
}

// TestAgentRunWithNoFleetKeepsTheRosterRefusalExactly: a box with no delegate_remotes has nobody to ask, so the refusal
// is byte-for-byte the one it always gave.
func TestAgentRunWithNoFleetKeepsTheRosterRefusalExactly(t *testing.T) {
	s, _ := nsDoor(t, []string{nsLocalSeat}, nil, nil)

	m := runNS(t, s, nsMarker(t), map[string]any{"model": nsFastSeat})

	want := fmt.Sprintf("explicitly requested model %q is not in the endpoint's served roster — pick a served model (offload_status lists them)", nsFastSeat)
	if m["deferred"] != true || nsReason(m) != want {
		t.Fatalf("reason = %q, want exactly %q", nsReason(m), want)
	}
	if _, has := m["defer_class"]; has {
		t.Errorf("no fleet was asked, so no class is claimed: %v", m)
	}
}

// TestAgentRunFleetPathSaysWhatDidNotTravel: read_root names a directory on THIS box and judge is a same-seat completion
// on this box's door; neither can ride a contract, and a caller who passed them is told instead of left to think the goal
// saw them.
func TestAgentRunFleetPathSaysWhatDidNotTravel(t *testing.T) {
	a := newNSNode(t, "node-a", nsFastSeat, nsFastSeat)
	s, _ := nsDoor(t, []string{nsLocalSeat}, []string{a.url}, nil)

	m := runNS(t, s, nsMarker(t), map[string]any{"model": nsFastSeat, "read_root": t.TempDir(), "judge": true})

	if m["deferred"] == true {
		t.Fatalf("want an answer, got %v", m)
	}
	notes, _ := m["notes"].([]any)
	joined := fmt.Sprint(notes)
	if !strings.Contains(joined, "read_root") || !strings.Contains(joined, "judge") {
		t.Errorf("the response must say read_root and judge did not travel, got notes %v", m["notes"])
	}
	// and a call that passed neither carries no notes
	m = runNS(t, s, nsMarker(t), map[string]any{"model": nsFastSeat})
	if _, has := m["notes"]; has {
		t.Errorf("a call with nothing left behind carries no notes: %v", m["notes"])
	}
}

// TestAgentRunNamedSeatRefusesABrowseGrant: the browse tool drives THIS machine's own browser, and a seat only a fleet
// node serves can only run there, where the grant cannot travel.
func TestAgentRunNamedSeatRefusesABrowseGrant(t *testing.T) {
	a := newNSNode(t, "node-a", nsFastSeat, nsFastSeat)
	s, _ := nsDoor(t, []string{nsLocalSeat}, []string{a.url}, func(c *config.Config) {
		b := browseCfg()
		c.AgentAllowBrowse, c.BrowsePython, c.BrowseScript, c.BrowseDecisionURL = true, b.BrowsePython, b.BrowseScript, b.BrowseDecisionURL
	})

	m := runNS(t, s, nsMarker(t), map[string]any{"model": nsFastSeat, "allow_browse": true, "browse_hosts": []string{"example.com"}})

	if m["deferred"] != true || !strings.Contains(nsReason(m), "allow_browse") {
		t.Fatalf("a browse grant on a seat served only by the fleet must be refused naming allow_browse, got %v", m)
	}
	if a.dispatches.Load() != 0 {
		t.Errorf("a refused call dispatches nothing")
	}
}
