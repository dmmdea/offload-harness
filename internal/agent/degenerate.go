// degenerate.go — the NaN shape, named (register D-118).
//
// A seat whose numerics are broken does not fail loudly: /health is green,
// /v1/models lists it, a speed probe passes, and every completion comes back as
// the same token repeated to the cap — `<tool_call>!!!!!!!!!!!!!!!!!!!!…` on the
// Qwen3.8-27B GSQ seat under vLLM 0.29 with fp8_e5m2 KV through FlashInfer
// (2026-09-16/17, blackwell-16; the ADR 0048 Amendment 1 arm). Every contract it
// took reported `unparsed_tool_call` after 126–336 s of that output.
//
// DegenerateRun is the one detector for that shape, so the admission probe
// (pipeline.ProbeSeatCoherence) and anything else that reads a raw completion
// judge it the same way.
package agent

// degenerateRunMin is the run length at which a repeated byte stops being
// punctuation and becomes a broken sampler. Twenty is well past anything prose,
// code or JSON produces (the longest natural run in this repo's own corpus is a
// Markdown rule), and well under the hundreds a NaN seat emits before its cap.
const degenerateRunMin = 20

// DegenerateRun returns the byte and length of the longest run of ONE repeated
// NON-WHITESPACE byte in content when that run reaches degenerateRunMin, and
// (0, 0) otherwise.
//
// Whitespace is excluded on purpose: indentation, blank-line padding and the
// space runs in an ASCII table are ordinary text, and counting them would flag
// healthy output. Bytes, not runes: a multi-byte rune repeated 20 times still
// repeats its own bytes far past the floor, so the byte scan catches it — it
// simply names a byte of the rune rather than the rune.
func DegenerateRun(content string) (byte, int) {
	var (
		bestByte byte
		bestRun  int
		run      int
	)
	for i := 0; i < len(content); i++ {
		c := content[i]
		if isProbeSpace(c) {
			run = 0
			continue
		}
		if i > 0 && content[i-1] == c {
			run++
		} else {
			run = 1
		}
		if run > bestRun {
			bestByte, bestRun = c, run
		}
	}
	if bestRun < degenerateRunMin {
		return 0, 0
	}
	return bestByte, bestRun
}

// isProbeSpace is ASCII whitespace, spelled out rather than pulled from
// unicode: this scan is over BYTES, and unicode.IsSpace on a byte cast to a
// rune would misjudge the continuation bytes of a multi-byte rune.
func isProbeSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}
