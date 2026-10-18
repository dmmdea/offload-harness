package pipeline

import (
	"fmt"
	"strings"
)

// The counting loop: the one degenerate tail that repeats no byte.
//
// A grammar-constrained seat that has written its last real item and cannot close the
// list sometimes counts: `"349", "350", "351", ...`. Every item differs from the one
// before, so neither the byte-level repeat test nor agent.DetectRepetitionLoop (identical
// lines) has anything to find, and the cut was read as a budget that was too small. On a
// delegated run of 2026-10-15 that sent the retry to the 8,192-token cap, where the seat
// counted on to 1482 for 145 s of a 197 s re-pack; the attempt that was finally told not
// to repeat finished the object in 14 s.
//
// What the tail has is the same text around a number that rises by one. That is what is
// read here, and it is read as a loop only when all of these hold:
//
//   - the numbers rise by exactly one each, and the text between every pair of them is
//     the same (so a numbered list of different items, "1. alpha" / "2. beta", is not one);
//   - there are degenerateMinCount of them and they span degenerateMinCover bytes (a short
//     run of years, or a list of twelve months, is ordinary content);
//   - the run reaches the END of the output: what follows the last number is only the
//     start of the next item (the separator, or the separator and the digits the cut fell
//     inside). A list that counts and then closes is not what spent the budget.
//
// The limit, stated: it counts up by ONE. A loop that counts by two, counts down, or
// carries other digits in each item (`"id 349 of 900"`) breaks the chain and is read as
// before, by whichever test already named it, or as content.
const (
	// degenerateMinCount is how many consecutive numbers make a counting loop. CHOSEN,
	// not measured: the loop that was measured counted for hundreds, and twelve is the
	// longest a count that is only formatting (the months, the hours) can run.
	degenerateMinCount = 12
	// maxDigitRun bounds the digits one number may have. A longer run is not a counter
	// (a hash, an identifier) and is never part of a chain.
	maxDigitRun = 18
)

// digitRun is one maximal run of ASCII digits in a window: where it sits and its value,
// -1 when it is too long to be a counter.
type digitRun struct {
	start, end int
	value      int64
}

// digitRuns lists the maximal digit runs of s in order.
func digitRuns(s string) []digitRun {
	var runs []digitRun
	for i := 0; i < len(s); {
		if s[i] < '0' || s[i] > '9' {
			i++
			continue
		}
		start := i
		var v int64
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			if i-start < maxDigitRun {
				v = v*10 + int64(s[i]-'0')
			}
			i++
		}
		if i-start > maxDigitRun {
			v = -1
		}
		runs = append(runs, digitRun{start: start, end: i, value: v})
	}
	return runs
}

// countingLoop says what the end of window is when it is a count that runs to the cut, and
// "" when it is not. window is the tail of a completion that hit max_tokens.
func countingLoop(window string) string {
	runs := digitRuns(window)
	// A run that touches an end of the window may have been cut there: the last one is the
	// item the budget ran out inside, and the first may be the window's own boundary.
	if n := len(runs); n > 0 && runs[n-1].end == len(window) {
		runs = runs[:n-1]
	}
	if len(runs) > 0 && runs[0].start == 0 {
		runs = runs[1:]
	}
	n := len(runs)
	if n < degenerateMinCount {
		return ""
	}
	gapBefore := func(i int) string { return window[runs[i-1].end:runs[i].start] }
	gap := gapBefore(n - 1)
	first := n - 1
	for first > 0 && runs[first-1].value >= 0 && runs[first].value == runs[first-1].value+1 && gapBefore(first) == gap {
		first--
	}
	last := runs[n-1]
	count := n - first
	if count < degenerateMinCount || last.end-runs[first].start < degenerateMinCover {
		return ""
	}
	// The count has to run to the end: the separator, cut anywhere, or the separator and the
	// digits of the next number.
	rest := window[last.end:]
	if !strings.HasPrefix(gap, rest) && !(strings.HasPrefix(rest, gap) && allDigits(rest[len(gap):])) {
		return ""
	}
	return fmt.Sprintf("counting up one at a time, %d numbers from %d to %d", count, runs[first].value, last.value)
}

// allDigits reports whether s is only ASCII digits (true for the empty string).
func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
