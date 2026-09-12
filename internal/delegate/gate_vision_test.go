package delegate

import "testing"

// TestPlaceVisionPicksTheLeastLoadedNodeThatServesTheLane: eligibility is
// "advertises vision, card not leased"; ranking is betterRemote's (queue
// depth here); the answer is an INDEX so the caller dispatches to the base
// URL it holds beside the view.
func TestPlaceVisionPicksTheLeastLoadedNodeThatServesTheLane(t *testing.T) {
	remotes := []NodeView{
		{NodeID: "no-lane", Tasks: []string{"agent", "image-gen"}, QueueDepth: 0},
		{NodeID: "leased", Tasks: []string{"vision"}, QueueDepth: 0, LeasedText: true},
		{NodeID: "busy-lease", Tasks: []string{"vision"}, QueueDepth: 0, LeaseBusy: true},
		{NodeID: "loaded", Tasks: []string{"vision", "agent"}, QueueDepth: 3},
		{NodeID: "idle", Tasks: []string{"vision"}, QueueDepth: 1},
	}
	i, ok := PlaceVision(remotes)
	if !ok || remotes[i].NodeID != "idle" {
		t.Fatalf("PlaceVision = (%d, %v), want the idle vision node (index 4)", i, ok)
	}
}

// TestPlaceVisionRefusesWhenNoNodeServesTheLane: an older node never lists
// "vision", so a fleet of pre-0.116.0 nodes yields ok=false — the caller
// defers (route remote) or runs local (route auto); nothing is guessed.
func TestPlaceVisionRefusesWhenNoNodeServesTheLane(t *testing.T) {
	remotes := []NodeView{
		{NodeID: "old", Tasks: []string{"agent"}},
		{NodeID: "media", Tasks: []string{"image-gen"}},
		{NodeID: "leased", Tasks: []string{"vision"}, LeasedText: true},
	}
	if i, ok := PlaceVision(remotes); ok {
		t.Fatalf("PlaceVision = (%d, true), want no eligible node", i)
	}
	if _, ok := PlaceVision(nil); ok {
		t.Fatal("PlaceVision(nil) must not find a node")
	}
}

// TestPlaceVisionKeepsRosterOrderOnTies: equal nodes stay in roster order,
// the same stable-preference rule Place keeps for contracts.
func TestPlaceVisionKeepsRosterOrderOnTies(t *testing.T) {
	remotes := []NodeView{
		{NodeID: "first", Tasks: []string{"vision"}, QueueDepth: 1},
		{NodeID: "second", Tasks: []string{"vision"}, QueueDepth: 1},
	}
	if i, ok := PlaceVision(remotes); !ok || i != 0 {
		t.Fatalf("PlaceVision = (%d, %v), want index 0 on a tie", i, ok)
	}
}
