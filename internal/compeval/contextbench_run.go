// contextbench_run.go - the contextbench driver: it feeds one generated task's
// operations to a seat, keeps the conversation inside a token budget with the
// production compaction ladder, records snapshots of the context, and grades them.
//
// This file is harness code, not a port. The shape of a run:
//
//   - The transcript starts with the instruction (system) and a one-line kickoff.
//   - Each operation is delivered either as the result of a synthetic `next_op` tool
//     call ("tool_result", the default) or as a user message ("user"). The ladder
//     shrinks tool-result bodies (dedupe, skeleton, elision) before it drops whole
//     turns, so tool_result is the delivery under which every rung can act; user
//     delivery reaches only the last rung, which drops older bare messages whole. The
//     upstream benchmark delivers operations as user messages, so "user" is the
//     closer reproduction and "tool_result" is the one that exercises the ladder.
//   - Before every send the transcript goes through agent.CompactReplay at the usable
//     budget, exactly the call the production loop makes, and the production loop's
//     habit of keeping the compacted transcript (the dropped content stays dropped).
//   - Two snapshots per step: "sent" (what the seat was handed) and "turn" (the
//     transcript right after the seat's reply, before the next compaction). The last
//     "turn" snapshot is the retained context the needle grader reads.
//   - A run ends when the last operation has been answered (stream_exhausted), the
//     agent submits after the stream is drained (submit), the context cannot be made
//     to fit (budget_wall), or the turn cap is reached (max_turns).
//
// The outcome word of a task is decided here and nowhere else; see classify.
package compeval

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// The two ways an operation reaches the seat.
const (
	CBDeliveryToolResult = "tool_result"
	CBDeliveryUser       = "user"
)

// Why a run ended.
const (
	CBTermStreamExhausted = "stream_exhausted"
	CBTermSubmit          = "submit"
	CBTermBudgetWall      = "budget_wall"
	CBTermMaxTurns        = "max_turns"
	CBTermSeatError       = "seat_error"
	CBTermCancelled       = "cancelled"
)

// The seven outcome words of the measurement gate, in the gate's fixed order.
const (
	CBOutcomePass       = "pass"
	CBOutcomeTaskFail   = "task_fail"
	CBOutcomeCheat      = "cheat"
	CBOutcomeInfraError = "infra_error"
	CBOutcomeTimeout    = "timeout"
	CBOutcomeJudgeError = "judge_error"
	CBOutcomeNotRun     = "not_run"
)

// CBOutcomes lists the seven words in order.
var CBOutcomes = []string{
	CBOutcomePass, CBOutcomeTaskFail, CBOutcomeCheat, CBOutcomeInfraError,
	CBOutcomeTimeout, CBOutcomeJudgeError, CBOutcomeNotRun,
}

// CBSeat is the model seam: one chat completion over the transcript, returning the
// reply text. The CLI wires the production client; tests inject a fake. An error
// means the seat could not answer and ends the task as infra_error.
type CBSeat func(ctx context.Context, msgs []agent.Msg, maxOut int) (string, error)

// CBRunOpts configures a run. Zero values take the defaults noted per field.
type CBRunOpts struct {
	// Budget and Reserve are tokens, counted with the ladder's own estimator. The
	// ladder fits the transcript to Budget-Reserve before every send. Default: the
	// published 32768 and 2048.
	Budget, Reserve int
	// MaxOut caps the reply, in tokens. Default 4096 (a sudoku panel is about a
	// thousand tokens).
	MaxOut int
	// MaxTurns caps seat replies per task, nudges included. Default 3*ops+10. A task
	// that reaches it ends as timeout: it hit its step budget, which says nothing about
	// the agent's score, so it is not graded.
	MaxTurns int
	// KeepRecent is the ladder's keep-recent window in messages. Default
	// agent.DefaultKeepRecent, the production value.
	KeepRecent int
	// Ladder selects the optional rungs.
	Ladder LadderOpts
	// Delivery is CBDeliveryToolResult (default) or CBDeliveryUser.
	Delivery string
	// PassAt is the reward at or above which a graded task is `pass`. Default 1.0: every
	// item right. The reward itself is always reported beside the word.
	PassAt float64
}

