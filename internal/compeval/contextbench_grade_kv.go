// contextbench_grade_kv.go - the key-value memory grader.
//
// Ported to Go from contextbench/graders/kv.py in
// https://github.com/RulinShao/ContextBench at commit
// fb533c18c78a82d3f1b2047a9c1700735cf829ae (Apache-2.0): the answer-block pattern and
// the rule that a query is credited when the gold value appears in any answer given
// for its key in any frame. The code and comments are written for this repository.
//
// Two rules differ from the original on purpose. The key an answer names is compared
// without its surrounding quotes and without regard to case, because a model that writes
// key="K00001" still answered K00001. The value is credited only when it stands as a whole
// in the answer: the character after it and the one before it are not letters or digits,
// so a value ending in its eight-hex-digit tag does not match the same words followed by
// a longer tag, and does not begin in the middle of a longer word. The original credits
// any substring.
package compeval

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// cbKVAnswerRE matches `<<<ANSWER key=K>>> value <<<ANSWER END>>>`.
var cbKVAnswerRE = regexp.MustCompile(`(?is)<<<\s*ANSWER\s+key\s*=\s*([^\s>]+)\s*>>>(.*?)<<<\s*ANSWER\s+END\s*>>>`)

// cbKVKey is the form in which a key is compared: whitespace collapsed, the quotes or
// backticks a model puts around it trimmed, case folded.
func cbKVKey(s string) string {
	return strings.ToLower(strings.Trim(cbNormalize(s), "\"'`\u201c\u201d\u2018\u2019"))
}

// cbKVHasValue reports whether answer carries want as a whole: the character before it and
// the one after it (if any) are not letters or digits. Every occurrence is tried, so a
// near-miss earlier in the answer does not hide the right value later in it.
func cbKVHasValue(answer, want string) bool {
	for from := 0; from <= len(answer); {
		i := strings.Index(answer[from:], want)
		if i < 0 {
			return false
		}
		start := from + i
		end := start + len(want)
		before, _ := utf8.DecodeLastRuneInString(answer[:start])
		after, _ := utf8.DecodeRuneInString(answer[end:])
		wordBefore := start > 0 && (unicode.IsLetter(before) || unicode.IsDigit(before))
		wordAfter := end < len(answer) && (unicode.IsLetter(after) || unicode.IsDigit(after))
		if !wordBefore && !wordAfter {
			return true
		}
		from = start + 1
	}
	return false
}

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
			key := cbKVKey(m[1])
			if answers[key] == nil {
				answers[key] = map[string]bool{}
			}
			answers[key][cbNormalize(m[2])] = true
		}
	}
	correct, answered := 0, 0
	for _, q := range gold.KV {
		seen := answers[cbKVKey(q.Key)]
		if len(seen) == 0 {
			continue
		}
		answered++
		want := cbNormalize(q.Value)
		for s := range seen {
			if cbKVHasValue(s, want) {
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
