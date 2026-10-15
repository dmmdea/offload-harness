package compeval

// The compaction eval can switch on the two optional rungs (search results,
// log collapse) in every instrument: the deterministic replay, the A/B and the
// KV bench. These tests use transcripts whose older turns hold a search listing
// or a long repetitive build log, so the rung fires.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
)

const rungObjective = "Find where the fetch timeout is enforced and explain why a request fails with a deadline error."

// rungListing is a grouped search listing: 20 files, 5 hits each, one relevant hit.
func rungListing() string {
	var b strings.Builder
	benign := []string{
		"let alpha = beta + gamma", "return delta", "for i := range items {", "value := lookup(key)",
		"x = y * 2", "count++", "names = append(names, n)", "// advance the cursor",
	}
	for f := 0; f < 20; f++ {
		fmt.Fprintf(&b, "internal/pkg%02d/file%02d.go:\n", f, f)
		for h := 0; h < 5; h++ {
			text := benign[(f*5+h)%len(benign)]
			if f == 7 && h == 2 {
				text = "const fetchTimeout = 30 // exceeded on a large request"
			}
			fmt.Fprintf(&b, "  L%d: %s\n", 10+h*7, text)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// rungLog is a server log: a long run of near-identical access lines with one
// warning and one error among them.
func rungLog(n int) string {
	var b strings.Builder
	b.WriteString("listening on port 18080\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "2026-10-14T09:12:%02d.%03dZ INFO  req=%08x GET /health 200 %dms\n", i%60, (i*37)%1000, uint32(i)*2654435761, 3+i%7)
		if i == n/3 {
			b.WriteString("2026-10-14T09:12:40.001Z WARN  slow request req=0badc0de took 2300ms\n")
		}
		if i == 2*n/3 {
			b.WriteString("2026-10-14T09:12:55.500Z ERROR upstream connection refused host=cache port=9000\n")
		}
	}
	b.WriteString("shutdown complete\n")
	return b.String()
}

func rungFile(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%4d  func step%d(ctx context.Context) error { return run(ctx, %d) }\n", i, i, i*3)
	}
	return b.String()
}

func rungEntry(id, objective string, budget int, calls ...[3]string) Entry {
	turns := []Turn{
		{Role: "system", Content: "You are a coding agent. Use the tools to answer."},
		{Role: "user", Content: objective},
	}
	for i, c := range calls {
		cid := fmt.Sprintf("c%d", i+1)
		turns = append(turns,
			Turn{Role: "assistant", ToolCalls: []TurnToolCall{{ID: cid, Name: c[0], Args: c[1]}}},
			Turn{Role: "tool", ToolCallID: cid, Content: c[2]},
		)
	}
	turns = append(turns, Turn{Role: "assistant", Content: "reading the result now"})
	return Entry{ID: id, Kind: "mixed", Turns: turns, BudgetTokens: budget, KeepRecent: 4, ProtectedPrefix: 2}
}

// rungEntries: one entry only the search rung can change, one only the log rung
// can change, and one the ladder never touches.
func rungEntries() (search, logs, plain Entry) {
	search = rungEntry("search", rungObjective, 0,
		[3]string{"search_files", `{"pattern":"timeout"}`, rungListing()},
		[3]string{"read_file", `{"path":"internal/pkg07/file07.go"}`, rungFile(30)},
		[3]string{"read_file", `{"path":"internal/pkg08/file08.go"}`, rungFile(30)},
		[3]string{"read_file", `{"path":"internal/pkg09/file09.go"}`, rungFile(10)},
	)
	logs = rungEntry("logs", "Find out why the widget build is red.", 0,
		[3]string{"run_shell", `{"command":"go test ./..."}`, rungLog(170)},
		[3]string{"run_shell", `{"command":"go build ./..."}`, rungLog(150)},
		[3]string{"read_file", `{"path":"widget.go"}`, rungFile(30)},
		[3]string{"read_file", `{"path":"widget_test.go"}`, rungFile(10)},
	)
	plain = rungEntry("plain", "Read the file.", 1<<20,
		[3]string{"read_file", `{"path":"a.go"}`, rungFile(20)},
		[3]string{"read_file", `{"path":"b.go"}`, rungFile(21)},
		[3]string{"read_file", `{"path":"c.go"}`, rungFile(22)},
	)
	return
}

var (
	rungFooterRE = regexp.MustCompile(`(?m)^\[\.\.\. \d+ of \d+ lines elided, \d+ of \d+ files elided \.\.\.\]$`)
	rungHeaderRE = regexp.MustCompile(`\[log-collapse: original \d+ chars, \d+ lines\]`)
)

func incumbent() LadderOpts { return LadderOpts{GCF: true, Skeleton: true} }

func withSearch(o LadderOpts) LadderOpts { o.Search = true; return o }
func withLogs(o LadderOpts) LadderOpts   { o.Logs = true; return o }

func TestLadderOptsLabelNamesEveryArm(t *testing.T) {
	cases := []struct {
		o    LadderOpts
		want string
	}{
		{LadderOpts{}, "base"},
		{LadderOpts{GCF: true}, "gcf"},
		{LadderOpts{Skeleton: true}, "skeleton"},
		{LadderOpts{GCF: true, Skeleton: true}, "gcf+skeleton"},
		{LadderOpts{Search: true}, "search"},
		{LadderOpts{Logs: true}, "logs"},
		{withSearch(incumbent()), "gcf+skeleton+search"},
		{withLogs(incumbent()), "gcf+skeleton+logs"},
		{withLogs(withSearch(incumbent())), "gcf+skeleton+search+logs"},
	}
	for _, c := range cases {
		if got := c.o.Label(); got != c.want {
			t.Errorf("%+v: label %q, want %q", c.o, got, c.want)
		}
	}
}

func tokensAfterByID(rep Report) map[string]int {
	m := map[string]int{}
	for _, e := range rep.Entries {
		m[e.ID] = e.TokensAfter
	}
	return m
}

func TestEvaluateRungFlagsChangeOnlyTheirOwnEntries(t *testing.T) {
	s, l, p := rungEntries()
	entries := []Entry{s, l, p}
	base := Evaluate(entries, "h", incumbent())
	search := Evaluate(entries, "h", withSearch(incumbent()))
	logs := Evaluate(entries, "h", withLogs(incumbent()))

	if search.Ladder != "gcf+skeleton+search" || logs.Ladder != "gcf+skeleton+logs" {
		t.Fatalf("report ladders %q / %q", search.Ladder, logs.Ladder)
	}
	b, sr, lg := tokensAfterByID(base), tokensAfterByID(search), tokensAfterByID(logs)
	if sr["search"] == b["search"] {
		t.Errorf("Search did not change the search entry (tokens_after %d both ways)", b["search"])
	}
	if sr["logs"] != b["logs"] || sr["plain"] != b["plain"] {
		t.Errorf("Search changed an entry it cannot fire on: logs %d->%d, plain %d->%d", b["logs"], sr["logs"], b["plain"], sr["plain"])
	}
	if lg["logs"] == b["logs"] {
		t.Errorf("Logs did not change the log entry (tokens_after %d both ways)", b["logs"])
	}
	if lg["search"] != b["search"] || lg["plain"] != b["plain"] {
		t.Errorf("Logs changed an entry it cannot fire on: search %d->%d, plain %d->%d", b["search"], lg["search"], b["plain"], lg["plain"])
	}
}

// recordingScorer scores a transcript by whether it carries the marker the
// control pairs use, and records everything it was asked to read.
type recordingScorer struct{ seen []string }

func (r *recordingScorer) score(_ context.Context, rendered string) (float64, error) {
	r.seen = append(r.seen, rendered)
	if strings.Contains(rendered, "GOOD") {
		return 1, nil
	}
	return 0.5, nil
}

func abPairs() []ControlPair {
	return []ControlPair{{Name: "p", Good: "GOOD answer", Degraded: "weak answer"}}
}

func compactedText(t *testing.T, opts LadderOpts, e Entry) string {
	t.Helper()
	rs := &recordingScorer{}
	if _, err := RunAB(context.Background(), rs.score, []Entry{e}, "h", opts, abPairs(), nil); err != nil {
		t.Fatal(err)
	}
	// Two control-pair calls, then the entry's full and compacted renders.
	if len(rs.seen) != 4 {
		t.Fatalf("scorer saw %d texts, want 4", len(rs.seen))
	}
	return rs.seen[3]
}

func TestRunABRungFlagsSwitchTheRungsOn(t *testing.T) {
	s, l, _ := rungEntries()

	if rungFooterRE.MatchString(compactedText(t, incumbent(), s)) {
		t.Fatal("the search rung ran with Search unset")
	}
	if !rungFooterRE.MatchString(compactedText(t, withSearch(incumbent()), s)) {
		t.Fatal("Search did not switch the search rung on in RunAB")
	}
	if rungHeaderRE.MatchString(compactedText(t, incumbent(), l)) {
		t.Fatal("the log rung ran with Logs unset")
	}
	if !rungHeaderRE.MatchString(compactedText(t, withLogs(incumbent()), l)) {
		t.Fatal("Logs did not switch the log rung on in RunAB")
	}

	rep, err := RunAB(context.Background(), (&recordingScorer{}).score, []Entry{s}, "h", withLogs(withSearch(incumbent())), abPairs(), nil)
	if err != nil || rep.Ladder != "gcf+skeleton+search+logs" {
		t.Fatalf("A/B report ladder %q (err %v)", rep.Ladder, err)
	}
}

// capture runs the bench with a fake server and returns the report together
// with every sent transcript in order.
func capture(t *testing.T, entries []Entry, opts BenchOpts) (KVReport, [][]agent.Msg) {
	t.Helper()
	var sent [][]agent.Msg
	f := &fakeServer{}
	probe := func(ctx context.Context, msgs []agent.Msg) (ProbeResult, error) {
		sent = append(sent, append([]agent.Msg(nil), msgs...))
		return f.probe(ctx, msgs)
	}
	opts.CtxTokens = 8192
	opts.MaxOut = 1024
	rep, err := RunBench(context.Background(), probe, entries, "h", opts)
	if err != nil {
		t.Fatal(err)
	}
	return rep, sent
}

func anySentMatches(sent [][]agent.Msg, re *regexp.Regexp) bool {
	for _, msgs := range sent {
		for _, m := range msgs {
			if m.Role == "tool" && re.MatchString(m.Content) {
				return true
			}
		}
	}
	return false
}

func TestRunBenchRungFlagsSwitchTheRungsOn(t *testing.T) {
	s, l, _ := rungEntries()
	base := BenchOpts{Rounds: 1, RunNonce: "N1", Arms: []string{"on"}}

	opts := base
	opts.Ladder = incumbent()
	if _, sent := capture(t, []Entry{s}, opts); anySentMatches(sent, rungFooterRE) {
		t.Fatal("the search rung ran with Search unset")
	}
	opts.Ladder = withSearch(incumbent())
	rep, sent := capture(t, []Entry{s}, opts)
	if !anySentMatches(sent, rungFooterRE) {
		t.Fatal("Search did not switch the search rung on in RunBench")
	}
	if rep.Ladder != "gcf+skeleton+search" {
		t.Errorf("bench ladder label %q", rep.Ladder)
	}
	fired := 0
	for _, sm := range rep.Samples {
		if sm.Fired {
			fired++
		}
	}
	if fired == 0 {
		t.Error("no sample is marked fired although the rung rewrote the sent bytes")
	}

	opts.Ladder = incumbent()
	if _, sent := capture(t, []Entry{l}, opts); anySentMatches(sent, rungHeaderRE) {
		t.Fatal("the log rung ran with Logs unset")
	}
	opts.Ladder = withLogs(incumbent())
	if _, sent := capture(t, []Entry{l}, opts); !anySentMatches(sent, rungHeaderRE) {
		t.Fatal("Logs did not switch the log rung on in RunBench")
	}
}

func TestRunBenchArmsOnSkipsTheOffArm(t *testing.T) {
	s, _, _ := rungEntries()
	both, bothSent := capture(t, []Entry{s}, BenchOpts{Rounds: 1, RunNonce: "N1", Ladder: incumbent()})
	on, onSent := capture(t, []Entry{s}, BenchOpts{Rounds: 1, RunNonce: "N1", Ladder: incumbent(), Arms: []string{"on"}})

	var bothOff int
	for _, sm := range both.Samples {
		if sm.Arm == "off" {
			bothOff++
		}
	}
	if bothOff == 0 {
		t.Fatal("the default run lost its off arm")
	}
	if len(on.Samples) == 0 {
		t.Fatal("--arms on produced no samples")
	}
	for _, sm := range on.Samples {
		if sm.Arm != "on" {
			t.Fatalf("--arms on ran a %q-arm sample", sm.Arm)
		}
	}
	for i, msgs := range onSent {
		if strings.Contains(msgs[0].Content, "/off]") {
			t.Fatalf("--arms on sent an off-arm request (#%d)", i)
		}
	}
	if len(onSent) >= len(bothSent) {
		t.Errorf("--arms on sent %d requests, the full run %d", len(onSent), len(bothSent))
	}
	for _, a := range on.Arms {
		if a.Arm == "off" {
			t.Error("the report lists stats for an off arm that did not run")
		}
	}
}

func TestRunBenchRejectsAnUnknownArm(t *testing.T) {
	s, _, _ := rungEntries()
	f := &fakeServer{}
	if _, err := RunBench(context.Background(), f.probe, []Entry{s}, "h",
		BenchOpts{Rounds: 1, CtxTokens: 8192, MaxOut: 1024, Arms: []string{"onn"}}); err == nil {
		t.Fatal("an unknown arm name was accepted")
	}
}

var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// samplesByStep keys a run's samples by entry and step.
func samplesByStep(rep KVReport) map[string]Sample {
	m := map[string]Sample{}
	for _, sm := range rep.Samples {
		m[fmt.Sprintf("%s/%d", sm.EntryID, sm.Step)] = sm
	}
	return m
}

func TestSentSHA256ProvesANoOpPairAndCatchesAChangedOne(t *testing.T) {
	s, _, p := rungEntries()
	entries := []Entry{s, p}
	run := func(o LadderOpts) map[string]Sample {
		rep, _ := capture(t, entries, BenchOpts{Rounds: 1, RunNonce: "SHARED", Ladder: o, Arms: []string{"on"}})
		return samplesByStep(rep)
	}
	inc, again, cand := run(incumbent()), run(incumbent()), run(withSearch(incumbent()))

	if len(inc) == 0 {
		t.Fatal("no samples")
	}
	for k, a := range inc {
		if !sha256RE.MatchString(a.SentSHA256) {
			t.Fatalf("%s: sent_sha256 %q is not a sha256", k, a.SentSHA256)
		}
		if again[k].SentSHA256 != a.SentSHA256 {
			t.Errorf("%s: two identical runs with a shared nonce sent different bytes", k)
		}
	}
	// The plain entry is never touched by the rung: its pair is a provable no-op.
	plainSteps := 0
	for k, a := range inc {
		if a.EntryID == "plain" {
			plainSteps++
			if cand[k].SentSHA256 != a.SentSHA256 {
				t.Errorf("%s: a no-op pair has different sent_sha256", k)
			}
		}
	}
	if plainSteps == 0 {
		t.Fatal("no plain-entry samples")
	}
	// The search entry's final step is rewritten by the rung: its hash must move.
	last := fmt.Sprintf("search/%d", len(s.Turns))
	if inc[last].SentSHA256 == "" || inc[last].SentSHA256 == cand[last].SentSHA256 {
		t.Errorf("%s: the rung changed the sent bytes but sent_sha256 did not move (%q vs %q)", last, inc[last].SentSHA256, cand[last].SentSHA256)
	}
	if !cand[last].Fired {
		t.Errorf("%s: the changed step is not marked fired", last)
	}
}

func TestSentSHA256CoversTheSaltedBytes(t *testing.T) {
	s, _, _ := rungEntries()
	a, _ := capture(t, []Entry{s}, BenchOpts{Rounds: 1, RunNonce: "ONE", Ladder: incumbent(), Arms: []string{"on"}})
	b, _ := capture(t, []Entry{s}, BenchOpts{Rounds: 1, RunNonce: "TWO", Ladder: incumbent(), Arms: []string{"on"}})
	ka, kb := samplesByStep(a), samplesByStep(b)
	if len(ka) == 0 {
		t.Fatal("no samples")
	}
	for k := range ka {
		if ka[k].SentSHA256 == kb[k].SentSHA256 {
			t.Fatalf("%s: runs with different nonces report the same sent_sha256, so it is not the hash of the bytes sent", k)
		}
	}
}

func TestOneNonceCoversEveryRequestOfARun(t *testing.T) {
	s, l, _ := rungEntries()
	rep, sent := capture(t, []Entry{s, l}, BenchOpts{Rounds: 2, Ladder: incumbent()})
	if rep.RunNonce == "" {
		t.Fatal("the report carries no nonce")
	}
	re := regexp.MustCompile(`^\[kvbench ([^/\]]+)/`)
	seen := map[string]bool{}
	for i, msgs := range sent {
		m := re.FindStringSubmatch(msgs[0].Content)
		if m == nil {
			t.Fatalf("request %d carries no kvbench marker: %.60q", i, msgs[0].Content)
		}
		seen[m[1]] = true
	}
	if len(seen) != 1 || !seen[rep.RunNonce] {
		t.Fatalf("requests carry nonces %v, the report says %q", seen, rep.RunNonce)
	}
}
