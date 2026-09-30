package delegate

import (
	"context"
	"reflect"
	"testing"
)

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
	i, ok := PlaceVision(remotes, "vqa")
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
	if i, ok := PlaceVision(remotes, "vqa"); ok {
		t.Fatalf("PlaceVision = (%d, true), want no eligible node", i)
	}
	if _, ok := PlaceVision(nil, "vqa"); ok {
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
	if i, ok := PlaceVision(remotes, "vqa"); !ok || i != 0 {
		t.Fatalf("PlaceVision = (%d, %v), want index 0 on a tie", i, ok)
	}
}

// TestPlaceVisionSkipsANodeThatDoesNotListTheTask (0.153.0): a node whose seat publishes
// vision_tasks serves only those. The idle NPU-class node would win on load for every task, but
// must be skipped for the one it does not list and still picked for the ones it does.
func TestPlaceVisionSkipsANodeThatDoesNotListTheTask(t *testing.T) {
	remotes := []NodeView{
		{NodeID: "npu", Tasks: []string{"vision"}, VisionTasks: []string{"vqa", "ocr"}, QueueDepth: 0},
		{NodeID: "gpu", Tasks: []string{"vision"}, QueueDepth: 5},
	}
	if i, ok := PlaceVision(remotes, "assess_image"); !ok || remotes[i].NodeID != "gpu" {
		t.Fatalf("assess_image: PlaceVision = (%d, %v), want the node that serves it (gpu)", i, ok)
	}
	for _, task := range []string{"vqa", "ocr"} {
		if i, ok := PlaceVision(remotes, task); !ok || remotes[i].NodeID != "npu" {
			t.Fatalf("%s: PlaceVision = (%d, %v), want the idle node that lists it (npu)", task, i, ok)
		}
	}
	if i, ok := PlaceVision(remotes[:1], "assess_image"); ok {
		t.Fatalf("a roster whose only node does not list the task must place nothing, got %d", i)
	}
}

// TestServesVisionTask: the lane first, then the task; no vision_tasks = all three.
func TestServesVisionTask(t *testing.T) {
	all := NodeView{Tasks: []string{"vision"}}
	narrow := NodeView{Tasks: []string{"vision"}, VisionTasks: []string{"vqa", "ocr"}}
	noLane := NodeView{Tasks: []string{"agent"}, VisionTasks: []string{"vqa", "ocr"}}
	for _, task := range []string{"vqa", "ocr", "assess_image"} {
		if !all.ServesVisionTask(task) {
			t.Errorf("a node that publishes no vision_tasks must serve %s", task)
		}
		if noLane.ServesVisionTask(task) {
			t.Errorf("a node without the vision lane serves no vision task (%s)", task)
		}
	}
	if !narrow.ServesVisionTask("vqa") || !narrow.ServesVisionTask("ocr") || narrow.ServesVisionTask("assess_image") {
		t.Error("a node listing vqa, ocr must serve exactly those")
	}
}

// TestFetchNodeViewDecodesVisionTasks: the field is additive. A node that predates it, or whose
// seat serves all three, publishes none and is eligible for every task, exactly as before.
func TestFetchNodeViewDecodesVisionTasks(t *testing.T) {
	older := healthServer(t, `{"node_id":"old","supported_task_types":["vision"],"vision_model":"vlm","queue_depth":0}`, nil)
	v, err := FetchNodeView(context.Background(), older.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.VisionTasks != nil {
		t.Fatalf("an absent vision_tasks decoded as %v, want nil (= all three)", v.VisionTasks)
	}
	for _, task := range []string{"vqa", "ocr", "assess_image"} {
		if i, ok := PlaceVision([]NodeView{v}, task); !ok || i != 0 {
			t.Errorf("an older node must stay eligible for %s, got (%d, %v)", task, i, ok)
		}
	}

	narrow := healthServer(t, `{"node_id":"npu","supported_task_types":["vision"],"vision_model":"vlm","vision_tasks":["vqa","ocr"],"queue_depth":0}`, nil)
	v, err = FetchNodeView(context.Background(), narrow.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"vqa", "ocr"}; !reflect.DeepEqual(v.VisionTasks, want) {
		t.Fatalf("VisionTasks = %v, want %v", v.VisionTasks, want)
	}
	if _, ok := PlaceVision([]NodeView{v}, "assess_image"); ok {
		t.Error("a node publishing vision_tasks [vqa ocr] must not be placed an assess_image")
	}
	if i, ok := PlaceVision([]NodeView{v}, "vqa"); !ok || i != 0 {
		t.Errorf("the same node must still be placed a vqa, got (%d, %v)", i, ok)
	}
}
