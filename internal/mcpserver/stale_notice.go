package mcpserver

// stale_notice.go: the stale-binary notice on every tool result of every door (D-36).
//
// The stale-binary guard (stalebinary.go) told a session that its MCP server runs an image older than the installed build
// on three of the doors only: agent_delegate, offload_research and offload_status. A session that called offload_summarize,
// offload_ask, a media door or any other door in the days after a deploy was never told, so a long-lived server went on
// stamping rows with old behaviour until somebody happened to ask for the status. The middleware in this file closes that:
// every tools/call result of a stale server carries the notice, whatever the door.
//
// The notice is a SECOND text block after the door's own answer. Content[0] is never touched, so every door's JSON still parses
// for a client that reads only the first block, and a client that hands the model every content block of a result hands it the
// notice with the answer (how each client renders a second block is for the release's live check to record). The text is the
// stale_binary note itself, the same string offload_status carries under that key, behind its name: one signal, one definition,
// one cache (staleBinaryNote), and the two cannot disagree.

import (
	"context"
	"slices"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
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

// staleNotice is the receiving middleware that appends the stale-binary notice to the result of every tools/call of a door
// that does not carry the signal itself. It is registered after the config gate, so it wraps outside it: the deferral a server
// with an invalid config answers with is a result of a stale server too.
//
// The doors in staleBinaryDoors are passed through BEFORE any check: they already stat the installed file (and may hash it, up to
// installedReadWait) for their own answer, and a check here would do the work twice on exactly those doors. For the others the
// check runs after the handler finished, as it does on the three doors, so a long call is never delayed by it before it runs.
//
// Nothing is added to a call that failed (an error is the client's to see as it is), to a result that holds no content (there
// is no first block to leave alone), or to a result whose pointer is nil: the SDK hands the middleware the handler's
// (*CallToolResult, error) as a (Result, error), so a handler that returns (nil, err) arrives as an interface that holds a nil
// pointer and the type assertion succeeds. A result is copied before a block is appended, never changed in place: a handler may
// keep the result it returned (a cached answer), and an append to its Content would grow it on every later call.
func (s *Server) staleNotice(next mcp.MethodHandler) mcp.MethodHandler {
	var blind atomic.Int64 // the calls that found the guard not watching, counted for the every-Nth rule
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, req)
		}
		params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
		if !ok || staleBinaryDoors[params.Name] {
			return next(ctx, method, req)
		}
		res, err := next(ctx, method, req)
		if err != nil {
			return res, err
		}
		out, ok := res.(*mcp.CallToolResult)
		if !ok || out == nil || len(out.Content) == 0 {
			return res, err
		}
		// The note first: its read of the stamp is what finds a stamp that cannot be read right now, which the state reads after.
		note, guard := s.staleBinaryFields(buildinfo.Version)
		var blocks []mcp.Content
		if note != "" {
			blocks = append(blocks, &mcp.TextContent{Text: "stale_binary: " + note})
		}
		if guard != "" && (blind.Add(1)-1)%staleGuardEvery == 0 {
			blocks = append(blocks, &mcp.TextContent{Text: "stale_binary_guard: " + guard})
		}
		if len(blocks) == 0 {
			return res, nil
		}
		annotated := *out
		annotated.Content = append(slices.Clip(out.Content), blocks...)
		return &annotated, nil
	}
}
