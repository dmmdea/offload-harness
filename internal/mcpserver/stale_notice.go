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
//
// Three doors (agent_delegate, offload_research, offload_status) already put stale_binary / stale_binary_guard in their own
// answer, but only on the path that ran the work: every refusal before it, and the config gate's deferral, answers with no key.
// So the middleware does not know those doors by name. It looks at the RESULT: a first block that is a JSON object holding one of
// the two keys at its top level is the door's own signal and is left alone, and every other result of every door, those doors'
// early returns included, gets the block. A name list would have left exactly the failures the guard exists for (a stale server
// that defers everything because its config is invalid) with no signal on the two doors that matter most.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
)

// staleGuardEvery is how often the "the guard is not watching" block rides on a result: on the first call that finds the guard
// blind and then on every staleGuardEvery-th such call. The stale notice itself is on EVERY call (a stale server stays stale
// until it is reconnected, and a cap would bring the silent failure back); a guard that cannot look is a standing condition of
// the process, and the same line on every call of every door would be noise nobody reads.
const staleGuardEvery = 20

// carriesStaleSignal reports whether a result's first block is the door's own signal: a JSON object that holds stale_binary or
// stale_binary_guard at its TOP level (the shape staleBinaryFields gives a delegating door's answer). The cheap test comes
// first, so the parse is paid only by an answer that has the text of a key in it at all; the parse then settles whether that
// text is a key of the object or only a mention (nested in a value, quoted in a string, a longer key, text that is not JSON), so
// a summary that quotes this very documentation is still told. Either key alone counts: a door's answer on a fresh server whose
// guard is blind carries only stale_binary_guard, and the guard state must not be added a second time beside it.
func carriesStaleSignal(first mcp.Content) bool {
	tc, ok := first.(*mcp.TextContent)
	if !ok || !strings.Contains(tc.Text, `"stale_binary`) {
		return false
	}
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(tc.Text), &top) != nil {
		return false
	}
	_, hasNote := top["stale_binary"]
	_, hasGuard := top["stale_binary_guard"]
	return hasNote || hasGuard
}

// staleNotice is the receiving middleware that appends the stale-binary notice to the result of every tools/call whose first
// block does not already carry the signal (carriesStaleSignal). It is registered after the config gate, so it wraps outside it:
// the deferral a server with an invalid config answers with is a result of a stale server too, on every door.
//
// A result that carries the key is passed through before any check: its door already stat the installed file (and may have
// hashed it, up to installedReadWait) for its own answer, and a check here would do the work twice. For every other result the
// check runs after the handler finished, so a long call is never delayed by it before it runs; on a server that was not swapped
// it is one stat, and on one that was the hash of the installed file is cached per stat (stale_image.go).
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
		res, err := next(ctx, method, req)
		if err != nil {
			return res, err
		}
		out, ok := res.(*mcp.CallToolResult)
		if !ok || out == nil || len(out.Content) == 0 || carriesStaleSignal(out.Content[0]) {
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
