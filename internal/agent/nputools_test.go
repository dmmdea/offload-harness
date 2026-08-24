package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

var npuToolNames = []string{"offload_face_detect", "offload_face_embed", "offload_object_detect",
	"offload_person_embed", "offload_depth", "offload_enhance_low_light", "offload_image_embed",
	"offload_pose", "offload_segment", "offload_text_embed", "offload_zero_shot"}

func toolByName(t *testing.T, tools []Tool, name string) *Tool {
	t.Helper()
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

// TestNPUToolsGated: nil NPUFunc => the advertised tool list carries NO NPU
// tool — the loop-side mirror of the MCP registration pin (ADR 0024). A leak
// here would advertise device tools on boxes without the device.
func TestNPUToolsGated(t *testing.T) {
	off := func(context.Context, string, string, map[string]any) (string, error) { return "", nil }
	without, err := ReadOnlyTools(t.TempDir(), off, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range npuToolNames {
		if toolByName(t, without, n) != nil {
			t.Errorf("nil NPUFunc leaked tool %s", n)
		}
	}
	npu := func(context.Context, string, map[string]any) (string, error) { return "{}", nil }
	with, err := ReadOnlyTools(t.TempDir(), off, npu)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range npuToolNames {
		if toolByName(t, with, n) == nil {
			t.Errorf("wired NPUFunc missing tool %s", n)
		}
	}
	// The gate must add EXACTLY the 7 NPU tools and change nothing else.
	if len(with) != len(without)+len(npuToolNames) {
		t.Errorf("NPU wiring changed the tool count unexpectedly: %d -> %d", len(without), len(with))
	}
}

// TestNPUToolPassThrough: the loop tool hands the sidecar tool name and the
// caller's args through unchanged (the sidecar's own keyword names).
func TestNPUToolPassThrough(t *testing.T) {
	var gotTool string
	var gotArgs map[string]any
	npu := func(_ context.Context, tool string, args map[string]any) (string, error) {
		gotTool, gotArgs = tool, args
		return `{"count":1}`, nil
	}
	tools := npuTools(npu)
	fd := toolByName(t, tools, "offload_object_detect")
	if fd == nil {
		t.Fatal("offload_object_detect not built")
	}
	out, err := fd.Exec(context.Background(), `{"image_path":"C:/x/a.jpg","score_threshold":0.5}`)
	if err != nil || out != `{"count":1}` {
		t.Fatalf("exec: out=%q err=%v", out, err)
	}
	if gotTool != "object_detect" {
		t.Errorf("sidecar tool = %q, want object_detect", gotTool)
	}
	if gotArgs["image_path"] != "C:/x/a.jpg" || gotArgs["score_threshold"] != 0.5 {
		t.Errorf("args not passed through: %v", gotArgs)
	}
}

// TestNPUToolEmptyImagePathDefers: an empty/missing image_path defers before
// the sidecar is ever contacted (defer-not-crash; also avoids a pointless spawn).
func TestNPUToolEmptyImagePathDefers(t *testing.T) {
	called := false
	npu := func(context.Context, string, map[string]any) (string, error) { called = true; return "{}", nil }
	fd := toolByName(t, npuTools(npu), "offload_face_detect")
	out, err := fd.Exec(context.Background(), `{"image_path":"  "}`)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Deferred bool `json:"deferred"`
	}
	if json.Unmarshal([]byte(out), &d) != nil || !d.Deferred {
		t.Fatalf("want a deferred result, got %q", out)
	}
	if called {
		t.Error("sidecar contacted despite empty image_path")
	}
}

// TestLoopOCREngineRouting pins the loop-side OCR ownership rule: default/gpu
// rides the offload closure (vision model), engine:npu routes to the NPUFunc
// with the sidecar's arg name, and engine:npu WITHOUT a wired NPUFunc defers
// honestly instead of silently falling back — the two engines read stylised
// text differently, so a silent switch would change results.
func TestLoopOCREngineRouting(t *testing.T) {
	var offTask string
	var offParams map[string]any
	off := func(_ context.Context, task, _ string, params map[string]any) (string, error) {
		offTask, offParams = task, params
		return `{"text":"gpu"}`, nil
	}
	var npuTool string
	var npuArgs map[string]any
	npu := func(_ context.Context, tool string, args map[string]any) (string, error) {
		npuTool, npuArgs = tool, args
		return `{"text":"npu"}`, nil
	}

	tools, err := ReadOnlyTools(t.TempDir(), off, npu)
	if err != nil {
		t.Fatal(err)
	}
	ocr := toolByName(t, tools, "offload_ocr")
	if ocr == nil {
		t.Fatal("offload_ocr not built")
	}
	if !strings.Contains(string(ocr.Schema), `"engine"`) {
		t.Fatal("ocr schema does not advertise the engine param")
	}

	// Default: the GPU vision path via the offload closure.
	out, err := ocr.Exec(context.Background(), `{"path":"a.png"}`)
	if err != nil || out != `{"text":"gpu"}` {
		t.Fatalf("default exec: out=%q err=%v", out, err)
	}
	if offTask != "ocr" || offParams["image"] != "a.png" {
		t.Errorf("default did not ride the offload ocr task: task=%q params=%v", offTask, offParams)
	}
	if npuTool != "" {
		t.Error("default engine touched the NPU")
	}

	// engine:npu routes to the sidecar with ITS arg name.
	out, err = ocr.Exec(context.Background(), `{"path":"b.png","engine":"npu"}`)
	if err != nil || out != `{"text":"npu"}` {
		t.Fatalf("npu exec: out=%q err=%v", out, err)
	}
	if npuTool != "ocr" || npuArgs["image_path"] != "b.png" {
		t.Errorf("engine:npu did not reach the sidecar ocr tool: tool=%q args=%v", npuTool, npuArgs)
	}

	// engine:npu on a box with no accelerator => honest defer, never a silent GPU fallback.
	offTask = ""
	toolsNoNPU, err := ReadOnlyTools(t.TempDir(), off, nil)
	if err != nil {
		t.Fatal(err)
	}
	ocr2 := toolByName(t, toolsNoNPU, "offload_ocr")
	out, err = ocr2.Exec(context.Background(), `{"path":"c.png","engine":"npu"}`)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Deferred bool `json:"deferred"`
	}
	if json.Unmarshal([]byte(out), &d) != nil || !d.Deferred {
		t.Fatalf("want deferred on npu-without-accelerator, got %q", out)
	}
	if offTask != "" {
		t.Error("npu-without-accelerator silently fell back to the GPU path")
	}

	// An UNRECOGNIZED engine defers with the value named — never a silent GPU
	// call indistinguishable from an intentional one (review finding 2026-08-23).
	offTask = ""
	out, err = ocr.Exec(context.Background(), `{"path":"d.png","engine":"cloud"}`)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal([]byte(out), &d) != nil || !d.Deferred || !strings.Contains(out, "cloud") {
		t.Fatalf("want a deferred result naming the bad engine, got %q", out)
	}
	if offTask != "" {
		t.Error("unrecognized engine silently fell back to the GPU path")
	}
}

// TestNPUToolWrongTypeImagePath: a non-string image_path is reported as a TYPE
// problem, not as "empty" — a wrong diagnosis sends the caller debugging a
// defect that did not occur (review finding 2026-08-23).
func TestNPUToolWrongTypeImagePath(t *testing.T) {
	called := false
	npu := func(context.Context, string, map[string]any) (string, error) { called = true; return "{}", nil }
	fd := toolByName(t, npuTools(npu), "offload_face_detect")
	out, err := fd.Exec(context.Background(), `{"image_path":123}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "must be a string") || strings.Contains(out, "empty image_path") {
		t.Fatalf("want a type-error defer, got %q", out)
	}
	if called {
		t.Error("sidecar contacted despite wrong-typed image_path")
	}
}
