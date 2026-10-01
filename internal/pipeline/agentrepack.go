package pipeline

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// Helpers of the node-side structured re-pack (repackStructuredWith): what its
// prompts say about types, and, further down, how it is bounded and observed.

// repackTypeRule is the one sentence both re-pack prompts carry about types.
// The prompts used to list field NAMES only, and the chat prompt added "numbers
// unquoted": a seat that read the name of a `numbers` list answered bare JSON
// numbers where the schema wants strings, on every lane that has no grammar
// (register C-80). The rule says what each kind of field holds, so the schema's
// own types are what the seat reads.
const repackTypeRule = `Respect each field's type exactly: a string, or a list of strings, holds quoted text only, even when that text is a number (write "42", never 42); a number or integer field holds a bare JSON number; a boolean field holds true or false.`

// repackFieldList renders the fields of a schema for a re-pack prompt, one
// `"name" (type)` each, in the order given. A property with no usable type reads
// as a string, which is what the grammar compiles it to.
func repackFieldList(names []string, props map[string]any) string {
	parts := make([]string, 0, len(names))
	for _, name := range names {
		spec, _ := props[name].(map[string]any)
		parts = append(parts, fmt.Sprintf("%q (%s)", name, fieldTypeText(spec)))
	}
	return strings.Join(parts, ", ")
}

// fieldTypeText spells one schema property's type for a prompt: "string",
// "number", "integer", "boolean", `string, one of "a", "b"` for an enum, and for a
// list the type of its items too ("array of strings"): the item type is exactly
// what a prompt that says only "array" leaves a seat to guess.
func fieldTypeText(spec map[string]any) string {
	if typ, _ := spec["type"].(string); typ == "array" {
		items, _ := spec["items"].(map[string]any)
		if len(items) == 0 {
			return "array"
		}
		return "array of " + pluralType(typeName(items)) + enumTail(items, ", each one of ")
	}
	return typeName(spec) + enumTail(spec, ", one of ")
}

// typeName is a property's declared scalar type, "string" when it names none.
func typeName(spec map[string]any) string {
	if typ, _ := spec["type"].(string); typ != "" {
		return typ
	}
	return "string"
}

// enumTail renders a property's string enum as `<lead>"a", "b"`, or "" when it has none.
func enumTail(spec map[string]any, lead string) string {
	raw, _ := spec["enum"].([]any)
	var quoted []string
	for _, v := range raw {
		if s, ok := v.(string); ok {
			quoted = append(quoted, fmt.Sprintf("%q", s))
		}
	}
	if len(quoted) == 0 {
		return ""
	}
	return lead + strings.Join(quoted, ", ")
}

func pluralType(name string) string {
	switch name {
	case "string", "number", "integer", "boolean", "object", "array":
		return name + "s"
	}
	return name
}

// repackOpts is what one structured re-pack call is handed beside its context.
// The zero value is the call every caller made before: no bound, no record.
type repackOpts struct {
	// WallEnd is when the contract's wall ends: the instant the wall began plus
	// timeout_sec. The zero time means the call carries no wall (the delegator's
	// rescue, which has a deadline of its own).
	WallEnd time.Time
	// Grace is how far past the wall a re-pack may be asked to finish. The run's
	// liveness policy gives its Slack (30 s in production), the padding the
	// re-pack's own stall allowance already adds to its generation estimate.
	Grace time.Duration
	// TokS is the seat's decode rate in tokens per second, and RateBasis says where
	// it came from. 0 means unknown, and then the call does no time arithmetic at
	// all: it fails open, like every other sizing decision that needs a rate.
	TokS      float64
	RateBasis string
	// Trace, when set, receives one record per attempt, for the wire.
	Trace *repackTrace
}

// repackRate is the decode rate the node sizes a re-pack against: the seat-rates
// store's calibrated rate, else this run's own observed rate (the liveness
// monitor's smoothed rate over its streamed deltas: 0 on a seat that answers JSON
// in one piece), else the box's agent_seat_tok_s. The basis names which.
func repackRate(known seatrate.Seat, observed, configured float64) (float64, string) {
	switch {
	case known.TokS > 0:
		return known.TokS, "the seat-rates store"
	case observed > 0:
		return observed, "this run's observed rate"
	case configured > 0:
		return configured, "agent_seat_tok_s"
	}
	return 0, ""
}

