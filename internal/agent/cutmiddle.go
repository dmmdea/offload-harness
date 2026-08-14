package agent

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
)

// cutmiddle.go — TO-4 `cut_middle_turns`: token-exact whole-message history
// compaction. This is the DROP rung of the compaction ladder re-based on the
// served model's REAL tokenizer instead of the chars/4 estimate.
//
// WHY. The legacy drop rung (compact step 4) decides how much to drop by the
// same estimate that mis-gated compaction in ADR 0017, and the reactive paths
// can degrade a fresh tool body by trimming INSIDE it — for a JSON tool result
// that means a half-message the next parse chokes on, a real failure mode for
// small local models. This rung never edits a message: it sentinel-indexes each
// message (byte spans in the serialized transcript — out-of-band indices that
// cannot collide with content, where an in-band sentinel string could), asks
// the server's /tokenize for the pieces of the ONE serialized transcript, maps
// piece byte lengths back to per-message token spans, keeps the messages that
// fit inside the first and last budget/2 tokens, and drops WHOLE messages from
// the middle — assistant+tool units atomically, so pairing survives and a
// half-message is unrepresentable by construction.
//
// INTEGRATION CONTRACT (one truncation mechanism, not two): when the loop has
// a working Tokenizer this rung REPLACES the estimate-driven whole-turn-drop
// rung. The legacy rung remains only as the explicit fail-open fallback for
// endpoints with no /tokenize (a generic OpenAI backend) or a mid-run
// tokenizer outage — the tokenizer wrapper is sticky (stickyTokenizer), so one
// failure downgrades the rest of the run to the legacy path instead of paying
// a doomed round-trip per step. Head (system/task preamble) and tail (recent
// state) always survive, exactly as the ladder already guarantees.

// Tokenizer is the seam cutMiddleTurns needs from the real-tokenizer path
// (internal/tokclient implements it): the byte length of every token piece of
// text, in order, with ok=false on any failure. Implementations MUST fail
// open — returning ok=false routes the ladder to the legacy estimate rung.
type Tokenizer interface {
	Pieces(ctx context.Context, text string) ([]int, bool)
}

// stickyTokenizer downgrades permanently once the endpoint is judged unable to
// answer /tokenize — paying a failed round-trip on every over-budget step would
// put a network stall inside the loop for nothing. The judgment is classified,
// not first-failure (review finding 2026-08-14 — one cold-start timeout or one
// 503 during a model swap must not degrade a healthy endpoint for the process
// life, which in --serve/--queue is every later goal):
//   - a DEFINITIVE failure (the endpoint answered 404/405 on every route — the
//     route positively does not exist, retrying cannot help) downgrades
//     immediately;
//   - a TRANSIENT failure (timeout, reset, 5xx, malformed body) downgrades only
//     on the SECOND consecutive failure; a success in between resets the count.
//     The cost of the second chance is at most one extra stalled probe.
//
// A failure while the caller's ctx is already cancelled says NOTHING about the
// endpoint (the run is shutting down, or a --serve client hung up) — that one
// is never counted, or a single mid-run cancellation would silently downgrade
// every later run sharing this Loop (--serve and --queue keep one Loop for the
// process). atomic — --serve shares one *Loop (and thus one tokenizer) across
// concurrent HTTP handlers.
type stickyTokenizer struct {
	inner   Tokenizer
	failed  atomic.Bool
	strikes atomic.Int32 // consecutive live-ctx transient failures
	reason  atomic.Value // string: WHY the downgrade happened, for the Result report
}

// errDetailer is the optional seam for implementations that can say WHY the
// last call failed (tokclient.Client does). Only consulted at downgrade time.
type errDetailer interface{ LastErr() string }

// definitiveFailer is the optional seam for implementations that can say the
// last failure was a positive route absence (tokclient.Client: all candidates
// answered 404/405). Absent the seam every failure counts as transient, so a
// second implementation gets the two-strike behavior, never a harsher one.
type definitiveFailer interface{ LastFailDefinitive() bool }

