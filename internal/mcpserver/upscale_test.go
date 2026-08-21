package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// TestUpscaleImageAdvertised pins offload_upscale_image on tools/list with the
// schema the pipeline actually consumes (image required; scale/width/height/
// method/model/out optional) — a tool that is registered but mis-advertised is
// invisible to every MCP client.
func TestUpscaleImageAdvertised(t *testing.T) {
	var found bool
	for _, tool := range listTools(t, config.Default()) {
		if tool.Name != "offload_upscale_image" {
			continue
		}
		found = true
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		s := string(schema)
		for _, want := range []string{`"required":["image"]`, `"scale"`, `"width"`, `"height"`, `"method"`, `"model"`, `"out"`} {
			if !strings.Contains(s, want) {
				t.Errorf("offload_upscale_image schema lacks %s: %s", want, s)
			}
		}
		for _, want := range []string{"ESRGAN", "upscale_model", "deferred:true", "offload_edit_image"} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("offload_upscale_image description lacks %q", want)
			}
		}
	}
	if !found {
		t.Fatal("offload_upscale_image not advertised on tools/list")
	}
}
