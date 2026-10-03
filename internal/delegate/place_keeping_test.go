package delegate

// A capacity wait ends in a kept place, not a bare defer (plan P7, invariant I4: a busy card is
// a place in line). When nothing took the work inside the wait, the result says WHERE the
// subtask stood in line - which node, behind what (a lease on its cards, a full queue, a
// cooldown, a backlog), and when that is expected to clear - and carries the soonest of those as
// a retry hint, so the caller re-asks when it can succeed instead of guessing. The durable token
// that resumes a place across calls is the media-admission change (plan P13); this is the
// delegator's half: the facts a re-call is placed by.

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// asMaps round-trips a typed value through JSON into the generic shape the fake node publishes.
func asMaps(t *testing.T, v any) []map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// renderNode is a remote on the flagship layout whose card 0 a media render holds for 90 more
// minutes: it publishes its layer rows (with each seat's cards), its card table and its leases.
func renderNode(t *testing.T) (*fakeNode, string) {
	t.Helper()
	view := flagshipRemote(t)
	f := &fakeNode{t: t, agentEnabled: true, resident: true, ctxTokens: 262144, nodeID: "node-render"}
	f.layers = view.Layers
	f.gpuDevices = asMaps(t, view.Devices)
	f.lease = map[string]any{"held": true, "class": "media", "pid": 77, "until": "2099-01-01T00:00:00Z", "busy": true, "remaining_sec": 5400}
	f.leases = []map[string]any{{
		"epoch": 7, "class": "media", "devices": []string{remCardA}, "scope": "declared",
		"until": "2099-01-01T00:00:00Z", "remaining_sec": 5400, "busy": true, "verdict": "held",
	}}
	return f, f.server().URL
}

