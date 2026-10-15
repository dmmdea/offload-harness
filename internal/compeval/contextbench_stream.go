// contextbench_stream.go - how an operation reaches the agent and how the agent asks
// for the next one.
//
// The operation rendering and the standalone-marker rule are ported from
// contextbench/stream.py (render_op, _standalone_line_re, is_ready_line,
// is_submit_line) in https://github.com/RulinShao/ContextBench at commit
// fb533c18c78a82d3f1b2047a9c1700735cf829ae (Apache-2.0); the code and the texts sent
// back to the agent are written for this repository. The shell-statement parser of
// the original is not ported: the adapter drives a chat seat that has no shell.
package compeval

import (
	"fmt"
	"regexp"
)

// The two markers an agent puts on a line of its own.
const (
	CBAdvanceMarker = "READY_FOR_NEXT_OP"
	CBSubmitMarker  = "COMPLETE_TASK_AND_SUBMIT_FINAL_OUTPUT"
)

// cbMarkerLine matches the marker alone on a line, tolerating surrounding whitespace,
// code-fence backticks and a trailing full stop. The marker inside a sentence or
// inside answer data never matches: a spurious advance would consume an operation
// that is then lost.
func cbMarkerLine(marker string) *regexp.Regexp {
	return regexp.MustCompile("(?m)^[ \\t]*(?:`{1,3})?" + regexp.QuoteMeta(marker) + "(?:`{1,3})?[ \\t]*\\.?[ \\t]*$")
}

var (
	cbReadyLineRE  = cbMarkerLine(CBAdvanceMarker)
	cbSubmitLineRE = cbMarkerLine(CBSubmitMarker)
)

// cbIsReady reports whether the reply carries the advance marker on a line of its own.
func cbIsReady(reply string) bool { return reply != "" && cbReadyLineRE.MatchString(reply) }

// cbIsSubmit reports whether the reply carries the submit marker on a line of its own.
func cbIsSubmit(reply string) bool { return reply != "" && cbSubmitLineRE.MatchString(reply) }

// cbRenderOp is the exact text of one operation turn; remaining counts the
// operations that follow it.
func cbRenderOp(op CBOp, remaining int) string {
	return fmt.Sprintf("=== %s (%d op(s) remaining after this) ===\n%s", op.Header, remaining, op.Body)
}

// Texts the harness sends when the agent's reply did not advance the stream.
const (
	cbRefuseSubmitText = "Nothing can be submitted yet: operations are still being delivered. " +
		"Finish the current operation, then put " + CBAdvanceMarker + " on a line of its own to receive the next one."
	cbNoMarkerText = "[The stream did not advance: there is no " + CBAdvanceMarker + " on a line of its own in your reply. " +
		"Finish the current operation, then put " + CBAdvanceMarker + " on a line of its own.]"
	cbNonStandaloneText = "[" + CBAdvanceMarker + " appeared inside other text, which does not count. " +
		"Put it on a line of its own to receive the next operation.]"
)
