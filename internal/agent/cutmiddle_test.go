package agent

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// runeTok is the deterministic fake tokenizer: one token per rune, so piece
// byte lengths are the rune encodings — sum reconstructs the text exactly, as
// the Tokenizer contract requires. calls counts invocations (sticky tests).
type runeTok struct{ calls int }

func (r *runeTok) Pieces(_ context.Context, text string) ([]int, bool) {
	r.calls++
	lens := make([]int, 0, len(text))
	for _, ru := range text {
		lens = append(lens, utf8.RuneLen(ru))
	}
	return lens, true
}

// failTok always fails (an endpoint without /tokenize).
type failTok struct{ calls int }

func (f *failTok) Pieces(context.Context, string) ([]int, bool) { f.calls++; return nil, false }

// runeTokens counts what runeTok charges for a transcript's serialized form —
// the test-side mirror of the cut's own arithmetic.
func runeTokens(msgs []Msg) int {
	n := 0
	for i, m := range msgs {
		if i > 0 {
			n++ // separator
		}
		n += utf8.RuneCountInString(serializeMsg(m))
	}
	return n
}

// cutFixture builds: [system, objective] preamble, then `units` tool cycles
// (assistant tool-call + one tool result of bodyRunes 'x's).
func cutFixture(units, bodyRunes int) []Msg {
	msgs := []Msg{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "do the task"},
	}
	for i := 1; i <= units; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			Msg{Role: "assistant", ToolCalls: []ToolCall{{ID: id, Name: "read_file", Args: fmt.Sprintf(`{"path":"f%d"}`, i)}}},
			Msg{Role: "tool", ToolCallID: id, Content: strings.Repeat("x", bodyRunes)},
		)
	}
	return msgs
}

// assertIdentitySubsequence fails unless out is an order-preserving
// subsequence of in with every member DEEP-EQUAL to its original — the
// never-half-message invariant: the cut may only DROP messages, never edit,
// split, or synthesize one.
func assertIdentitySubsequence(t *testing.T, in, out []Msg) {
	t.Helper()
	j := 0
	for i := range out {
		found := false
		for j < len(in) {
			if reflect.DeepEqual(in[j], out[i]) {
				found = true
				j++
				break
			}
			j++
		}
		if !found {
			t.Fatalf("output message %d (%q role=%s) is not an unmodified input message in order — a half-message or mutation escaped the cut", i, clipForLog(out[i].Content), out[i].Role)
		}
	}
}

