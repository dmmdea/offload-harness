package occontext

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 0.05 }

func pct(t *testing.T, p *float64) float64 {
	t.Helper()
	if p == nil {
		t.Fatal("percentage is nil")
	}
	return *p
}

// A child session is attributed through session.parent_id — never through the message's
// agent name. The zero-token subtask launcher the parent writes is a stub, not a call.
func TestChildSessionAttribution(t *testing.T) {
	f := newFixture(t)
	f.session("ses_p", "", "primary", t0)
	f.user("ses_p", t0, text("go"))
	f.assistant("ses_p", call{at: t0.Add(time.Second), prompt: 11000, output: 40, reasoning: 30})
	f.assistant("ses_p", call{agent: "offload", at: t0.Add(time.Minute), stub: true,
		tools: []toolOut{{name: "task", output: "child result"}}})
	f.assistant("ses_p", call{at: t0.Add(3 * time.Minute), prompt: 11800, cached: 10976, output: 20, reasoning: 10})

	f.session("ses_c", "ses_p", "child", t0.Add(time.Minute))
	f.user("ses_c", t0.Add(time.Minute), text("leg"))
	f.assistant("ses_c", call{agent: "offload", at: t0.Add(61 * time.Second), prompt: 24000, output: 10, reasoning: 500})
	f.assistant("ses_c", call{agent: "offload", at: t0.Add(2 * time.Minute), prompt: 25000, cached: 23520, output: 10, reasoning: 200})

	// A primary session driven by the offload agent directly is primary/offload.
	f.session("ses_q", "", "primary on offload", t0.Add(time.Hour))
	f.user("ses_q", t0.Add(time.Hour), text("direct"))
	f.assistant("ses_q", call{agent: "offload", at: t0.Add(time.Hour + time.Second), prompt: 13000, output: 5})

	r := f.analyze(Options{})

	c := findSession(t, r, "ses_c")
	if c.Kind != "child" || c.ParentID != "ses_p" {
		t.Fatalf("ses_c kind=%q parent=%q, want child of ses_p", c.Kind, c.ParentID)
	}
	p := findSession(t, r, "ses_p")
	if p.Kind != "primary" || len(p.Calls) != 2 || p.Stubs != 1 {
		t.Fatalf("ses_p kind=%q calls=%d stubs=%d, want primary with 2 calls and 1 stub", p.Kind, len(p.Calls), p.Stubs)
	}

	child := findGroup(t, r, "child", "offload")
	if child.Calls != 2 || child.Sessions != 1 || len(child.FirstCallPrompts) != 1 || child.FirstCallPrompts[0] != 24000 {
		t.Fatalf("child/offload = %+v, want 2 calls in 1 session, first call 24000", child)
	}
	build := findGroup(t, r, "primary", "build")
	if build.Calls != 2 || len(build.FirstCallPrompts) != 1 || build.FirstCallPrompts[0] != 11000 {
		t.Fatalf("primary/build = %+v, want 2 calls, first call 11000", build)
	}
	direct := findGroup(t, r, "primary", "offload")
	if direct.Calls != 1 || direct.FirstCallPrompts[0] != 13000 {
		t.Fatalf("primary/offload = %+v, want the one direct call (13000), not the stub", direct)
	}
}

