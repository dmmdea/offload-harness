package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// handleRunTemplate - STUB (red commit): the handler is written in the next commit.
func (s *Server) handleRunTemplate(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return jsonResult(map[string]any{"deferred": true, "reason": "not implemented"})
}
