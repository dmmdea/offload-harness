package compeval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
)

const cbSevenWords = "pass task_fail cheat infra_error timeout judge_error not_run"

func cbSmallInstance(t *testing.T, family string) *CBInstance {
	t.Helper()
	switch family {
	case CBFamilyKV:
		return GenerateKV(42, CBKnobs{Records: 40, ValueWords: 8, Queries: 6, RecordsPerChunk: 10})
	case CBFamilyLog:
		return GenerateLog(42, CBKnobs{Lines: 60, LinesPerBatch: 15, Queries: 6})
	case CBFamilyNeedle:
		return GenerateNeedle(42, CBKnobs{NeedlesPerChunk: 2, Chunks: 4, FillerLines: 12, WordsPerLine: 6})
	case CBFamilySudoku:
		return GenerateSudoku(42, CBKnobs{Box: 3, Givens: 60, Boards: 1})
	}
	t.Fatalf("family %q", family)
	return nil
}

var (
	cbGetRE     = regexp.MustCompile(`GET (K\d+)\n`)
	cbQueryRE   = regexp.MustCompile(`QUERY (\d+):`)
	cbVersionRE = regexp.MustCompile(`(?:VERSION line to|becomes version) (\d+)`)
	cbNeedleRE  = regexp.MustCompile(`(?m)^\[n\d+i\d+#[0-9a-f]+\] .*$`)
)

// cbOracleSeat answers each operation from the gold it was generated with, and keeps
// the needles it has seen in every reply. It never looks at what the harness kept,
// so it proves the plumbing, not the ladder.
func cbOracleSeat(in *CBInstance) CBSeat {
	_, boards := cbFold(in)
	versions, _ := cbFold(in)
	seen := map[string]bool{}
	var needles []string
	return func(ctx context.Context, msgs []agent.Msg, maxOut int) (string, error) {
		last := msgs[len(msgs)-1].Content
		reply := ""
		switch in.Family {
		case CBFamilyKV:
			if m := cbGetRE.FindStringSubmatch(last); m != nil {
				for _, q := range in.Gold.KV {
					if q.Key == m[1] {
						reply = "<<<ANSWER key=" + q.Key + ">>>\n" + q.Value + "\n<<<ANSWER END>>>\n"
					}
				}
			}
		case CBFamilyLog:
			if m := cbQueryRE.FindStringSubmatch(last); m != nil {
				id, _ := strconv.Atoi(m[1])
				reply = "<<<ANSWER qid=" + m[1] + ">>>\n" + in.Gold.Log[id].Answer + "\n<<<ANSWER END>>>\n"
			}
		case CBFamilyNeedle:
			for _, l := range cbNeedleRE.FindAllString(last, -1) {
				if !seen[l] {
					seen[l] = true
					needles = append(needles, l)
				}
			}
			reply = strings.Join(needles, "\n") + "\n"
		case CBFamilySudoku:
			if m := cbVersionRE.FindStringSubmatch(last); m != nil {
				v, _ := strconv.Atoi(m[1])
				for i, ver := range versions {
					if ver == v {
						reply = cbTestPanelText(in.Gold.Side, boards[i], v) + "\n"
					}
				}
			}
		}
		return reply + CBAdvanceMarker, nil
	}
}

func cbRunOpts() CBRunOpts {
	return CBRunOpts{Budget: 200000, Reserve: 2000, MaxOut: 512}
}

func TestRunCBOracleSeatPassesEveryFamilyUnderBothDeliveries(t *testing.T) {
	for _, delivery := range []string{CBDeliveryToolResult, CBDeliveryUser} {
		for _, family := range CBFamilies {
			in := cbSmallInstance(t, family)
			o := cbRunOpts()
			o.Delivery = delivery
			r := RunCBTask(context.Background(), in, cbOracleSeat(in), o)
			if r.Outcome != CBOutcomePass || r.Reward == nil || *r.Reward != 1 {
				t.Errorf("%s/%s: outcome %s reward %v detail %q", delivery, family, r.Outcome, r.Reward, r.Detail)
				continue
			}
			if r.OpsDelivered != len(in.Ops) || r.TerminatedBy != CBTermStreamExhausted {
				t.Errorf("%s/%s: delivered %d/%d terminated %s", delivery, family, r.OpsDelivered, len(in.Ops), r.TerminatedBy)
			}
		}
	}
}

