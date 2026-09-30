package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

var coralToolNames = []string{"offload_classify_image", "offload_object_detect", "offload_semantic_segment", "offload_image_embed"}

// rknpuRoutes maps each RKNPU tool to the sidecar route it drives. The routes are the contract with
// accelerators/rknpu/server.py (POST /v1/<tool>), so a renamed row would call a route that 404s.
var rknpuRoutes = map[string]string{"offload_classify_image": "classify", "offload_object_detect": "object_detect", "offload_image_embed": "embed"}

func fakeLane(id string, seen *[]string) NPUFunc {
	return func(ctx context.Context, tool string, args map[string]any) (string, error) {
		*seen = append(*seen, id+"/"+tool)
		b, _ := json.Marshal(map[string]any{"device": id, "tool": tool, "args": args})
		return string(b), nil
	}
}

func names(tools []Tool) map[string]bool {
	m := map[string]bool{}
	for _, t := range tools {
		m[t.Name] = true
	}
	return m
}

// No lanes => the tool list is byte-identical to a box without any device:
// the registration pin ADR 0024 set for the Hailo lane holds for the Coral one.
func TestAccelLanesGated(t *testing.T) {
	without, err := ReadOnlyToolsWithLanes(t.TempDir(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range coralToolNames {
		if names(without)[n] {
			t.Errorf("no lanes leaked tool %s", n)
		}
	}
	var seen []string
	with, err := ReadOnlyToolsWithLanes(t.TempDir(), nil, nil, []AccelLane{{ID: "coral-edgetpu", Call: fakeLane("coral-edgetpu", &seen)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range coralToolNames {
		if !names(with)[n] {
			t.Errorf("coral lane missing tool %s", n)
		}
	}
	// Everything that is not a Coral tool is identical between the two lists.
	var stripped []string
	for _, tl := range with {
		if !names(coralToolsFor(&seen))[tl.Name] {
			stripped = append(stripped, tl.Name)
		}
	}
	var base []string
	for _, tl := range without {
		base = append(base, tl.Name)
	}
	if strings.Join(stripped, ",") != strings.Join(base, ",") {
		t.Errorf("the lane changed the tool list beyond adding its own tools:\n with=%v\n base=%v", stripped, base)
	}
}

func coralToolsFor(seen *[]string) []Tool { return coralTools(fakeLane("coral-edgetpu", seen)) }

// Pass-through: the sidecar tool name and the JSON args reach the lane
// unchanged, and the device's answer comes back as the tool's string.
func TestAccelLanePassThrough(t *testing.T) {
	var seen []string
	tools := coralTools(fakeLane("coral-edgetpu", &seen))
	var classify Tool
	for _, tl := range tools {
		if tl.Name == "offload_classify_image" {
			classify = tl
		}
	}
	out, err := classify.Exec(context.Background(), `{"image_path":"/tmp/parrot.jpg","domain":"birds","top_k":3}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "coral-edgetpu/classify" {
		t.Fatalf("lane saw %v, want [coral-edgetpu/classify]", seen)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(out), &got)
	args, _ := got["args"].(map[string]any)
	if args["domain"] != "birds" || args["image_path"] != "/tmp/parrot.jpg" {
		t.Errorf("args did not pass through: %v", got)
	}
	// Empty / mistyped required arg defers instead of calling the device.
	out, _ = classify.Exec(context.Background(), `{"image_path":""}`)
	if !strings.Contains(out, `"deferred":true`) || len(seen) != 1 {
		t.Errorf("empty image_path did not defer (or reached the lane): %s", out)
	}
	out, _ = classify.Exec(context.Background(), `{"image_path":42}`)
	if !strings.Contains(out, "must be a string") || len(seen) != 1 {
		t.Errorf("mistyped image_path did not defer with the type: %s", out)
	}
}

// The shared-name rule (Coral D5): offload_object_detect and offload_image_embed
// are owned by BOTH devices, and the FIRST listed lane wins. Asserted in both
// orders so the rule is about order, not about which device is "primary".
func TestAccelLanesSharedNameRuleFollowsOrder(t *testing.T) {
	for _, order := range [][]string{{"hailo-8l", "coral-edgetpu"}, {"coral-edgetpu", "hailo-8l"}} {
		var seen []string
		lanes := []AccelLane{{ID: order[0], Call: fakeLane(order[0], &seen)}, {ID: order[1], Call: fakeLane(order[1], &seen)}}
		tools, err := ReadOnlyToolsWithLanes(t.TempDir(), nil, nil, lanes)
		if err != nil {
			t.Fatal(err)
		}
		count := map[string]int{}
		var detect Tool
		for _, tl := range tools {
			count[tl.Name]++
			if tl.Name == "offload_object_detect" {
				detect = tl
			}
		}
		for _, shared := range []string{"offload_object_detect", "offload_image_embed"} {
			if count[shared] != 1 {
				t.Errorf("order %v: %s registered %d times, want exactly 1", order, shared, count[shared])
			}
		}
		// Every device-unique tool is present regardless of order.
		for _, n := range []string{"offload_face_detect", "offload_zero_shot", "offload_classify_image", "offload_semantic_segment"} {
			if count[n] != 1 {
				t.Errorf("order %v: %s registered %d times", order, n, count[n])
			}
		}
		// And the shared tool routes to the FIRST listed device.
		_, _ = detect.Exec(context.Background(), `{"image_path":"/tmp/x.jpg"}`)
		if len(seen) != 1 || !strings.HasPrefix(seen[0], order[0]+"/") {
			t.Errorf("order %v: offload_object_detect routed to %v, want %s", order, seen, order[0])
		}
	}
}

// An id with no adapter registers nothing — never invented tools.
func TestAccelLaneUnknownDeviceRegistersNothing(t *testing.T) {
	var seen []string
	got := accelLaneTools([]AccelLane{{ID: "tpu-from-the-future", Call: fakeLane("x", &seen)}}, nil)
	if len(got) != 0 {
		t.Errorf("unknown device registered %d tools", len(got))
	}
	// A nil Call is skipped too.
	if got := accelLaneTools([]AccelLane{{ID: "coral-edgetpu"}}, nil); len(got) != 0 {
		t.Errorf("nil lane registered %d tools", len(got))
	}
}

// The RKNPU lane is gated like every device: no lane, none of its tools; a lane, exactly its three, and
// the rest of the tool list untouched. Each tool drives its own sidecar route and hands the arguments
// through unchanged.
func TestRknpuLaneRegistersItsToolsOnly(t *testing.T) {
	without, err := ReadOnlyToolsWithLanes(t.TempDir(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for n := range rknpuRoutes {
		if names(without)[n] {
			t.Errorf("no lanes leaked tool %s", n)
		}
	}
	var seen []string
	with, err := ReadOnlyToolsWithLanes(t.TempDir(), nil, nil, []AccelLane{{ID: "rknpu", Call: fakeLane("rknpu", &seen)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(with) != len(without)+len(rknpuRoutes) {
		t.Errorf("the rknpu lane added %d tools, want %d", len(with)-len(without), len(rknpuRoutes))
	}
	args := `{"image_path":"/tmp/x.jpg","top_k":3,"score_threshold":0.4}`
	found := map[string]bool{}
	for _, tl := range with {
		route, ok := rknpuRoutes[tl.Name]
		if !ok {
			continue
		}
		found[tl.Name] = true
		seen = nil
		out, err := tl.Exec(context.Background(), args)
		if err != nil {
			t.Fatal(err)
		}
		if len(seen) != 1 || seen[0] != "rknpu/"+route {
			t.Errorf("%s drove %v, want [rknpu/%s]", tl.Name, seen, route)
		}
		var got map[string]any
		_ = json.Unmarshal([]byte(out), &got)
		sent, _ := got["args"].(map[string]any)
		if sent["image_path"] != "/tmp/x.jpg" || sent["top_k"] != float64(3) || sent["score_threshold"] != 0.4 {
			t.Errorf("%s: arguments did not pass through: %v", tl.Name, got)
		}
	}
	if len(found) != len(rknpuRoutes) {
		t.Errorf("the rknpu lane registered %v, want %d tools", found, len(rknpuRoutes))
	}
}

// The shared-name rule over all three devices, in every order. classify_image is owned by the Coral and
// the RKNPU only, object_detect and image_embed by all three, so a name goes to the FIRST listed device
// that owns it — which is not always the first device listed (a Hailo listed first owns no classify).
func TestAccelLanesSharedNameRuleOverThreeDevices(t *testing.T) {
	owners := map[string][]string{
		"offload_classify_image": {"coral-edgetpu", "rknpu"},
		"offload_object_detect":  {"hailo-8l", "coral-edgetpu", "rknpu"},
		"offload_image_embed":    {"hailo-8l", "coral-edgetpu", "rknpu"},
	}
	orders := [][]string{
		{"hailo-8l", "coral-edgetpu", "rknpu"}, {"hailo-8l", "rknpu", "coral-edgetpu"},
		{"coral-edgetpu", "hailo-8l", "rknpu"}, {"coral-edgetpu", "rknpu", "hailo-8l"},
		{"rknpu", "hailo-8l", "coral-edgetpu"}, {"rknpu", "coral-edgetpu", "hailo-8l"},
	}
	for _, order := range orders {
		var seen []string
		var lanes []AccelLane
		for _, id := range order {
			lanes = append(lanes, AccelLane{ID: id, Call: fakeLane(id, &seen)})
		}
		tools, err := ReadOnlyToolsWithLanes(t.TempDir(), nil, nil, lanes)
		if err != nil {
			t.Fatal(err)
		}
		count := map[string]int{}
		byName := map[string]Tool{}
		for _, tl := range tools {
			count[tl.Name]++
			byName[tl.Name] = tl
		}
		for name, owning := range owners {
			want := ""
			for _, id := range order {
				if slices.Contains(owning, id) {
					want = id
					break
				}
			}
			if count[name] != 1 {
				t.Errorf("order %v: %s registered %d times, want exactly 1", order, name, count[name])
				continue
			}
			seen = nil
			_, _ = byName[name].Exec(context.Background(), `{"image_path":"/tmp/x.jpg"}`)
			if len(seen) != 1 || !strings.HasPrefix(seen[0], want+"/") {
				t.Errorf("order %v: %s routed to %v, want %s", order, name, seen, want)
			}
		}
		// The device-unique tools survive whatever the order.
		for _, n := range []string{"offload_face_detect", "offload_zero_shot", "offload_semantic_segment"} {
			if count[n] != 1 {
				t.Errorf("order %v: %s registered %d times", order, n, count[n])
			}
		}
	}
}