// stickyStrikeLimit is how many CONSECUTIVE transient failures downgrade the
// run to the legacy rung. Two: the first may be a cold start or a model swap;
// two in a row on a live ctx is an endpoint that cannot currently serve the
// route, and each additional probe is a stall inside the agent loop.
const stickyStrikeLimit = 2

func (s *stickyTokenizer) Pieces(ctx context.Context, text string) ([]int, bool) {
	if s.failed.Load() {
		return nil, false
	}
	lens, ok := s.inner.Pieces(ctx, text)
	if !ok {
		if ctx.Err() == nil {
			why := "tokenizer failed (no detail available)"
			if d, hasDetail := s.inner.(errDetailer); hasDetail {
				if e := d.LastErr(); e != "" {
					why = e
				}
			}
			definitive := false
			if df, hasClass := s.inner.(definitiveFailer); hasClass {
				definitive = df.LastFailDefinitive()
			}
			switch {
			case definitive:
				s.reason.Store(why)
				s.failed.Store(true)
			case s.strikes.Add(1) >= stickyStrikeLimit:
				s.reason.Store(fmt.Sprintf("%d consecutive failures, last: %s", stickyStrikeLimit, why))
				s.failed.Store(true)
			}
		}
		return nil, false
	}
	s.strikes.Store(0) // a success breaks any transient streak
	return lens, true
}

// reportBroken records a CONTRACT failure detected by the caller (pieces that
// do not reconstruct the input) and downgrades immediately: a tokenizer whose
// mapping cannot be trusted is broken for this run regardless of transport
// health, and without recording it Result.TokenizerPath would keep claiming
// token-exact while every cut silently fell to the legacy rung (review finding
// 2026-08-14 — the observability field must never lie).
func (s *stickyTokenizer) reportBroken(why string) {
	s.reason.Store(why)
	s.failed.Store(true)
}

// degraded reports whether (and why) the sticky downgrade fired.
func (s *stickyTokenizer) degraded() (string, bool) {
	if !s.failed.Load() {
		return "", false
	}
	why, _ := s.reason.Load().(string)
	return why, true
}

// msgSeparator joins serialized messages in the one tokenized transcript. A
// newline is a natural token boundary for every BPE vocabulary we serve, which
// keeps cross-message token merges rare; the mapping below tolerates the ones
// that still happen (an overlapping token counts against every message it
// touches — the conservative direction).
const msgSeparator = "\n"

// realMsgOverheadTokens approximates the chat-template framing each message
// costs beyond its serialized content (<start_of_turn>/role/<end_of_turn> and
// kin — special tokens /tokenize with add_special=false cannot see).
// Deliberately a touch high: overestimating drops a message early, which is
// the safe direction; underestimating re-opens the server-400 class this rung
// exists to close.
const realMsgOverheadTokens = 8

// serializeMsg renders one message the way the cut accounts for it: role tag,
// call ids, content, and any tool-call payloads (ids, names and raw args go on
// the wire too — omitting the ids undercounted tool-heavy transcripts, the
// unsafe direction). This is a token-accounting serialization, not the wire
// template — the template's framing is covered by realMsgOverheadTokens.
func serializeMsg(m Msg) string {
	var b strings.Builder
	b.WriteString(m.Role)
	if m.ToolCallID != "" {
		b.WriteString(" ")
		b.WriteString(m.ToolCallID)
	}
	b.WriteString("\n")
	b.WriteString(m.Content)
	for _, c := range m.ToolCalls {
		b.WriteString("\n")
		b.WriteString(c.ID)
		b.WriteString(" ")
		b.WriteString(c.Name)
		b.WriteString(" ")
		b.WriteString(c.Args)
	}
	return b.String()
}