func TestRunCBNoSeatReachableIsInfraErrorNeverTaskFail(t *testing.T) {
	down := func(ctx context.Context, msgs []agent.Msg, maxOut int) (string, error) {
		return "", errors.New("dial tcp: connection refused")
	}
	var instances []*CBInstance
	for _, f := range CBFamilies {
		instances = append(instances, cbSmallInstance(t, f))
	}
	rep := RunCB(context.Background(), instances, down, cbRunOpts(), "seat-x")
	for _, r := range rep.Results {
		if r.Outcome != CBOutcomeInfraError {
			t.Errorf("%s: outcome %s, want infra_error", r.ID, r.Outcome)
		}
		if r.Reward != nil {
			t.Errorf("%s: an infra_error carries no reward, got %v", r.ID, *r.Reward)
		}
		if !strings.Contains(r.Detail, "connection refused") {
			t.Errorf("%s: detail %q", r.ID, r.Detail)
		}
	}
	if rep.Counts[CBOutcomeInfraError] != len(instances) || rep.Counts[CBOutcomeTaskFail] != 0 {
		t.Fatalf("counts = %v", rep.Counts)
	}
}

func TestRunCBSeatDyingMidRunIsInfraErrorNotAGrade(t *testing.T) {
	in := cbSmallInstance(t, CBFamilyKV)
	calls := 0
	oracle := cbOracleSeat(in)
	seat := func(ctx context.Context, msgs []agent.Msg, maxOut int) (string, error) {
		calls++
		if calls > 3 {
			return "", errors.New("EOF")
		}
		return oracle(ctx, msgs, maxOut)
	}
	r := RunCBTask(context.Background(), in, seat, cbRunOpts())
	if r.Outcome != CBOutcomeInfraError || r.Reward != nil || r.Turns != 3 {
		t.Fatalf("%+v", r)
	}
}

