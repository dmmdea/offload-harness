package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// A box that carries no device but lists one in fleet_accelerators registers
// that device's tools (forwarded) and nothing else changes — the same
// byte-identical pin the local registration holds.
func TestFleetAcceleratorRegistersForwardedToolsOnly(t *testing.T) {
	off := listTools(t, config.Default())
	cfg := config.Default()
	cfg.FleetAccelerators = []string{"coral-edgetpu"}
	on := listTools(t, cfg)
	found := map[string]bool{}
	var stripped []*mcp.Tool
	for _, tool := range on {
		isCoral := false
		for _, c := range coralTools {
			if tool.Name == c {
				found[c] = true
				isCoral = true
				if !strings.Contains(tool.Description, "[FLEET:") {
					t.Errorf("%s registered from fleet_accelerators without saying it is forwarded", c)
				}
			}
		}
		if !isCoral {
			stripped = append(stripped, tool)
		}
	}
	if len(found) != len(coralTools) {
		t.Fatalf("expected all %d Coral tools forwarded, found %v", len(coralTools), found)
	}
	offJSON, _ := json.Marshal(off)
	strippedJSON, _ := json.Marshal(stripped)
	if !bytes.Equal(offJSON, strippedJSON) {
		t.Fatal("fleet_accelerators changed the tool list beyond adding the device's tools")
	}
}

// Local device always wins: a box that both carries the Coral and lists it as
// a fleet device registers the LOCAL lane, not the forwarder.
func TestLocalDeviceWinsOverTheSameFleetDevice(t *testing.T) {
	cfg := config.Default()
	cfg.Accelerators = []string{"coral-edgetpu"}
	cfg.FleetAccelerators = []string{"coral-edgetpu"}
	s := New(pipeline.New(cfg, nil, nil, nil))
	owner := s.registerAccelTools(mcp.NewServer(&mcp.Implementation{Name: "t"}, nil), cfg)
	for _, c := range coralTools {
		if owner[c] != "coral-edgetpu" {
			t.Errorf("%s owned by %q, want the local device", c, owner[c])
		}
	}
	// And the reverse shape — a Hailo box reaching a Coral over the fleet —
	// keeps the Hailo's shared names local and forwards only the Coral-only ones.
	cfg2 := config.Default()
	cfg2.Accelerators = []string{"hailo-8l"}
	cfg2.FleetAccelerators = []string{"coral-edgetpu"}
	s2 := New(pipeline.New(cfg2, nil, nil, nil))
	owner2 := s2.registerAccelTools(mcp.NewServer(&mcp.Implementation{Name: "t"}, nil), cfg2)
	if owner2["offload_object_detect"] != "hailo-8l" || owner2["offload_image_embed"] != "hailo-8l" {
		t.Errorf("shared names left the local Hailo: %v", owner2)
	}
	if owner2["offload_classify_image"] != "coral-edgetpu"+FleetOwnerSuffix || owner2["offload_semantic_segment"] != "coral-edgetpu"+FleetOwnerSuffix {
		t.Errorf("Coral-only names not forwarded: %v", owner2)
	}
}

// A forwarded call with no remotes configured is a device-prefixed defer,
// never a tool error — the caller does the work another way.
func TestForwardedToolDefersWithoutRemotes(t *testing.T) {
	cfg := config.Default()
	cfg.FleetAccelerators = []string{"coral-edgetpu"}
	s := New(pipeline.New(cfg, nil, nil, nil))
	h := s.handleFleetAccelTool("coral-edgetpu", "classify", "image_path")
	args, _ := json.Marshal(map[string]any{"image_path": "x.jpg"})
	res, err := h(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}})
	if err != nil {
		t.Fatal(err)
	}
	txt := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(txt, `"deferred":true`) || !strings.Contains(txt, "coral-edgetpu (fleet)") || !strings.Contains(txt, "delegate_remotes") {
		t.Fatalf("result = %s", txt)
	}
	// Empty required arg is refused before any forwarding.
	empty, _ := json.Marshal(map[string]any{"image_path": ""})
	res, _ = h(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: empty}})
	if txt := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(txt, "empty image_path") {
		t.Fatalf("empty arg: %s", txt)
	}
}

func TestStatusListsAFleetAccelerator(t *testing.T) {
	cfg := config.Default()
	cfg.FleetAccelerators = []string{"coral-edgetpu"}
	st := accelStatus(context.Background(), cfg)
	entry, ok := st["coral-edgetpu"].(map[string]any)
	if !ok || entry["fleet"] != true {
		t.Fatalf("status = %v", st)
	}
	if accelStatus(context.Background(), config.Default()) != nil {
		t.Fatal("status block present with nothing listed")
	}
}