// repackFit is what the time left lets one attempt ask for.
type repackFit struct {
	// Tokens is the max_tokens to send: the budget, or what the time buys.
	Tokens int
	// Clamped is true when Tokens is under the budget asked for.
	Clamped bool
	// Skip is true when the time left cannot buy even the answer.
	Skip bool
	// Note is the clamp's or the skip's arithmetic ("" when the budget fitted).
	Note string
}

// fit sizes one attempt against the time left: before the earlier of the run's
// own deadline (its liveness ceiling) and the wall's end plus the grace, at the
// seat's rate. An attempt whose budget fits is sent as it is; one that does not is
// clamped to the tokens the time buys, as long as that still holds the answer
// (least, the answer's own size in tokens); one that cannot is skipped, and the
// note says why. It is a token budget and a deadline check, never a transport
// timeout (ADR 0061): a request in flight is not cut, and the busy hold stays.
func (o repackOpts) fit(now, ctxDeadline time.Time, budget, least int) repackFit {
	if o.TokS <= 0 {
		return repackFit{Tokens: budget}
	}
	var deadline time.Time
	what := ""
	if !ctxDeadline.IsZero() {
		deadline, what = ctxDeadline, "the ceiling"
	}
	if !o.WallEnd.IsZero() {
		if end := o.WallEnd.Add(o.Grace); deadline.IsZero() || end.Before(deadline) {
			deadline, what = end, fmt.Sprintf("the wall + %.0f s grace", o.Grace.Seconds())
		}
	}
	if deadline.IsZero() {
		return repackFit{Tokens: budget}
	}
	left := max(deadline.Sub(now), 0)
	// Compared as a float first: a context with a distant deadline would overflow
	// an int on a 32-bit build, and nothing is clamped when the budget fits.
	buysF := left.Seconds() * o.TokS
	if buysF >= float64(budget) {
		return repackFit{Tokens: budget}
	}
	buys := int(buysF)
	if buys >= least {
		return repackFit{Tokens: buys, Clamped: true, Note: fmt.Sprintf(
			"max_tokens %d clamped to %d: %.0f s left to %s at %.1f tok/s", budget, buys, left.Seconds(), what, o.TokS)}
	}
	basis := ""
	if o.RateBasis != "" {
		basis = " (rate: " + o.RateBasis + ")"
	}
	return repackFit{Skip: true, Note: fmt.Sprintf(
		"re-pack skipped: %.0f s left to %s at %.1f tok/s buys %d tokens < the answer's %d%s", left.Seconds(), what, o.TokS, buys, least, basis)}
}

// repackSkipErr marks a re-pack that was not sent, or not sent again, because the
// time left could not buy the answer: the wall's doing, not the seat's, so the
// defer is a budget one and the finished answer stays flagged for the delegator
// to re-pack (register C-80).
type repackSkipErr struct{ msg string }

func (e *repackSkipErr) Error() string { return e.msg }

// asRepackSkip reports whether err is a *repackSkipErr, and returns it.
func asRepackSkip(err error) (*repackSkipErr, bool) {
	var s *repackSkipErr
	ok := errors.As(err, &s)
	return s, ok
}

// repackClampedErr marks a re-pack attempt that the time left had narrowed and
// that was cut at the narrowed budget with a tail that was not a runaway: the clock
// decided how much the seat was given, not the seat and not the schema, so the
// defer is a budget one. Filed as an abstention it would be retried on another node,
// a failure charged to a seat that was never given room to finish, and a contract
// the clock could not fit would be spent twice. The finished answer stays flagged
// for the delegator to re-pack (register C-80).
type repackClampedErr struct{ msg string }

func (e *repackClampedErr) Error() string { return e.msg }

// asRepackClamped reports whether err is, or wraps, a *repackClampedErr.
func asRepackClamped(err error) bool {
	var c *repackClampedErr
	return errors.As(err, &c)
}

