package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// callReq wraps raw argument JSON in a CallToolRequest the handlers accept.
func callReq(rawArgs string) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(rawArgs)}}
}

// decodeResult unmarshals the single text-content payload of a tool result.
func decodeResult(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res == nil || len(res.Content) != 1 {
		t.Fatalf("expected one content item, got %+v", res)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &m); err != nil {
		t.Fatalf("result not JSON: %v (%q)", err, tc.Text)
	}
	return m
}

// TestAllHandlersSurfaceBadArguments (LO-10): EVERY tool handler must turn an
// argument-decode error into {deferred:true, reason:"bad arguments: ..."} —
// never silently run on zero values (the old `_ = json.Unmarshal` swallowed
// e.g. a wrongly-typed "text", producing a misleading downstream defer).
// A handler that hits the bad-args path never touches the pipeline, so a
// Server with a nil pipeline proves the guard fires FIRST (a miss panics).
func TestAllHandlersSurfaceBadArguments(t *testing.T) {
	s := New(nil) // nil pipeline: any handler that gets past parseArgs will panic
	type handler func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error)
	cases := []struct {
		name string
		h    handler
		args string // type-mismatched for that tool's input struct
	}{
		{"summarize", s.handleSummarize, `{"text":123}`},
		{"classify", s.handleClassify, `{"text":"x","labels":"not-an-array"}`},
		{"extract", s.handleExtract, `{"text":"x","schema":"not-an-object"}`},
		{"triage", s.handleTriage, `{"text":true,"question":"q"}`},
		{"vqa", s.handleVQA, `{"image":1,"question":"q"}`},
		{"video_describe", s.handleVideoDescribe, `{"video":[],"question":"q"}`},
		{"transcribe", s.handleTranscribe, `{"audio":{},"hq":"yes"}`},
		{"extract_image", s.handleExtractImage, `{"image":"x","schema":[1,2]}`},
		{"assess_image", s.handleAssessImage, `{"image":9}`},
		{"ocr", s.handleOCR, `{"image":{}}`},
		{"generate_image", s.handleGenerateImage, `{"prompt":"p","width":"wide"}`},
		{"generate_svg", s.handleGenerateSVG, `{"kind":"gauge","spec":"not-an-object"}`},
		{"generate_video", s.handleGenerateVideo, `{"prompt":"p","frames":"many"}`},
		{"generate_audio", s.handleGenerateAudio, `{"text":"t","seconds":"ten"}`},
		{"nim", s.handleNIM, `{"prompt":"p","max_tokens":"lots"}`},
		{"agent_run", s.handleAgentRun, `{"goal":"g","max_steps":"all"}`},
		{"truncated json", s.handleSummarize, `{"text":"unterminated`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tc.h(context.Background(), callReq(tc.args))
			if err != nil {
				t.Fatalf("bad args must be a defer result, not a handler error: %v", err)
			}
			m := decodeResult(t, res)
			if m["deferred"] != true {
				t.Fatalf("want deferred:true, got %v", m)
			}
			reason, _ := m["reason"].(string)
			if !strings.HasPrefix(reason, "bad arguments: ") {
				t.Fatalf("reason = %q, want prefix 'bad arguments: '", reason)
			}
		})
	}
}

// TestParseArgsValidAndAbsent: well-formed arguments decode into the struct
// (nil result = proceed), and absent/empty arguments keep the prior
// zero-value behavior (required-field validation stays with the task).
func TestParseArgsValidAndAbsent(t *testing.T) {
	var in struct {
		Text string `json:"text"`
	}
	if bad := parseArgs(json.RawMessage(`{"text":"hola"}`), &in); bad != nil {
		t.Fatalf("valid args must pass, got %+v", bad)
	}
	if in.Text != "hola" {
		t.Fatalf("decoded Text = %q, want hola", in.Text)
	}
	if bad := parseArgs(nil, &in); bad != nil {
		t.Fatalf("absent args must pass with zero values, got %+v", bad)
	}
}
