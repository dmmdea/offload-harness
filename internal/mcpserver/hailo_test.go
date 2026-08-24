package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

var hailoTools = []string{"offload_face_detect", "offload_face_embed", "offload_object_detect",
	"offload_person_embed", "offload_depth", "offload_enhance_low_light", "offload_image_embed",
	"offload_pose", "offload_segment", "offload_text_embed", "offload_zero_shot"}

func TestHailoToolsRegistrationGatedOnAccelerator(t *testing.T) {
	off := listTools(t, config.Default())
	for _, tool := range off {
		for _, h := range hailoTools {
			if tool.Name == h {
				t.Fatalf("%s advertised with no accelerator", h)
			}
		}
	}
	cfgOn := config.Default()
	cfgOn.Accelerators = []string{"hailo-8l"}
	on := listTools(t, cfgOn)
	var stripped []*mcp.Tool
	found := map[string]bool{}
	for _, tool := range on {
		isHailo := false
		for _, h := range hailoTools {
			if tool.Name == h {
				found[h] = true
				isHailo = true
			}
		}
		if !isHailo {
			stripped = append(stripped, tool)
		}
	}
	if len(found) != len(hailoTools) {
		t.Fatalf("expected all %d NPU tools, found %v", len(hailoTools), found)
	}
	offJSON, _ := json.Marshal(off)
	strippedJSON, _ := json.Marshal(stripped)
	if !bytes.Equal(offJSON, strippedJSON) {
		t.Fatal("the accelerator changed the tool list beyond adding its own tools (offload_ocr's schema must only GAIN an optional field — check it is identical when the accelerator is absent)")
	}
}

func fakeHailo(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"enabled":true,"hefs_missing":[]}`)) })
	mux.HandleFunc("/v1/face_embed", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"faces":[{"x":1,"y":2,"w":3,"h":4,"score":0.9,"embedding":[0.5,0.5]}],"count":1}`))
	})
	mux.HandleFunc("/v1/ocr", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"text":"NPU READ THIS","char_count":13,"boxes":[]}`)) })
	return httptest.NewServer(mux)
}

func hailoServer(t *testing.T, endpoint string) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Accelerators = []string{"hailo-8l"}
	cfg.HailoEndpoint = endpoint
	return New(pipeline.New(cfg, nil, nil, nil))
}

func callText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	return res.Content[0].(*mcp.TextContent).Text
}

func TestFaceEmbedPassesSidecarResultThrough(t *testing.T) {
	srv := fakeHailo(t)
	defer srv.Close()
	s := hailoServer(t, srv.URL)
	res, err := s.handleHailoTool("face_embed", "image_path")(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{"image_path":"a.jpg"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	out := callText(t, res)
	if !strings.Contains(out, `"count":1`) || !strings.Contains(out, `"embedding":[0.5,0.5]`) {
		t.Fatalf("result not passed through: %s", out)
	}
}

func TestNPUToolDefersWhenSidecarDownAndUnspawnable(t *testing.T) {
	s := hailoServer(t, "http://127.0.0.1:1")
	res, _ := s.handleHailoTool("face_embed", "image_path")(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{"image_path":"a.jpg"}`)}})
	out := callText(t, res)
	if !strings.Contains(out, `"deferred":true`) || !strings.Contains(out, "hailo_sidecar_cmd") {
		t.Fatalf("want a defer naming the missing sidecar cmd, got %s", out)
	}
}

func TestOCREngineNPURoutesToSidecar(t *testing.T) {
	srv := fakeHailo(t)
	defer srv.Close()
	s := hailoServer(t, srv.URL)
	res, _ := s.handleOCR(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{"image":"a.jpg","engine":"npu"}`)}})
	if out := callText(t, res); !strings.Contains(out, "NPU READ THIS") {
		t.Fatalf("engine:npu did not reach the sidecar: %s", out)
	}
}

func TestOCREngineNPUWithoutAcceleratorDefers(t *testing.T) {
	s := New(pipeline.New(config.Default(), nil, nil, nil))
	res, _ := s.handleOCR(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{"image":"a.jpg","engine":"npu"}`)}})
	if out := callText(t, res); !strings.Contains(out, `"deferred":true`) || !strings.Contains(out, "no hailo-8l") {
		t.Fatalf("engine:npu on a box without the accelerator must defer plainly, got %s", out)
	}
}

func TestStatusReportsAcceleratorHealth(t *testing.T) {
	srv := fakeHailo(t)
	defer srv.Close()
	s := hailoServer(t, srv.URL)
	res, _ := s.handleStatus(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{}})
	out := callText(t, res)
	if !strings.Contains(out, `"accelerators"`) || !strings.Contains(out, `"hailo-8l"`) || !strings.Contains(out, `"enabled":true`) {
		t.Fatalf("status lacks the accelerator block: %s", out)
	}
	plain := New(pipeline.New(config.Default(), nil, nil, nil))
	res2, _ := plain.handleStatus(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{}})
	if strings.Contains(callText(t, res2), `"accelerators"`) {
		t.Fatal("a box with no accelerator must not grow an accelerators block")
	}
}
