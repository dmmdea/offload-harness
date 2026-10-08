package delegate

import (
	"context"
	"testing"
)

// PlaceSTT picks the fleet node that transcribes ONE uploaded audio file (the stt upload door, ADR
// 0072) when the caller has decided the work leaves the box. Eligibility is the door's own gate: the
// node must ADVERTISE it (stt-upload in supported_task_types: an older node lists only the legacy
// path-taking "stt", which cannot take bytes, so it is never a target), have an hq model when hq is
// asked, accept a file this size, and have a card that is not spoken for. Ranking is betterRemote's,
// as for vision; the answer is an INDEX into the roster.
func TestPlaceSTTPicksTheLeastLoadedNodeThatTakesTheUpload(t *testing.T) {
	up := []string{"stt", "stt-upload"}
	remotes := []NodeView{
		{NodeID: "legacy", Tasks: []string{"stt"}, QueueDepth: 0},
		{NodeID: "no-stt", Tasks: []string{"agent"}, QueueDepth: 0},
		{NodeID: "leased", Tasks: up, QueueDepth: 0, LeasedText: true},
		{NodeID: "busy-lease", Tasks: up, QueueDepth: 0, LeaseBusy: true},
		{NodeID: "loaded", Tasks: up, QueueDepth: 3},
		{NodeID: "idle", Tasks: up, QueueDepth: 1},
	}
	i, ok := PlaceSTT(remotes, false, 1<<20)
	if !ok || remotes[i].NodeID != "idle" {
		t.Fatalf("PlaceSTT = (%d, %v), want the idle upload node (index 5)", i, ok)
	}
	if _, ok := PlaceSTT(remotes[:4], false, 1<<20); ok {
		t.Fatal("PlaceSTT placed on a legacy-only, laneless or leased node")
	}
	if _, ok := PlaceSTT(nil, false, 1); ok {
		t.Fatal("PlaceSTT(nil) found a node")
	}
}

// hq asks for a node that has an hq model: a node publishing stt_hq false, or none (an older node), is
// skipped, so the quality the caller asked for is never silently dropped.
func TestPlaceSTTHonoursTheHQCapability(t *testing.T) {
	yes, no := true, false
	remotes := []NodeView{
		{NodeID: "std", Tasks: []string{"stt-upload"}, STTHQ: &no, QueueDepth: 0},
		{NodeID: "unknown", Tasks: []string{"stt-upload"}, QueueDepth: 0},
		{NodeID: "hq", Tasks: []string{"stt-upload"}, STTHQ: &yes, QueueDepth: 5},
	}
	if i, ok := PlaceSTT(remotes, true, 1); !ok || remotes[i].NodeID != "hq" {
		t.Fatalf("hq: PlaceSTT = (%d, %v), want the node with an hq model", i, ok)
	}
	if i, ok := PlaceSTT(remotes, false, 1); !ok || remotes[i].NodeID != "std" {
		t.Fatalf("standard: PlaceSTT = (%d, %v), want the least loaded node (std)", i, ok)
	}
	if _, ok := PlaceSTT(remotes[:2], true, 1); ok {
		t.Fatal("hq: a node that does not publish an hq model was placed")
	}
}

// A node advertising a smaller upload cap than the file is skipped; one that publishes none (an
// older build of the door) is held to the built-in 48 MiB.
func TestPlaceSTTSkipsANodeWhoseUploadCapIsTooSmall(t *testing.T) {
	remotes := []NodeView{
		{NodeID: "small", Tasks: []string{"stt-upload"}, STTUploadMaxMB: 2, QueueDepth: 0},
		{NodeID: "default", Tasks: []string{"stt-upload"}, QueueDepth: 1},
		{NodeID: "big", Tasks: []string{"stt-upload"}, STTUploadMaxMB: 200, QueueDepth: 2},
	}
	for _, c := range []struct {
		size int64
		want string
	}{{1 << 20, "small"}, {3 << 20, "default"}, {60 << 20, "big"}} {
		i, ok := PlaceSTT(remotes, false, c.size)
		if !ok || remotes[i].NodeID != c.want {
			t.Errorf("size %d: PlaceSTT = (%d, %v), want %s", c.size, i, ok, c.want)
		}
	}
	if _, ok := PlaceSTT(remotes, false, 500<<20); ok {
		t.Error("a file over every node's cap was placed")
	}
}

func TestPlaceSTTKeepsRosterOrderOnTies(t *testing.T) {
	remotes := []NodeView{
		{NodeID: "first", Tasks: []string{"stt-upload"}, QueueDepth: 1},
		{NodeID: "second", Tasks: []string{"stt-upload"}, QueueDepth: 1},
	}
	if i, ok := PlaceSTT(remotes, false, 1); !ok || i != 0 {
		t.Fatalf("PlaceSTT = (%d, %v), want index 0 on a tie", i, ok)
	}
}

func TestServesSTTUpload(t *testing.T) {
	if (NodeView{Tasks: []string{"stt"}}).ServesSTTUpload() {
		t.Error("the legacy path-taking stt lane is not the upload door")
	}
	if !(NodeView{Tasks: []string{"image-gen", "stt-upload"}}).ServesSTTUpload() {
		t.Error("a node listing stt-upload serves it")
	}
	if (NodeView{}).ServesSTTUpload() {
		t.Error("a node that lists nothing serves nothing")
	}
}

// The health fields are additive: an older node publishes none and decodes to the zero view, and
// stt_hq false is told apart from an absent one.
func TestFetchNodeViewDecodesTheSTTUploadCapability(t *testing.T) {
	older := healthServer(t, `{"node_id":"old","supported_task_types":["stt"],"queue_depth":0}`, nil)
	v, err := FetchNodeView(context.Background(), older.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.ServesSTTUpload() || v.STTHQ != nil || v.STTUploadMaxMB != 0 {
		t.Fatalf("an older node decoded as %+v, want no upload capability", v)
	}
	if _, ok := PlaceSTT([]NodeView{v}, false, 1); ok {
		t.Fatal("an older node was placed an upload")
	}

	std := healthServer(t, `{"node_id":"std","supported_task_types":["stt","stt-upload"],"stt_hq":false,"stt_upload_max_mb":48,"queue_depth":0}`, nil)
	v, err = FetchNodeView(context.Background(), std.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if !v.ServesSTTUpload() || v.STTHQ == nil || *v.STTHQ || v.STTUploadMaxMB != 48 {
		t.Fatalf("decoded %+v, want the door, stt_hq false (present) and a 48 MiB cap", v)
	}
	if i, ok := PlaceSTT([]NodeView{v}, false, 1); !ok || i != 0 {
		t.Fatalf("PlaceSTT = (%d, %v), want the node", i, ok)
	}
	if _, ok := PlaceSTT([]NodeView{v}, true, 1); ok {
		t.Fatal("an hq upload was placed on a node with stt_hq false")
	}
}
