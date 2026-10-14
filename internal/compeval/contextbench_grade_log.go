// contextbench_grade_log.go - the incident-log triage grader.
//
// Ported to Go from contextbench/graders/log.py in
// https://github.com/RulinShao/ContextBench at commit
// fb533c18c78a82d3f1b2047a9c1700735cf829ae (Apache-2.0): the answer-block pattern, the
// lookup rule (the message without its trailing uniqueness tag appears in an answer)
// and the count rule (the answer states exactly one number and it is the gold
// integer). The code and comments are written for this repository.
//
// The count rule reads the answer after two clean-ups the original does not make: a
// question label the model repeats ("Query 3:", "qid=3") is removed, and digit groups
// joined by thousands separators ("12,800") are read as one number. Otherwise the label's
// own number, or the groups, would be counted as a second number and a right answer
// would be refused.
package compeval

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	// cbLogAnswerRE matches `<<<ANSWER qid=N>>> answer <<<ANSWER END>>>`.
	cbLogAnswerRE = regexp.MustCompile(`(?is)<<<\s*ANSWER\s+qid\s*=\s*([0-9]+)\s*>>>(.*?)<<<\s*ANSWER\s+END\s*>>>`)
	// cbLogTagRE is the trailing `#hex` tag a gold message ends in; a model treats it as
	// a noise id, and the lookup measures retrieval, not copying that tag.
	cbLogTagRE = regexp.MustCompile(`\s*#[0-9a-f]+\s*$`)
	// cbLogNumberRE is a complete number: optional sign, digits, optional decimals.
	cbLogNumberRE = regexp.MustCompile(`[-+]?\d+(?:\.\d+)?`)
	// cbLogLabelRE is the label a model puts before its answer: "Query 3:", "qid=3", "QID #3 ->".
	cbLogLabelRE = regexp.MustCompile(`(?i)\b(?:qid|query)\s*[=#:]?\s*\d+\s*[:.)>=]*`)
	// cbLogGroupedRE is a number written with thousands separators: groups of exactly three
	// digits after the first group of one to three ("1,234", "12,800", "1_234").
	cbLogGroupedRE = regexp.MustCompile(`\b\d{1,3}(?:[,_]\d{3})+\b`)
)

// cbLogNumbers lists the numbers an answer states once its question label is removed and
// grouped digits are joined.
func cbLogNumbers(answer string) []string {
	answer = cbLogLabelRE.ReplaceAllString(answer, " ")
	answer = cbLogGroupedRE.ReplaceAllStringFunc(answer, func(g string) string {
		return strings.NewReplacer(",", "", "_", "").Replace(g)
	})
	return cbLogNumberRE.FindAllString(answer, -1)
}

func logCountCorrect(want string, answers map[string]bool) bool {
	for a := range answers {
		nums := cbLogNumbers(a)
		if len(nums) == 1 && nums[0] == want {
			return true
		}
	}
	return false
}

func logLookupCorrect(want string, answers map[string]bool) bool {
	core := cbNormalize(cbLogTagRE.ReplaceAllString(want, ""))
	if core == "" {
		return false
	}
	for a := range answers {
		if strings.Contains(a, core) {
			return true
		}
	}
	return false
}

func gradeLog(gold CBGold, frames []CBFrame) CBGrade {
	total := len(gold.Log)
	if total == 0 {
		return invalidGrade(len(frames), "gold has no queries")
	}
	if len(frames) == 0 {
		return invalidGrade(0, "no context snapshots")
	}
	answers := map[string]map[string]bool{}
	for _, f := range frames {
		for _, m := range cbLogAnswerRE.FindAllStringSubmatch(cbFrameText(f), -1) {
			id := cbNormalize(m[1])
			if answers[id] == nil {
				answers[id] = map[string]bool{}
			}
			answers[id][cbNormalize(m[2])] = true
		}
	}
	correct, answered := 0, 0
	byTotal := map[string]int{"lookup": 0, "count": 0}
	byCorrect := map[string]int{"lookup": 0, "count": 0}
	for _, q := range gold.Log {
		typ := q.Type
		if typ == "" {
			typ = "lookup"
		}
		byTotal[typ]++
		seen := answers[strconv.Itoa(q.QID)]
		if len(seen) == 0 {
			continue
		}
		answered++
		want := cbNormalize(q.Answer)
		ok := false
		if typ == "count" {
			ok = logCountCorrect(want, seen)
		} else {
			ok = logLookupCorrect(want, seen)
		}
		if ok {
			correct++
			byCorrect[typ]++
		}
	}
	rate := func(t string) float64 {
		if byTotal[t] == 0 {
			return 0
		}
		return float64(byCorrect[t]) / float64(byTotal[t])
	}
	attempted := 0.0
	if answered > 0 {
		attempted = float64(correct) / float64(answered)
	}
	return CBGrade{
		Reward: float64(correct) / float64(total), Valid: true, Matched: correct, Total: total, Frames: len(frames),
		Sub: map[string]float64{
			"reward_lookup": rate("lookup"), "reward_count": rate("count"),
			"reward_attempted": attempted, "n_answered": float64(answered),
		},
	}
}