// Cache figures from before --cache-valid-since are reporting artifacts: those calls stay
// listed (prompt, first-call size) but leave every cache ratio.
func TestCacheValidSinceExcludesEarlierRows(t *testing.T) {
	build := func(f *fixture) {
		f.session("ses_1", "", "early", t0)
		f.assistant("ses_1", call{at: t0.Add(time.Second), prompt: 10000})
		f.assistant("ses_1", call{at: t0.Add(time.Minute), prompt: 11000}) // warm, but logged 0
		f.session("ses_2", "", "cold start", t0.Add(2*time.Hour))
		f.assistant("ses_2", call{at: t0.Add(2*time.Hour + time.Second), prompt: 12000})
		f.assistant("ses_2", call{at: t0.Add(2*time.Hour + time.Minute), prompt: 13000, cached: 10976})
		f.assistant("ses_2", call{at: t0.Add(2*time.Hour + 2*time.Minute), prompt: 14000, cached: 12544})
		f.session("ses_3", "", "warm start", t0.Add(3*time.Hour))
		f.assistant("ses_3", call{at: t0.Add(3*time.Hour + time.Second), prompt: 12500, cached: 10976})
		f.assistant("ses_3", call{at: t0.Add(3*time.Hour + time.Minute), prompt: 13500, cached: 12544})
	}
	f := newFixture(t)
	build(f)
	cut := t0.Add(time.Hour)
	r := f.analyze(Options{CacheValidSince: []Cutoff{{Since: cut}}})

	if r.Cache.ValidCalls != 5 || r.Cache.ExcludedCalls != 2 {
		t.Fatalf("valid=%d excluded=%d, want 5 and 2", r.Cache.ValidCalls, r.Cache.ExcludedCalls)
	}
	if r.Cache.Prompt != 65000 || r.Cache.Cached != 47040 || !near(pct(t, r.Cache.Pct), 72.37) {
		t.Fatalf("overall = %d/%d %.2f%%, want 47040/65000 72.37%%", r.Cache.Cached, r.Cache.Prompt, pct(t, r.Cache.Pct))
	}
	// Excluding EACH session's first call drops 12,000 and the warm 12,500.
	if r.Cache.ExclFirstPrompt != 40500 || r.Cache.ExclFirstCached != 36064 || !near(pct(t, r.Cache.ExclFirstPct), 89.05) {
		t.Fatalf("excl-first = %d/%d, want 36064/40500 89.05%%", r.Cache.ExclFirstCached, r.Cache.ExclFirstPrompt)
	}
	// Excluding only COLD session-first calls (cached 0) drops 12,000 alone.
	if r.Cache.ExclColdFirstPrompt != 53000 || !near(pct(t, r.Cache.ExclColdFirstPct), 88.75) {
		t.Fatalf("excl-cold-first = %d/%d, want 47040/53000 88.75%%", r.Cache.ExclColdFirstCached, r.Cache.ExclColdFirstPrompt)
	}
	s1 := findSession(t, r, "ses_1")
	if s1.Calls[0].CacheValid || s1.Calls[1].CacheValid || s1.Calls[0].Prompt != 10000 {
		t.Fatalf("pre-cutoff calls must stay listed but invalid for cache: %+v", s1.Calls)
	}
	g := findGroup(t, r, "primary", "build")
	if len(g.FirstCallPrompts) != 3 || g.FirstCallPrompts[0] != 10000 {
		t.Fatalf("first-call prompts %v must still include the pre-cutoff session", g.FirstCallPrompts)
	}

	// Without the cutoff the artifacts drag the overall figure down.
	f2 := newFixture(t)
	build(f2)
	if r2 := f2.analyze(Options{}); r2.Cache.Prompt != 86000 || !near(pct(t, r2.Cache.Pct), 54.70) {
		t.Fatalf("no cutoff: %d/%d, want 47040/86000", r2.Cache.Cached, r2.Cache.Prompt)
	}
	// A cutoff scoped to another model does not touch this one.
	f3 := newFixture(t)
	build(f3)
	if r3 := f3.analyze(Options{CacheValidSince: []Cutoff{{Model: "m-other", Since: cut}}}); r3.Cache.Prompt != 86000 {
		t.Fatalf("a cutoff for m-other excluded m-seat calls: prompt %d", r3.Cache.Prompt)
	}
	// A cutoff scoped to this model applies.
	f4 := newFixture(t)
	build(f4)
	if r4 := f4.analyze(Options{CacheValidSince: []Cutoff{{Model: "m-seat", Since: cut}}}); r4.Cache.Prompt != 65000 {
		t.Fatalf("a cutoff for m-seat did not apply: prompt %d", r4.Cache.Prompt)
	}
}

