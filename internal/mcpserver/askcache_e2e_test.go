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

// TestAskCacheMCPLive is the live proof of the ask lane's result cache, run the only way
// that can prove anything: three real calls to a REAL local seat, over the real MCP stdio
// transport, inside ONE server process — because the process is the session, and a cache
// that only ever demonstrates itself against a fake runner has demonstrated nothing.
//
// The three calls are the whole argument:
//
//  1. cold      — the seat runs (tens of seconds), cache_hit:false
//  2. identical — no seat, cache_hit:true, near-instant
//  3. after an EDIT to the attached file, same question and same path — the seat runs
//     again, cache_hit:false. This is the safety property: the key is the file's BYTES,
//     so a stale answer about the pre-edit content is not merely unlikely, it is
//     unreachable.
//
// Gated by OFFLOAD_AGENT_E2E (needs the local planner on :11436), so a normal `go test`
// skips it. The fixture is written into a temp dir: step 3 edits it, and an e2e test must
// never mutate the repo it is running out of.
func TestAskCacheMCPLive(t *testing.T) {
	if os.Getenv("OFFLOAD_AGENT_E2E") == "" {
		t.Skip("set OFFLOAD_AGENT_E2E=1 to run the live offload_ask cache e2e (needs the local planner on :11436)")
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

	// A small, self-contained fixture rather than a large repo file, and step 3 edits it —
	// so it must live in a temp dir either way. Deliberately SMALL: this test measures
	// cache semantics, not how hard a question the seat can carry, and a heavy contract
	// puts the cold call up against the seat's 300 s wall budget, where a defer leaves
	// nothing cached and the run proves nothing. The identifiers are long and distinctive
	// so askjob can mine a grounded anchor.
	body := []byte(`package fleetcfg

// FleetMaxQueueDepth caps how many jobs one node accepts before it refuses more.
const FleetMaxQueueDepth = 32

// FleetDispatchTimeoutSec is the wall budget a dispatched job gets.
const FleetDispatchTimeoutSec = 300
`)
	root := t.TempDir()
	fixture := filepath.Join(root, "fleetcfg.go")
	if werr := os.WriteFile(fixture, body, 0o644); werr != nil {
		t.Fatal(werr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "askcache-e2e", Version: "1"}, nil)
	sess, err := client.Connect(ctx, &mcp.CommandTransport{Command: exec.Command(bin, "mcp")}, nil)
	if err != nil {
		t.Fatalf("connect to spawned MCP server: %v", err)
	}
	defer sess.Close()

	type askOut struct {
		Answer             string   `json:"answer"`
		Evidence           string   `json:"evidence"`
		Verified           bool     `json:"verified"`
		Acceptance         []string `json:"acceptance"`
		AcceptanceFailures []string `json:"acceptance_failures"`
		Seat               string   `json:"seat"`
		Steps              int      `json:"steps"`
		StopReason         string   `json:"stop_reason"`
		CacheHit           *bool    `json:"cache_hit"` // pointer: absent must be distinguishable from false
		Deferred           bool     `json:"deferred"`
		Reason             string   `json:"reason"`
		DeferClass         string   `json:"defer_class"`
	}
	question := "What is the queue-depth cap, and which constant sets it?"
	ask := func(label string) (askOut, time.Duration, string) {
		t.Helper()
		start := time.Now()
		res, cerr := sess.CallTool(ctx, &mcp.CallToolParams{
			Name: "offload_ask",
			Arguments: map[string]any{
				"question":  question,
				"paths":     []string{"fleetcfg.go"},
				"read_root": root,
			},
		})
		took := time.Since(start)
		if cerr != nil {
			t.Fatalf("%s: call offload_ask over MCP: %v", label, cerr)
		}
		text := textOf(res)
		var out askOut
		if jerr := json.Unmarshal([]byte(text), &out); jerr != nil {
			t.Fatalf("%s: parse offload_ask result %q: %v", label, text, jerr)
		}
		t.Logf("=== %s === took=%s\n%s", label, took.Round(time.Millisecond), text)
		return out, took, text
	}

	// --- 1. COLD: the seat actually runs ---------------------------------------------
	first, coldTook, _ := ask("call 1 (cold)")
	if first.Deferred {
		// The lane honestly reporting it could not do the work is a valid outcome, but it
		// leaves nothing cached and therefore nothing to prove. Skip loudly rather than
		// report a green that measured nothing.
		t.Skipf("call 1 DEFERRED (class=%s reason=%s) — no cacheable result was produced, so the cache property is untestable this run", first.DeferClass, first.Reason)
	}
	if first.CacheHit == nil {
		t.Fatal("cache_hit absent from a live response — a caller cannot tell a cached answer from a fresh one")
	}
	if *first.CacheHit {
		t.Fatal("the FIRST call into a fresh server process reported cache_hit:true")
	}
	if strings.TrimSpace(first.Answer) == "" {
		t.Fatal("a non-deferred call must return an answer")
	}

	// --- 2. IDENTICAL REPEAT: served from cache ---------------------------------------
	second, hitTook, _ := ask("call 2 (identical repeat)")
	if second.Deferred {
		t.Fatalf("the identical repeat DEFERRED instead of being served from cache: %s", second.Reason)
	}
	if second.CacheHit == nil || !*second.CacheHit {
		t.Fatalf("the identical repeat must report cache_hit:true, got %v", second.CacheHit)
	}
	if second.Answer != first.Answer || second.Evidence != first.Evidence || second.Verified != first.Verified {
		t.Fatalf("the cached answer diverged from the one it replaced:\n1: %q / %q / %v\n2: %q / %q / %v",
			first.Answer, first.Evidence, first.Verified, second.Answer, second.Evidence, second.Verified)
	}
	// The seat cannot answer in seconds; near-instant is what "the seat did not run" looks
	// like from outside. Generous on purpose — cache_hit is the assertion, this is the
	// corroboration that it is not merely a mislabelled second run.
	if hitTook > 5*time.Second {
		t.Fatalf("a cache hit took %s — that is not a short-circuit", hitTook)
	}
	t.Logf("SPEEDUP: cold=%s hit=%s", coldTook.Round(time.Millisecond), hitTook.Round(time.Millisecond))

	// --- 3. EDIT THE FILE: same question, same path, different bytes -> MISS -----------
	// The smallest edit that changes the ANSWER, not just the bytes: the cap goes 32 -> 64.
	edited := []byte(strings.Replace(string(body), "FleetMaxQueueDepth = 32", "FleetMaxQueueDepth = 64", 1))
	if werr := os.WriteFile(fixture, edited, 0o644); werr != nil {
		t.Fatal(werr)
	}
	third, editTook, _ := ask("call 3 (after editing the file)")
	if third.CacheHit != nil && *third.CacheHit {
		t.Fatal("an EDITED file was served from cache — the key is not content-addressed, and a stale answer just went out")
	}
	// What proves the MISS is that the call reached the SEAT: a hit returns in
	// milliseconds, so anything spending real seat time went past the cache. Both
	// outcomes below are that.
	if editTook < 2*time.Second {
		t.Fatalf("call 3 returned in %s — too fast to have reached the seat, so the edit did not invalidate the entry", editTook)
	}
	if third.Deferred {
		// A defer carries NO cache_hit, by design and not by omission: a defer is never
		// stored, so it is always a fresh run and the field would have nothing to
		// distinguish. Reaching the seat and honestly reporting it ran out of budget is
		// still a miss, which is what this step measures.
		t.Logf("call 3 reached the seat and DEFERRED after %s (class=%s reason=%s) — a MISS, which is what this step measures", editTook.Round(time.Millisecond), third.DeferClass, third.Reason)
		return
	}
	if third.CacheHit == nil {
		t.Fatal("cache_hit absent from a non-deferred answer — a caller cannot tell a cached answer from a fresh one")
	}
	t.Logf("call 3 re-ran on the seat after %s and answered fresh (cache_hit=false)", editTook.Round(time.Millisecond))
}
