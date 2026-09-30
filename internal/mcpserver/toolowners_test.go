package mcpserver

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

var threeDeviceOrders = [][]string{
	{"hailo-8l", "coral-edgetpu", "rknpu"}, {"hailo-8l", "rknpu", "coral-edgetpu"},
	{"coral-edgetpu", "hailo-8l", "rknpu"}, {"coral-edgetpu", "rknpu", "hailo-8l"},
	{"rknpu", "hailo-8l", "coral-edgetpu"}, {"rknpu", "coral-edgetpu", "hailo-8l"},
}

// accelerator_tool_owners (ADR 0068) moves one shared name to the device it names, in every order, and
// leaves every other shared name to the first listed owner. Each name is still advertised exactly once.
func TestToolOwnersOverrideTheListedOrder(t *testing.T) {
	for _, order := range threeDeviceOrders {
		cfg := config.Default()
		cfg.Accelerators = order
		cfg.AcceleratorToolOwners = map[string]string{"offload_object_detect": "rknpu", "offload_classify_image": "coral-edgetpu"}
		owner, ignored := accelOwnerPlan(cfg)
		if len(ignored) != 0 {
			t.Errorf("order %v: valid entries were ignored: %v", order, ignored)
		}
		if owner["offload_object_detect"] != "rknpu" || owner["offload_classify_image"] != "coral-edgetpu" {
			t.Errorf("order %v: object_detect -> %q, classify_image -> %q; want rknpu, coral-edgetpu", order, owner["offload_object_detect"], owner["offload_classify_image"])
		}
		// image_embed has no entry: the first listed device owns it, as before.
		if want := order[0]; owner["offload_image_embed"] != want {
			t.Errorf("order %v: image_embed -> %q, want the first listed %q", order, owner["offload_image_embed"], want)
		}
		s := New(pipeline.New(cfg, nil, nil, nil))
		if got := s.registerAccelTools(mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil), cfg); !maps.Equal(got, owner) {
			t.Errorf("order %v: registration owner map %v differs from the plan %v", order, got, owner)
		}
		count := map[string]int{}
		desc := map[string]string{}
		for _, tool := range listTools(t, cfg) {
			count[tool.Name]++
			desc[tool.Name] = tool.Description
		}
		for _, name := range []string{"offload_object_detect", "offload_classify_image", "offload_image_embed"} {
			if count[name] != 1 {
				t.Errorf("order %v: %s advertised %d times, want exactly 1", order, name, count[name])
			}
		}
		if !strings.Contains(desc["offload_object_detect"], "Rockchip") {
			t.Errorf("order %v: the advertised offload_object_detect is not the RKNPU's: %q", order, desc["offload_object_detect"])
		}
	}
}

// An entry that cannot apply never removes a tool: it is reported, and the first-listed rule decides the
// name as if the entry were absent.
func TestToolOwnersEntryThatCannotApplyIsIgnored(t *testing.T) {
	cfg := config.Default()
	cfg.Accelerators = []string{"coral-edgetpu", "rknpu"}
	cfg.AcceleratorToolOwners = map[string]string{
		"offload_object_detect":    "hailo-8l", // not listed on this box
		"offload_semantic_segment": "rknpu",    // the RKNPU has no such tool
		"offload_image_embed":      "coral",    // a typo of a device id
	}
	owner, ignored := accelOwnerPlan(cfg)
	if len(ignored) != 3 {
		t.Fatalf("ignored = %v, want all three entries", ignored)
	}
	for name, want := range map[string]string{"offload_object_detect": "coral-edgetpu", "offload_semantic_segment": "coral-edgetpu", "offload_image_embed": "coral-edgetpu", "offload_classify_image": "coral-edgetpu"} {
		if owner[name] != want {
			t.Errorf("%s -> %q, want the first listed owner %q", name, owner[name], want)
		}
	}
	count := map[string]int{}
	for _, tool := range listTools(t, cfg) {
		count[tool.Name]++
	}
	for _, name := range []string{"offload_object_detect", "offload_semantic_segment", "offload_image_embed", "offload_classify_image"} {
		if count[name] != 1 {
			t.Errorf("%s advertised %d times after an ignored entry, want exactly 1", name, count[name])
		}
	}
}

