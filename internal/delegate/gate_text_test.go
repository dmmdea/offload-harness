package delegate

import (
	"context"
	"reflect"
	"testing"
)

// PlaceText: eligibility is "advertises text, lists THIS task, card not leased"; ranking is
// betterRemote's. Unlike vision, an absent task list is not "all": the lane ships dark.
func TestPlaceTextPicksTheLeastLoadedNodeThatListsTheTask(t *testing.T) {
	both := []string{"classify", "extract"}
	remotes := []NodeView{
		{NodeID: "no-lane", Tasks: []string{"agent", "vision"}, TextTasks: both},
		{NodeID: "lane-no-tasks", Tasks: []string{"text"}},
		{NodeID: "classify-only", Tasks: []string{"text"}, TextTasks: []string{"classify"}},
		{NodeID: "leased", Tasks: []string{"text"}, TextTasks: both, LeasedText: true},
		{NodeID: "busy-lease", Tasks: []string{"text"}, TextTasks: both, LeaseBusy: true},
		{NodeID: "loaded", Tasks: []string{"text"}, TextTasks: both, QueueDepth: 3},
		{NodeID: "idle", Tasks: []string{"text"}, TextTasks: both, QueueDepth: 1},
	}
	if i, ok := PlaceText(remotes, "extract"); !ok || remotes[i].NodeID != "idle" {
		t.Fatalf("PlaceText(extract) = (%d, %v), want the idle node", i, ok)
	}
	// classify-only has queue depth 0 and lists classify, so it wins that task on load.
	if i, ok := PlaceText(remotes, "classify"); !ok || remotes[i].NodeID != "classify-only" {
		t.Fatalf("PlaceText(classify) = (%d, %v), want classify-only", i, ok)
	}
	for _, task := range []string{"summarize", "triage", "vqa", ""} {
		if i, ok := PlaceText(remotes, task); ok {
			t.Errorf("PlaceText(%q) = (%d, true): only classify and extract are ever placeable", task, i)
		}
	}
	if _, ok := PlaceText(nil, "classify"); ok {
		t.Fatal("PlaceText(nil) must not find a node")
	}
}

func TestServesTextTask(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    NodeView
		task string
		want bool
	}{
		{"lane and task", NodeView{Tasks: []string{"text"}, TextTasks: []string{"classify"}}, "classify", true},
		{"lane, other task", NodeView{Tasks: []string{"text"}, TextTasks: []string{"classify"}}, "extract", false},
		{"lane, no list", NodeView{Tasks: []string{"text"}}, "classify", false},
		{"list but no lane", NodeView{Tasks: []string{"agent"}, TextTasks: []string{"classify"}}, "classify", false},
		{"older node", NodeView{Tasks: []string{"agent", "vision"}}, "classify", false},
	} {
		if got := tc.v.ServesTextTask(tc.task); got != tc.want {
			t.Errorf("%s: ServesTextTask(%q) = %v, want %v", tc.name, tc.task, got, tc.want)
		}
	}
}

// The field is additive: a node that predates it (or whose tier declares none) publishes nothing
// and is never a text target.
func TestFetchNodeViewDecodesTextTasks(t *testing.T) {
	older := healthServer(t, `{"node_id":"old","supported_task_types":["vision","agent"],"queue_depth":0}`, nil)
	v, err := FetchNodeView(context.Background(), older.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.TextTasks != nil || v.ServesText() {
		t.Fatalf("an older node decoded as text-capable: %+v", v)
	}
	if _, ok := PlaceText([]NodeView{v}, "classify"); ok {
		t.Error("an older node was placed a text task")
	}

	lane := healthServer(t, `{"node_id":"npu","supported_task_types":["text"],"text_tasks":["classify","extract"],"queue_depth":0}`, nil)
	v, err = FetchNodeView(context.Background(), lane.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"classify", "extract"}; !reflect.DeepEqual(v.TextTasks, want) {
		t.Fatalf("TextTasks = %v, want %v", v.TextTasks, want)
	}
	if i, ok := PlaceText([]NodeView{v}, "extract"); !ok || i != 0 {
		t.Errorf("a node publishing the lane must be placed an extract, got (%d, %v)", i, ok)
	}
}