func (o CBRunOpts) withDefaults(nOps int) CBRunOpts {
	if o.Budget <= 0 {
		o.Budget = CBDefaultBudget
	}
	if o.Reserve <= 0 {
		o.Reserve = CBDefaultReserve
	}
	if o.MaxOut <= 0 {
		o.MaxOut = 4096
	}
	if o.MaxTurns <= 0 {
		o.MaxTurns = 3*nOps + 10
	}
	if o.KeepRecent <= 0 {
		o.KeepRecent = agent.DefaultKeepRecent
	}
	if o.Delivery == "" {
		o.Delivery = CBDeliveryToolResult
	}
	if o.PassAt <= 0 {
		o.PassAt = 1.0
	}
	return o
}

// CBTaskResult is one task's record. Reward is nil unless the task was graded: a
// task that never reached a verdict has no score, and a zero would read as one.
type CBTaskResult struct {
	ID             string             `json:"id"`
	Family         string             `json:"family"`
	Level          string             `json:"level"`
	Seed           int64              `json:"seed"`
	Outcome        string             `json:"outcome"`
	Reward         *float64           `json:"reward"`
	Matched        int                `json:"matched"`
	Total          int                `json:"total"`
	Rewards        map[string]float64 `json:"rewards,omitempty"`
	TerminatedBy   string             `json:"terminated_by"`
	Turns          int                `json:"turns"`
	OpsDelivered   int                `json:"ops_delivered"`
	OpsTotal       int                `json:"ops_total"`
	Compactions    int                `json:"compactions"`
	PeakSentTokens int                `json:"peak_sent_tokens"`
	Pager          *agent.PagerReport `json:"pager,omitempty"`
	Detail         string             `json:"detail,omitempty"`
}

// CBReport is the artifact of a contextbench run. It carries no timestamps: two runs
// of the same configuration against a deterministic seat are byte-identical.
type CBReport struct {
	Schema    string         `json:"schema"`
	Seat      string         `json:"seat"`
	Budget    int            `json:"budget_tokens"`
	Reserve   int            `json:"reserve_tokens"`
	Estimator string         `json:"size_estimator"`
	Delivery  string         `json:"delivery"`
	Ladder    string         `json:"ladder"`
	PassAt    float64        `json:"pass_at"`
	Results   []CBTaskResult `json:"results"`
	Counts    map[string]int `json:"outcome_counts"`
	// MeanReward is the mean reward over graded tasks, per family; absent for a family
	// with none. Read it beside the counts: it says nothing about tasks that did not
	// reach a verdict.
	MeanReward map[string]float64 `json:"mean_reward_by_family,omitempty"`
}

// CBPagerNotExercised is the pager basis of every contextbench task: evictions are
// recorded, a re-fetch rate is not (see RunCBTask).
const CBPagerNotExercised = "not_exercised"

// CBSchema stamps the report format.
const CBSchema = "contextbench/1"

// CBEstimator names the size estimator: the ladder's own, not the original's tokenizer.
const CBEstimator = "ladder_estimate_chars_div_4"

// BuildCBTasks generates n instances per selected family, with seeds seed..seed+n-1.
// family is one family name or "all"; level (optional) names a published level and is
// only valid with a single family.
func BuildCBTasks(family, level string, seed int64, n int) ([]*CBInstance, error) {
	if n <= 0 {
		return nil, fmt.Errorf("contextbench: --tasks must be at least 1, got %d", n)
	}
	families := []string{family}
	if family == "" || family == "all" {
		if level != "" {
			return nil, fmt.Errorf("contextbench: --level %q names one family's level; pick that family with --family", level)
		}
		families = CBFamilies
	}
	var out []*CBInstance
	for _, f := range families {
		for i := 0; i < n; i++ {
			in, err := GenerateCB(f, level, seed+int64(i))
			if err != nil {
				return nil, err
			}
			out = append(out, in)
		}
	}
	return out, nil
}