func TestAwaitCapacityEndsInPlaceKeeping(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	b := newFlagshipLeaseBox(t)
	l, err := b.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render", Devices: []string{leaseCard0}, TTL: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	node, url := renderNode(t)
	cfg := b.cfg
	cfg.AgentPlacementWaitSec = 1

	var localCalls atomic.Int64
	results, sum, err := RunWith(t.Context(), cfg, passingLocal(&localCalls),
		[]core.AgentContract{remoteContract()}, "auto", []string{url}, fencedRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if localCalls.Load() != 0 || node.dispatches.Load() != 0 {
		t.Fatalf("local=%d remote dispatches=%d: both seats are fenced by the render on card 0, so neither is dialled", localCalls.Load(), node.dispatches.Load())
	}
	pr := results[0]
	if !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassCapacity || sum.Deferred != 1 {
		t.Fatalf("result = %+v summary = %+v, want a capacity defer", pr.Result, sum)
	}

	byNode := map[string]PlaceWait{}
	for _, p := range pr.PlaceKeeping {
		byNode[p.Node] = p
	}
	local, remote := byNode[b.runner().localView().NodeID], byNode["node-render"]
	if len(pr.PlaceKeeping) != 2 || remote.Node == "" {
		t.Fatalf("place_keeping = %+v, want one place per node it stood behind (the local seat and node-render)", pr.PlaceKeeping)
	}
	if remote.On != "lease" || remote.EtaSec < 5300 || remote.EtaSec > 5400 || !strings.Contains(remote.Detail, "media") {
		t.Fatalf("the remote's place = %+v, want the media lease on its card with the 90 minutes it published", remote)
	}
	if local.On != "lease" || local.EtaSec < 7000 || local.EtaSec > 7200 {
		t.Fatalf("the local place = %+v, want the render's two-hour term", local)
	}
	if pr.RetryAfterSec < 5300 || pr.RetryAfterSec > 5400 {
		t.Fatalf("retry_after_sec = %d, want the soonest known end (about 5400)", pr.RetryAfterSec)
	}
	if !strings.Contains(pr.Result.Reason, "standing in line") || !strings.Contains(pr.Result.Reason, "node-render") ||
		!strings.Contains(pr.Result.Reason, "1h30m") {
		t.Fatalf("reason = %q, want the places in prose too", pr.Result.Reason)
	}

	// And it reaches the caller on the wire.
	wire := WireResponse(results, sum, nil)
	raw, _ := json.Marshal(wire.Results[0])
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	places, _ := got["place_keeping"].([]any)
	if len(places) != 2 || got["retry_after_sec"] == nil {
		t.Fatalf("published result = %s, want place_keeping and retry_after_sec", raw)
	}
}

// A result nothing waited for carries no place facts, so a healthy run publishes exactly what
// it published before.
func TestWireCarriesNoPlaceKeepingOnAnOrdinaryResult(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, url := eligibleNode(t, "node-a", "zorblax from A")
	cfg := testCfg(t)
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(WireResponse(results, sum, nil).Results[0])
	if strings.Contains(string(raw), "place_keeping") || strings.Contains(string(raw), "retry_after_sec") {
		t.Fatalf("an ordinary result must publish neither place_keeping nor retry_after_sec: %s", raw)
	}
}

// A node that refused for capacity is a place in line too: the wait names it, with the cooldown
// its refusal put it on, even though no lease is involved.
func TestPlaceKeepingNamesANodeThatRefusedForCapacity(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	// No cooldown: the node is asked, and refuses, on every tick, so the wait ends right after a
	// refusal and the place it publishes is the one that refusal recorded.
	compressWait(t, 20*time.Millisecond, 0)
	_, url := refusingNode(t, "node-full", http.StatusServiceUnavailable, nil)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1
	results, _, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if !pr.Result.Deferred || len(pr.PlaceKeeping) != 1 || pr.PlaceKeeping[0].Node != "node-full" {
		t.Fatalf("place_keeping = %+v on %+v, want the one node that refused", pr.PlaceKeeping, pr.Result)
	}
	if p := pr.PlaceKeeping[0]; p.On != "queue" || !strings.Contains(p.Detail, "refused the dispatch for capacity") || p.EtaSec != 0 {
		t.Fatalf("place = %+v, want it to say it stood behind the node's own refusal, with no ETA the node did not give", p)
	}
}

// A node whose backlog outlasts what the caller will wait is a place in line with the arithmetic
// the node itself published: held out every tick, never asked, and named with its ETA.
func TestPlaceKeepingNamesABacklogHeldNodeWithItsEstimate(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	slow := 444.0
	node, url := acceptingNode(t, "node-slow", "never asked", func(f *fakeNode) { f.queueWaitEstimate = &slow })
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1
	c := plainContract()
	c.TimeoutSec = 300
	results, _, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{c}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if node.dispatches.Load() != 0 || !pr.Result.Deferred {
		t.Fatalf("dispatches=%d result=%+v, want the backlog-held node never asked and a defer", node.dispatches.Load(), pr.Result)
	}
	if len(pr.PlaceKeeping) != 1 || pr.PlaceKeeping[0].Node != "node-slow" || pr.PlaceKeeping[0].On != "backlog" || pr.PlaceKeeping[0].EtaSec != 444 {
		t.Fatalf("place_keeping = %+v, want node-slow behind its backlog with the 444 s it published", pr.PlaceKeeping)
	}
	if pr.RetryAfterSec != 444 {
		t.Fatalf("retry_after_sec = %d, want 444", pr.RetryAfterSec)
	}
}

// A node that says it has no room (its admission ceiling is met) is a place in line, named with its
// own words and no ETA it did not give.
func TestPlaceKeepingNamesANodeWithNoRoom(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := refusingNode(t, "node-full", http.StatusServiceUnavailable, func(f *fakeNode) {
		f.maxQueueDepth, f.queueDepth, f.jobsRunning = 1, 1, 1
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1
	results, _, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if node.dispatches.Load() != 1 || !pr.Result.Deferred {
		t.Fatalf("dispatches=%d result=%+v, want the one ask that refused (the wait never re-asks a node that says it has no room) and a defer", node.dispatches.Load(), pr.Result.Deferred)
	}
	if len(pr.PlaceKeeping) != 1 || pr.PlaceKeeping[0].Node != "node-full" || pr.PlaceKeeping[0].On != "queue" ||
		!strings.Contains(pr.PlaceKeeping[0].Detail, "queue_depth 1 of max_queue_depth 1") || pr.PlaceKeeping[0].EtaSec != 0 {
		t.Fatalf("place_keeping = %+v, want node-full behind its own admission ceiling with no ETA", pr.PlaceKeeping)
	}
	if pr.RetryAfterSec != 0 {
		t.Fatalf("retry_after_sec = %d, want none when no place has an ETA", pr.RetryAfterSec)
	}
}

// A node that refused with a Retry-After is a place in line until that cooldown ends.
func TestPlaceKeepingNamesTheCooldownOfANodeThatRefused(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	_, url := refusingNode(t, "node-a", http.StatusServiceUnavailable, func(f *fakeNode) {
		f.dispatchRetryAfter = "5"
		f.maxQueueDepth = 4
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1
	results, _, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if len(pr.PlaceKeeping) != 1 || pr.PlaceKeeping[0].Node != "node-a" {
		t.Fatalf("place_keeping = %+v, want node-a", pr.PlaceKeeping)
	}
	p := pr.PlaceKeeping[0]
	if (p.On != "cooldown" && p.On != "queue") || p.EtaSec < 1 || p.EtaSec > 5 || pr.RetryAfterSec != p.EtaSec {
		t.Fatalf("place = %+v retry_after_sec = %d, want its Retry-After of 5 s counted down and used as the hint", p, pr.RetryAfterSec)
	}
}

func TestEtaPhraseWordsSecondsMinutesAndHours(t *testing.T) {
	for sec, want := range map[int]string{45: "45s", 89: "89s", 90: "2m", 600: "10m", 3599: "60m", 3600: "1h00m", 5400: "1h30m", 7261: "2h01m"} {
		if got := etaPhrase(sec); got != want {
			t.Errorf("etaPhrase(%d) = %q, want %q", sec, got, want)
		}
	}
}

// A wait whose last probe is cut by its own deadline keeps the last good reading: the place the
// refusal put the node in survives, so the defer still says where the subtask stood.
func TestPlaceKeepingSurvivesAWaitThatEndsMidProbe(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	_, url := refusingNode(t, "node-a", http.StatusServiceUnavailable, func(f *fakeNode) {
		f.dispatchRetryAfter = "5"
		f.maxQueueDepth = 4
		f.healthDelay = 300 * time.Millisecond
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1
	results, _, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if len(pr.PlaceKeeping) != 1 || pr.PlaceKeeping[0].Node != "node-a" || pr.PlaceKeeping[0].EtaSec < 1 {
		t.Fatalf("place_keeping = %+v, want the node that refused, with its cooldown, although the last probe was cut", pr.PlaceKeeping)
	}
}