// With no cutoff given, warm calls that logged 0 cached before the model's first nonzero
// report get a hint naming the model — the operator decides whether they are artifacts.
func TestHintWhenWarmCallsReportZeroBeforeFirstCacheHit(t *testing.T) {
	f := newFixture(t)
	f.session("ses_1", "", "early", t0)
	f.assistant("ses_1", call{at: t0.Add(time.Second), prompt: 10000})
	f.assistant("ses_1", call{at: t0.Add(time.Minute), prompt: 11000})
	f.session("ses_2", "", "late", t0.Add(2*time.Hour))
	f.assistant("ses_2", call{at: t0.Add(2*time.Hour + time.Second), prompt: 12000})
	f.assistant("ses_2", call{at: t0.Add(2*time.Hour + time.Minute), prompt: 13000, cached: 10976})
	r := f.analyze(Options{})
	if len(r.Hints) != 1 || !strings.Contains(r.Hints[0], "m-seat") || !strings.Contains(r.Hints[0], "--cache-valid-since") {
		t.Fatalf("hints = %q, want one naming m-seat and --cache-valid-since", r.Hints)
	}
	if r2 := f.analyze(Options{CacheValidSince: []Cutoff{{Since: t0.Add(time.Hour)}}}); len(r2.Hints) != 0 {
		t.Fatalf("hint must go away once a cutoff is given: %q", r2.Hints)
	}
}

// Compaction: the compaction agent's summary call is an event with before/after prompt
// sizes; the growth pair that would span it is not growth.
func TestCompactionDetection(t *testing.T) {
	f := newFixture(t)
	f.session("ses_k", "", "long", t0)
	f.user("ses_k", t0, text("start"))
	f.assistant("ses_k", call{at: t0.Add(time.Second), prompt: 88000, cached: 86240, output: 900, reasoning: 400})
	f.assistant("ses_k", call{at: t0.Add(time.Minute), prompt: 90000, cached: 87808, output: 2000, reasoning: 1000})
	f.user("ses_k", t0.Add(2*time.Minute), compactionPart(true, true))
	f.assistant("ses_k", call{agent: "compaction", summary: true, at: t0.Add(2 * time.Minute), prompt: 20000,
		output: 6000, reasoning: 4000, wall: 3 * time.Minute})
	f.user("ses_k", t0.Add(5*time.Minute), synthetic("continue"))
	f.assistant("ses_k", call{at: t0.Add(5 * time.Minute), prompt: 70000, output: 100, reasoning: 50})
	f.assistant("ses_k", call{at: t0.Add(6 * time.Minute), prompt: 71000, cached: 68992, output: 100, reasoning: 50})

	r := f.analyze(Options{})
	s := findSession(t, r, "ses_k")
	if len(s.Compactions) != 1 {
		t.Fatalf("compactions = %d, want 1", len(s.Compactions))
	}
	c := s.Compactions[0]
	if !c.Auto || !c.Overflow {
		t.Errorf("auto=%v overflow=%v, want both true (from the compaction part)", c.Auto, c.Overflow)
	}
	if c.BeforePrompt != 90000 || c.BeforeTotal != 93000 {
		t.Errorf("before = %d prompt / %d total, want 90000 / 93000", c.BeforePrompt, c.BeforeTotal)
	}
	if c.Prompt != 20000 || c.Output != 6000 || c.Reasoning != 4000 || !near(c.WallSec, 180) {
		t.Errorf("compaction call = %+v, want prompt 20000 out 6000 reasoning 4000 wall 180 s", c)
	}
	if c.AfterPrompt == nil || *c.AfterPrompt != 70000 {
		t.Errorf("after prompt = %v, want 70000", c.AfterPrompt)
	}
	// Pairs: 88k->90k and 70k->71k. Nothing across the compaction.
	if s.Growth.Pairs != 2 || s.Growth.Delta != 3000 {
		t.Fatalf("growth pairs=%d delta=%d, want 2 pairs summing 3000 (none across the compaction)", s.Growth.Pairs, s.Growth.Delta)
	}
	// The summary call itself is a primary/compaction call; the event is counted on the
	// group whose context was compacted.
	if g := findGroup(t, r, "primary", "compaction"); g.Calls != 1 || g.Compactions != 0 {
		t.Fatalf("primary/compaction = %+v, want the one summary call and no event", g)
	}
	if g := findGroup(t, r, "primary", "build"); g.Compactions != 1 {
		t.Fatalf("primary/build compactions = %d, want 1", g.Compactions)
	}

	// A server that counts reasoning inside output (llama.cpp): the event's reasoning is
	// estimated from the stored reasoning text and flagged.
	f.user("ses_k", t0.Add(7*time.Minute), compactionPart(true, false))
	f.assistant("ses_k", call{agent: "compaction", summary: true, at: t0.Add(7 * time.Minute), prompt: 21000,
		output: 7000, reasoningText: strings.Repeat("r", 3900)})
	r = f.analyze(Options{})
	s = findSession(t, r, "ses_k")
	if len(s.Compactions) != 2 {
		t.Fatalf("compactions = %d, want 2", len(s.Compactions))
	}
	if k := s.Compactions[1]; k.Reasoning != 1000 || !k.ReasoningEstimated || k.Output != 7000 || k.Overflow || k.AfterPrompt != nil {
		t.Fatalf("second compaction = %+v, want reasoning ~1000 (estimated), output 7000 as reported, not overflow, no after-prompt", k)
	}
	if s.Compactions[0].ReasoningEstimated {
		t.Error("the first compaction's reasoning was reported; it must not be flagged estimated")
	}
}

