// contextbench_grade_base.go - the shared plumbing of the contextbench graders: the
// snapshot frame, the grade, whitespace normalisation and the text a grader scans.
//
// Ported to Go from contextbench/graders/base.py in
// https://github.com/RulinShao/ContextBench at commit
// fb533c18c78a82d3f1b2047a9c1700735cf829ae (Apache-2.0): the normalisation, the
// flattening of a message list into scannable text, and the rule that a grade that
// cannot be computed is reported as invalid, never as a zero. The code and comments
// are written for this repository; the readers of snapshot files, trajectories and
// closing reports are not ported because the adapter keeps its frames in memory.
//
// THE SNAPSHOT THE GRADE USES. A frame is the []agent.Msg the run recorded at one
// moment (CBFrame.Messages), and a grader reads exactly one field of it: the Content
// of every message, joined by newlines. Role, tool-call arguments and tool-call ids
// are not scanned. kv_store, log_triage and sudoku_sketchpad scan every frame; a
// reply is credited once it appears in any frame. needle_retention scans the LAST
// frame only: what the context still holds when the run ends.
package compeval

import (
	"strings"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// CBFrame is one snapshot of the agent's context. Kind is "sent" (the context handed
// to the seat after compaction) or "turn" (the context right after the seat replied,
// before any compaction). Messages is the whole transcript at that moment.
type CBFrame struct {
	Kind     string      `json:"kind"`
	Step     int         `json:"step"`
	Tokens   int         `json:"tokens"`
	Messages []agent.Msg `json:"-"`
}

// CBGrade is a family grader's result. Reward is the family's primary score in
// [0, 1]. Valid false means the grade could not be computed (nothing to read, or a
// gold the grader refuses); the reward is then meaningless and Note says why.
type CBGrade struct {
	Reward  float64            `json:"reward"`
	Valid   bool               `json:"grader_valid"`
	Note    string             `json:"grader_note,omitempty"`
	Matched int                `json:"matched"`
	Total   int                `json:"total"`
	Frames  int                `json:"frames"`
	Sub     map[string]float64 `json:"rewards,omitempty"`
}

func invalidGrade(frames int, why string) CBGrade {
	return CBGrade{Valid: false, Note: why, Frames: frames}
}

// cbNormalize collapses whitespace so a reward never turns on reflow or indentation.
func cbNormalize(s string) string { return strings.Join(strings.Fields(s), " ") }

// cbFrameText flattens a frame to the text a grader scans: the content of every
// message, one per line.
func cbFrameText(f CBFrame) string {
	parts := make([]string, 0, len(f.Messages))
	for _, m := range f.Messages {
		parts = append(parts, m.Content)
	}
	return strings.Join(parts, "\n")
}

// GradeCB scores a run's frames against an instance's gold with the family's grader.
func GradeCB(in *CBInstance, frames []CBFrame) CBGrade {
	switch in.Family {
	case CBFamilyKV:
		return gradeKV(in.Gold, frames)
	case CBFamilyLog:
		return gradeLog(in.Gold, frames)
	case CBFamilyNeedle:
		return gradeNeedle(in.Gold, frames)
	case CBFamilySudoku:
		return gradeSudoku(in.Gold, frames)
	}
	return invalidGrade(len(frames), "no grader for family "+in.Family)
}
