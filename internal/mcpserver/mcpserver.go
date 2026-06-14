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

	return srv.Run(ctx, &mcp.StdioTransport{})
}

func result(r core.Result) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
}
