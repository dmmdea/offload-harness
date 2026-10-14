package mcpserver

// stale_notice.go: the stale-binary notice on every tool result of every door (D-36).
//
// The stale-binary guard (stalebinary.go) told a session that its MCP server runs an image older than the installed build
// on three of the doors only: agent_delegate, offload_research and offload_status. A session that called offload_summarize,
// offload_ask, a media door or any other door in the days after a deploy was never told, so a long-lived server went on
// stamping rows with old behaviour until somebody happened to ask for the status. The middleware in this file closes that:
// every tools/call result of a stale server carries the notice, whatever the door.
//
// Seam: the types and the pass-through method come first; the registration in buildServer and the behaviour follow.

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// staleGuardEvery is how often the "the guard is not watching" block rides on a result: on the first call that finds the guard
// blind and then on every staleGuardEvery-th such call. The stale notice itself is on EVERY call (a stale server stays stale
// until it is reconnected, and a cap would bring the silent failure back); a guard that cannot look is a standing condition of
// the process, and the same line on every call of every door would be noise nobody reads.
const staleGuardEvery = 20

// staleBinaryDoors are the doors whose own answer already carries stale_binary and stale_binary_guard (they call
// staleBinaryFields / staleBinaryNote themselves), so the middleware leaves their results alone. A test pins the set against the
// call sites in mcpserver.go: a fourth door that gains the keys without being listed here would carry the signal twice.
var staleBinaryDoors = map[string]bool{
	"offload_status":   true,
	"agent_delegate":   true,
	"offload_research": true,
}

// staleNotice is the receiving middleware that appends the stale-binary notice to the result of every tools/call.
func (s *Server) staleNotice(next mcp.MethodHandler) mcp.MethodHandler { return next }
