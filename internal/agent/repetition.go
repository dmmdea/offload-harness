// repetition.go — the degenerate-loop guard for a final answer (register
// D-95b).
//
// A small seat asked for a long grounded extraction can stop making progress
// and start repeating itself, generating the same block until the completion
// budget runs out. The engine reports that as an ordinary `length` cut (or
// even as `stop`), so nothing downstream could tell it from an answer that
// merely ran long: the Lenovo 4B's METHODOLOGY.md digest of 2026-09-14 spent
// 381 s and its whole final budget emitting one four-line block about twenty
// times, and the node deferred with the loop riding in `output`.
//
// The guard reads the RETURNED TEXT — it never rewrites what the engine
// reported. It answers one question: does the tail of this answer consist of
// the same block, repeated? If so the run treats the final as cut (loop.go),
// re-issues once with an explicit do-not-repeat instruction on top of the list
// caps, and trims the repeated tail off the partial the caller sees.
package agent

import (
	"fmt"
	"strings"
)

// minRepeats is how many consecutive occurrences of a block make a loop. Four
// is the smallest count that cannot be a legitimate parallel structure: a
// schema's own repeated scaffolding ("- 1 sentence cap …" once per section)
// tops out at three in the contracts this fleet runs, and the measured
// degenerate runs repeat twenty times or more. Raising it would let a short
// loop through; lowering it to three would trim correct answers.
const minRepeats = 4

// maxPeriod bounds the block length the detector will consider, in lines. The
// live loop's period was 4; 32 covers a repeated paragraph without turning the
// scan into a quadratic search over a long answer.
const maxPeriod = 32

// maxScanLines bounds how much of the tail is scanned. A loop that matters
// always runs to the END of the answer (it is what consumed the budget), so
// the last few hundred lines are the whole evidence.
const maxScanLines = 400

// RepetitionLoop is one degenerate loop found in an answer: the block that
// recurs, how many times, and where the SECOND occurrence starts in the
// original text (the byte offset the trim cuts at).
type RepetitionLoop struct {
	// Count is the number of consecutive occurrences of the block, the first
	// one included.
	Count int
	// Period is the block's length in non-blank lines (1 = a single repeated
	// line).
	Period int
	// First is the block's first line as the seat wrote it — the evidence
	// quoted in the stop note. Comparison is done on the normalised form;
	// what an operator greps for is the original.
	First string
	// CutAt is the byte offset in the original text where the SECOND
	// occurrence begins. Text before it is the answer plus one copy.
	CutAt int
}

// NoRepeatInstruction is the sentence a re-issue adds when the answer it
// replaces was a repetition loop. It is deliberately concrete — a seat that
// looped once will loop again on the same request unless it is told what went
// wrong (the same reason the list-cap instruction says the answer was cut).
const NoRepeatInstruction = "Your previous answer got stuck REPEATING the same lines over and over, which is why it ran out of room. Do not repeat any line; write each list item exactly once and then stop."

// DetectRepetitionLoop reports whether the tail of s is the same block
// repeated at least minRepeats times. It normalises each line (trimmed,
// inner whitespace collapsed, case-folded) so a loop that drifts in spacing is
// still one loop, and it ignores blank lines entirely — a run of empty lines
// is formatting, never a degenerate generation.
func DetectRepetitionLoop(s string) (RepetitionLoop, bool) {
	norm, raw, offsets := normalizedLines(s)
	if len(norm) < minRepeats {
		return RepetitionLoop{}, false
	}
	// Only the tail is evidence; a loop that matters ran to the end.
	if len(norm) > maxScanLines {
		cut := len(norm) - maxScanLines
		norm, raw, offsets = norm[cut:], raw[cut:], offsets[cut:]
	}
	n := len(norm)
	// Smallest period first: a 4-line block repeated 20 times also "matches"
	// an 8-line block repeated 10 times, and the fundamental block is the one
	// worth reporting and the one that keeps the most of the answer.
	for p := 1; p <= maxPeriod && p*minRepeats <= n; p++ {
		// The last block can be CUT mid-way (the budget ran out inside it), so
		// the aligned-at-the-end window is not always a whole block. Slide the
		// window back up to one period to find the alignment that repeats.
		for drop := 0; drop < p; drop++ {
			end := n - drop
			if end-p < 0 {
				continue
			}
			block := norm[end-p : end]
			count, start := 1, end-p
			for start-p >= 0 && sameLines(norm[start-p:start], block) {
				count++
				start -= p
			}
			if count >= minRepeats {
				return RepetitionLoop{
					Count:  count,
					Period: p,
					First:  raw[start],
					CutAt:  offsets[start+p],
				}, true
			}
		}
	}
	return RepetitionLoop{}, false
}

// TrimRepetitionLoop returns the answer with the repeated tail cut to ONE
// copy plus a marker, the one-line stop note naming the loop, and whether a
// loop was found. On a clean answer it is a byte-for-byte no-op with an empty
// note — it runs on every final.
func TrimRepetitionLoop(s string) (string, string, bool) {
	rep, ok := DetectRepetitionLoop(s)
	if !ok {
		return s, "", false
	}
	kept := strings.TrimRight(s[:rep.CutAt], "\r\n")
	out := kept + fmt.Sprintf("\n[repetition trimmed x%d]", rep.Count-1)
	return out, fmt.Sprintf("repetition loop (%dx %q)", rep.Count, clip(rep.First, 40)), true
}

// normalizedLines splits s into its NON-BLANK lines — the normalised form for
// comparison, the line as written for the note, and the byte offset in s at
// which each starts. The offsets are what the trim cuts on, so they must index
// the ORIGINAL text, never the normalised copy.
func normalizedLines(s string) ([]string, []string, []int) {
	var lines []string
	var raws []string
	var offsets []int
	for off := 0; off < len(s); {
		end := strings.IndexByte(s[off:], '\n')
		var raw string
		next := len(s)
		if end < 0 {
			raw = s[off:]
		} else {
			raw = s[off : off+end]
			next = off + end + 1
		}
		if t := normalizeLine(raw); t != "" {
			lines = append(lines, t)
			raws = append(raws, strings.TrimSpace(raw))
			offsets = append(offsets, off)
		}
		off = next
	}
	return lines, raws, offsets
}

// normalizeLine collapses a line to its comparable form: trimmed, inner
// whitespace runs squeezed to one space, case-folded. A loop that re-emits the
// same item with different indentation is the same loop.
func normalizeLine(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func sameLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