// truncatedRepackErr is the error of a re-pack attempt cut at max_tokens: what was
// asked (budget), what the cut showed (cut.observed), and, when the time left set
// the budget, the clamp's own arithmetic beside it (clampNote, "" when the budget
// fitted), then extra, any further finding the caller made about what came next.
// Both lanes build it, so a clamped cut reads and is filed the same on either. It
// is a *repackClampedErr only when the clock set the budget and the tail was not a
// runaway: a loop or whitespace to the cap is the seat's, whatever the clock did.
func truncatedRepackErr(what string, budget, answerChars int, cut repackCut, clampNote, extra string) error {
	seen := cut.observed(budget)
	if clampNote != "" {
		seen = "the time left set this budget: " + clampNote + "; " + seen
	}
	if extra != "" {
		seen += "; " + extra
	}
	msg := fmt.Sprintf("%s truncated at %d tokens (the answer is %d chars; %s)", what, budget, answerChars, seen)
	if clampNote != "" && cut.Degenerate == "" {
		return &repackClampedErr{msg: msg}
	}
	return errors.New(msg)
}

// deadlineOf is ctx's deadline, the zero time when it has none.
func deadlineOf(ctx context.Context) time.Time {
	dl, _ := ctx.Deadline()
	return dl
}

// repackTrace collects what each attempt of one re-pack call did.
type repackTrace struct {
	Attempts []core.AgentRepackAttempt
}

// repackClipBytes is how much of what an attempt wrote rides on the wire at each
// end: enough to see a whitespace tail, a loop or the shape of a prefix, never
// the answer.
const repackClipBytes = 80

// add records one attempt: what was asked (lane, max_tokens), what came back
// (tokens, finish reason, a clip of the content), how long it took and why it
// failed ("" when it produced the object). clampedFrom is the budget the time left
// narrowed it from (0 when it was not). A nil trace records nothing.
func (tr *repackTrace) add(attemptNum int, lane string, maxTokens, clampedFrom int, g llamaclient.GenResult, took time.Duration, why string) {
	if tr == nil {
		return
	}
	a := core.AgentRepackAttempt{
		Attempt: attemptNum, Lane: lane, MaxTokens: maxTokens, ClampedFrom: clampedFrom,
		TokensOut: g.TokensOut, FinishReason: g.FinishReason, Ms: took.Milliseconds(), Why: why,
	}
	a.Head, a.Tail = clipEnds(g.Content)
	tr.Attempts = append(tr.Attempts, a)
}

// skip records an attempt that was never sent, and why.
func (tr *repackTrace) skip(attemptNum int, lane, why string) {
	if tr == nil {
		return
	}
	tr.Attempts = append(tr.Attempts, core.AgentRepackAttempt{Attempt: attemptNum, Lane: lane, Skipped: true, Why: why})
}

// clipEnds returns the first and the last repackClipBytes bytes of s, each cut on
// a character boundary. Content that fits in both clips is returned whole as the
// head, with no tail, so a short answer is not shown twice.
func clipEnds(s string) (head, tail string) {
	if len(s) <= 2*repackClipBytes {
		return s, ""
	}
	h := repackClipBytes
	for h > 0 && !utf8.RuneStart(s[h]) {
		h--
	}
	t := len(s) - repackClipBytes
	for t < len(s) && !utf8.RuneStart(s[t]) {
		t++
	}
	return s[:h], s[t:]
}

// repackWhy is an error as one clipped line for an attempt's record: the
// validator's message spans lines, and the record is a note, not a log.
func repackWhy(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) <= 240 {
		return s
	}
	cut := 240
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// repackCut is what a grammar completion cut at max_tokens shows about WHY it was
// cut. The retry used to go to the completion cap on every truncation, a second
// request that a greedy seat answers byte for byte like the first (register C-80:
// a seat that ran away at 1,439 tokens ran away again at 8,192, 24 minutes at 5.6
// tok/s). More tokens help only when the budget was the problem.
type repackCut struct {
	// Degenerate says what the tail of the output is when it is no content: only
	// whitespace, or a short block repeated. "" when it is not.
	Degenerate string
	// Needed is the tokens the answer's object needs, from the answer's bytes at the
	// density the seat actually wrote: never under the code's own estimate (a token
	// per three bytes, expectedRepackTokens), and above it when the seat's tokenizer
	// is denser on this text (digits, symbols, other scripts).
	Needed int
	// BytesPerToken is the density the truncated completion showed, 0 when it was
	// too short to measure.
	BytesPerToken float64
}

