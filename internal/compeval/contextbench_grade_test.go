package compeval

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// frame builds a one-message snapshot frame holding text as an assistant message.
func cbFrame(kind string, texts ...string) CBFrame {
	f := CBFrame{Kind: kind}
	for _, s := range texts {
		f.Messages = append(f.Messages, agent.Msg{Role: "assistant", Content: s})
	}
	return f
}

func cbKVGold() *CBInstance {
	return &CBInstance{Family: CBFamilyKV, Gold: CBGold{KV: []CBKVQuery{
		{QID: 0, Key: "K00001", Value: "lantern ember #aaaa1111"},
		{QID: 1, Key: "K00002", Value: "orbit cedar #bbbb2222"},
	}}}
}

func TestCBGradeKVCreditsAnAnswerSeenInAnyFrame(t *testing.T) {
	in := cbKVGold()
	good := []CBFrame{
		cbFrame("turn", "<<<ANSWER key=K00001>>>\nlantern ember #aaaa1111\n<<<ANSWER END>>>"),
		// The second answer is given later and the first one is long gone from this frame.
		cbFrame("turn", "<<<ANSWER key=K00002>>>\n  orbit   cedar #bbbb2222 \n<<<ANSWER END>>>"),
	}
	g := GradeCB(in, good)
	if !g.Valid || g.Reward != 1 || g.Matched != 2 || g.Total != 2 {
		t.Fatalf("known-good kv grade = %+v", g)
	}

	bad := []CBFrame{
		cbFrame("turn", "<<<ANSWER key=K00001>>>\nlantern ember #wrong000\n<<<ANSWER END>>>",
			"<<<ANSWER key=K00002>>>\nI do not remember\n<<<ANSWER END>>>"),
	}
	g = GradeCB(in, bad)
	if !g.Valid || g.Reward != 0 || g.Matched != 0 || g.Sub["reward_attempted"] != 0 {
		t.Fatalf("known-bad kv grade = %+v", g)
	}

	half := []CBFrame{cbFrame("turn", "<<<ANSWER key=K00001>>>\nthe value is lantern ember #aaaa1111.\n<<<ANSWER END>>>")}
	g = GradeCB(in, half)
	if g.Reward != 0.5 || g.Sub["n_answered"] != 1 || g.Sub["reward_attempted"] != 1 {
		t.Fatalf("an answer embedded in a longer line must count once: %+v", g)
	}
}

func cbLogGold() *CBInstance {
	return &CBInstance{Family: CBFamilyLog, Gold: CBGold{Log: []CBLogQuery{
		{QID: 0, Type: "lookup", Answer: "timeout retry shard #deadbeef"},
		{QID: 1, Type: "count", Answer: "5"},
	}}}
}

func TestCBGradeLogLookupDropsTheTagAndCountNeedsExactlyOneNumber(t *testing.T) {
	in := cbLogGold()
	good := []CBFrame{cbFrame("turn",
		"<<<ANSWER qid=0>>>\nthe message was: timeout retry shard\n<<<ANSWER END>>>",
		"<<<ANSWER qid=1>>>\n5\n<<<ANSWER END>>>")}
	g := GradeCB(in, good)
	if !g.Valid || g.Reward != 1 || g.Sub["reward_lookup"] != 1 || g.Sub["reward_count"] != 1 {
		t.Fatalf("known-good log grade = %+v", g)
	}
	for _, ans := range []string{"5 or 6", "-5", "5.9", "no idea"} {
		bad := []CBFrame{cbFrame("turn", "<<<ANSWER qid=1>>>\n"+ans+"\n<<<ANSWER END>>>")}
		if got := GradeCB(in, bad); got.Sub["reward_count"] != 0 {
			t.Errorf("count answer %q must not be credited: %+v", ans, got)
		}
	}
	bad := []CBFrame{cbFrame("turn", "<<<ANSWER qid=0>>>\ntimeout retry\n<<<ANSWER END>>>")}
	if got := GradeCB(in, bad); got.Sub["reward_lookup"] != 0 {
		t.Errorf("a partial message must not be credited: %+v", got)
	}
}

func TestCBGradeNeedleReadsOnlyTheFinalFrame(t *testing.T) {
	in := &CBInstance{Family: CBFamilyNeedle, Gold: CBGold{NeedleLines: []string{
		"[n00000i00#aaaaaaaa] lantern ember.", "[n00000i01#bbbbbbbb] orbit cedar.",
	}}}
	kept := "[n00000i00#aaaaaaaa] lantern ember.\n[n00000i01#bbbbbbbb]  orbit   cedar.\nfiller"
	good := []CBFrame{cbFrame("turn", "noise"), cbFrame("turn", kept)}
	g := GradeCB(in, good)
	if !g.Valid || g.Reward != 1 || g.Matched != 2 {
		t.Fatalf("known-good needle grade = %+v", g)
	}
	// Present in the first frame, gone from the last: the paper metric is the final context.
	bad := []CBFrame{cbFrame("turn", kept), cbFrame("turn", "[n00000i00#aaaaaaaa] lantern ember.")}
	g = GradeCB(in, bad)
	if g.Reward != 0.5 || g.Matched != 1 {
		t.Fatalf("known-bad needle grade = %+v", g)
	}
	// A needle must be a whole line, not a fragment inside a longer one.
	frag := []CBFrame{cbFrame("turn", "see [n00000i00#aaaaaaaa] lantern ember. and more")}
	if got := GradeCB(in, frag); got.Matched != 0 {
		t.Fatalf("a fragment inside a longer line must not match: %+v", got)
	}
}

