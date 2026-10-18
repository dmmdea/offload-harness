package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// A re-pack that COUNTS is a loop no byte-level test sees: every item differs from the one
// before it, so there is no block to repeat. Measured on a delegated run of 2026-10-15 (a
// 9B seat, a 4,729-character answer to structure): the grammar attempt ran out its
// 2,088-token budget writing `"349", "350", "351", ...` after its last real fact; the cut
// was read as "the budget was too small", the retry went to the 8,192-token cap and counted
// on to `"1482"`, and that second request spent 145 s of a 197 s re-pack before the
// attempt told not to repeat finished the object in 14 s.

// countedItems is `from` to `to` inclusive, each number quoted, separated by sep.
func countedItems(from, to int, sep string) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		if i > from {
			b.WriteString(sep)
		}
		fmt.Fprintf(&b, `"%d"`, i)
	}
	return b.String()
}

// countedLines is `<label><n>` on a line each, from `from` to `to` inclusive.
func countedLines(label string, from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "%s%d\n", label, i)
	}
	return b.String()
}

func TestDegenerateTailReadsAMonotoneCountingLoop(t *testing.T) {
	facts := `{"key_facts": ["Device posture management lets you define a posture", "It is an access control feature, not a hardening page", `
	cases := []struct {
		name    string
		content string
		want    string // "" = reads as content; else a substring of the finding
	}{
		// The shape the measured run wrote: a single-line JSON list that counts, cut inside
		// the next item.
		{"quoted numbers on one line, cut inside an item", facts + countedItems(10, 359, ", ") + `, "3`, "counting"},
		{"quoted numbers on one line, cut inside the separator", facts + countedItems(10, 359, ", ") + `, "`, "counting"},
		{"quoted numbers on one line, cut exactly after a number", facts + countedItems(10, 359, ", "), "counting"},
		{"quoted numbers that cross a power of ten", facts + countedItems(80, 140, ", ") + `, "14`, "counting"},
		// The same loop written line by line.
		{"a label and a number on each line", proseNoise(300) + "\n" + countedLines("Line ", 1, 140) + "Line 14", "counting"},
		{"bare numbers on each line", proseNoise(300) + "\n" + countedLines("", 1, 200) + "20", "counting"},
		{"unquoted numbers on one line", proseNoise(300) + " " + strings.ReplaceAll(countedItems(1, 200, ", "), `"`, "") + ", 20", "counting"},

		// Content that has numbers in it and is not a loop.
		{"three consecutive years closing a list", facts + `"2024", "2025", "2026"]}`, ""},
		{"nine consecutive numbers, short of the floor", facts + countedItems(1, 9, ", ") + `, "1`, ""},
		{"twenty-five consecutive years, closed", facts + countedItems(2002, 2026, ", ") + `]}`, ""},
		{"consecutive numbers followed by prose", facts + countedItems(100, 140, ", ") + `, "and then the seat wrote an ordinary closing sentence that is not a number at all"`, ""},
		{"a numbered list of distinct items", proseNoise(100) + "\n" + numberedDistinctList(30), ""},
		{"consecutive numbers that restart", facts + countedItems(1, 12, ", ") + `, ` + countedItems(1, 12, ", ") + `, "7`, ""},
		{"numbers that jump", facts + `"101", "205", "309", "413", "517", "621", "725", "829", "933", "1037", "1141", "1245", "1349", "1453", "1557", "1661", "1765", "1869", "1973", "20`, ""},
		{"ordinary prose with digits", proseNoise(400) + " 2026 10 15", ""},
	}
	for _, tc := range cases {
		got := degenerateTail(tc.content)
		if tc.want == "" && got != "" {
			t.Errorf("%s: degenerateTail = %q, want it to read as content", tc.name, got)
		}
		if tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: degenerateTail = %q, want it to say %q", tc.name, got, tc.want)
		}
	}
}

// numberedDistinctList is "1. <word>" through "n. <word>", every item different: a
// numbered list is the most ordinary place for a consecutive run of numbers.
func numberedDistinctList(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%d. %s item about %s\n", i, proseWords[(i*7)%len(proseWords)], proseWords[(i*13)%len(proseWords)])
	}
	return b.String()
}

// The loop is read by the cut judge as a loop, not as a budget that was too small: the
// retry is asked not to repeat, at the budget the first request had, and not sent to the
// 8,192-token cap with the same sampling to count on.
func TestRunAgentTaskRepackAfterACountingLoopIsNotEscalatedToTheCap(t *testing.T) {
	answer := answerOfChars(4700)
	budget := repackBudget(answer)
	// About 2 bytes per token: dense enough that the answer "needs more than the budget",
	// which is what made the measured run escalate.
	loop := `{"answer":["`
	for n := 1; len(loop) < budget*2; n++ {
		loop += fmt.Sprintf(`%d", "`, n)
	}
	loop += `3`

	// The fixture models the measured run only if, with the loop not recognised, the cut
	// would have been escalated: pin that, so a change to the density arithmetic cannot
	// quietly turn this into a test of something else.
	cut := judgeCut(answer, llamaclient.GenResult{Content: loop, TokensOut: budget})
	unrecognised := cut
	unrecognised.Degenerate = ""
	if !unrecognised.escalates(budget) {
		t.Fatalf("fixture: a cut of %d bytes in %d tokens against a %d-byte answer would not be escalated (%+v)", len(loop), budget, len(answer), unrecognised)
	}

	fake := loopSeat(answer, loop, `{"answer":"42"}`)
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s\nrepack_note: %s", wire.DeferClass, wire.Reason, wire.RepackNote)
	}
	var st map[string]string
	if err := json.Unmarshal(wire.Structured, &st); err != nil || st["answer"] != "42" {
		t.Fatalf("structured = %s (%v), want the object the conditioned attempt wrote", wire.Structured, err)
	}
	first, second := <-fake.repackBodies, <-fake.repackBodies
	if askedNoRepeat(first) {
		t.Fatalf("the FIRST attempt carries the no-repeat conditioning: %v", first)
	}
	if !askedNoRepeat(second) {
		t.Fatalf("the attempt after a counting loop is not conditioned against repeating (a plain request to the cap): %v", second)
	}
	firstMax, _ := first["max_tokens"].(float64)
	secondMax, _ := second["max_tokens"].(float64)
	if int(secondMax) >= agentRepackMaxTokensCap || secondMax > firstMax {
		t.Fatalf("second attempt max_tokens = %v after a first of %v, want the first request's budget and not the %d-token cap", secondMax, firstMax, agentRepackMaxTokensCap)
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 2 || !strings.Contains(d[0].Why, "degenerate") || !strings.Contains(d[0].Why, "counting") {
		t.Fatalf("detail = %+v, want a first attempt filed as a degenerate counting loop, then the conditioned one", d)
	}
}