func clipForLog(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

// assertPairing fails on any orphaned tool result or half-dropped unit.
func assertPairing(t *testing.T, msgs []Msg) {
	t.Helper()
	byID := map[string]bool{}
	for _, m := range msgs {
		if m.Role == "assistant" {
			for _, c := range m.ToolCalls {
				byID[c.ID] = true
			}
		}
	}
	for _, m := range msgs {
		if m.Role == "tool" && !byID[m.ToolCallID] {
			t.Fatalf("orphaned tool result for call %q — its assistant turn was dropped without it", m.ToolCallID)
		}
	}
	have := map[string]bool{}
	for _, m := range msgs {
		if m.Role == "tool" {
			have[m.ToolCallID] = true
		}
	}
	for _, m := range msgs {
		if m.Role == "assistant" {
			for _, c := range m.ToolCalls {
				if !have[c.ID] {
					t.Fatalf("assistant turn kept but its tool result %q was dropped — half a unit survived", c.ID)
				}
			}
		}
	}
}

func TestCutMiddleDropsWholeMiddleMessagesHeadTailSurvive(t *testing.T) {
	msgs := cutFixture(8, 200)
	tok := &runeTok{}
	out, _, ok := cutMiddleTurns(context.Background(), tok, msgs, 800, 2, 2, nil)
	if !ok {
		t.Fatal("cut reported tokenizer failure against the healthy fake")
	}
	if len(out) >= len(msgs) {
		t.Fatalf("nothing dropped: %d -> %d messages (fixture is far over budget)", len(msgs), len(out))
	}
	assertIdentitySubsequence(t, msgs, out)
	assertPairing(t, out)
	// Head: the protected preamble survives verbatim at the front.
	if !reflect.DeepEqual(out[0], msgs[0]) || !reflect.DeepEqual(out[1], msgs[1]) {
		t.Fatal("protected preamble (system + objective) did not survive at the head")
	}
	// Tail: the keepRecent tail survives verbatim at the back.
	if !reflect.DeepEqual(out[len(out)-1], msgs[len(msgs)-1]) || !reflect.DeepEqual(out[len(out)-2], msgs[len(msgs)-2]) {
		t.Fatal("keepRecent tail did not survive at the back")
	}
	// Budget arithmetic on the REAL yardstick: kept content plus the reserved
	// per-message framing must fit the real budget (nothing here is forced
	// beyond the windows).
	if got := runeTokens(out) + realMsgOverheadTokens*len(out); got > 800 {
		t.Fatalf("kept transcript costs %d real tokens, over the 800 budget", got)
	}
}

// The never-half-message property, exercised across random transcripts and
// budgets: output is always an unmodified, order-preserving subsequence with
// intact pairing, protected preamble, and keepRecent tail.
func TestCutMiddleNeverEmitsHalfMessageProperty(t *testing.T) {
	for seed := int64(0); seed < 25; seed++ {
		rng := rand.New(rand.NewSource(seed))
		msgs := []Msg{
			{Role: "system", Content: "sys prompt"},
			{Role: "user", Content: "objective: seed " + fmt.Sprint(seed)},
		}
		units := 3 + rng.Intn(10)
		for i := 0; i < units; i++ {
			switch rng.Intn(3) {
			case 0: // bare assistant note (no tool calls)
				msgs = append(msgs, Msg{Role: "assistant", Content: strings.Repeat("nota ", 1+rng.Intn(80))})
			case 1: // multibyte-heavy tool cycle
				id := fmt.Sprintf("s%dm%d", seed, i)
				msgs = append(msgs,
					Msg{Role: "assistant", ToolCalls: []ToolCall{{ID: id, Name: "grep", Args: `{"q":"añíล"}`}}},
					Msg{Role: "tool", ToolCallID: id, Content: strings.Repeat("áß≠ล", 5+rng.Intn(150))},
				)
			default: // plain tool cycle
				id := fmt.Sprintf("s%dp%d", seed, i)
				msgs = append(msgs,
					Msg{Role: "assistant", ToolCalls: []ToolCall{{ID: id, Name: "read_file", Args: `{"p":"x"}`}}},
					Msg{Role: "tool", ToolCallID: id, Content: strings.Repeat("y", 10+rng.Intn(600))},
				)
			}
		}
		keepRecent := rng.Intn(4)
		// Tokenizer diversity (review round 2, 2026-08-14): runeTok never lets
		// a piece cross a message boundary, so it cannot exercise the
		// straddling-token arm of the byte→token mapping. sparseTok (100-byte
		// pieces) and chunkTok (pseudo-random 2–7-byte pieces) both produce
		// boundary-spanning tokens — the real BPE-merge regime.
		for _, tok := range []Tokenizer{&runeTok{}, sparseTok{}, chunkTok{}} {
			for _, budget := range []int{1, 50, 200, 500, 1000, 1 << 20} {
				out, _, ok := cutMiddleTurns(context.Background(), tok, msgs, budget, 2, keepRecent, nil)
				if !ok {
					t.Fatalf("seed %d budget %d tok %T: tokenizer failure from the healthy fake", seed, budget, tok)
				}
				assertIdentitySubsequence(t, msgs, out)
				assertPairing(t, out)
				if !reflect.DeepEqual(out[0], msgs[0]) || !reflect.DeepEqual(out[1], msgs[1]) {
					t.Fatalf("seed %d budget %d tok %T: preamble lost", seed, budget, tok)
				}
				for k := 1; k <= keepRecent && k <= len(out); k++ {
					if !reflect.DeepEqual(out[len(out)-k], msgs[len(msgs)-k]) {
						t.Fatalf("seed %d budget %d tok %T: keepRecent tail message %d lost", seed, budget, tok, k)
					}
				}
			}
		}
	}
}

// chunkTok cuts pseudo-random 2–7-byte pieces, seeded from the text length so
// repeated calls on one text agree: pieces routinely straddle message
// boundaries AND rune boundaries (byte-fallback regime).
type chunkTok struct{}

func (chunkTok) Pieces(_ context.Context, text string) ([]int, bool) {
	rng := rand.New(rand.NewSource(int64(len(text))))
	var lens []int
	for rem := len(text); rem > 0; {
		n := 2 + rng.Intn(6)
		if n > rem {
			n = rem
		}
		lens = append(lens, n)
		rem -= n
	}
	return lens, true
}

// liarTok answers ok=true with piece lengths that do NOT reconstruct the text
// — the contract violation a non-verifying second implementation could ship.
type liarTok struct{}

func (liarTok) Pieces(_ context.Context, text string) ([]int, bool) {
	if len(text) < 2 {
		return []int{len(text)}, true
	}
	return []int{len(text) - 1}, true // short by one byte: mapping built on sand
}

// The cut's own reconstruction check must refuse to cut AND report the broken
// contract through the sticky wrapper — before this round the refusal was
// silent and Result.TokenizerPath kept claiming token-exact (review finding
// 2026-08-14).
func TestCutMiddleRefusesNonReconstructingPiecesAndReportsBroken(t *testing.T) {
	msgs := cutFixture(8, 200)
	s := &stickyTokenizer{inner: liarTok{}}
	out, fits, ok := cutMiddleTurns(context.Background(), s, msgs, 100, 2, 2, nil)
	if ok || fits {
		t.Fatal("a non-reconstructing mapping must fail open (ok=false), never cut")
	}
	if !reflect.DeepEqual(out, msgs) {
		t.Fatal("the transcript must be returned untouched on a refused mapping")
	}
	why, down := s.degraded()
	if !down {
		t.Fatal("the broken contract must trip the sticky downgrade — otherwise every step pays a doomed round-trip while TokenizerPath lies")
	}
	if !strings.Contains(why, "reconstruction contract") {
		t.Fatalf("degrade reason %q must name the contract violation", why)
	}
}

func TestCutMiddleRealFitReturnsUnchanged(t *testing.T) {
	msgs := cutFixture(4, 50)
	out, _, ok := cutMiddleTurns(context.Background(), &runeTok{}, msgs, 1<<20, 2, 2, nil)
	if !ok {
		t.Fatal("tokenizer failure from the healthy fake")
	}
	if !reflect.DeepEqual(out, msgs) {
		t.Fatal("a transcript whose REAL count fits the budget must be returned unchanged (the estimate was pessimistic)")
	}
}

func TestCutMiddlePinnedAndSignalUnitsSurvive(t *testing.T) {
	msgs := cutFixture(8, 200)
	// Unit c5's tool result carries an error line — FORCE_PRESERVE residue.
	for i := range msgs {
		if msgs[i].Role == "tool" && msgs[i].ToolCallID == "c5" {
			msgs[i].Content = "error: kaboom at line 3\n" + msgs[i].Content
		}
	}
	pinned := map[string]bool{"c4": true} // H8: the model re-requested c4
	out, _, ok := cutMiddleTurns(context.Background(), &runeTok{}, msgs, 800, 2, 2, pinned)
	if !ok {
		t.Fatal("tokenizer failure from the healthy fake")
	}
	assertPairing(t, out)
	seen := map[string]bool{}
	for _, m := range out {
		if m.Role == "tool" {
			seen[m.ToolCallID] = true
		}
	}
	if !seen["c4"] {
		t.Fatal("pinned unit c4 was dropped — H8 exemption not honored by the cut")
	}
	if !seen["c5"] {
		t.Fatal("signal-carrying unit c5 was dropped — FORCE_PRESERVE exemption not honored by the cut")
	}
}

func TestCutMiddleUnitStraddlingRecentBoundaryKeptWhole(t *testing.T) {
	msgs := cutFixture(8, 200)
	// keepRecent=1 forces ONLY the final tool result; its assistant partner
	// sits before the boundary. The unit must survive whole — a dropped
	// assistant with a surviving result is a wire error.
	out, _, ok := cutMiddleTurns(context.Background(), &runeTok{}, msgs, 600, 2, 1, nil)
	if !ok {
		t.Fatal("tokenizer failure from the healthy fake")
	}
	assertPairing(t, out)
	last := msgs[len(msgs)-1]
	found := false
	for _, m := range out {
		if reflect.DeepEqual(m, last) {
			found = true
		}
	}
	if !found {
		t.Fatal("forced keepRecent tail message missing")
	}
}

func TestCutMiddleTokenizerFailureFallsBackToLegacyDrop(t *testing.T) {
	msgs := cutFixture(8, 200)
	// Estimate-space budget BELOW what body-elision alone can reach (~260 for
	// this fixture): the ladder must fall through to the drop rung, or neither
	// path under test executes.
	budget := 100
	legacy := compact(context.Background(), msgs, budget, 2, 2, compactOpts{})
	ft := &failTok{}
	withTok := compact(context.Background(), msgs, budget, 2, 2, compactOpts{Tok: ft, RealBudget: 800})
	if ft.calls == 0 {
		t.Fatal("the cut rung never consulted the tokenizer")
	}
	if !reflect.DeepEqual(legacy, withTok) {
		t.Fatal("tokenizer failure must fall open to the EXACT legacy drop rung — the two paths diverged")
	}
}

func TestCutMiddleReplacesLegacyDropInCompact(t *testing.T) {
	msgs := cutFixture(8, 200)
	tok := &runeTok{}
	out := compact(context.Background(), msgs, 100, 2, 2, compactOpts{Tok: tok, RealBudget: 800})
	if tok.calls == 0 {
		t.Fatal("compact never reached the token-exact cut (gate not wired)")
	}
	assertPairing(t, out)
	// Ladder rungs before the cut may have elided tool BODIES (whole-body
	// markers — a different, disclosed artifact class); the cut itself must
	// still never leave a role/pairing mutation. Every survivor is either
	// byte-identical to an input message or an input tool message whose body
	// became a compaction artifact.
	for _, m := range out {
		matched := false
		for _, in := range msgs {
			if reflect.DeepEqual(in, m) {
				matched = true
				break
			}
			if in.Role == "tool" && m.Role == "tool" && in.ToolCallID == m.ToolCallID && IsCompactionArtifact(m.Content) {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("survivor (role=%s id=%s %q) is neither an input message nor a disclosed compaction artifact", m.Role, m.ToolCallID, clipForLog(m.Content))
		}
	}
}

// The sticky downgrade is CLASSIFIED (review finding 2026-08-14): a transient
// failure gets one second chance (a cold start or a 503 mid-swap must not
// degrade a healthy endpoint for the process life), two consecutive transient
// failures trip it, and after the trip the inner is never consulted again.
func TestStickyTokenizerTransientFailsTwiceThenShortCircuits(t *testing.T) {
	ft := &failTok{}
	s := &stickyTokenizer{inner: ft}
	if _, ok := s.Pieces(context.Background(), "x"); ok {
		t.Fatal("sticky must propagate the failure")
	}
	if s.failed.Load() {
		t.Fatal("ONE transient failure must not trip the downgrade — the second chance is the point")
	}
	if _, ok := s.Pieces(context.Background(), "x"); ok {
		t.Fatal("sticky must propagate the second failure")
	}
	if !s.failed.Load() {
		t.Fatal("two consecutive transient failures must trip the downgrade")
	}
	s.Pieces(context.Background(), "x")
	if ft.calls != 2 {
		t.Fatalf("inner tokenizer called %d times, want exactly 2 — after the trip no more doomed round-trips", ft.calls)
	}
	// A healthy inner stays healthy.
	rt := &runeTok{}
	s2 := &stickyTokenizer{inner: rt}
	s2.Pieces(context.Background(), "ab")
	s2.Pieces(context.Background(), "cd")
	if rt.calls != 2 {
		t.Fatalf("healthy inner called %d times, want 2", rt.calls)
	}
}

// definitiveFailTok models tokclient after an all-404 probe: the route
// positively does not exist, so the downgrade must not burn a second stall.
type definitiveFailTok struct{ calls int }

func (d *definitiveFailTok) Pieces(context.Context, string) ([]int, bool) {
	d.calls++
	return nil, false
}
func (d *definitiveFailTok) LastErr() string          { return "HTTP 404 on both routes" }
func (d *definitiveFailTok) LastFailDefinitive() bool { return true }

func TestStickyTokenizerDefinitiveFailureTripsImmediately(t *testing.T) {
	dt := &definitiveFailTok{}
	s := &stickyTokenizer{inner: dt}
	s.Pieces(context.Background(), "x")
	if !s.failed.Load() {
		t.Fatal("a definitive route absence (all candidates 404/405) must trip on the FIRST failure")
	}
	why, _ := s.degraded()
	if why != "HTTP 404 on both routes" {
		t.Fatalf("definitive reason = %q, want the implementation's own detail verbatim", why)
	}
}

// flakyTok fails on selected calls: pins that a SUCCESS between two transient
// failures resets the streak (consecutive means consecutive).
type flakyTok struct {
	calls    int
	failOn   map[int]bool
	delegate runeTok
}

func (f *flakyTok) Pieces(ctx context.Context, text string) ([]int, bool) {
	f.calls++
	if f.failOn[f.calls] {
		return nil, false
	}
	return f.delegate.Pieces(ctx, text)
}

func TestStickyTokenizerSuccessResetsTransientStreak(t *testing.T) {
	ft := &flakyTok{failOn: map[int]bool{1: true, 3: true}}
	s := &stickyTokenizer{inner: ft}
	s.Pieces(context.Background(), "ab") // transient failure 1
	s.Pieces(context.Background(), "ab") // success — streak resets
	s.Pieces(context.Background(), "ab") // transient failure 1 again
	if s.failed.Load() {
		t.Fatal("failure-success-failure tripped the downgrade — the streak must reset on success, or a flaky-but-healthy endpoint degrades")
	}
}

func TestWithTokenizerWiresTheLadder(t *testing.T) {
	l := NewLoop(nil, nil, 1).WithContextTokens(4096).WithMaxTokens(512)
	if opts := l.ladderOpts(); opts.Tok != nil {
		t.Fatal("no tokenizer configured, yet ladderOpts advertises one")
	}
	l.WithTokenizer(&runeTok{})
	opts := l.ladderOpts()
	if opts.Tok == nil {
		t.Fatal("WithTokenizer did not reach ladderOpts")
	}
	if _, isSticky := opts.Tok.(*stickyTokenizer); !isSticky {
		t.Fatal("the loop must wrap the tokenizer sticky (one failure = legacy rung for the rest of the run)")
	}
	if opts.RealBudget != l.inputBudget() {
		t.Fatalf("RealBudget = %d, want the loop's real input budget %d", opts.RealBudget, l.inputBudget())
	}
	// nil is a no-op, not a panic and not a sticky-nil wrapper.
	l2 := NewLoop(nil, nil, 1).WithTokenizer(nil)
	if l2.tok != nil {
		t.Fatal("WithTokenizer(nil) must leave the legacy rung in place")
	}
}

// A failure while the caller's ctx is cancelled says nothing about the
// endpoint — recording it would let one --serve client hang-up silently
// downgrade every later request sharing the Loop (review finding 2026-08-14).
func TestStickyTokenizerIgnoresContextCancellation(t *testing.T) {
	ft := &failTok{}
	s := &stickyTokenizer{inner: ft}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	// Any number of cancelled-ctx failures must count for NOTHING — not a trip,
	// not even a strike (or two hang-ups would equal one real strike pair).
	for i := 0; i < 3; i++ {
		if _, ok := s.Pieces(cancelled, "x"); ok {
			t.Fatal("failure must still propagate")
		}
	}
	if s.failed.Load() || s.strikes.Load() != 0 {
		t.Fatalf("cancelled-ctx failures recorded (failed=%v strikes=%d) — one hang-up would help downgrade the whole process", s.failed.Load(), s.strikes.Load())
	}
	// REAL endpoint failures (live ctx) still trip it — two, transient class.
	s.Pieces(context.Background(), "x")
	s.Pieces(context.Background(), "x")
	if !s.failed.Load() {
		t.Fatal("two live-ctx endpoint failures must trip the sticky downgrade")
	}
}

// The fit verdict is judged on the REAL yardstick, in both directions: a
// transcript whose forced keeps exceed the real budget reports overReal (the
// estimate would hide it), and one the tokenizer verified as fitting reports
// fitReal (the estimate would report it exhausted forever).
func TestCompactVerdictRealOverridesEstimate(t *testing.T) {
	// overReal: pin EVERY unit so nothing may drop, tiny real budget.
	msgs := cutFixture(6, 200)
	pinned := map[string]bool{}
	for i := 1; i <= 6; i++ {
		pinned[fmt.Sprintf("c%d", i)] = true
	}
	out, v := compactWithVerdict(context.Background(), msgs, 100, 2, 2, compactOpts{Tok: &runeTok{}, RealBudget: 300, Pinned: pinned})
	if v != overReal {
		t.Fatalf("verdict = %v, want overReal: every unit is pinned and the real budget is 300", v)
	}
	assertPairing(t, out)

	// fitReal: estimate-space budget forces the ladder in (estimate over), but
	// the REAL count fits the real budget — the cut must return fitReal so the
	// loop does not count a verified-fitting request as exhausted.
	msgs2 := cutFixture(3, 100)
	_, v2 := compactWithVerdict(context.Background(), msgs2, 1, 2, 2, compactOpts{Tok: &runeTok{}, RealBudget: 1 << 20})
	if v2 != fitReal {
		t.Fatalf("verdict = %v, want fitReal: the tokenizer measured the transcript inside the real budget", v2)
	}

	// fitUnknown: no tokenizer — the estimate path, exactly as before.
	if _, v3 := compactWithVerdict(context.Background(), msgs2, 1, 2, 2, compactOpts{}); v3 != fitUnknown {
		t.Fatalf("verdict = %v, want fitUnknown on the estimate-only path", v3)
	}
}

// TokenizerPath is the downgrade's visibility: none / token-exact / degraded
// with the recorded reason (review finding 2026-08-14: fail-open must not be
// fail-unobservable).
func TestTokenizerPathReporting(t *testing.T) {
	if got := NewLoop(nil, nil, 1).tokPath(); got != "" {
		t.Fatalf("no tokenizer: tokPath = %q, want empty", got)
	}
	l := NewLoop(nil, nil, 1).WithTokenizer(&runeTok{})
	if got := l.tokPath(); got != "token-exact" {
		t.Fatalf("healthy tokenizer: tokPath = %q, want token-exact", got)
	}
	l2 := NewLoop(nil, nil, 1).WithTokenizer(&failTok{})
	l2.tok.Pieces(context.Background(), "x") // strike 1 (transient class)
	l2.tok.Pieces(context.Background(), "x") // strike 2 — trips
	// Anchored on the EXPORTED prefix: the CLI's degrade note and the queue
	// traces branch on it, so producer and consumers must share one constant
	// (review finding 2026-08-14 — two independent literals let a wording
	// tweak silently kill the operator-facing note).
	if got := l2.tokPath(); !strings.HasPrefix(got, TokenizerDegradedPrefix) {
		t.Fatalf("tripped tokenizer: tokPath = %q, want a degraded report prefixed %q", got, TokenizerDegradedPrefix)
	}
}

// detailFailTok fails and can say why — the errDetailer seam tokclient uses.
type detailFailTok struct{}

func (detailFailTok) Pieces(context.Context, string) ([]int, bool) { return nil, false }
func (detailFailTok) LastErr() string                              { return "HTTP 404 on both routes" }

func TestStickyTokenizerCapturesFailureDetail(t *testing.T) {
	s := &stickyTokenizer{inner: detailFailTok{}}
	s.Pieces(context.Background(), "x") // strike 1 (no definitive seam => transient class)
	s.Pieces(context.Background(), "x") // strike 2 — trips
	why, down := s.degraded()
	if !down || !strings.Contains(why, "HTTP 404 on both routes") {
		t.Fatalf("degraded() = (%q, %v), want the implementation's own failure detail embedded", why, down)
	}
	if !strings.Contains(why, "consecutive failures") {
		t.Fatalf("degraded() = %q, want the transient-streak trip named — the reason must say WHY it stuck (2-strike), not just the last error", why)
	}
}

// sparseTok tokenizes ~100 bytes per token: the REAL count sits far BELOW the
// chars/4 estimate, the regime where the estimate would misreport a
// verified-fitting request as exhausted forever.
type sparseTok struct{}

func (sparseTok) Pieces(_ context.Context, text string) ([]int, bool) {
	var lens []int
	for len(text) > 0 {
		n := 100
		if n > len(text) {
			n = len(text)
		}
		// back off to a rune boundary so pieces reconstruct the bytes exactly
		for n > 0 && n < len(text) && !utf8.RuneStart(text[n]) {
			n--
		}
		lens = append(lens, n)
		text = text[n:]
	}
	return lens, true
}

// The loop judges exhaustion by the tokenizer's verdict, not the estimate —
// both directions, at the REAL call site (Run):
func TestLoopExhaustionFollowsRealVerdict(t *testing.T) {
	long := strings.Repeat("tool output line with routine content\n", 200)
	newClient := func() *fakeClient {
		return &fakeClient{script: []Completion{
			{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "read_file", `{"p":"a"}`)}}, FinishReason: "tool_calls"},
			{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
		}}
	}
	tools := []Tool{{ToolSpec: ToolSpec{Name: "read_file"}, Exec: func(_ context.Context, _ string) (string, error) { return long, nil }}}

	// Direction 1 (fitReal): the estimate overflows the 256-token floor on
	// every step, but the sparse REAL count fits comfortably — a verified-
	// fitting run must report ZERO exhausted compactions.
	loop := NewLoop(newClient(), tools, 5).WithSystem(strings.Repeat("big system prompt ", 100)).
		WithContextTokens(300).WithMaxTokens(64).WithToolResultCap(len(long) + 1).WithTokenizer(sparseTok{})
	res, err := loop.Run(context.Background(), "objective")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.CompactionsExhausted != 0 {
		t.Fatalf("CompactionsExhausted = %d on a run the tokenizer VERIFIED fits — the estimate overruled the real yardstick", res.CompactionsExhausted)
	}
	if res.TokenizerPath != "token-exact" {
		t.Fatalf("TokenizerPath = %q, want token-exact", res.TokenizerPath)
	}

	// Direction 2 (overReal): with a one-token-per-rune tokenizer the huge
	// FORCED preamble alone overflows the real floor — the run must count it,
	// even though nothing here consults the estimate.
	loop2 := NewLoop(newClient(), tools, 5).WithSystem(strings.Repeat("big system prompt ", 100)).
		WithContextTokens(300).WithMaxTokens(64).WithToolResultCap(len(long) + 1).WithTokenizer(&runeTok{})
	res2, err := loop2.Run(context.Background(), "objective")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res2.CompactionsExhausted == 0 {
		t.Fatal("forced keeps exceed the REAL budget but CompactionsExhausted = 0 — the real overflow was hidden")
	}
}