// Growth between consecutive calls splits into the previous call's replayed reasoning and
// output (server-reported), tool outputs and user text (estimated from bytes), and a residual.
func TestReasoningGrowthAttribution(t *testing.T) {
	f := newFixture(t)
	f.session("ses_g", "", "growth", t0)
	f.user("ses_g", t0, text("start"))
	f.assistant("ses_g", call{at: t0.Add(time.Second), prompt: 10000, output: 50, reasoning: 300,
		tools: []toolOut{{name: "read", output: strings.Repeat("x", 270)}}}) // 270 B / 2.7 = 100
	f.user("ses_g", t0.Add(time.Minute), text(strings.Repeat("y", 39))) // 39 B / 3.9 = 10
	f.assistant("ses_g", call{at: t0.Add(2 * time.Minute), prompt: 10485, cached: 9408, output: 180,
		reasoningText: strings.Repeat("z", 390)}) // server reported reasoning 0: estimate 100 from the text
	f.assistant("ses_g", call{at: t0.Add(3 * time.Minute), prompt: 10665, cached: 9408, output: 10})

	r := f.analyze(Options{})
	s := findSession(t, r, "ses_g")
	g1 := s.Calls[1].Growth
	if g1 == nil {
		t.Fatal("call 2 has no growth record")
	}
	if g1.Delta != 485 || g1.Reasoning != 300 || g1.Output != 50 || g1.ToolOutput != 100 || g1.UserText != 10 || g1.Residual != 25 {
		t.Fatalf("growth into call 2 = %+v, want delta 485 = reasoning 300 + output 50 + tool 100 + user 10 + residual 25", *g1)
	}
	if g1.ReasoningEstimated {
		t.Error("reasoning was reported by the server; it must not be flagged estimated")
	}
	g2 := s.Calls[2].Growth
	if g2 == nil || g2.Delta != 180 || g2.Reasoning != 100 || !g2.ReasoningEstimated || g2.Output != 80 || g2.Residual != 0 {
		t.Fatalf("growth into call 3 = %+v, want reasoning 100 (estimated, taken out of output 180 -> 80), residual 0", g2)
	}
	// The call row keeps the server's figures and carries the estimate beside them.
	if c := s.Calls[1]; c.Reasoning != 0 || c.Output != 180 || c.ReasoningEst == nil || *c.ReasoningEst != 100 {
		t.Fatalf("call 2 row = reasoning %d output %d est %v, want reported 0/180 and est 100", c.Reasoning, c.Output, c.ReasoningEst)
	}
	if s.Calls[0].ReasoningEst != nil {
		t.Error("call 1's reasoning was reported; it must carry no estimate")
	}
	if s.Growth.Pairs != 2 || s.Growth.Delta != 665 || s.Growth.Reasoning != 400 {
		t.Fatalf("session growth = %+v", s.Growth)
	}
	if !near(pct(t, s.Growth.ReasoningShare), 60.15) || !near(pct(t, s.Growth.ToolOutputShare), 15.04) {
		t.Fatalf("reasoning share %.2f%% tool share %.2f%%, want 60.15%% and 15.04%%",
			pct(t, s.Growth.ReasoningShare), pct(t, s.Growth.ToolOutputShare))
	}
	// The group rolls the same pairs up.
	if g := findGroup(t, r, "primary", "build"); g.Growth.Delta != 665 || !near(pct(t, g.Growth.ReasoningShare), 60.15) {
		t.Fatalf("group growth = %+v", g.Growth)
	}
}

