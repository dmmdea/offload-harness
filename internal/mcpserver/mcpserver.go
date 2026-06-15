// Package mcpserver exposes the offload pipeline as MCP tools over stdio so
// Claude Code can delegate grunt work. Tools return the full Result JSON as
// text — a defer is a valid result (Claude then does the task itself), not an
// error.
package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/local-offload-pp-cli/internal/core"
	"github.com/dmmdea/local-offload-pp-cli/internal/pipeline"
)

type Server struct{ p *pipeline.Pipeline }

func New(p *pipeline.Pipeline) *Server { return &Server{p: p} }

// Run serves the MCP tools on stdin/stdout until the client disconnects.
func (s *Server) Run(ctx context.Context, version string) error {
	srv := mcp.NewServer(&mcp.Implementation{Name: "local-offload", Version: version}, nil)

	srv.AddTool(&mcp.Tool{
		Name:        "offload_summarize",
		Description: "Summarize text on a free local model. Use for bulk/low-judgment summaries to keep tokens out of your context. Returns {summary, bullets}; if it can't do it confidently it returns deferred:true and you should summarize it yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"text to summarize"},"max_points":{"type":"integer","description":"max bullet points (default 5)"}},"required":["text"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Text      string `json:"text"`
			MaxPoints int    `json:"max_points"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		params := map[string]any{}
		if in.MaxPoints > 0 {
			params["max_points"] = in.MaxPoints
		}
		return result(s.p.Run(ctx, core.Request{Task: core.TaskSummarize, Input: in.Text, Params: params}))
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_classify",
		Description: "Classify text into one of the given labels on a free local model. Returns {label, confidence}; low-confidence results are deferred back to you.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},"labels":{"type":"array","items":{"type":"string"},"description":"allowed labels (>=2)"}},"required":["text","labels"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Text   string   `json:"text"`
			Labels []string `json:"labels"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return result(s.p.Run(ctx, core.Request{Task: core.TaskClassify, Input: in.Text, Params: map[string]any{"labels": in.Labels}}))
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_extract",
		Description: "Extract structured fields from text on a free local model, constrained to the provided JSON schema. Returns the extracted object or defers.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},"schema":{"type":"object","description":"JSON schema with a properties object describing the fields to extract"}},"required":["text","schema"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Text   string         `json:"text"`
			Schema map[string]any `json:"schema"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return result(s.p.Run(ctx, core.Request{Task: core.TaskExtract, Input: in.Text, Params: map[string]any{"schema": in.Schema}}))
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_triage",
		Description: "Answer a yes/no/unsure question about text on a free local model. Returns {decision, reason} or defers.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},"question":{"type":"string"}},"required":["text","question"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Text     string `json:"text"`
			Question string `json:"question"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return result(s.p.Run(ctx, core.Request{Task: core.TaskTriage, Input: in.Text, Params: map[string]any{"question": in.Question}}))
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_vqa",
		Description: "Answer a question about an IMAGE on a free local vision model (VQA). image is a local file path or a data:image/... URI; question is what to ask about it. Returns {answer}; if it can't answer confidently it returns deferred:true and you should look at the image yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"image":{"type":"string","description":"local image file path or a data:image/...;base64 URI"},"question":{"type":"string","description":"the question to answer about the image"}},"required":["image","question"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Image    string `json:"image"`
			Question string `json:"question"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return result(s.p.Run(ctx, core.Request{Task: core.TaskVQA, Image: in.Image, Params: map[string]any{"question": in.Question}}))
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_extract_image",
		Description: "Extract structured fields from an IMAGE on a free local model: it OCRs the image, then extracts the fields from the transcribed text constrained to the provided JSON schema (values are grounded against the OCR text). image is a local file path or a data:image/... URI; schema is a JSON schema with a properties object. Returns the extracted object or defers.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"image":{"type":"string","description":"local image file path or a data:image/...;base64 URI"},"schema":{"type":"object","description":"JSON schema with a properties object describing the fields to extract"}},"required":["image","schema"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Image  string         `json:"image"`
			Schema map[string]any `json:"schema"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return result(s.p.Run(ctx, core.Request{Task: core.TaskExtractImage, Image: in.Image, Params: map[string]any{"schema": in.Schema}}))
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_assess_image",
		Description: "QA a generated IMAGE against hard exclusions on a free local vision model. Emits a grammar-constrained {has_people, has_text, matches_brief, notes}: has_people=true if any person/face/body part is visible, has_text=true if any readable letters/words/numbers are rendered, matches_brief=whether it matches the optional brief (true if no brief), notes=one short phrase. image is a local file path or a data:image/... URI; brief is optional. Returns the object or deferred:true.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"image":{"type":"string","description":"local image file path or a data:image/...;base64 URI"},"brief":{"type":"string","description":"optional description the image should match"}},"required":["image"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Image string `json:"image"`
			Brief string `json:"brief"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		params := map[string]any{}
		if in.Brief != "" {
			params["brief"] = in.Brief
		}
		return result(s.p.Run(ctx, core.Request{Task: core.TaskAssessImage, Image: in.Image, Params: params}))
	})

	srv.AddTool(&mcp.Tool{
		Name:        "offload_ocr",
		Description: "Transcribe ALL text in an IMAGE on a free local vision model (OCR). image is a local file path or a data:image/... URI. Returns {text} with the transcribed text in reading order; if it can't transcribe confidently it returns deferred:true and you should read the image yourself.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"image":{"type":"string","description":"local image file path or a data:image/...;base64 URI"}},"required":["image"]}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Image string `json:"image"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		return result(s.p.Run(ctx, core.Request{Task: core.TaskOCR, Image: in.Image}))
	})

	return srv.Run(ctx, &mcp.StdioTransport{})
}

func result(r core.Result) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
}