// The live shape: the delegator carries the Coral and reaches the RKNPU over the fleet. Without an entry
// the local Coral owns every shared name; with one, that name forwards to the fleet node and the rest
// stay local. Status reports what each device actually serves.
func TestToolOwnersRouteANameToAFleetDevice(t *testing.T) {
	cfg := config.Default()
	cfg.Accelerators = []string{"coral-edgetpu"}
	cfg.FleetAccelerators = []string{"rknpu"}
	owner, _ := accelOwnerPlan(cfg)
	if owner["offload_object_detect"] != "coral-edgetpu" {
		t.Fatalf("without an entry object_detect -> %q, want the local coral-edgetpu", owner["offload_object_detect"])
	}
	cfg.AcceleratorToolOwners = map[string]string{"offload_object_detect": "rknpu"}
	owner, ignored := accelOwnerPlan(cfg)
	if len(ignored) != 0 || owner["offload_object_detect"] != "rknpu"+FleetOwnerSuffix {
		t.Fatalf("object_detect -> %q (ignored %v), want %q", owner["offload_object_detect"], ignored, "rknpu"+FleetOwnerSuffix)
	}
	for _, name := range []string{"offload_classify_image", "offload_image_embed", "offload_semantic_segment"} {
		if owner[name] != "coral-edgetpu" {
			t.Errorf("%s -> %q, want the local coral-edgetpu", name, owner[name])
		}
	}
	var detect *mcp.Tool
	for _, tool := range listTools(t, cfg) {
		if tool.Name == "offload_object_detect" {
			detect = tool
		}
	}
	if detect == nil || !strings.Contains(detect.Description, "[FLEET: this box has no rknpu") {
		t.Fatalf("offload_object_detect is not the rknpu forwarder: %+v", detect)
	}

	st := accelStatus(context.Background(), cfg)
	b, _ := json.Marshal(st)
	var got map[string]struct {
		Serves []string `json:"serves"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got["rknpu"].Serves, []string{"offload_object_detect"}) {
		t.Errorf("status rknpu serves %v, want [offload_object_detect]", got["rknpu"].Serves)
	}
	if want := []string{"offload_classify_image", "offload_image_embed", "offload_semantic_segment"}; !slices.Equal(got["coral-edgetpu"].Serves, want) {
		t.Errorf("status coral-edgetpu serves %v, want %v", got["coral-edgetpu"].Serves, want)
	}
}

// The agent loop resolves the same entries to the same device as the MCP surface (ADR 0037's parity,
// extended by ADR 0068): the lanes carry cfg.ToolOwnerClaims, and the owner a loop tool routes to is the
// owner accelOwnerPlan names, in every order and for entries that apply and entries that do not.
func TestToolOwnersLoopMatchesMCP(t *testing.T) {
	entries := []map[string]string{
		{"offload_object_detect": "rknpu"},
		{"offload_object_detect": "coral-edgetpu", "offload_image_embed": "rknpu", "offload_classify_image": "rknpu"},
		{"offload_object_detect": "nope", "offload_face_detect": "rknpu"},
	}
	for _, e := range entries {
		for _, order := range threeDeviceOrders {
			cfg := config.Default()
			cfg.Accelerators = order
			cfg.AcceleratorToolOwners = e
			plan, _ := accelOwnerPlan(cfg)
			var seen []string
			var lanes []agent.AccelLane
			for _, id := range order {
				device := id
				lanes = append(lanes, agent.AccelLane{ID: id, Claims: cfg.ToolOwnerClaims(id), Call: func(ctx context.Context, tool string, args map[string]any) (string, error) {
					seen = append(seen, device)
					return `{}`, nil
				}})
			}
			tools, err := agent.ReadOnlyToolsWithLanes(t.TempDir(), nil, nil, lanes)
			if err != nil {
				t.Fatal(err)
			}
			for _, tl := range tools {
				want, ok := plan[tl.Name]
				if !ok {
					continue
				}
				seen = nil
				_, _ = tl.Exec(context.Background(), `{"image_path":"/tmp/x.jpg","text":"x"}`)
				if len(seen) != 1 || seen[0] != want {
					t.Errorf("entries %v, order %v: the loop's %s routed to %v, the MCP plan says %s", e, order, tl.Name, seen, want)
				}
			}
		}
	}
	// The loop builder carries the claims from config onto each lane.
	cfg := config.Default()
	cfg.Accelerators = []string{"coral-edgetpu", "rknpu"}
	cfg.AcceleratorToolOwners = map[string]string{"offload_object_detect": "rknpu", "offload_classify_image": "rknpu"}
	for _, lane := range pipeline.NewLoopAccel(cfg) {
		if want := cfg.ToolOwnerClaims(lane.ID); !slices.Equal(lane.Claims, want) {
			t.Errorf("NewLoopAccel lane %s claims %v, want %v", lane.ID, lane.Claims, want)
		}
	}
}
