// contextbench_grade_needle.go - the needle-retention grader.
//
// Ported to Go from contextbench/graders/needle.py in
// https://github.com/RulinShao/ContextBench at commit
// fb533c18c78a82d3f1b2047a9c1700735cf829ae (Apache-2.0): the rule that the reward is
// the fraction of gold needle lines present, as whole normalised lines, in the final
// context. The code and comments are written for this repository. The upstream
// fallbacks to a trajectory file and to a closing report block are not ported: the
// adapter always has the in-memory frames.
package compeval

import "strings"

func gradeNeedle(gold CBGold, frames []CBFrame) CBGrade {
	want := map[string]bool{}
	for _, l := range gold.NeedleLines {
		want[cbNormalize(l)] = true
	}
	if len(want) == 0 {
		return invalidGrade(len(frames), "gold has no answer_lines")
	}
	if len(frames) == 0 {
		return invalidGrade(0, "no context snapshots")
	}
	present := map[string]bool{}
	for _, line := range strings.Split(cbFrameText(frames[len(frames)-1]), "\n") {
		if strings.TrimSpace(line) != "" {
			present[cbNormalize(line)] = true
		}
	}
	matched := 0
	for l := range want {
		if present[l] {
			matched++
		}
	}
	return CBGrade{Reward: float64(matched) / float64(len(want)), Valid: true, Matched: matched, Total: len(want), Frames: len(frames)}
}