// judgeCut reads a truncated completion against the answer it re-packs. The
// measured density is trusted only on a real sample: a short cut says nothing
// about the tokenizer.
func judgeCut(output string, g llamaclient.GenResult) repackCut {
	c := repackCut{Degenerate: degenerateTail(g.Content)}
	perToken := 3.0
	if g.TokensOut >= 64 && len(g.Content) > 0 {
		c.BytesPerToken = float64(len(g.Content)) / float64(g.TokensOut)
		if c.BytesPerToken < perToken {
			perToken = math.Max(c.BytesPerToken, 1)
		}
	}
	c.Needed = int(math.Ceil(float64(len(output))/perToken)) + 64
	return c
}

// escalates reports whether a second request at the completion cap can do what the
// first could not: the cap must be above the budget that was cut, the output must
// not be a loop or whitespace (more tokens only lengthen it), and the answer must
// need more tokens than the budget held.
func (c repackCut) escalates(budget int) bool {
	return c.Degenerate == "" && budget < agentRepackMaxTokensCap && c.Needed > budget
}

// observed spells what the truncated attempt showed, for its note: the
// degenerate tail, a budget that was too small, one that already is the cap, or
// an answer that fits the budget and an output that ran on past it.
func (c repackCut) observed(budget int) string {
	switch {
	case c.Degenerate != "":
		return fmt.Sprintf("the output was degenerate (%s): a larger budget would not help", c.Degenerate)
	case c.Needed > budget && budget >= agentRepackMaxTokensCap:
		return fmt.Sprintf("the answer needs about %d tokens at the density the seat wrote and the budget is already the %d-token cap", c.Needed, agentRepackMaxTokensCap)
	case c.Needed > budget:
		return fmt.Sprintf("the answer needs about %d tokens at the %.1f bytes per token the seat wrote: the %d-token budget was too small", c.Needed, c.BytesPerToken, budget)
	}
	return fmt.Sprintf("the answer needs about %d tokens, inside the %d-token budget: the output ran on past it", c.Needed, budget)
}

const (
	// degenerateTailBytes is how much of the end of a cut completion is read for a loop.
	degenerateTailBytes = 256
	// degenerateMaxPeriod and degenerateMinBytes shape the repeat test: a block of
	// up to 16 bytes, repeated at least four times and over at least 32 bytes.
	degenerateMaxPeriod = 16
	degenerateMinReps   = 4
	degenerateMinBytes  = 32
)

// degenerateTail says what the end of a cut completion is when it is no content,
// and "" when it reads as content. Whitespace, one byte repeated, a short block
// repeated and a block of lines repeated are the shapes a constrained decoder
// falls into when the grammar masks what the model wanted: agent.DegenerateRun
// skips whitespace on purpose and agent.DetectRepetitionLoop wants four lines,
// and neither sees `"", "", ""` or spaces on ONE line, which is the shape a
// single-line JSON object runs to its cap in.
func degenerateTail(content string) string {
	if strings.TrimSpace(content) == "" {
		return "nothing but whitespace"
	}
	tail := content
	if len(tail) > degenerateTailBytes {
		cut := len(tail) - degenerateTailBytes
		for cut < len(tail) && !utf8.RuneStart(tail[cut]) {
			cut++
		}
		tail = tail[cut:]
	}
	if strings.TrimSpace(tail) == "" {
		return fmt.Sprintf("only whitespace in its last %d bytes", len(tail))
	}
	if b, n := agent.DegenerateRun(tail); n > 0 {
		return fmt.Sprintf("%q repeated %d times", string(b), n)
	}
	for period := 1; period <= degenerateMaxPeriod; period++ {
		window := max(period*degenerateMinReps, degenerateMinBytes)
		if len(tail) < window {
			continue
		}
		w := tail[len(tail)-window:]
		periodic := true
		for i := period; i < len(w); i++ {
			if w[i] != w[i-period] {
				periodic = false
				break
			}
		}
		if periodic {
			return fmt.Sprintf("repeating %q", w[len(w)-period:])
		}
	}
	if rep, ok := agent.DetectRepetitionLoop(content); ok {
		return fmt.Sprintf("repeating a %d-line block %d times", rep.Period, rep.Count)
	}
	return ""
}
