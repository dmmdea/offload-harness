package mcpserver

import (
	"encoding/json"
	"testing"

	"github.com/dmmdea/offload-harness/internal/accelremote"
)

// TestAccelPlacementNodeNamesTheHostBehindTheBase pins the node a forwarded
// accelerator call is attributed to. accelremote stamps a typed Placement (a
// map only after a JSON round trip); until 0.130.5 the helper read the map
// form alone, so every successful forward was recorded as "<device>@fleet" and
// its PAIR card ran on the wrong box. The name is the host of the node's base
// URL — the name PAIR knows the member by — with the harness node id as the
// fallback when there is no base.
func TestAccelPlacementNodeNamesTheHostBehindTheBase(t *testing.T) {
	typed := map[string]any{"placement": accelremote.Placement{Node: "node-b-ampere16", Base: "http://node-b:18811", Accelerator: "coral-edgetpu"}}
	if got := accelPlacementNode(typed); got != "node-b" {
		t.Fatalf("typed placement: got %q, want node-b", got)
	}
	ptr := map[string]any{"placement": &accelremote.Placement{Node: "node-b-ampere16", Base: "http://node-b:18811"}}
	if got := accelPlacementNode(ptr); got != "node-b" {
		t.Fatalf("pointer placement: got %q, want node-b", got)
	}
	noBase := map[string]any{"placement": accelremote.Placement{Node: "node-b-ampere16"}}
	if got := accelPlacementNode(noBase); got != "node-b-ampere16" {
		t.Fatalf("placement without base: got %q, want the node id", got)
	}

	// The JSON form (a result that went through the wire) resolves the same way.
	raw, _ := json.Marshal(typed)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if got := accelPlacementNode(decoded); got != "node-b" {
		t.Fatalf("json placement: got %q, want node-b", got)
	}

	if got := accelPlacementNode(map[string]any{}); got != "" {
		t.Fatalf("no placement: got %q, want empty", got)
	}
	if got := accelPlacementNode(map[string]any{"placement": "garbage"}); got != "" {
		t.Fatalf("foreign placement: got %q, want empty", got)
	}
}

func TestHostOfBase(t *testing.T) {
	cases := map[string]string{
		"http://node-b:18811":       "node-b",
		"https://node-b/":           "node-b",
		"http://node-b:18811/fleet": "node-b",
		"http://[fd00::1]:18811":    "fd00::1",
		"http://192.0.2.10:18811":   "192.0.2.10",
		"node-b:18811":              "",
		"":                          "",
		"  http://node-b:18811  ":   "node-b",
		"http://node-b:18811?x=1#y": "node-b",
	}
	for in, want := range cases {
		if got := hostOfBase(in); got != want {
			t.Errorf("hostOfBase(%q) = %q, want %q", in, got, want)
		}
	}
}