func TestRunCBTurnCapIsTimeout(t *testing.T) {
	in := cbSmallInstance(t, CBFamilyKV)
	silent := func(ctx context.Context, msgs []agent.Msg, maxOut int) (string, error) { return "thinking", nil }
	o := cbRunOpts()
	o.MaxTurns = 5
	r := RunCBTask(context.Background(), in, silent, o)
	if r.Outcome != CBOutcomeTimeout || r.TerminatedBy != CBTermMaxTurns || r.Turns != 5 || r.OpsDelivered != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestRunCBGradeThatCannotBeComputedIsJudgeError(t *testing.T) {
	in := cbSmallInstance(t, CBFamilySudoku)
	in.Gold.Side = 0
	r := RunCBTask(context.Background(), in, cbOracleSeat(in), cbRunOpts())
	if r.Outcome != CBOutcomeJudgeError || r.Reward != nil || r.Detail == "" {
		t.Fatalf("%+v", r)
	}
}

func TestRunCBWrongAnswersAreTaskFailWithTheRewardKept(t *testing.T) {
	in := cbSmallInstance(t, CBFamilyKV)
	empty := func(ctx context.Context, msgs []agent.Msg, maxOut int) (string, error) { return CBAdvanceMarker, nil }
	r := RunCBTask(context.Background(), in, empty, cbRunOpts())
	if r.Outcome != CBOutcomeTaskFail || r.Reward == nil || *r.Reward != 0 || r.Total != 6 {
		t.Fatalf("%+v", r)
	}
	// One of six right is still a task_fail, with its fraction.
	oracle := cbOracleSeat(in)
	n := 0
	partial := func(ctx context.Context, msgs []agent.Msg, maxOut int) (string, error) {
		rep, err := oracle(ctx, msgs, maxOut)
		if strings.Contains(rep, "<<<ANSWER") {
			n++
			if n > 1 {
				return CBAdvanceMarker, nil
			}
		}
		return rep, err
	}
	r = RunCBTask(context.Background(), in, partial, cbRunOpts())
	if r.Outcome != CBOutcomeTaskFail || *r.Reward != 1.0/6 {
		t.Fatalf("%+v reward %v", r, r.Reward)
	}
	o := cbRunOpts()
	o.PassAt = 0.1
	if r = RunCBTask(context.Background(), in, partial2(oracle), o); r.Outcome != CBOutcomePass {
		t.Fatalf("with --pass-at 0.1 one of six should pass: %+v", r)
	}
}

func partial2(oracle CBSeat) CBSeat {
	n := 0
	return func(ctx context.Context, msgs []agent.Msg, maxOut int) (string, error) {
		rep, err := oracle(ctx, msgs, maxOut)
		if strings.Contains(rep, "<<<ANSWER") {
			n++
			if n > 1 {
				return CBAdvanceMarker, nil
			}
		}
		return rep, err
	}
}

func TestRunCBFirstOperationOverTheBudgetIsABudgetWall(t *testing.T) {
	in := cbSmallInstance(t, CBFamilyKV)
	o := cbRunOpts()
	o.Budget, o.Reserve = 300, 100
	called := false
	seat := func(ctx context.Context, msgs []agent.Msg, maxOut int) (string, error) { called = true; return "", nil }
	r := RunCBTask(context.Background(), in, seat, o)
	if called || r.TerminatedBy != CBTermBudgetWall || r.Outcome != CBOutcomeTaskFail {
		t.Fatalf("called=%v %+v", called, r)
	}
}

// valuesInContext answers a GET only from a SET line the harness still shows it.
func cbHonestKVSeat(in *CBInstance) CBSeat {
	return func(ctx context.Context, msgs []agent.Msg, maxOut int) (string, error) {
		last := msgs[len(msgs)-1].Content
		m := cbGetRE.FindStringSubmatch(last)
		if m == nil {
			return CBAdvanceMarker, nil
		}
		for _, msg := range msgs[:len(msgs)-1] {
			for _, line := range strings.Split(msg.Content, "\n") {
				if strings.HasPrefix(line, "SET "+m[1]+" = ") {
					v := strings.TrimPrefix(line, "SET "+m[1]+" = ")
					return "<<<ANSWER key=" + m[1] + ">>>\n" + v + "\n<<<ANSWER END>>>\n" + CBAdvanceMarker, nil
				}
			}
		}
		return "<<<ANSWER key=" + m[1] + ">>>\nunknown\n<<<ANSWER END>>>\n" + CBAdvanceMarker, nil
	}
}

func TestRunCBTheLadderActsOnToolResultsAndTheGradeShowsWhatItCost(t *testing.T) {
	in := GenerateKV(42, CBKnobs{Records: 60, ValueWords: 8, Queries: 6, RecordsPerChunk: 10})

	roomy := cbRunOpts()
	r := RunCBTask(context.Background(), in, cbHonestKVSeat(in), roomy)
	if r.Outcome != CBOutcomePass || r.Compactions != 0 {
		t.Fatalf("with room to spare nothing is compacted and the honest seat passes: %+v", r)
	}

	tight := cbRunOpts()
	tight.Budget, tight.Reserve = 1500, 200
	r = RunCBTask(context.Background(), in, cbHonestKVSeat(in), tight)
	if r.Compactions == 0 || r.Pager == nil || r.Pager.Evictions == 0 {
		t.Fatalf("a tight budget must engage the ladder and the pager record: %+v", r)
	}
	if r.PeakSentTokens > tight.Budget-tight.Reserve {
		t.Fatalf("a context sent to the seat was over the usable budget: %d > %d", r.PeakSentTokens, tight.Budget-tight.Reserve)
	}
	if r.Outcome != CBOutcomeTaskFail || r.Reward == nil || *r.Reward >= 1 {
		t.Fatalf("an honest seat that lost the values must fail the task, with a reward below 1: %+v", r)
	}
}

func TestRunCBUserDeliveryReachesOnlyTheDropRung(t *testing.T) {
	in := GenerateKV(42, CBKnobs{Records: 60, ValueWords: 8, Queries: 6, RecordsPerChunk: 10})
	o := cbRunOpts()
	o.Budget, o.Reserve = 1500, 200
	o.Delivery = CBDeliveryUser
	r := RunCBTask(context.Background(), in, cbHonestKVSeat(in), o)
	if r.Compactions == 0 {
		t.Fatalf("user turns older than the recent window are dropped whole once the budget is exceeded: %+v", r)
	}
	// Nothing is a tool result in this mode, so the pager (which tracks tool payloads) sees nothing.
	if r.Pager != nil && r.Pager.Evictions != 0 {
		t.Fatalf("user-turn delivery should leave the tool-payload pager empty: %+v", r.Pager)
	}
}

func TestRunCBReportUsesOnlyTheSevenOutcomeWordsAndCarriesNoClock(t *testing.T) {
	var instances []*CBInstance
	for _, f := range CBFamilies {
		instances = append(instances, cbSmallInstance(t, f))
	}
	n := 0
	seat := func(ctx context.Context, msgs []agent.Msg, maxOut int) (string, error) {
		n++
		switch {
		case n%3 == 0:
			return "", errors.New("boom")
		case n%3 == 1:
			return CBAdvanceMarker, nil
		}
		return "no marker here", nil
	}
	o := cbRunOpts()
	o.MaxTurns = 6
	rep := RunCB(context.Background(), instances, seat, o, "seat-x")
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Results []struct {
			Outcome string `json:"outcome"`
		} `json:"results"`
		Counts map[string]int `json:"outcome_counts"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	words := strings.Fields(cbSevenWords)
	for _, r := range parsed.Results {
		if !containsWord(words, r.Outcome) {
			t.Errorf("outcome %q is not one of the seven words", r.Outcome)
		}
	}
	if len(parsed.Counts) != 7 {
		t.Errorf("outcome_counts has %d keys, want all seven: %v", len(parsed.Counts), parsed.Counts)
	}
	total := 0
	for k, v := range parsed.Counts {
		if !containsWord(words, k) {
			t.Errorf("count key %q", k)
		}
		total += v
	}
	if total != len(instances) {
		t.Errorf("counts sum to %d, want %d", total, len(instances))
	}
	re := regexp.MustCompile(`(?i)"[a-z_]*(time|date|duration|wall|latency|seconds|_ms)[a-z_]*"\s*:`)
	for _, m := range re.FindAllString(string(raw), -1) {
		if m != `"timeout":` { // the outcome word, not a clock
			t.Errorf("the report carries a clock-shaped field: %s", m)
		}
	}
}

func containsWord(ws []string, w string) bool {
	for _, x := range ws {
		if x == w {
			return true
		}
	}
	return false
}

func TestRunCBCancelledRunLeavesTheRestNotRun(t *testing.T) {
	var instances []*CBInstance
	for _, f := range CBFamilies {
		instances = append(instances, cbSmallInstance(t, f))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := 0
	seat := func(c context.Context, msgs []agent.Msg, maxOut int) (string, error) {
		done++
		if done == len(instances[0].Ops)+1 {
			cancel()
		}
		return CBAdvanceMarker, nil
	}
	rep := RunCB(ctx, instances, seat, cbRunOpts(), "seat-x")
	if rep.Results[0].Outcome == CBOutcomeNotRun {
		t.Fatal("the first task ran")
	}
	if last := rep.Results[len(rep.Results)-1]; last.Outcome != CBOutcomeNotRun {
		t.Fatalf("a task the cancelled run never reached is not_run, got %s", last.Outcome)
	}
}

func TestRunCBReportIsDeterministic(t *testing.T) {
	run := func() []byte {
		in := cbSmallInstance(t, CBFamilyNeedle)
		rep := RunCB(context.Background(), []*CBInstance{in}, cbOracleSeat(in), cbRunOpts(), "seat-x")
		b, _ := json.Marshal(rep)
		return b
	}
	if a, b := run(), run(); string(a) != string(b) {
		t.Fatalf("two identical runs differ:\n%s\n%s", a, b)
	}
}

func TestCBMarkerRule(t *testing.T) {
	for reply, want := range map[string]bool{
		"done\nREADY_FOR_NEXT_OP":              true,
		"READY_FOR_NEXT_OP":                    true,
		"  READY_FOR_NEXT_OP  ":                true,
		"`READY_FOR_NEXT_OP`":                  true,
		"```\nREADY_FOR_NEXT_OP\n```":          true,
		"READY_FOR_NEXT_OP.":                   true,
		"I will say READY_FOR_NEXT_OP later":   false,
		"print(\"READY_FOR_NEXT_OP\")":         false,
		"<<<ANSWER qid=0>>>READY_FOR_NEXT_OP":  false,
		"":                                     false,
		"READY_FOR_NEXT_OP and then some more": false,
	} {
		if got := cbIsReady(reply); got != want {
			t.Errorf("cbIsReady(%q) = %v, want %v", reply, got, want)
		}
	}
	if !cbIsSubmit("x\n"+CBSubmitMarker) || cbIsSubmit("x "+CBSubmitMarker) {
		t.Error("submit marker rule")
	}
}

// The instruction is in the context the graders scan; it must not hand out credit.
func TestCBInstructionNeverLooksLikeAnAnswer(t *testing.T) {
	for _, delivery := range []string{CBDeliveryToolResult, CBDeliveryUser} {
		for _, family := range CBFamilies {
			in := cbSmallInstance(t, family)
			text := CBInstruction(in, delivery)
			g := GradeCB(in, []CBFrame{{Kind: "sent", Messages: []agent.Msg{{Role: "system", Content: text}}}})
			if g.Matched != 0 {
				t.Errorf("%s/%s: the bare instruction already scores %d: %+v", delivery, family, g.Matched, g)
			}
			if !strings.Contains(text, CBAdvanceMarker) {
				t.Errorf("%s/%s: the instruction must name the readiness marker", delivery, family)
			}
		}
	}
}

func TestBuildCBTasks(t *testing.T) {
	ins, err := BuildCBTasks("all", "", 42, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 8 {
		t.Fatalf("all families x 2 tasks = %d, want 8", len(ins))
	}
	var ids []string
	for _, in := range ins {
		ids = append(ids, in.ID)
	}
	want := "kv_store-R1000-seed42 kv_store-R1000-seed43 log_triage-L1600-seed42"
	if got := strings.Join(ids[:3], " "); got != want {
		t.Fatalf("ids = %q", got)
	}
	if _, err := BuildCBTasks("all", "R130", 42, 1); err == nil {
		t.Fatal("--level with all families is ambiguous and must be refused")
	}
	if _, err := BuildCBTasks(CBFamilyKV, "L160", 42, 1); err == nil {
		t.Fatal("a level of another family must be refused")
	}
	if _, err := BuildCBTasks(CBFamilyKV, "", 42, 0); err == nil {
		t.Fatal("zero tasks must be refused")
	}
	one, err := BuildCBTasks(CBFamilySudoku, "t4", 7, 1)
	if err != nil || len(one) != 1 || one[0].ID != "sudoku_sketchpad-t4-seed7" {
		t.Fatalf("%v %v", one, err)
	}
	_ = fmt.Sprint
}