// cbPanel renders a sudoku panel for gold cells the way the agent is told to.
func cbPanel(side int, cells map[string]string, version int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<<<SKETCHPAD BEGIN>>>\nVERSION: %d\n", version)
	for r := 1; r <= side; r++ {
		for c := 1; c <= side; c++ {
			v := cells[fmt.Sprintf("r%dc%d", r, c)]
			sym := "."
			if strings.HasPrefix(v, "=") {
				var n int
				fmt.Sscanf(v, "=%d", &n)
				sym = string(CBSudokuSymbols[n-1])
			}
			fmt.Fprintf(&b, "(%d,%d,%s) ", r, c, sym)
		}
		b.WriteString("\n")
	}
	b.WriteString("<<<SKETCHPAD END>>>")
	return b.String()
}

func cbFold(in *CBInstance) (versions []int, boards []map[string]string) {
	cur := map[string]string{}
	for _, d := range in.Gold.Deltas {
		if d.Reset {
			cur = map[string]string{}
		}
		for k, v := range d.Cells {
			cur[k] = v
		}
		cp := map[string]string{}
		for k, v := range cur {
			cp[k] = v
		}
		versions = append(versions, d.V)
		boards = append(boards, cp)
	}
	return
}

func TestCBGradeSudokuScoresDisplayedVersions(t *testing.T) {
	in := GenerateSudoku(5, CBKnobs{Box: 2, Givens: 6, Boards: 1})
	if in.Gold.Side != 4 || len(in.Ops) != 10 {
		t.Fatalf("fixture shape: side %d ops %d", in.Gold.Side, len(in.Ops))
	}
	versions, boards := cbFold(in)

	var all []CBFrame
	for i := range versions {
		all = append(all, cbFrame("turn", cbPanel(4, boards[i], versions[i])))
	}
	g := GradeCB(in, all)
	if !g.Valid || g.Reward != 1 || g.Total != 10 || g.Sub["reward_versioned"] != 1 || g.Sub["reward_final_em"] != 1 {
		t.Fatalf("known-good sudoku grade = %+v", g)
	}

	// Skip every second version: half the versions were never displayed.
	var half []CBFrame
	for i := 0; i < len(versions); i += 2 {
		half = append(half, cbFrame("turn", cbPanel(4, boards[i], versions[i])))
	}
	g = GradeCB(in, half)
	if g.Reward != 0.5 {
		t.Fatalf("known-bad sudoku grade = %+v", g)
	}

	// The right grid under the wrong version label counts for content, not for the versioned score.
	mislabeled := []CBFrame{}
	for i := 1; i < len(versions); i++ {
		mislabeled = append(mislabeled, cbFrame("turn", cbPanel(4, boards[i], versions[i]+100)))
	}
	g = GradeCB(in, mislabeled)
	if g.Reward != 1 || g.Sub["reward_versioned"] != 0 {
		t.Fatalf("mislabeled versions: %+v", g)
	}
}

func TestCBGradeSudokuRefusesAGoldWithoutItsSide(t *testing.T) {
	in := GenerateSudoku(5, CBKnobs{Box: 2, Givens: 6, Boards: 1})
	in.Gold.Side = 0
	g := GradeCB(in, []CBFrame{cbFrame("turn", "x")})
	if g.Valid || g.Note == "" {
		t.Fatalf("a gold with no side must be an invalid grade, not a zero: %+v", g)
	}
}

func TestCBGradeWithNothingToReadIsInvalidNotZero(t *testing.T) {
	for _, in := range []*CBInstance{cbKVGold(), cbLogGold(),
		{Family: CBFamilyNeedle, Gold: CBGold{NeedleLines: []string{"x"}}},
		GenerateSudoku(5, CBKnobs{Box: 2, Givens: 6, Boards: 1})} {
		g := GradeCB(in, nil)
		if g.Valid {
			t.Errorf("%s: no frames must be an invalid grade: %+v", in.Family, g)
		}
	}
	empty := &CBInstance{Family: CBFamilyKV}
	if g := GradeCB(empty, []CBFrame{cbFrame("turn", "x")}); g.Valid {
		t.Errorf("a gold with no queries must be invalid: %+v", g)
	}
	if g := GradeCB(&CBInstance{Family: "nope"}, []CBFrame{cbFrame("turn", "x")}); g.Valid {
		t.Errorf("an unknown family must be invalid: %+v", g)
	}
}

// Only message content is read: tool-call arguments and roles do not count.
func TestCBGradeReadsMessageContentOnly(t *testing.T) {
	in := cbKVGold()
	f := CBFrame{Kind: "turn", Messages: []agent.Msg{{
		Role: "assistant",
		ToolCalls: []agent.ToolCall{{ID: "c1", Name: "x",
			Args: "<<<ANSWER key=K00001>>>lantern ember #aaaa1111<<<ANSWER END>>>"}},
	}}}
	if g := GradeCB(in, []CBFrame{f}); g.Matched != 0 {
		t.Fatalf("an answer inside tool-call arguments must not count: %+v", g)
	}
}
