package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

var coralTools = []string{"offload_classify_image", "offload_object_detect", "offload_semantic_segment", "offload_image_embed"}

// Registration is gated on the device being LISTED (Coral D5): none of the four
// tools without it, all four with it, and the rest of tools/list byte-identical
// — the same pin ADR 0024 set for the Hailo lane.
func TestCoralToolsRegistrationGatedOnAccelerator(t *testing.T) {
	off := listTools(t, config.Default())
	for _, tool := range off {
		for _, c := range coralTools {
			if tool.Name == c {
				t.Fatalf("%s advertised with no accelerator", c)
			}
		}
	}
	cfgOn := config.Default()
	cfgOn.Accelerators = []string{"coral-edgetpu"}
	on := listTools(t, cfgOn)
	found := map[string]bool{}
	var stripped []*mcp.Tool
	for _, tool := range on {
		isCoral := false
		for _, c := range coralTools {
			if tool.Name == c {
				found[c] = true
				isCoral = true
			}
		}
		if !isCoral {
			stripped = append(stripped, tool)
		}
	}
	if len(found) != len(coralTools) {
		t.Fatalf("expected all %d Coral tools, found %v", len(coralTools), found)
	}
	offJSON, _ := json.Marshal(off)
	strippedJSON, _ := json.Marshal(stripped)
	if !bytes.Equal(offJSON, strippedJSON) {
		t.Fatal("the Coral accelerator changed the tool list beyond adding its own tools")
	}
}

// The shared-name rule: offload_object_detect and offload_image_embed are owned
// by both devices, so on a box listing both exactly ONE of each registers, owned
// by whichever device is listed FIRST — in both orders, so the rule is about
// order and not about a favourite device.
func TestSharedNameRuleFirstListedOwnerWins(t *testing.T) {
	for _, order := range [][]string{{"hailo-8l", "coral-edgetpu"}, {"coral-edgetpu", "hailo-8l"}} {
		cfg := config.Default()
		cfg.Accelerators = order
		s := New(pipeline.New(cfg, nil, nil, nil))
		srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
		owner := s.registerAccelTools(srv, cfg)
		for _, shared := range []string{"offload_object_detect", "offload_image_embed"} {
			if owner[shared] != order[0] {
				t.Errorf("order %v: %s owned by %q, want the first listed %q", order, shared, owner[shared], order[0])
			}
		}
		// Device-unique tools are all present regardless of order.
		for _, n := range []string{"offload_face_detect", "offload_zero_shot", "offload_classify_image", "offload_semantic_segment"} {
			if _, ok := owner[n]; !ok {
				t.Errorf("order %v: %s not registered", order, n)
			}
		}
		// And the advertised list carries each shared name exactly once.
		count := map[string]int{}
		for _, tool := range listTools(t, cfg) {
			count[tool.Name]++
		}
		if count["offload_object_detect"] != 1 || count["offload_image_embed"] != 1 {
			t.Errorf("order %v: shared names registered %d/%d times", order, count["offload_object_detect"], count["offload_image_embed"])
		}
	}
}

// A Coral tool call passes the sidecar's dict through verbatim and a down,
// unspawnable sidecar defers with the DEVICE named — never a tool error.
func TestCoralToolPassThroughAndDefer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"enabled":true,"models_missing":[]}`)) })
	mux.HandleFunc("/v1/classify", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["domain"] != "birds" {
			t.Errorf("domain did not pass through: %v", in)
		}
		w.Write([]byte(`{"results":[{"label":"Ara macao (Scarlet Macaw)","score":0.758}],"best":{"label":"Ara macao (Scarlet Macaw)","score":0.758},"model":"mobilenet_v2_1.0_224_inat_bird_quant_edgetpu.tflite","domain":"birds"}`))
	})
	fake := httptest.NewServer(mux)
	defer fake.Close()

	cfg := config.Default()
	cfg.Accelerators = []string{"coral-edgetpu"}
	cfg.CoralEndpoint = fake.URL
	s := New(pipeline.New(cfg, nil, nil, nil))
	res, err := s.handleAccelTool("coral-edgetpu", "classify", "image_path")(context.Background(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{"image_path":"/tmp/parrot.jpg","domain":"birds"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "Ara macao") || strings.Contains(text, "deferred") {
		t.Fatalf("pass-through failed: %s", text)
	}

	// Down + no coral_sidecar_cmd: defer naming the device.
	fake.Close()
	cfg2 := config.Default()
	cfg2.Accelerators = []string{"coral-edgetpu"}
	cfg2.CoralEndpoint = fake.URL
	cfg2.CoralSidecarCmd = ""
	s2 := New(pipeline.New(cfg2, nil, nil, nil))
	res, _ = s2.handleAccelTool("coral-edgetpu", "classify", "image_path")(context.Background(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{"image_path":"/tmp/parrot.jpg"}`)}})
	text = res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, `"deferred":true`) || !strings.Contains(text, "coral-edgetpu:") {
		t.Fatalf("down sidecar did not defer with the device named: %s", text)
	}
}

// The status block lists every listed device with its lane config, owned
// capabilities and a live health probe — and never spawns the sidecar.
func TestStatusListsCoral(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"enabled":true,"device":"/dev/apex_0","status":"ALIVE","temp_c":50.3,"loaded":[],"models_missing":[]}`))
	})
	fake := httptest.NewServer(mux)
	defer fake.Close()
	cfg := config.Default()
	cfg.Accelerators = []string{"coral-edgetpu"}
	cfg.CoralEndpoint = fake.URL
	st := accelStatus(context.Background(), cfg)
	entry, ok := st["coral-edgetpu"].(map[string]any)
	if !ok {
		t.Fatalf("status has no coral-edgetpu entry: %v", st)
	}
	if entry["endpoint"] != fake.URL || !reflect.DeepEqual(entry["owns"], accelOwns("coral-edgetpu")) {
		t.Errorf("entry = %v", entry)
	}
	h, ok := entry["health"].(map[string]any)
	if !ok || h["status"] != "ALIVE" {
		t.Errorf("live health not reported: %v", entry)
	}
	if accelStatus(context.Background(), config.Default()) != nil {
		t.Error("status reported accelerators on a box listing none")
	}
}

// The agent loop advertises the SAME Coral tool names as the MCP surface — the
// parity the design's test list requires, so a contract landing on the node
// and a Claude session on it see one tool set.
func TestCoralLoopAndMCPToolParity(t *testing.T) {
	var mcpNames []string
	for _, tl := range accelMCPTools("coral-edgetpu") {
		mcpNames = append(mcpNames, tl.name)
	}
	var loopNames []string
	lanes := []agent.AccelLane{{ID: "coral-edgetpu", Call: func(context.Context, string, map[string]any) (string, error) { return "{}", nil }}}
	tools, err := agent.ReadOnlyToolsWithLanes(t.TempDir(), nil, nil, lanes)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		for _, n := range mcpNames {
			if tl.Name == n {
				loopNames = append(loopNames, tl.Name)
			}
		}
	}
	sort.Strings(mcpNames)
	sort.Strings(loopNames)
	if !reflect.DeepEqual(mcpNames, loopNames) {
		t.Errorf("loop %v != mcp %v", loopNames, mcpNames)
	}
}
