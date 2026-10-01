package pipeline

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// What the tail of a cut completion is: whitespace, a loop, or content. A loop runs
// to the END of the output (it is what spent the budget), so a repeat counts as
// evidence only when it reaches the end and covers half the 256-byte window the tail
// is read through (whitespace, which no JSON value needs in quantity, needs 32
// bytes): the same byte 30 times, a markdown rule, base64 padding or a list of a
// few identical items is ordinary content on a 1,000-token output (register C-80).
func TestDegenerateTail(t *testing.T) {
	line := `"a long repeated list item that is well over sixteen bytes long",` + "\n"
	cases := []struct {
		name    string
		content string
		want    string // "" = reads as content; else a substring of the finding
	}{
		{"empty", "", "whitespace"},
		{"only whitespace", "  \n\t  \n", "whitespace"},
		{"a JSON prefix and then spaces", `{"a":[` + strings.Repeat(" ", 400), "whitespace"},
		{"spaces after real text, inside the window", `{"a":["x"]` + strings.Repeat(" ", 100), "repeating"},
		{"one byte repeated", proseNoise(300) + strings.Repeat("!", 160), "repeated"},
		{"a short block repeated, on one line", proseNoise(300) + strings.Repeat(`"", `, 40), "repeating"},
		{"a block of 24 bytes repeated, cut inside the last copy", proseNoise(300) + strings.Repeat(`"alpha item", "beta", `, 9) + `"alpha it`, "repeating"},
		{"thirty copies of a five-byte block reach the half window", proseNoise(300) + strings.Repeat(`"0", `, 30), "repeating"},
		{"a block of lines repeated", proseNoise(200) + "\n" + strings.Repeat(line, 6), "line block"},
		// Ordinary content that a short repeat used to read as a loop: every one of these
		// is a legitimate tail of a cut completion.
		{"a markdown rule at the end", proseNoise(300) + strings.Repeat("-", 40), ""},
		{"a markdown rule in the middle", proseNoise(200) + strings.Repeat("-", 40) + proseNoise(100), ""},
		{"base64 padding", proseNoise(300) + strings.Repeat("=", 26), ""},
		{"a leader of dots", proseNoise(300) + strings.Repeat(".", 30), ""},
		{"zeros in an identifier", proseNoise(300) + "id-" + strings.Repeat("0", 25) + "7", ""},
		{"a list of twenty identical items", proseNoise(300) + strings.Repeat(`"0", `, 20), ""},
		{"a list of a few identical pairs", proseNoise(300) + strings.Repeat(`"yes","no",`, 8), ""},
		{"a short column of identical zero pairs", proseNoise(300) + strings.Repeat("00 ", 20), ""},
		{"six identical lines", proseNoise(200) + "\n" + strings.Repeat(`  "N/A",`+"\n", 6), ""},
		{"ordinary prose", proseNoise(500), ""},
		{"dense digits", digitNoise(500), ""},
		{"a short JSON prefix", `{"answer":"4`, ""},
		{"a repeat too short to be evidence", "ab ab ab", ""},
		{"a legitimate list of distinct items", `{"k":["alpha","beta","gamma","delta","epsilon","zeta","eta","theta","iota","kappa"]`, ""},
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

// The escalation gate: a larger budget is worth a request only when the answer
// needs more than the budget held, at the density the seat actually wrote, and
// the output was not a loop.
func TestJudgeCutAndEscalation(t *testing.T) {
	answer := answerOfChars(2800)
	budget := repackBudget(answer)

	// Prose density (~4 bytes per token): the answer needs len/3 + 64, inside the
	// budget the code already sizes with 512 tokens of margin. Not the budget.
	prose := judgeCut(answer, llamaclient.GenResult{Content: proseNoise(budget * 4), TokensOut: budget})
	if prose.Degenerate != "" || prose.escalates(budget) || prose.Needed != (len(answer)+2)/3+64 || !strings.Contains(prose.observed(budget), "inside") {
		t.Errorf("prose: %+v escalates=%v observed=%q, want the code's own estimate and no escalation", prose, prose.escalates(budget), prose.observed(budget))
	}

	// Dense text (~1.4 bytes per token): len/3 under-counts, the measured density
	// says the answer needs more than the budget held.
	dense := judgeCut(answer, llamaclient.GenResult{Content: digitNoise(2000), TokensOut: budget})
	if dense.Degenerate != "" || !dense.escalates(budget) || dense.Needed <= budget || !strings.Contains(dense.observed(budget), "too small") {
		t.Errorf("dense: %+v escalates=%v observed=%q, want an escalation: the budget was too small", dense, dense.escalates(budget), dense.observed(budget))
	}
	// ...but never above the cap: an answer that needs more than the cap, cut AT
	// the cap, is a request that would be the first one again.
	big := answerOfChars(30000)
	atCap := judgeCut(big, llamaclient.GenResult{Content: digitNoise(agentRepackMaxTokensCap), TokensOut: agentRepackMaxTokensCap})
	if atCap.Needed <= agentRepackMaxTokensCap || atCap.escalates(agentRepackMaxTokensCap) || !strings.Contains(atCap.observed(agentRepackMaxTokensCap), "cap") {
		t.Errorf("at the cap: %+v escalates=%v observed=%q, want no escalation and the cap named", atCap, atCap.escalates(agentRepackMaxTokensCap), atCap.observed(agentRepackMaxTokensCap))
	}

	// A loop overrides everything: more tokens only lengthen it.
	loop := judgeCut(answer, llamaclient.GenResult{Content: digitNoise(2000) + strings.Repeat(" ", 300), TokensOut: budget})
	if loop.Degenerate == "" || loop.escalates(budget) || !strings.Contains(loop.observed(budget), "degenerate") {
		t.Errorf("loop: %+v escalates=%v observed=%q, want a degenerate verdict and no escalation", loop, loop.escalates(budget), loop.observed(budget))
	}

	// A short sample says nothing about the tokenizer: the code's estimate stands.
	short := judgeCut(answer, llamaclient.GenResult{Content: digitNoise(40), TokensOut: 40})
	if short.BytesPerToken != 0 || short.Needed != (len(answer)+2)/3+64 {
		t.Errorf("short sample: %+v, want no measured density and the code's own estimate", short)
	}
}

// periodicSuffix is how far back from the end a block repeats: the evidence the
// loop test counts.
func TestPeriodicSuffix(t *testing.T) {
	cases := []struct {
		s      string
		period int
		want   int
	}{
		{"hello", 1, 1},     // nothing repeats at the end: the block itself
		{"aaaa", 1, 4},      // all of it
		{"xaaaa", 1, 4},     // up to the first byte that differs
		{"abab", 2, 4},      // a whole number of copies
		{"xabcabcab", 3, 8}, // the last copy is cut mid-block
		{"abcd", 2, 2},      // two different pairs are no repeat
		{"abcabcabc", 3, 9}, // the whole string is the repeat
		{"abcabcabc", 1, 1}, // a different period sees none of it
		{"ab", 3, 2},        // shorter than one block
		{"", 1, 0},          // nothing at all
	}
	for _, tc := range cases {
		if got := periodicSuffix(tc.s, tc.period); got != tc.want {
			t.Errorf("periodicSuffix(%q, %d) = %d, want %d", tc.s, tc.period, got, tc.want)
		}
	}
}