// A model switch mid-session is not growth: the next prompt is a different render.
func TestGrowthSkipsModelSwitch(t *testing.T) {
	f := newFixture(t)
	f.session("ses_m", "", "switch", t0)
	f.assistant("ses_m", call{at: t0.Add(time.Second), prompt: 10000, output: 10})
	f.assistant("ses_m", call{model: "m-other", at: t0.Add(time.Minute), prompt: 30000, output: 10})
	r := f.analyze(Options{})
	s := findSession(t, r, "ses_m")
	if s.Growth.Pairs != 0 || s.Growth.SkippedModelSwitch != 1 || s.Calls[1].Growth != nil {
		t.Fatalf("growth = %+v, want the switch skipped", s.Growth)
	}
}

func TestCacheBlocksAndTTFT(t *testing.T) {
	f := newFixture(t)
	f.session("ses_b", "", "blocks", t0)
	f.assistant("ses_b", call{at: t0, prompt: 24680, ttft: 35137 * time.Millisecond, wall: 65 * time.Second})
	f.assistant("ses_b", call{at: t0.Add(2 * time.Minute), prompt: 25734, cached: 25088, ttft: 997 * time.Millisecond})
	f.assistant("ses_b", call{at: t0.Add(3 * time.Minute), prompt: 26000, cached: 25000})
	r := f.analyze(Options{CacheBlock: 1568})
	s := findSession(t, r, "ses_b")
	if s.Calls[1].CachedBlocks == nil || *s.Calls[1].CachedBlocks != 16 || s.Calls[1].BlockRemainder != 0 {
		t.Fatalf("25088 cached = %v blocks rem %d, want 16 x 1568", s.Calls[1].CachedBlocks, s.Calls[1].BlockRemainder)
	}
	if s.Calls[2].BlockRemainder != 25000%1568 || r.Cache.BlockMisaligned != 1 {
		t.Fatalf("25000 cached: remainder %d misaligned %d, want %d and 1", s.Calls[2].BlockRemainder, r.Cache.BlockMisaligned, 25000%1568)
	}
	if s.Calls[0].TTFTSec == nil || !near(*s.Calls[0].TTFTSec, 35.137) || s.Calls[0].WallSec == nil || !near(*s.Calls[0].WallSec, 65) {
		t.Fatalf("ttft=%v wall=%v, want 35.137 s and 65 s", s.Calls[0].TTFTSec, s.Calls[0].WallSec)
	}
	g := findGroup(t, r, "primary", "build")
	if len(g.TTFTFirstSec) != 1 || !near(g.TTFTFirstSec[0], 35.137) || g.TTFTWarmMedianSec == nil || !near(*g.TTFTWarmMedianSec, 1.0) {
		t.Fatalf("group ttft first=%v warm median=%v", g.TTFTFirstSec, g.TTFTWarmMedianSec)
	}
}