// cutMiddleTurns drops whole middle messages until the transcript's REAL token
// count fits realBudget. Returns (result, fits, true) when the tokenizer
// answered — including the no-cut case where the real count already fits — and
// (msgs, false, false) untouched when it did not (caller falls back to the
// legacy estimate-driven drop rung). fits is the REAL-token verdict on the
// returned transcript: false means forced keeps (preamble, keepRecent, pinned,
// signal residue) exceed the budget and the caller must count it exhausted —
// the chars/4 estimate must never decide that, in either direction (the
// estimate under-counts dense content, so an over-real-budget transcript can
// read as "fits" in estimate space and the honest-overflow telemetry would
// silently never fire).
//
// Survival rules, in priority order:
//   - the protected preamble [0, protectedPrefix) ALWAYS survives (system +
//     objective — the ladder-wide contract), and the last keepRecent messages
//     ALWAYS survive;
//   - a unit (an assistant turn with tool calls plus ALL its tool results, or
//     a single unpaired message) survives when any member is pinned (H8: the
//     model demonstrably re-requested it) or any member's remaining content
//     still carries signal lines (the elide rung's FORCE_PRESERVE residue) —
//     the same two exemptions the legacy drop rung honors;
//   - otherwise a unit survives only when EVERY member's token span lies fully
//     inside the head window (first budget/2 content tokens) or fully inside
//     the tail window (last budget/2) — anything straddling or between is
//     dropped whole.
//
// Forced keeps can leave the result over budget (a huge preamble or pinned
// unit); that is the ladder's existing honest-overflow contract — the caller's
// fit telemetry reports it, nothing is half-dropped to hide it.
func cutMiddleTurns(ctx context.Context, tok Tokenizer, msgs []Msg, realBudget, protectedPrefix, keepRecent int, pinned map[string]bool) ([]Msg, bool, bool) {
	if tok == nil || realBudget <= 0 || len(msgs) == 0 {
		return msgs, false, false
	}
	if protectedPrefix < 0 {
		protectedPrefix = 0
	}
	if protectedPrefix > len(msgs) {
		protectedPrefix = len(msgs)
	}
	if keepRecent < 0 {
		keepRecent = 0
	}
	recentStart := len(msgs) - keepRecent
	if recentStart < protectedPrefix {
		recentStart = protectedPrefix
	}

	// Sentinel-index: serialize every message and record its byte span in the
	// one concatenated transcript. Byte offsets are the sentinel index — they
	// live outside the content, so no message bytes can forge a boundary.
	var sb strings.Builder
	type span struct{ start, end int }
	spans := make([]span, len(msgs))
	for i, m := range msgs {
		if i > 0 {
			sb.WriteString(msgSeparator)
		}
		s := sb.Len()
		sb.WriteString(serializeMsg(m))
		spans[i] = span{start: s, end: sb.Len()}
	}
	text := sb.String()

	pieces, ok := tok.Pieces(ctx, text)
	if !ok {
		return msgs, false, false
	}
	// Contract check (tokclient verifies this too; a second implementation
	// might not): the pieces must reconstruct the text's bytes exactly, or the
	// byte→token mapping below would be built on sand. Fail open, never cut on
	// an unreliable mapping — and REPORT it through the sticky wrapper's
	// broken-contract seam, so a non-verifying implementation downgrades
	// observably instead of paying a doomed round-trip per step while
	// Result.TokenizerPath keeps claiming token-exact (review finding
	// 2026-08-14: the earlier comment claimed this was "visible in the
	// degraded/latency profile" — no such profile signal existed).
	sum := 0
	for _, n := range pieces {
		sum += n
	}
	if sum != len(text) {
		if br, canReport := tok.(interface{ reportBroken(string) }); canReport {
			br.reportBroken(fmt.Sprintf("tokenizer pieces sum to %d bytes for a %d-byte transcript — mapping violates the reconstruction contract", sum, len(text)))
		}
		return msgs, false, false
	}

	// Map bytes → tokens: tokSpans[i] = [firstTok, lastTok] where firstTok is
	// the first token overlapping the message's bytes and lastTok the last. A
	// token that straddles a boundary (BPE merge across the separator) counts
	// against BOTH neighbors — the conservative direction for "fully inside a
	// window" tests.
	type tspan struct{ first, last int }
	tokSpans := make([]tspan, len(msgs))
	{
		pos, ti := 0, 0
		for i := range msgs {
			// Advance to the first token overlapping spans[i].start.
			for ti < len(pieces) && pos+pieces[ti] <= spans[i].start {
				pos += pieces[ti]
				ti++
			}
			first := ti
			p, t := pos, ti
			for t < len(pieces) && p < spans[i].end {
				p += pieces[t]
				t++
			}
			last := t - 1
			if last < first {
				last = first // zero-length serialization (cannot happen: role is non-empty)
			}
			tokSpans[i] = tspan{first: first, last: last}
			// Do NOT advance pos/ti past the boundary token: the next message may
			// share it.
		}
	}

	totalContent := len(pieces)
	// Content-token budget: the framing overhead of every message that could
	// survive is reserved up front. Computing it from len(msgs) rather than the
	// (unknown) survivor count only over-reserves, which is the safe direction.
	contentBudget := realBudget - realMsgOverheadTokens*len(msgs)
	if totalContent+realMsgOverheadTokens*len(msgs) <= realBudget {
		return msgs, true, true // real count already fits — the estimate was pessimistic
	}
	if contentBudget < 0 {
		contentBudget = 0 // only forced keeps survive; honest overflow
	}
	headEnd := contentBudget / 2               // tokens [0, headEnd) are the head window
	tailStart := totalContent - contentBudget/2 // tokens [tailStart, totalContent) are the tail window

	// Unitize: an assistant turn with tool calls owns every tool result whose
	// ToolCallID matches one of its calls (matched across the whole transcript,
	// like the legacy rung — never by adjacency assumptions).
	unitOf := make([]int, len(msgs)) // message index → unit id
	for i := range unitOf {
		unitOf[i] = i
	}
	for i, m := range msgs {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		ids := make(map[string]bool, len(m.ToolCalls))
		for _, c := range m.ToolCalls {
			ids[c.ID] = true
		}
		for j := i + 1; j < len(msgs); j++ {
			if msgs[j].Role == "tool" && ids[msgs[j].ToolCallID] {
				unitOf[j] = i
			}
		}
	}

	// Decide survival per unit: a unit is FORCED whole when any member is in
	// the protected preamble, the keepRecent tail, pinned, or carrying signal
	// residue (unit-atomic, so a unit straddling the recentStart boundary can
	// never be half-dropped into an orphaned tool result); otherwise it
	// survives only when EVERY member sits fully inside the head or tail
	// token window.
	forced := func(i int, m Msg) bool {
		if i < protectedPrefix || i >= recentStart {
			return true
		}
		if m.Role == "tool" && pinned[m.ToolCallID] {
			return true // H8: the model demonstrably re-requested this result
		}
		// FORCE_PRESERVE residue: same test the legacy drop rung applies —
		// signal vocabulary in a tool body's remaining content.
		return m.Role == "tool" && signalLine.MatchString(m.Content)
	}
	inWindows := func(ts tspan) bool {
		return ts.last < headEnd || ts.first >= tailStart
	}
	unitForced := map[int]bool{}
	unitAllInWindows := map[int]bool{}
	for i, m := range msgs {
		u := unitOf[i]
		if _, seen := unitAllInWindows[u]; !seen {
			unitAllInWindows[u] = true
		}
		if forced(i, m) {
			unitForced[u] = true
		}
		if !inWindows(tokSpans[i]) {
			unitAllInWindows[u] = false
		}
	}
	out := make([]Msg, 0, len(msgs))
	keptTokens := 0
	for i, m := range msgs {
		if u := unitOf[i]; unitForced[u] || unitAllInWindows[u] {
			out = append(out, m)
			// Span length, boundary-shared tokens counted per message — the
			// conservative (over-counting) side, matching the window tests.
			keptTokens += tokSpans[i].last - tokSpans[i].first + 1
		}
	}
	// The REAL-token fit verdict on what survived: forced keeps can exceed the
	// budget, and only this yardstick may say so (see the doc comment). The
	// separators joining the survivors are counted too (spans start AFTER each
	// separator, so span sums alone under-count the re-serialized transcript by
	// one separator per boundary — review finding 2026-08-14: every asymmetry
	// in this verdict must lean conservative, never loose; a straddling token
	// already counted in both neighbors over-counts here, which is the safe
	// direction).
	sepTokens := 0
	if len(out) > 1 {
		sepTokens = len(out) - 1
	}
	fits := keptTokens+sepTokens+realMsgOverheadTokens*len(out) <= realBudget
	return out, fits, true
}
