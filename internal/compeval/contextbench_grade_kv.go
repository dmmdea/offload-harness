// contextbench_grade_kv.go - the key-value memory grader.
//
// Ported to Go from contextbench/graders/kv.py in
// https://github.com/RulinShao/ContextBench at commit
// fb533c18c78a82d3f1b2047a9c1700735cf829ae (Apache-2.0): the answer-block pattern and
// the rule that a query is credited when the gold value appears in any answer given
// for its key in any frame. The code and comments are written for this repository.
package compeval

import (
	"regexp"
	"strings"
)

// cbKVAnswerRE matches `<<<ANSWER key=K>>> value <<<ANSWER END>>>`.
var cbKVAnswerRE = regexp.MustCompile(`(?is)<<<\s*ANSWER\s+key\s*=\s*([^\s>]+)\s*>>>(.*?)<<<\s*ANSWER\s+END\s*>>>`)

func gradeKV(gold CBGold, frames []CBFrame) CBGrade {
	total := len(gold.KV)
	if total == 0 {
		return invalidGrade(len(frames), "gold has no queries")
	}
	if len(frames) == 0 {
		return invalidGrade(0, "no context snapshots")
	}
	answers := map[string]map[string]bool{}
	for _, f := range frames {
		for _, m := range cbKVAnswerRE.FindAllStringSubmatch(cbFrameText(f), -1) {
			key := cbNormalize(m[1])
			if answers[key] == nil {
				answers[key] = map[string]bool{}
			}
			answers[key][cbNormalize(m[2])] = true
		}
	}
	correct, answered := 0, 0
	for _, q := range gold.KV {
		seen := answers[cbNormalize(q.Key)]
		if len(seen) == 0 {
			continue
		}
		answered++
		want := cbNormalize(q.Value)
		for s := range seen {
			if strings.Contains(s, want) {
				correct++
				break
			}
		}
	}
	attempted := 0.0
	if answered > 0 {
		attempted = float64(correct) / float64(answered)
	}
	return CBGrade{
		Reward: float64(correct) / float64(total), Valid: true, Matched: correct, Total: total, Frames: len(frames),
		Sub: map[string]float64{"reward_attempted": attempted, "n_answered": float64(answered)},
	}
}