func sameMsgs(a, b []agent.Msg) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Role != b[i].Role || a[i].Content != b[i].Content || a[i].ToolCallID != b[i].ToolCallID || len(a[i].ToolCalls) != len(b[i].ToolCalls) {
			return false
		}
	}
	return true
}

// RunCBTask drives one instance to its end and classifies it.
func RunCBTask(ctx context.Context, in *CBInstance, seat CBSeat, opts CBRunOpts) CBTaskResult {
	o := opts.withDefaults(len(in.Ops))
	usable := o.Budget - o.Reserve
	res := CBTaskResult{ID: in.ID, Family: in.Family, Level: in.Level, Seed: in.Seed, OpsTotal: len(in.Ops)}

	msgs := []agent.Msg{
		{Role: "system", Content: CBInstruction(in, o.Delivery)},
		{Role: "user", Content: "Begin. The first operation follows."},
	}
	const preamble = 2
	var pager agent.PagerStats
	var frames []CBFrame
	record := func(kind string) {
		cp := make([]agent.Msg, len(msgs))
		copy(cp, msgs)
		frames = append(frames, CBFrame{Kind: kind, Step: len(frames), Tokens: agent.EstimateTokens(cp), Messages: cp})
	}
	deliver := func(i int) {
		text := cbRenderOp(in.Ops[i], len(in.Ops)-(i+1))
		if o.Delivery == CBDeliveryUser {
			msgs = append(msgs, agent.Msg{Role: "user", Content: text})
		} else {
			id := fmt.Sprintf("op-%d", i)
			msgs = append(msgs,
				agent.Msg{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: id, Name: "next_op", Args: "{}"}}},
				agent.Msg{Role: "tool", Content: text, ToolCallID: id})
		}
		res.OpsDelivered = i + 1
	}

	if len(in.Ops) > 0 {
		deliver(0)
	}
	end := func(term string) { res.TerminatedBy = term }
loop:
	for {
		if err := ctx.Err(); err != nil {
			end(CBTermCancelled)
			res.Outcome = CBOutcomeNotRun
			res.Detail = err.Error()
			break loop
		}
		before := msgs
		msgs = agent.CompactReplay(msgs, usable, o.KeepRecent, preamble, agent.ReplayOpts{GCF: o.Ladder.GCF, Skeleton: o.Ladder.Skeleton})
		if !sameMsgs(before, msgs) {
			res.Compactions++
			pager.NoteCompaction(before, msgs)
		}
		record("sent")
		if est := agent.EstimateTokens(msgs); est > usable {
			end(CBTermBudgetWall)
			break loop
		} else if est > res.PeakSentTokens {
			res.PeakSentTokens = est
		}
		reply, err := seat(ctx, msgs, o.MaxOut)
		if err != nil {
			end(CBTermSeatError)
			res.Outcome = CBOutcomeInfraError
			res.Detail = err.Error()
			break loop
		}
		msgs = append(msgs, agent.Msg{Role: "assistant", Content: reply})
		record("turn")
		res.Turns++

		var pending string
		switch {
		case cbIsSubmit(reply):
			if res.OpsDelivered < len(in.Ops) {
				pending = cbRefuseSubmitText
			} else {
				end(CBTermSubmit)
				break loop
			}
		case cbIsReady(reply):
			if res.OpsDelivered >= len(in.Ops) {
				end(CBTermStreamExhausted)
				break loop
			}
			deliver(res.OpsDelivered)
		default:
			// The stream ends once the last operation has been answered; the marker is
			// not demanded after it.
			if res.OpsDelivered >= len(in.Ops) {
				end(CBTermStreamExhausted)
				break loop
			}
			if strings.Contains(reply, CBAdvanceMarker) {
				pending = cbNonStandaloneText
			} else {
				pending = cbNoMarkerText
			}
		}
		if pending != "" {
			msgs = append(msgs, agent.Msg{Role: "user", Content: pending})
		}
		if res.Turns >= o.MaxTurns {
			end(CBTermMaxTurns)
			res.Outcome = CBOutcomeTimeout
			break loop
		}
	}

	// The seat here has no tool to fetch evicted content, so the re-fetch question is never
	// posed: the pager's own verdict ("below gate, no pager needed") was written for a loop
	// that can re-fetch and would read as a measured zero. Keep what was evicted; null the rate.
	rep := pager.Report()
	rep.RefetchRate = nil
	rep.Basis = CBPagerNotExercised
	rep.Verdict = "re-fetch not exercised: the seat has no tool to fetch evicted content, so only evictions are measured"
	res.Pager = &rep
	if res.Outcome == "" {
		classify(&res, GradeCB(in, frames), o.PassAt)
	}
	return res
}

