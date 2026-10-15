package mcpserver

import (
	"slices"
	"strings"
	"testing"
)

// resultReadingPins are sentences the compact list used to drop as "result reading" although each one stops a caller from misreading
// a result it has already received, and a caller that never saw the sentence has nothing to go on: a result that is partial is still a
// success, an unverified answer is not a wrong one, a repeated call is not a second attempt, a word in one field is not the answer to
// where the work ran, a word-level time that cannot be cut on, a defer that blames the contract and not a box, a deadline whose
// unfinished subtasks are to be re-issued. Each pin is a phrase of the sentence, looked up in the FULL description first (it must be in
// exactly one sentence, so a rewording fails loudly), and the whole sentence that holds it must then be in the compact description,
// verbatim.
var resultReadingPins = []struct{ tool, phrase string }{
	{"offload_transcribe", "so do not cut audio at word boundaries from those times"},
	{"offload_research", "Read summary FIRST; a PARTIAL result (some pages digested, some failed) is a successful call"},
	{"agent_delegate", `read it, not the word "local" in results[].placement`},
	{"offload_ask", "it is a prompt to look, not proof the answer is wrong"},
	{"offload_ask", "A REPEAT DOES NOT RE-ROLL THE SEAT"},
	{"agent_delegate", `defer_class "contract" is YOUR contract, not a box`},
	{"agent_delegate", "re-issue those subtasks in a smaller call"},
}

// TestCompactKeepsTheResultReadingSentencesThatStopAMisreading: see resultReadingPins.
func TestCompactKeepsTheResultReadingSentencesThatStopAMisreading(t *testing.T) {
	full, compact := map[string]map[string]string{}, map[string]map[string]string{}
	for _, tool := range listTools(t, fullSurfaceCfg("full")) {
		full[tool.Name] = descriptionsByPath(tool)
	}
	for _, tool := range listTools(t, fullSurfaceCfg("compact")) {
		compact[tool.Name] = descriptionsByPath(tool)
	}
	for _, p := range resultReadingPins {
		sentences := 0
		for path, desc := range full[p.tool] {
			for _, s := range splitSentences(desc) {
				if !strings.Contains(s, p.phrase) {
					continue
				}
				sentences++
				if !slices.Contains(splitSentences(compact[p.tool][path]), s) {
					t.Errorf("%s %s: compact dropped the sentence that holds %q:\n %s", p.tool, path, p.phrase, s)
				}
			}
		}
		if sentences != 1 {
			t.Errorf("%s: the phrase %q is in %d sentences of the full description, want exactly 1 (reworded?)", p.tool, p.phrase, sentences)
		}
	}
}
