// contextbench_gen_needle.go - the needle-retention task generator.
//
// Ported to Go from contextbench/tasks/needle_retention/generate.py in
// https://github.com/RulinShao/ContextBench at commit
// fb533c18c78a82d3f1b2047a9c1700735cf829ae (Apache-2.0). The word table, the line and
// sentinel construction and the order of random draws follow the original so the same
// seed streams the same operations. The operation mix is ported: the texts of the
// streamed operations (sentinel lines, headers, the needle and filler wording) are the
// original's, byte for byte, because the stream digest has to match. The code and
// comments are written for this repository and none of the upstream's documentation or
// instruction prose is carried over.
//
// The task: each streamed chunk carries a few needle lines to keep verbatim and a
// large filler block to let go. The grade is the share of needle lines still present
// in the agent's final context.
package compeval

import (
	"fmt"
	"strings"
)

var cbNeedleWords = []string{
	"lantern", "crimson", "zebra", "orbit", "meadow", "cobalt", "thistle",
	"ember", "quartz", "willow", "harbor", "nimbus", "cedar", "pewter",
	"marble", "falcon", "indigo", "saffron", "boulder", "trellis", "glacier",
	"verdant", "copper", "lattice", "auburn", "spruce", "onyx", "cinder",
}

func cbNeedleLine(rng *pyRand, tag string, nWords int) string {
	if nWords < 1 {
		nWords = 1
	}
	ws := make([]string, nWords)
	for i := range ws {
		ws[i] = cbNeedleWords[rng.choiceIndex(len(cbNeedleWords))]
	}
	return "[" + tag + "] " + strings.Join(ws, " ") + "."
}

// GenerateNeedle builds one needle_retention instance.
func GenerateNeedle(seed int64, k CBKnobs) *CBInstance {
	rng := newPyRand(uint64(seed))
	var ops []CBOp
	var answers, source []string
	for c := 0; c < k.Chunks; c++ {
		var needles, filler []string
		for n := 0; n < k.NeedlesPerChunk; n++ {
			tag := fmt.Sprintf("n%05di%02d#%s", c, n, pyHashTag(seed, "ndl", c, n))
			line := cbNeedleLine(rng, tag, k.WordsPerLine)
			needles = append(needles, line)
			answers = append(answers, line)
		}
		for f := 0; f < k.FillerLines; f++ {
			tag := fmt.Sprintf("f%05dx%03d#%s", c, f, pyHashTag(seed, "f", c, f))
			filler = append(filler, cbNeedleLine(rng, tag, k.WordsPerLine))
		}
		source = append(source, needles...)
		source = append(source, filler...)
		id := fmt.Sprintf("%05d#%s", c, pyHashTag(seed, "fb", c))
		start := fmt.Sprintf("<<<FILLER-BLOCK %s START -- delete this entire block (through its END line) from your context this turn>>>", id)
		end := fmt.Sprintf("<<<FILLER-BLOCK %s END>>>", id)
		body := "NEEDLES (keep these lines verbatim in your context):\n" + strings.Join(needles, "\n") + "\n" +
			start + "\n" + strings.Join(filler, "\n") + "\n" + end
		ops = append(ops, CBOp{Index: c, Header: fmt.Sprintf("chunk %d/%d", c+1, k.Chunks), Body: body})
	}
	keptTok, inputTok := approxTokens4(answers), approxTokens4(source)
	alpha := 0.0
	if inputTok > 0 {
		alpha = float64(keptTok) / float64(inputTok)
	}
	perChunk := inputTok
	if k.Chunks > 0 {
		perChunk = inputTok / k.Chunks
	}
	return &CBInstance{
		Family: CBFamilyNeedle,
		Seed:   seed,
		Ops:    ops,
		Gold:   CBGold{NeedleLines: answers},
		Meta: map[string]any{
			"task_family": CBFamilyNeedle, "seed": seed, "chunks": k.Chunks,
			"needles_per_chunk": k.NeedlesPerChunk, "filler_lines_per_chunk": k.FillerLines,
			"words_per_line": k.WordsPerLine, "n_answer_lines": len(answers), "n_source_lines": len(source),
			"alpha_nominal": alpha, "approx_input_tokens": inputTok, "approx_kept_tokens": keptTok,
			"approx_chunk_tokens": perChunk,
		},
	}
}