// classify turns a grade into the task's outcome word.
//
//	infra_error  the seat could not answer (set by the loop; never graded)
//	timeout      the turn cap was reached before the stream drained (never graded)
//	not_run      the run was cancelled before the task finished or began
//	judge_error  the grade could not be computed (grader_valid 0)
//	pass         graded, reward >= PassAt
//	task_fail    graded, reward < PassAt (the reward is kept: 0.958 is information)
//	cheat        never produced here: the adapter has no check for gaming the grader
func classify(res *CBTaskResult, g CBGrade, passAt float64) {
	res.Matched, res.Total = g.Matched, g.Total
	if !g.Valid {
		res.Outcome = CBOutcomeJudgeError
		res.Detail = g.Note
		return
	}
	r := g.Reward
	res.Reward = &r
	res.Rewards = g.Sub
	if r >= passAt {
		res.Outcome = CBOutcomePass
	} else {
		res.Outcome = CBOutcomeTaskFail
	}
}

// RunCB runs every instance in order and assembles the report. A cancelled context
// marks the tasks it never reached not_run.
func RunCB(ctx context.Context, instances []*CBInstance, seat CBSeat, opts CBRunOpts, seatName string) CBReport {
	o := opts.withDefaults(0)
	rep := CBReport{
		Schema: CBSchema, Seat: seatName, Budget: o.Budget, Reserve: o.Reserve, Estimator: CBEstimator,
		Delivery: o.Delivery, Ladder: o.Ladder.Label(), PassAt: o.PassAt, Counts: map[string]int{},
	}
	for _, w := range CBOutcomes {
		rep.Counts[w] = 0
	}
	sums, graded := map[string]float64{}, map[string]int{}
	for _, in := range instances {
		var r CBTaskResult
		if ctx.Err() != nil {
			r = CBTaskResult{ID: in.ID, Family: in.Family, Level: in.Level, Seed: in.Seed, Outcome: CBOutcomeNotRun,
				TerminatedBy: CBTermCancelled, OpsTotal: len(in.Ops), Detail: ctx.Err().Error()}
		} else {
			r = RunCBTask(ctx, in, seat, opts)
		}
		rep.Results = append(rep.Results, r)
		rep.Counts[r.Outcome]++
		if r.Reward != nil {
			sums[r.Family] += *r.Reward
			graded[r.Family]++
		}
	}
	if len(graded) > 0 {
		rep.MeanReward = map[string]float64{}
		fams := make([]string, 0, len(graded))
		for f := range graded {
			fams = append(fams, f)
		}
		sort.Strings(fams)
		for _, f := range fams {
			rep.MeanReward[f] = round4(sums[f] / float64(graded[f]))
		}
	}
	return rep
}
