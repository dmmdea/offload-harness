// The ask lane's result cache, AS WIRED. internal/askcache tests the container; what is
// pinned HERE is the only thing that makes it a feature rather than dead code — that
// handleAsk actually consults it, that a hit is OBSERVABLE in the response, and that the
// three things which must never be cached never are.

package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// countingSeat returns a local runner that answers correctly and counts how many times the
// SEAT actually ran. Seat time (46-75 s measured) is the entire cost this cache exists to
// avoid, so "did the seat run" is the only honest assertion about a hit.
func countingSeat(t *testing.T, runs *int) func(context.Context, core.AgentContract) (core.AgentWireResult, error) {
	t.Helper()
	return func(_ context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		*runs++
		anchor := anchorsOf(t, c)[0]
		return core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Seat:          "fake-seat",
			Output:        "the cap is 32, set by " + anchor,
			Structured:    json.RawMessage(`{"answer":"the cap is 32","evidence":"const ` + anchor + ` = 32"}`),
			Steps:         2,
			StopReason:    "done",
		}, nil
	}
}

// TestAskSecondIdenticalCallSkipsTheSeatAndSaysSo is the wiring pin. An unwired cache is
// worse than no cache: it costs code and buys nothing. So this asserts the seat ran ONCE
// across two identical calls, and that the second response declares itself cached — a
// cached answer presented as a fresh run would be a lie about how the answer was obtained.
func TestAskSecondIdenticalCallSkipsTheSeatAndSaysSo(t *testing.T) {
	dir, p := askFixture(t)
	runs := 0
	s := askTestServer(t, countingSeat(t, &runs))
	args := askArgs("what is the queue cap", p, dir)

	first, err := s.handleAsk(context.Background(), callReq(args))
	if err != nil {
		t.Fatalf("handleAsk: %v", err)
	}
	m1 := decodeResult(t, first)
	if m1["cache_hit"] != false {
		t.Fatalf("a fresh run must publish cache_hit:false, not %v (an absent field reads as unknown)", m1["cache_hit"])
	}

	second, err := s.handleAsk(context.Background(), callReq(args))
	if err != nil {
		t.Fatalf("handleAsk (repeat): %v", err)
	}
	m2 := decodeResult(t, second)
	if runs != 1 {
		t.Fatalf("the seat ran %d times over two identical calls — the cache is not wired into handleAsk", runs)
	}
	if m2["cache_hit"] != true {
		t.Fatalf("a served-from-cache answer must say so: %v", m2)
	}
	// Same answer, same verdict — a cheaper path must not be a different answer.
	for _, f := range []string{"answer", "evidence", "verified", "acceptance"} {
		a, _ := json.Marshal(m1[f])
		b, _ := json.Marshal(m2[f])
		if string(a) != string(b) {
			t.Fatalf("cached %s diverged: %s vs %s", f, a, b)
		}
	}
}

// TestAskEditedFileMissesTheCache is the safety property made observable at the front door:
// the key is the file BYTES, so editing a file between two otherwise-identical calls must
// re-run the seat. Key on the path instead and this test hands back the pre-edit answer.
func TestAskEditedFileMissesTheCache(t *testing.T) {
	dir, p := askFixture(t)
	runs := 0
	s := askTestServer(t, countingSeat(t, &runs))
	args := askArgs("what is the queue cap", p, dir)

	if _, err := s.handleAsk(context.Background(), callReq(args)); err != nil {
		t.Fatalf("handleAsk: %v", err)
	}
	// Same path, same question, same read_root — different bytes.
	edited := "package x\n\n// FleetMaxQueueDepth caps accepted+running work.\nconst FleetMaxQueueDepth = 64\n"
	if err := os.WriteFile(p, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := s.handleAsk(context.Background(), callReq(args))
	if err != nil {
		t.Fatalf("handleAsk (after edit): %v", err)
	}
	m := decodeResult(t, res)
	if runs != 2 {
		t.Fatalf("an edited file must MISS and re-run the seat; seat ran %d times", runs)
	}
	if m["cache_hit"] != false {
		t.Fatalf("an edited file must not read as a cache hit: %v", m)
	}
}

// TestAskDoesNotCacheDeferredResults: a transient seat failure must never become sticky.
// A defer is the harness saying "do it yourself this time", not a durable fact about the
// files — caching one would turn one bad minute into a dead lane for the whole session.
func TestAskDoesNotCacheDeferredResults(t *testing.T) {
	dir, p := askFixture(t)
	runs := 0
	good := countingSeat(t, &runs)
	s := askTestServer(t, func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		if runs == 0 {
			runs++
			return core.AgentWireResult{
				SchemaVersion: core.AgentWireSchemaVersion,
				Seat:          "fake-seat",
				Deferred:      true,
				Reason:        "seat busy",
				DeferClass:    core.DeferClassAbstention,
			}, nil
		}
		return good(ctx, c)
	})
	args := askArgs("what is the queue cap", p, dir)

	first, err := s.handleAsk(context.Background(), callReq(args))
	if err != nil {
		t.Fatalf("handleAsk: %v", err)
	}
	if decodeResult(t, first)["deferred"] != true {
		t.Fatal("fixture did not defer")
	}
	second, err := s.handleAsk(context.Background(), callReq(args))
	if err != nil {
		t.Fatalf("handleAsk (retry): %v", err)
	}
	m := decodeResult(t, second)
	if m["deferred"] != nil {
		t.Fatalf("the defer was cached — a transient failure became sticky: %v", m)
	}
	if m["answer"] != "the cap is 32" {
		t.Fatalf("the retry must reach the seat: %v", m)
	}
	if runs != 2 {
		t.Fatalf("seat ran %d times, want 2 (a defer must not be served from cache)", runs)
	}
}

