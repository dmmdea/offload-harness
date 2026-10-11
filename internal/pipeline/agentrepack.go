package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gbnf"
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
// own types are what the seat reads. An object field holds exactly the fields
// listed for it and a list of objects one such object per item: the list of fields
// names the keys, and the first rule keeps a seat from collapsing an object into
// a string (register F43).
const repackTypeRule = `Respect each field's type exactly: a string, or a list of strings, holds quoted text only, even when that text is a number (write "42", never 42); a number or integer field holds a bare JSON number; a boolean field holds true or false; an object field holds a JSON object with exactly the fields listed for it, and a list of objects holds one such object per item.`

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
// what a prompt that says only "array" leaves a seat to guess. An object, and a
// list of objects, also name their fields, each with its own type, to any depth:
// `array of objects, each with fields "name" (string), "score" (number)`. The
// grammar fixes the keys, but only this text tells the seat what each one is for,
// and the grammar-free chat lane has nothing else (a chat re-pack of an array of
// objects came back with the items present and their required keys missing).
func fieldTypeText(spec map[string]any) string {
	if typ, _ := spec["type"].(string); typ == "array" {
		items, _ := spec["items"].(map[string]any)
		if len(items) == 0 {
			return "array"
		}
		if typ, _ := items["type"].(string); typ == "array" {
			return "array of " + fieldTypeText(items)
		}
		return "array of " + pluralType(typeName(items)) + enumTail(items, ", each one of ") + fieldsTail(items, ", each with ")
	}
	return typeName(spec) + enumTail(spec, ", one of ") + fieldsTail(spec, " with ")
}

// fieldsTail renders an object schema's properties as `<lead>fields "a" (type),
// "b" (type)`, in the order the grammar writes them (the schema's required list
// first, then the rest by name), or "" for a schema that lists none.
func fieldsTail(spec map[string]any, lead string) string {
	fields := gbnf.FromJSONSchema(spec)
	if len(fields) == 0 {
		return ""
	}
	props, _ := spec["properties"].(map[string]any)
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		names = append(names, f.Name)
	}
	return lead + "fields " + repackFieldList(names, props)
}

