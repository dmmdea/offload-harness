package delegate

import (
	"context"
	"testing"
)

// media_routes (ADR 0076) is additive: a node that publishes it has its verdicts decoded, and a node that
// predates it is UNKNOWN, never read as "no route is configured".
func TestFetchNodeViewDecodesMediaRoutesAndKeepsAnOlderNodeUnknown(t *testing.T) {
	withRoutes := `{
		"node_id": "node-a", "schema_version": 1,
		"supported_task_types": ["image-gen", "video-gen"],
		"queue_depth": 0,
		"lease": {"held": true, "class": "media", "busy": false},
		"media_routes": [
			{"route": "generate_video", "engine": "comfyui", "state": "CONFIGURED"},
			{"route": "animate_character", "engine": "comfyui", "state": "BOUND-BUT-MISSING"}
		]
	}`
	v, err := FetchNodeView(context.Background(), healthServer(t, withRoutes, nil).URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if !v.MediaRoutesKnown || len(v.MediaRoutes) != 2 {
		t.Fatalf("media routes not decoded: known=%v routes=%+v", v.MediaRoutesKnown, v.MediaRoutes)
	}
	if st, ok := v.RouteState("generate_video"); !ok || st != MediaRouteConfigured {
		t.Errorf("generate_video = %q, %v", st, ok)
	}
	if st, ok := v.RouteState("animate_character"); !ok || st != "BOUND-BUT-MISSING" {
		t.Errorf("animate_character = %q, %v", st, ok)
	}
	if _, ok := v.RouteState("run_graph"); ok {
		t.Error("a route the node did not list must be unknown, not a verdict")
	}
	if !v.LeaseHeld || v.LeasedText {
		t.Errorf("a held media lease must decode as held, not as a text lease: held=%v text=%v", v.LeaseHeld, v.LeasedText)
	}

	old, err := FetchNodeView(context.Background(), healthServer(t, mediaHealthJSON, nil).URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if old.MediaRoutesKnown || len(old.MediaRoutes) != 0 || old.LeaseHeld {
		t.Fatalf("an older node publishes no media_routes: known=%v routes=%+v held=%v", old.MediaRoutesKnown, old.MediaRoutes, old.LeaseHeld)
	}
	if _, ok := old.RouteState("generate_video"); ok {
		t.Error("an older node's routes are unknown, never CONFIGURED or missing")
	}
}
