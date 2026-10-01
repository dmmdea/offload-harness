package pipeline

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// What the tail of a cut completion is: whitespace, a loop, or content.
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
		{"one byte repeated", proseNoise(300) + strings.Repeat("!", 30), "repeated"},
		{"a short block repeated, on one line", proseNoise(300) + strings.Repeat(`"", `, 40), "repeating"},
		{"a block of lines repeated", proseNoise(200) + "\n" + strings.Repeat(line, 6), "line block"},
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