// Selection: the N most recent PRIMARY sessions, each with its child sessions.
func TestSelectLastPrimariesWithTheirChildren(t *testing.T) {
	f := newFixture(t)
	for i, id := range []string{"ses_a", "ses_b", "ses_c"} {
		at := t0.Add(time.Duration(i) * time.Hour)
		f.session(id, "", id, at)
		f.assistant(id, call{at: at.Add(time.Second), prompt: 1000 + i})
	}
	f.session("ses_bc", "ses_b", "child of b", t0.Add(time.Hour+time.Minute))
	f.assistant("ses_bc", call{agent: "offload", at: t0.Add(time.Hour + 2*time.Minute), prompt: 5000})
	r := f.analyze(Options{Last: 2})
	var ids []string
	for _, s := range r.Sessions {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, ",") != "ses_b,ses_bc,ses_c" {
		t.Fatalf("selected %v, want ses_b,ses_bc,ses_c (oldest first, child after its parent)", ids)
	}
	r2 := f.analyze(Options{Sessions: []string{"ses_b"}})
	if len(r2.Sessions) != 2 {
		t.Fatalf("--session ses_b selected %d sessions, want it and its child", len(r2.Sessions))
	}
}

// The report is safe to paste: no session text, tool output or (by default) title.
func TestReportCarriesNoSessionContentByDefault(t *testing.T) {
	f := newFixture(t)
	f.session("ses_s", "", "SECRET-TITLE", t0)
	f.user("ses_s", t0, text("SECRET-USER-TEXT"))
	f.assistant("ses_s", call{at: t0.Add(time.Second), prompt: 1000, output: 5, reasoning: 5, reasoningText: "SECRET-REASONING",
		text: "SECRET-ASSISTANT", tools: []toolOut{{name: "read", output: "SECRET-TOOL-OUTPUT"}}})
	f.assistant("ses_s", call{at: t0.Add(time.Minute), prompt: 1100, output: 5})
	for _, titles := range []bool{false, true} {
		r := f.analyze(Options{Titles: titles})
		var tbl bytes.Buffer
		WriteTable(&tbl, r)
		js, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		for name, out := range map[string]string{"table": tbl.String(), "json": string(js)} {
			for _, secret := range []string{"SECRET-USER-TEXT", "SECRET-REASONING", "SECRET-ASSISTANT", "SECRET-TOOL-OUTPUT"} {
				if strings.Contains(out, secret) {
					t.Errorf("%s (titles=%v) leaks %s", name, titles, secret)
				}
			}
			if strings.Contains(out, "SECRET-TITLE") != titles {
				t.Errorf("%s: title present=%v with titles=%v", name, strings.Contains(out, "SECRET-TITLE"), titles)
			}
		}
		if !strings.Contains(tbl.String(), "ses_s") || !strings.Contains(tbl.String(), "1,100") {
			t.Errorf("table misses the session or its figures:\n%s", tbl.String())
		}
	}
}