// TestAskDoesNotCacheRunnerErrors: same rule, the other failure shape — a runner error is
// not an answer about these files.
func TestAskDoesNotCacheRunnerErrors(t *testing.T) {
	dir, p := askFixture(t)
	calls := 0
	s := askTestServer(t, func(_ context.Context, _ core.AgentContract) (core.AgentWireResult, error) {
		calls++
		return core.AgentWireResult{}, errors.New("endpoint refused")
	})
	args := askArgs("what is the queue cap", p, dir)
	for i := 0; i < 2; i++ {
		res, err := s.handleAsk(context.Background(), callReq(args))
		if err != nil {
			t.Fatalf("handleAsk: %v", err)
		}
		if m := decodeResult(t, res); m["deferred"] != true {
			t.Fatalf("an errored run must defer: %v", m)
		}
	}
	if calls != 2 {
		t.Fatalf("an error was cached: runner called %d times, want 2", calls)
	}
}

// TestAskCacheIsPerServer: the cache lives on the Server, which for a stdio MCP process IS
// the session — one client, one process, one cache, gone when the connection dies. A second
// Server must not see the first one's answers.
func TestAskCacheIsPerServer(t *testing.T) {
	dir, p := askFixture(t)
	runsA, runsB := 0, 0
	a := askTestServer(t, countingSeat(t, &runsA))
	b := askTestServer(t, countingSeat(t, &runsB))
	args := askArgs("what is the queue cap", p, dir)
	if _, err := a.handleAsk(context.Background(), callReq(args)); err != nil {
		t.Fatalf("handleAsk: %v", err)
	}
	res, err := b.handleAsk(context.Background(), callReq(args))
	if err != nil {
		t.Fatalf("handleAsk: %v", err)
	}
	if runsB != 1 {
		t.Fatalf("a second server saw the first's cache: seat ran %d times", runsB)
	}
	if decodeResult(t, res)["cache_hit"] != false {
		t.Fatal("a fresh server must not report a hit")
	}
}

// TestAskToolDescriptionDocumentsTheCacheAndItsLimit: the caller decides whether to trust a
// cache_hit, and the honest limitation — this pays on an EXACT repeat only — has to travel
// with the tool, not live in a changelog nobody reads at the decision point.
func TestAskToolDescriptionDocumentsTheCacheAndItsLimit(t *testing.T) {
	desc := ""
	for _, tool := range listTools(t, config.Default()) {
		if tool.Name == "offload_ask" {
			desc = tool.Description
		}
	}
	if desc == "" {
		t.Fatal("offload_ask not advertised")
	}
	if !strings.Contains(desc, "cache_hit") {
		t.Fatal("the description must name the cache_hit field a caller will receive")
	}
	if !strings.Contains(strings.ToLower(desc), "identical") {
		t.Fatal("the description must say the cache pays only on an IDENTICAL repeat, not imply a general speedup")
	}
	// The identical/cache_hit checks above pass even if the actual LIMITATION sentence —
	// "Do NOT read this as a general speedup: a DIFFERENT question over the same files pays
	// full seat time..." — is deleted outright, because "identical" also appears earlier in
	// the description describing what triggers a hit. Assert on the limitation itself, not
	// just on words that happen to co-occur with it.
	lower := strings.ToLower(desc)
	if !strings.Contains(lower, "different question") {
		t.Fatal("the description must warn that a DIFFERENT question over the same files is not helped by the cache")
	}
	if !strings.Contains(lower, "full seat time") {
		t.Fatal("the description must state that a cache miss pays FULL seat time, not a discounted one")
	}
}

// TestAskCacheKeyMatchesTheContractTheSeatWouldSee guards against the subtle version of a
// stale hit: the key must be derived from the RESOLVED docs (post read_root confinement,
// post de-dupe, post name de-collision), not from the caller's raw path strings — those can
// name the same bytes two different ways.
func TestAskCacheKeyMatchesTheContractTheSeatWouldSee(t *testing.T) {
	dir, p := askFixture(t)
	runs := 0
	s := askTestServer(t, countingSeat(t, &runs))
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	// The same file, named absolutely once and relatively once.
	if _, err := s.handleAsk(context.Background(), callReq(askArgs("what is the queue cap", abs, dir))); err != nil {
		t.Fatalf("handleAsk: %v", err)
	}
	res, err := s.handleAsk(context.Background(), callReq(askArgs("what is the queue cap", "cfg.go", dir)))
	if err != nil {
		t.Fatalf("handleAsk: %v", err)
	}
	if runs != 1 {
		t.Fatalf("the same file named two ways must be ONE key; seat ran %d times", runs)
	}
	if decodeResult(t, res)["cache_hit"] != true {
		t.Fatal("expected a hit on the equivalent path spelling")
	}
}
