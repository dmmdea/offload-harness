package main

import (
	"testing"

	"llamaswap-pp-cli/pkg/llamaswap"
)

// noneProtected is the keep-set answer for a box whose config protects nothing.
func noneProtected(string) bool { return false }

// protect returns an IsProtected-shaped predicate over a fixed name set.
func protect(names ...string) func(string) bool {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return func(id string) bool { return set[id] }
}

// TestReclaimableUsesConfigTruthNotTheServerTTL is the regression for the bug this
// call site carried: GET /running publishes `ttl: 0` for a seat CONFIGURED
// `ttl: -1` (verified live on llama-swap v249 — both mem0-stack seats report 0
// there). The old rule read residency off that field.
//
// Both rows below arrive as ttl:0. Only one of them is actually resident in the
// config; the other is a swapping seat the server also happens to report as 0.
// The ttl rule calls the whole snapshot idle and folds a live workhorse into the
// idle baseline; the keep-set rule sees the reclaimable seat.
func TestReclaimableUsesConfigTruthNotTheServerTTL(t *testing.T) {
	running := []llamaswap.RunningModel{
		{ID: "embeddinggemma", State: "ready", TTL: 0}, // configured ttl:-1, misreported as 0
		{ID: "gemma-4-26b", State: "ready", TTL: 0},    // a swapping seat, also reported as 0
	}
	if got := anyReclaimable(running, protect("embeddinggemma"), true); !got {
		t.Error("a loaded swapping seat must be reclaimable even when the server reports ttl:0 for it")
	}
	// The same snapshot under the old server-ttl rule: everything looks resident.
	if got := anyReclaimable(running, noneProtected, false); got {
		t.Error("the documented fallback must still read the ttl field when no config is available")
	}
}

// TestReclaimableProtectsTheKeepSet: a resident support seat is baseline, not
// capacity. Unloading the embedder and reranker defeats the reason they are
// co-resident (one RAG query paid three full model loads without them).
func TestReclaimableProtectsTheKeepSet(t *testing.T) {
	running := []llamaswap.RunningModel{
		{ID: "embeddinggemma", State: "ready", TTL: 0},
		{ID: "bge-reranker-v2-m3", State: "ready", TTL: 0},
	}
	if anyReclaimable(running, protect("embeddinggemma", "bge-reranker-v2-m3"), true) {
		t.Error("a snapshot of nothing but keep-set members holds no reclaimable VRAM")
	}
}

// TestReclaimableCountsASupportSeatWithARealTTL: the old rule's other half. A
// support seat given a real TTL instead of 0 was counted as reclaimable, which
// over-stated capacity by the size of an embedder. Config truth files it under
// the keep-set regardless of what ttl says.
func TestReclaimableCountsASupportSeatWithARealTTL(t *testing.T) {
	running := []llamaswap.RunningModel{{ID: "embeddinggemma", State: "ready", TTL: 900}}
	if anyReclaimable(running, protect("embeddinggemma"), true) {
		t.Error("a keep-set member with a non-zero ttl is still baseline, not capacity")
	}
	if !anyReclaimable(running, noneProtected, false) {
		t.Error("the ttl fallback keeps its old verdict for a seat with a real ttl")
	}
}

// TestReclaimableIgnoresStoppedSeats: a stopped/stopping row holds no VRAM, so it
// is neither baseline nor capacity. Unchanged by this refactor, and asserted so a
// future edit to the classifier cannot drop it silently.
func TestReclaimableIgnoresStoppedSeats(t *testing.T) {
	for _, state := range []string{"stopped", "stopping", "STOPPED", "Stopping"} {
		running := []llamaswap.RunningModel{{ID: "gemma-4-26b", State: state, TTL: 300}}
		if anyReclaimable(running, noneProtected, true) {
			t.Errorf("state %q holds no VRAM and must not count as reclaimable", state)
		}
	}
	if anyReclaimable(nil, noneProtected, true) {
		t.Error("an empty /running holds nothing")
	}
}