func fileSum(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func countSessions(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM session").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The snapshot copies the db AND its -wal and -shm, so rows that live only in an
// uncheckpointed WAL (a live opencode) are read — and the source files are never touched.
func TestSnapshotCopiesWALAndLeavesSourceUntouched(t *testing.T) {
	f := newFixture(t)
	f.exec("PRAGMA journal_mode=WAL")
	f.exec("PRAGMA wal_autocheckpoint=0")
	f.session("ses_ckpt", "", "checkpointed", t0)
	f.exec("PRAGMA wal_checkpoint(TRUNCATE)")
	for i := 0; i < 3; i++ { // these rows exist only in the WAL: the writer stays open
		f.session(f.id("ses"), "", "wal only", t0.Add(time.Duration(i+1)*time.Minute))
	}
	wal := f.path + "-wal"
	if st, err := os.Stat(wal); err != nil || st.Size() == 0 {
		t.Fatalf("fixture has no live WAL (%v)", err)
	}
	before := map[string][32]byte{"db": fileSum(t, f.path), "wal": fileSum(t, wal)}

	// Control: the main file alone is missing the WAL rows, so this test discriminates.
	ctl := filepath.Join(t.TempDir(), "main-only.db")
	raw, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ctl, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if n := countSessions(t, ctl); n != 1 {
		t.Fatalf("control: main file alone holds %d sessions, want 1", n)
	}

	snap, err := Snapshot(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(snap.Path) == filepath.Dir(f.path) {
		t.Fatal("snapshot must live in its own temp dir")
	}
	var n int
	if err := snap.DB.QueryRow("SELECT count(*) FROM session").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("snapshot reads %d sessions, want 4 (1 checkpointed + 3 WAL-only)", n)
	}
	if got := strings.Join(snap.Info.Copied, ","); got != "opencode.db,opencode.db-wal,opencode.db-shm" {
		t.Fatalf("copied %q, want the db, -wal and -shm", got)
	}
	if !snap.Info.QuickCheckOK {
		t.Fatal("quick_check not recorded ok")
	}
	dir := filepath.Dir(snap.Path)
	snap.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("snapshot dir survives Close (%v)", err)
	}
	after := map[string][32]byte{"db": fileSum(t, f.path), "wal": fileSum(t, wal)}
	if before["db"] != after["db"] || before["wal"] != after["wal"] {
		t.Fatal("the source db or WAL changed: the instrument must be read-only")
	}
}

// A source that changes between copying the db and its WAL is copied again, so a live
// writer cannot hand the reader a torn db/WAL pair.
func TestSnapshotRetriesWhenSourceChangesMidCopy(t *testing.T) {
	f := newFixture(t)
	f.exec("PRAGMA journal_mode=WAL")
	f.exec("PRAGMA wal_autocheckpoint=0")
	f.session("ses_1", "", "one", t0)
	attempts := 0
	snap, err := snapshot(f.path, func(attempt int) {
		attempts = attempt
		if attempt == 1 { // a live writer lands between the copies of attempt 1
			f.session("ses_2", "", "two", t0.Add(time.Minute))
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if attempts < 2 || snap.Info.Attempts < 2 {
		t.Fatalf("attempts=%d info=%d, want a retry after the source changed", attempts, snap.Info.Attempts)
	}
	var n int
	if err := snap.DB.QueryRow("SELECT count(*) FROM session").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("snapshot reads %d sessions, want 2 (the retry picks up the late write)", n)
	}
}

func TestSnapshotRejectsWhatIsNotAnOpencodeDB(t *testing.T) {
	dir := t.TempDir()
	junk := filepath.Join(dir, "opencode.db")
	if err := os.WriteFile(junk, bytes.Repeat([]byte("not a database "), 512), 0o600); err != nil {
		t.Fatal(err)
	}
	if snap, err := Snapshot(junk); err == nil {
		snap.Close()
		t.Fatal("a non-sqlite file must be refused")
	}
	if _, err := Snapshot(filepath.Join(dir, "missing.db")); err == nil {
		t.Fatal("a missing db must be refused")
	}
	empty := filepath.Join(dir, "empty.db")
	db, err := sql.Open("sqlite", empty)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE other (x int)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	snap, err := Snapshot(empty)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if _, err := Load(snap.DB); err == nil || !strings.Contains(err.Error(), "session") {
		t.Fatalf("a db without opencode's tables must be refused naming the table, got %v", err)
	}
}

func TestParseCutoff(t *testing.T) {
	for in, want := range map[string]Cutoff{
		"2026-09-22T17:31:00Z":                      {Since: time.Date(2026, 9, 22, 17, 31, 0, 0, time.UTC)},
		"m-seat=2026-09-22T17:31:00Z":               {Model: "m-seat", Since: time.Date(2026, 9, 22, 17, 31, 0, 0, time.UTC)},
		"llamacpp/m-seat=2026-09-22T12:31:00-05:00": {Model: "llamacpp/m-seat", Since: time.Date(2026, 9, 22, 17, 31, 0, 0, time.UTC)},
	} {
		got, err := ParseCutoff(in)
		if err != nil || got.Model != want.Model || !got.Since.Equal(want.Since) {
			t.Errorf("ParseCutoff(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	local, err := ParseCutoff("2026-09-22 12:31")
	if err != nil || !local.Since.Equal(time.Date(2026, 9, 22, 12, 31, 0, 0, time.Local)) {
		t.Errorf("local form = %+v, %v", local, err)
	}
	if _, err := ParseCutoff("yesterday"); err == nil {
		t.Error("an unparseable cutoff must be an error")
	}
}