// typeName is a property's declared scalar type, "string" when it names none, or
// "object" when it names none and lists properties.
func typeName(spec map[string]any) string {
	if typ, _ := spec["type"].(string); typ != "" {
		return typ
	}
	if _, ok := spec["properties"].(map[string]any); ok {
		return "object"
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
	// OwedEnd is the instant the time this re-pack is OWED ends: when the call began
	// plus what its own answer needs at the seat's rate (repackOwed). The wall is an
	// expectation (ADR 0055 item 2) and a slow seat's loop routinely ends past it, so
	// a re-pack held to the wall plus the grace was told "0 s left" and skipped, with
	// a finished answer in hand (register F44/C13: 23 jobs in 48 h). The bound is the
	// LATER of the wall's end and this one, plus the grace; the run's ceiling still
	// caps it. The zero time is no owed time: the wall alone bounds the call, as
	// before. It is set once per call, never per attempt, so a later attempt does
	// not get a fresh allowance: time only runs out.
	OwedEnd time.Time
	// OwedCap, when set, says OwedEnd was cut short by something other than the time
	// the answer needs (the call's deadline), and is the words a note uses for it.
	OwedCap string
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
// in one piece), else the rate this run's own completions measured (the effective
// rate over the loop's completions of 1,024 tokens or more, which includes the
// prefill and so errs toward the slower side), else the box's agent_seat_tok_s. The
// basis names which. 0 means no rate is known, and the wall bound then does no
// arithmetic (repackBoundOff says so).
func repackRate(known seatrate.Seat, observed, measured, configured float64) (float64, string) {
	switch {
	case known.TokS > 0:
		return known.TokS, "the seat-rates store"
	case observed > 0:
		return observed, "this run's observed rate"
	case measured > 0:
		return measured, "this run's measured rate"
	case configured > 0:
		return configured, "agent_seat_tok_s"
	}
	return 0, ""
}

// repackBoundOffNote is what a failed re-pack says when no decode rate was known
// for its seat: the wall bound fails open without one (repackOpts.fit), which is
// right, and a failed re-pack is exactly the one it would have bounded, so failing
// open must not be silent (register C-80).
const repackBoundOffNote = "re-pack time bound off: no decode rate is known for this seat (no seat-rates sample, none streamed or measured this run, no agent_seat_tok_s), so the wall could not bound it"

// repackBoundOffLogged remembers the seats that were logged as running a re-pack
// with its time bound off, so the log says it once per seat and process, not once
// per run.
var repackBoundOffLogged sync.Map

// repackBoundOff returns the note a failed re-pack carries when its time bound was
// off, and logs the first time a seat is seen so.
func repackBoundOff(seat string) string {
	if _, seen := repackBoundOffLogged.LoadOrStore(seat, true); !seen {
		log.Printf("agent task: %s (seat %s); the re-pack is bounded by the run's ceiling alone", repackBoundOffNote, seat)
	}
	return repackBoundOffNote
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
	if end, label := o.boundEnd(); !end.IsZero() {
		if end = end.Add(o.Grace); deadline.IsZero() || end.Before(deadline) {
			deadline, what = end, fmt.Sprintf("%s + %.0f s grace", label, o.Grace.Seconds())
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

// boundEnd is where the time a re-pack may use ends before the grace: the wall's
// end, or the end of the time the re-pack is owed (OwedEnd) when that is later,
// with the words the notes use for it. The zero time when neither is set.
func (o repackOpts) boundEnd() (time.Time, string) {
	if o.OwedEnd.After(o.WallEnd) {
		if o.OwedCap != "" {
			return o.OwedEnd, o.OwedCap
		}
		return o.OwedEnd, "the re-pack's owed time"
	}
	if o.WallEnd.IsZero() {
		return time.Time{}, ""
	}
	return o.WallEnd, "the wall"
}

// repackOwed is the time a re-pack of output is owed at the seat's decode rate,
// whatever the loop left of the wall: the answer's expected size in tokens
// (expectedRepackTokens) at that rate, padded by the factor the re-pack's own stall
// allowance pads its generation estimate with (agent.RepackMargin). 0 when no rate
// is known: the wall bound then does no arithmetic at all, so there is nothing to
// extend.
func repackOwed(output string, tokS float64) time.Duration {
	if tokS <= 0 {
		return 0
	}
	return time.Duration(float64(expectedRepackTokens(output)) / tokS * agent.RepackMargin * float64(time.Second))
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
	// Coerced says which fields the node re-typed to make the returned object fit the
	// schema ("" when it was returned as the seat wrote it): the delivered object is
	// then not byte for byte what the seat wrote, and nothing else says so.
	Coerced string
}

// setCoerced records that the object the call returned was re-typed. A nil trace
// records nothing.
func (tr *repackTrace) setCoerced(note string) {
	if tr != nil {
		tr.Coerced = note
	}
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

// partialOf is what an attempt generated, for its record and the run's token count,
// whether or not it produced a usable answer: the completion when one came back,
// else what a STREAM that died mid-body had delivered (a dropped connection, an
// engine error frame, the stall watch or the ceiling cutting a producing seat; the
// deltas heard are a lower bound of the tokens). A request that never got a body
// (refused, unreachable, a busy answer past the budget) generated nothing.
func partialOf(g llamaclient.GenResult, err error) llamaclient.GenResult {
	if err == nil || g.TokensOut > 0 || g.Content != "" {
		return g
	}
	var be *llamaclient.BodyError
	if errors.As(err, &be) {
		return be.Partial
	}
	return g
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

const (
	// repackLoopPenalty is the frequency_penalty a re-pack attempt carries once an
	// earlier attempt of the same call has looped (register C14). CHOSEN, not
	// measured: no seat was replayed with it. 0.5 is a moderate OpenAI-scale value
	// (the field runs 0 to 2): the penalty on a token grows with how often the seat
	// has already written it, so a list item written ten times is pushed off far
	// harder than a quote or a comma written once, which is what ends a loop, and
	// it is sent only on a retry that would otherwise be the request that just
	// failed, so the fidelity it costs on an honest copy is never paid by a re-pack
	// that finished the first time.
	repackLoopPenalty = 0.5
	// repackNoRepeatNote is the sentence a conditioned attempt adds to its prompt,
	// the re-pack's counterpart of agent.NoRepeatInstruction: a seat that looped
	// once on a request loops again on the same request unless it is told.
	repackNoRepeatNote = "A previous attempt to write this object got stuck repeating the same list item until it ran out of room. Write each list item exactly once, in the order the TEXT gives it, then close the list and the object."
	// repackLoopRun is how many times in a row a list item (or a short block of
	// items) may repeat in a re-packed object before it is read as a loop when the
	// answer it was written from does not hold the item that often. CHOSEN, not
	// measured: the agent loop reads four repeats of a line as a loop; a list of
	// short items has more honest repeats in it than a list of lines, so this is
	// 6, and the answer-count test below is what spares a repeat the answer holds.
	repackLoopRun = 6
	// repackLoopMaxPeriod is the longest block of items degenerateList looks for: a
	// pair or a triple alternating, which is what a greedy seat falls into between
	// two items it cannot choose between. CHOSEN.
	repackLoopMaxPeriod = 3
)

// degenerateList reads a COMPLETE re-packed object for the repetition a cut
// completion shows at its tail: a list field that repeats one item, or a block of up
// to repackLoopMaxPeriod items, at least repackLoopRun times in a row, where the
// answer the object was written from holds the first item fewer times than it was
// repeated. The object validates (a list of strings has no bound in the grammar,
// and a loop that ended before the cap closed itself), so without this it was
// published as the structured result. It says what it found, or "" for an object
// that reads as content. An empty item is the schema's empty value, never a loop.
func degenerateList(content []byte, source string) string {
	var obj map[string]any
	if json.Unmarshal(content, &obj) != nil {
		return ""
	}
	names := make([]string, 0, len(obj))
	for name := range obj {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		arr, _ := obj[name].([]any)
		if len(arr) < repackLoopRun {
			continue
		}
		items := make([]string, len(arr))
		for i, el := range arr {
			items[i] = listItemText(el)
		}
		for period := 1; period <= repackLoopMaxPeriod; period++ {
			for i := 0; i+period*repackLoopRun <= len(items); {
				k := 1
				for i+(k+1)*period <= len(items) && equalItems(items[i:i+period], items[i+k*period:i+(k+1)*period]) {
					k++
				}
				if k < repackLoopRun {
					i++
					continue
				}
				if first := items[i]; first != "" {
					if held := strings.Count(source, first); held < k {
						quoted := make([]string, period)
						for j := range quoted {
							quoted[j] = strconv.Quote(items[i+j])
						}
						return fmt.Sprintf("repeated the list item %s %d times in a row in %q and the answer holds %q %d time(s): a loop, not content",
							strings.Join(quoted, ", "), k, name, first, held)
					}
				}
				i += k * period
			}
		}
	}
	return ""
}

// listItemText is a list element as the text it would have in the answer: the
// string itself, a number or boolean as written, and "" for a nested value (which
// is never compared).
func listItemText(el any) string {
	switch v := el.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func equalItems(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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

// loops reports whether the cut is no budget problem: the output was a loop, or the
// seat ran on past a budget that already held what the answer needs. A request
// sent again as it was would write the same thing (the seat decodes greedy), so
// the attempt after it must differ (register C14). It is the other half of
// escalates: a cut is one, the other, or neither (the budget is already the cap and
// still too small).
func (c repackCut) loops(budget int) bool {
	return c.Degenerate != "" || c.Needed <= budget
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
	// degenerateTailBytes is how much of the end of a cut completion is read for
	// whitespace, and degenerateLoopBytes how much for a repeated block.
	degenerateTailBytes = 256
	degenerateLoopBytes = 1024
	// degenerateShortPeriod bounds the block the first repeat test looks for, in
	// bytes (the whole of the original test), and degenerateMinReps is how many times
	// a block must repeat. degenerateMaxPeriod (192) bounds the second repeat test,
	// which runs after the line-block test and reads only what the first two cannot:
	// two alternating list items of about 80 bytes, the loop a 64-byte block could not
	// see, which was called "ran on past the budget" and escalated to the cap. The
	// window must hold degenerateMinReps periods, hence degenerateLoopBytes. CHOSEN,
	// not measured: the real tails were not recorded.
	degenerateShortPeriod = 64
	degenerateMaxPeriod   = 192
	degenerateMinReps     = 4
	// degenerateMinCover is how much of the end of the output a repeat of visible text
	// must cover: half the window. A loop runs to the END (it is what spent the budget),
	// and a run of one byte or a short block that is not that long is ordinary content on
	// a 1,000-token output: a markdown rule, base64 padding, a leader of dots, a list of a
	// few identical items. degenerateMinBlank is the same for a repeat made of whitespace
	// alone, which no JSON value needs in quantity.
	degenerateMinCover = 128
	degenerateMinBlank = 32
)

// degenerateTail says what the end of a cut completion is when it is no content,
// and "" when it reads as content. Whitespace, one byte repeated, a short block
// repeated and a block of lines repeated are the shapes a constrained decoder
// falls into when the grammar masks what the model wanted. The evidence must
// reach the end of the output and cover enough of it: agent.DegenerateRun's
// twenty-byte floor was built for a sampler that emits one token to the cap, and a
// 40-byte rule or 26 bytes of padding anywhere in the window read as a runaway on
// output that was only long. agent.DetectRepetitionLoop wants four lines and sees
// neither `"", "", ""` nor spaces on ONE line, which is the shape a single-line JSON
// object runs to its cap in, so the byte-level repeat test covers that.
func degenerateTail(content string) string {
	if strings.TrimSpace(content) == "" {
		return "nothing but whitespace"
	}
	tail := lastBytes(content, degenerateTailBytes)
	if strings.TrimSpace(tail) == "" {
		return fmt.Sprintf("only whitespace in its last %d bytes", len(tail))
	}
	window := lastBytes(content, degenerateLoopBytes)
	if why := repeatedBlock(window, 1, degenerateShortPeriod); why != "" {
		return why
	}
	if rep, ok := agent.DetectRepetitionLoop(content); ok && len(content)-rep.CutAt >= degenerateMinCover {
		return fmt.Sprintf("repeating a %d-line block %d times", rep.Period, rep.Count)
	}
	// Longer blocks last, so that every output the original two tests named keeps the
	// words it had: this one only names what they read as content.
	return repeatedBlock(window, degenerateShortPeriod+1, degenerateMaxPeriod)
}

// repeatedBlock says what the end of tail repeats when a block of between from and
// to bytes covers enough of it to be a loop (degenerateMinCover of it, or
// degenerateMinBlank for whitespace, and at least degenerateMinReps copies), "" when
// none does.
func repeatedBlock(tail string, from, to int) string {
	for period := from; period <= to && period <= len(tail); period++ {
		block := tail[len(tail)-period:]
		need := degenerateMinCover
		if strings.TrimSpace(block) == "" {
			need = degenerateMinBlank
		}
		need = max(need, period*degenerateMinReps)
		covered := periodicSuffix(tail, period)
		if covered < need {
			continue
		}
		if period == 1 && strings.TrimSpace(block) != "" {
			return fmt.Sprintf("%q repeated %d times", block, covered)
		}
		return fmt.Sprintf("repeating %q", block)
	}
	return ""
}

// lastBytes is the last n bytes of s (all of it when it is shorter), cut on a
// character boundary.
func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return s[cut:]
}

// periodicSuffix is the length of the longest suffix of s that repeats with the
// given period: the last `period` bytes, and every byte before them back to the
// first that differs from the byte one period after it. It is `period` when the end
// of s repeats nothing, and all of s when s is shorter than that.
func periodicSuffix(s string, period int) int {
	if period >= len(s) {
		return len(s)
	}
	i := len(s) - 1
	for i-period >= 0 && s[i] == s[i-period] {
		i--
	}
	return len(s) - 1 - i + period
}
