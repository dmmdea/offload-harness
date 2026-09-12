package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

func callVision(t *testing.T, h mcp.ToolHandler, args map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(args)
	res, err := h(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: raw}})
	if err != nil {
		t.Fatal(err)
	}
	return res.Content[0].(*mcp.TextContent).Text
}

// TestVisionToolsAdvertiseTheRoute: the three single-image tools carry the
// same route enum; video tools do not (a video never travels).
func TestVisionToolsAdvertiseTheRoute(t *testing.T) {
	want := map[string]bool{"offload_vqa": true, "offload_assess_image": true, "offload_ocr": true, "offload_video_describe": false}
	for _, tool := range listTools(t, config.Default()) {
		expect, care := want[tool.Name]
		if !care {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s: schema not JSON: %v", tool.Name, err)
		}
		route, has := schema.Properties["route"]
		if has != expect {
			t.Fatalf("%s route param present = %v, want %v", tool.Name, has, expect)
		}
		if has && strings.Join(route.Enum, ",") != "local,auto,remote" {
			t.Fatalf("%s route enum = %v", tool.Name, route.Enum)
		}
	}
}

// TestVisionRouteRefusalsNeverRunThePipeline: an unknown route, and route
// remote with no remotes, are defers produced BEFORE the pipeline runs — the
// server here has no endpoint, so a run would defer with a different reason.
func TestVisionRouteRefusalsNeverRunThePipeline(t *testing.T) {
	s := New(pipeline.New(config.Default(), nil, nil, nil))
	txt := callVision(t, s.handleAssessImage, map[string]any{"image": "x.png", "route": "cloud"})
	if !strings.Contains(txt, `"deferred":true`) || !strings.Contains(txt, `"defer_class":"contract"`) || !strings.Contains(txt, "unrecognized route") {
		t.Fatalf("unknown route: %s", txt)
	}
	txt = callVision(t, s.handleVQA, map[string]any{"image": "x.png", "question": "what?", "route": "remote"})
	if !strings.Contains(txt, `"deferred":true`) || !strings.Contains(txt, `"defer_class":"config"`) || !strings.Contains(txt, "delegate_remotes") {
		t.Fatalf("remote without remotes: %s", txt)
	}
	// engine npu never travels: a non-local route is refused, not silently local.
	txt = callVision(t, s.handleOCR, map[string]any{"image": "x.png", "engine": "npu", "route": "remote"})
	if !strings.Contains(txt, `"deferred":true`) || !strings.Contains(txt, "engine gpu only") {
		t.Fatalf("npu + remote: %s", txt)
	}
}
