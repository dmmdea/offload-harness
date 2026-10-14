package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gcf"
)

// --- fixtures -------------------------------------------------------------

// lcHead and lcTail are distinct, signal-free lines: the head and tail windows
// a collapsed body must keep verbatim. None of them matches signalLine.
var lcHead = []string{
	"boot: reading the configuration file",
	"boot: opening the listener socket",
	"boot: warming the build cache",
	"boot: loading the route table",
	"boot: starting the worker pool",
	"boot: registering the health probe",
	"boot: attaching the metrics sink",
	"boot: ready to serve requests",
}

var lcTail = []string{
	"shutdown: draining open connections",
	"shutdown: flushing the metrics sink",
	"shutdown: closing the listener socket",
	"shutdown: complete",
}

func lcRepeat(line string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = line
	}
	return out
}

// lcJoin joins line groups into one body with a trailing newline.
func lcJoin(groups ...[]string) string {
	var all []string
	for _, g := range groups {
		all = append(all, g...)
	}
	return strings.Join(all, "\n") + "\n"
}

// lcAccess is an access-log line whose timestamp, request id and latency vary.
func lcAccess(i int) string {
	return fmt.Sprintf("2026-10-14T09:12:%02d.%03dZ INFO  req=%08x GET /health 200 %dms", i%60, (i*37)%1000, uint32(i+1)*2654435761, 3+i%9)
}

func lcAccessRun(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = lcAccess(i)
	}
	return out
}

// lcSubtestLog is `go test -v` output for a table-driven test: the sequential
// subtests announce themselves in a block, then the report lists them in a
// block of indented pass lines with one failure among them.
func lcSubtestLog(n int) string {
	var b strings.Builder
	b.WriteString("go test -v ./widget/...\n=== RUN   TestWidget\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "=== RUN   TestWidget/case_%03d\n", i)
	}
	b.WriteString("--- FAIL: TestWidget (0.02s)\n")
	for i := 0; i < n; i++ {
		if i == n/2 {
			fmt.Fprintf(&b, "    --- FAIL: TestWidget/case_%03d (0.01s)\n        widget_test.go:88: got 3, want 4\n", i)
			continue
		}
		fmt.Fprintf(&b, "    --- PASS: TestWidget/case_%03d (0.00s)\n", i)
	}
	b.WriteString("FAIL\nFAIL\texample.com/widget\t0.412s\n")
	return b.String()
}

// lcLogOptsOn is the rung enabled on its own.
var lcLogOptsOn = compactOpts{LogCollapse: true}

// lcNamed returns a copy of a fixture whose assistant tool calls are all named
// name (the shipped mini corpus carries call ids only).
func lcNamed(fx lrFixture, name string) lrFixture {
	msgs := make([]Msg, len(fx.msgs))
	for i, m := range fx.msgs {
		msgs[i] = m
		if len(m.ToolCalls) > 0 {
			calls := make([]ToolCall, len(m.ToolCalls))
			for j, c := range m.ToolCalls {
				c.Name = name
				calls[j] = c
			}
			msgs[i].ToolCalls = calls
		}
	}
	fx.msgs = msgs
	return fx
}

// lcLadderFixtures is every golden fixture with its tool calls named as the
// command tool, so the rung's producer gate lets the bodies through.
func lcLadderFixtures(t *testing.T) []lrFixture {
	t.Helper()
	var out []lrFixture
	for _, fx := range append(lrMiniCorpus(t), lrSyntheticFixtures()...) {
		out = append(out, lcNamed(fx, "run"))
	}
	return out
}

// --- the matching rules ------------------------------------------------------

func TestLogsKeyMasksNumbersIdsAndTimestamps(t *testing.T) {
	same := [][2]string{
		{"2026-10-14T09:12:01.123Z INFO req=0badc0de GET /health 200 4ms", "2026-10-14T09:12:02.457Z INFO req=9f31a7c2 GET /health 200 11ms"},
		{"worker7 started", "worker12 started"},
		{"downloading 12/100 KB", "downloading 98/100 KB"},
		{"connected to 192.0.2.1 port 8080", "connected to 198.51.100.22 port 9090"},
		{"commit 3f2a9c1d applied", "commit 0b77e4a5 applied"},
		{"id 123e4567-e89b-12d3-a456-426614174000 ready", "id 9b2d4c6e-1a3f-4e5d-8c7b-0a1b2c3d4e5f ready"},
		{"id cafebabe-cafe-face-feed-deadbeefcafe ready", "id 123e4567-e89b-12d3-a456-426614174000 ready"},
		{"trace deadbeefcafe seen", "trace 0123456789ab seen"},
		{"\x1b[32mok\x1b[0m step 3", "ok step 4"},
		{"retrying build cache lookup   ", "retrying build cache lookup"},
		{"retrying build cache lookup\r", "retrying build cache lookup"},
	}
	for _, p := range same {
		if a, b := logsKey(p[0]), logsKey(p[1]); a != b {
			t.Errorf("lines should match after masking:\n  %q -> %q\n  %q -> %q", p[0], a, p[1], b)
		}
	}
	differ := [][2]string{
		{"retry in 5s", "retry in a moment"},
		{"  indented line 1", "indented line 1"},
		{"GET /a 200", "GET /b 200"},
		{"connected to 192.0.2.1", "connected to 192.0.2"},
		{"stage build finished", "stage test finished"},
	}
	for _, p := range differ {
		if a, b := logsKey(p[0]), logsKey(p[1]); a == b {
			t.Errorf("lines must NOT match: %q and %q both -> %q", p[0], p[1], a)
		}
	}
}

func TestLogsKeepRecognisesProblemAndSummaryLines(t *testing.T) {
	keep := []string{
		"ERROR upstream connection refused host=cache",
		"2026-10-14T09:12:40.001Z WARN  slow request took 2300ms",
		"panic: runtime error: index out of range",
		"fatal: unable to access the remote",
		"FAIL",
		"PASS",
		"FAIL\texample.com/widget\t0.412s",
		"ok  \texample.com/widget\t0.311s",
		"--- FAIL: TestWidget_broken (0.01s)",
		"    at com.example.Widget.render(Widget.java:88)",
		`  File "widget.py", line 4, in render`,
		"Traceback (most recent call last):",
		"goroutine 1 [running]:",
		"Ran 5 tests in 0.104s",
		"Tests: 5 passed, 5 total",
		"Test Suites: 1 passed, 1 total",
		"test result: ok. 5 passed; 0 failed",
		"thread 'main' panicked at src/main.rs:4:5",
		"2 failed, 10 passed",
		"",
		"   ",
	}
	for _, l := range keep {
		if !logsKeep(l) {
			t.Errorf("line must be kept verbatim: %q", l)
		}
	}
	routine := []string{
		"info: starting the worker pool",
		"GET /health 200 4ms",
		"downloading module.tgz 12/100 KB",
		"retrying the build cache lookup",
		"=== RUN   TestWidget_001",
	}
	for _, l := range routine {
		if logsKeep(l) {
			t.Errorf("routine line must be foldable: %q", l)
		}
	}
}

// --- the line-level rung -------------------------------------------------------

// TestLogCollapseExactOutput pins the bytes of the simplest case: one run of 48
// identical lines between the head and tail windows.
func TestLogCollapseExactOutput(t *testing.T) {
	run := "retrying the build cache lookup"
	in := lcJoin(lcHead, lcRepeat(run, 48), lcTail)
	got, ok := logCollapse(in)
	if !ok {
		t.Fatal("a 48-line run between the windows must collapse")
	}
	want := fmt.Sprintf("[log-collapse: original %d chars, 60 lines]\n", len(in)) +
		strings.Join(lcHead, "\n") + "\n" +
		run + "\n" +
		"[... 47 similar lines folded ...]\n" +
		strings.Join(lcTail, "\n") + "\n" +
		"[log-collapse: folded 1 run(s) of near-identical lines (47 lines hidden); kept verbatim: the first 8 and last 4 lines and every line flagged as a problem or a test summary]"
	if got != want {
		t.Errorf("collapsed bytes differ.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestLogCollapseFoldsNearIdenticalRunsAndKeepsWindows(t *testing.T) {
	in := lcJoin(lcHead, lcAccessRun(100), lcTail)
	got, ok := logCollapse(in)
	if !ok {
		t.Fatal("100 access lines differing only in timestamp, id and latency must collapse")
	}
	if len(got) >= len(in) {
		t.Fatalf("collapsed body is not shorter: %d >= %d", len(got), len(in))
	}
	if n := strings.Count(got, "GET /health"); n != 1 {
		t.Errorf("want one representative access line, got %d", n)
	}
	if !strings.Contains(got, "[... 99 similar lines folded ...]") {
		t.Errorf("count marker missing or wrong:\n%s", got)
	}
	for _, l := range append(append([]string{}, lcHead...), lcTail...) {
		if !strings.Contains(got, l+"\n") && !strings.HasSuffix(got, l) {
			t.Errorf("window line lost: %q", l)
		}
	}
	// The first line of the run is the representative, verbatim.
	if !strings.Contains(got, lcAccess(0)+"\n") {
		t.Errorf("first line of the run must survive verbatim:\n%s", got)
	}
}

func TestLogCollapseKeepsEverySignalLineInOrder(t *testing.T) {
	mid := lcAccessRun(40)
	mid[5] = "2026-10-14T09:12:05.000Z ERROR upstream connection refused host=cache"
	mid[12] = "2026-10-14T09:12:12.000Z WARN  slow request took 2300ms"
	mid[20] = "ok  \texample.com/widget\t0.412s"
	mid[25] = "FAIL"
	mid[26] = "3 passed, 1 failed"
	mid[30] = "    at com.example.Widget.render(Widget.java:88)"
	in := lcJoin(lcHead, mid, lcTail)
	got, ok := logCollapse(in)
	if !ok {
		t.Fatal("body must collapse around the signal lines")
	}
	var want []string
	for _, l := range strings.Split(strings.TrimSuffix(in, "\n"), "\n") {
		if signalLine.MatchString(l) || logsKeep(l) && l != "" {
			want = append(want, l)
		}
	}
	var have []string
	for _, l := range strings.Split(got, "\n") {
		if signalLine.MatchString(l) || logsKeep(l) && l != "" {
			have = append(have, l)
		}
	}
	if strings.Join(have, "\n") != strings.Join(want, "\n") {
		t.Errorf("signal lines changed.\nwant:\n%s\nhave:\n%s", strings.Join(want, "\n"), strings.Join(have, "\n"))
	}
	if n := strings.Count(got, "GET /health"); n >= 30 {
		t.Errorf("routine lines were not folded (%d left)", n)
	}
}

func TestLogCollapseNeverFoldsIdenticalSignalLines(t *testing.T) {
	in := lcJoin(lcHead, lcRepeat("WARN  retrying the connection pool", 50), lcTail)
	got, ok := logCollapse(in)
	if ok || got != in {
		t.Fatalf("50 identical warning lines must all stay (nothing else to fold): ok=%v, changed=%v", ok, got != in)
	}
}

func TestLogCollapseKeepsTheTailWindowOfABodyThatEndsInARun(t *testing.T) {
	// The last four lines are part of the run but belong to the tail window:
	// they stay, and only the run before them folds.
	run := "retrying the build cache lookup"
	in := lcJoin(lcHead, lcRepeat(run, 40))
	got, ok := logCollapse(in)
	if !ok {
		t.Fatal("must collapse")
	}
	if n := strings.Count(got, run+"\n"); n != 5 {
		t.Errorf("want the representative plus the four tail lines (5), got %d:\n%s", n, got)
	}
	if !strings.Contains(got, "[... 35 similar lines folded ...]\n"+strings.Repeat(run+"\n", 4)+"[log-collapse: folded") {
		t.Errorf("the four tail lines must follow the marker unchanged:\n%s", got)
	}
}

func TestLogCollapseWindowsApplyToRawLineIndices(t *testing.T) {
	// A run that starts inside the head window: only the part past the window
	// folds, the window lines stay verbatim and in place.
	run := "retrying the build cache lookup"
	in := lcJoin(lcRepeat(run, 30), []string{"final summary line follows", "another closing line", "one more closing line", "last closing line"})
	got, ok := logCollapse(in)
	if !ok {
		t.Fatal("must collapse")
	}
	lines := strings.Split(got, "\n")
	// header, then 8 verbatim head lines.
	for i := 1; i <= 8; i++ {
		if lines[i] != run {
			t.Fatalf("head window line %d altered: %q", i, lines[i])
		}
	}
	if !strings.HasSuffix(strings.Join(lines[len(lines)-5:len(lines)-1], "\n"), "last closing line") {
		t.Errorf("tail window not kept: %v", lines[len(lines)-5:])
	}
}

func TestLogCollapseMinimumRunIsThree(t *testing.T) {
	// Runs of exactly two never fold: twelve pairs, the letter alternating per
	// pair so neighbouring pairs do not match each other.
	var pairs []string
	for k := 0; k < 12; k++ {
		c := 'a' + k%2
		pairs = append(pairs,
			fmt.Sprintf("GET /%c 200 5ms handled by the worker pool, which echoes the whole query string and the full list of response headers back into the log line", c),
			fmt.Sprintf("GET /%c 200 6ms handled by the worker pool, which echoes the whole query string and the full list of response headers back into the log line", c))
	}
	in := lcJoin(lcHead, pairs, lcTail)
	if got, ok := logCollapse(in); ok || got != in {
		t.Errorf("runs of two must stay verbatim: ok=%v changed=%v", ok, got != in)
	}
	// Exactly three long lines do fold.
	long := "GET /export?format=csv&columns=all 200 %dms streamed through the worker pool without buffering, with the full column list echoed back to the caller so that the line is long enough to be worth folding"
	three := []string{fmt.Sprintf(long, 5), fmt.Sprintf(long, 6), fmt.Sprintf(long, 7)}
	in3 := lcJoin(lcHead, three, []string{"between", "the", "windows", "we", "check"}, lcTail)
	got, ok := logCollapse(in3)
	if !ok || !strings.Contains(got, "[... 2 similar lines folded ...]") {
		t.Errorf("a run of three long lines must fold to one line plus a count of two:\n%s", got)
	}
}

func TestLogCollapseSkipsARunThatWouldNotShrink(t *testing.T) {
	// Three tiny lines: the count marker is longer than what it replaces.
	mid := []string{"a 1", "a 2", "a 3"}
	for i := 0; i < 12; i++ {
		mid = append(mid, fmt.Sprintf("distinct middle line number %c of the transcript body", 'a'+i))
	}
	in := lcJoin(lcHead, mid, lcTail)
	if got, ok := logCollapse(in); ok || got != in {
		t.Errorf("a run whose replacement is not shorter must stay: ok=%v changed=%v", ok, got != in)
	}
	// Beside a run that does pay for itself, the tiny run still stays verbatim
	// (the body as a whole shrinks, so only the per-run check can keep it).
	big := lcRepeat("retrying the build cache lookup", 40)
	got, ok := logCollapse(lcJoin(lcHead, mid, big, lcTail))
	if !ok {
		t.Fatal("the long run should fold")
	}
	if !strings.Contains(got, "a 1\na 2\na 3\n") {
		t.Errorf("a run whose marker is longer than the run itself was folded:\n%s", got)
	}
}

// TestLogCollapseDeclinesWhenHeaderAndFooterCostMoreThanTheFold: a run that
// does fold profitably can still leave the whole body longer once the header
// and footer are added; the rewrite itself must then decline, not rely on the
// wrapper to notice.
func TestLogCollapseDeclinesWhenHeaderAndFooterCostMoreThanTheFold(t *testing.T) {
	long := "GET /export?format=csv&columns=all 200 %dms streamed through the worker pool without buffering"
	three := []string{fmt.Sprintf(long, 5), fmt.Sprintf(long, 6), fmt.Sprintf(long, 7)}
	in := lcJoin(lcHead, three, []string{"between", "the", "windows", "we", "check"}, lcTail)
	if got, ok := collapseLogs(in); ok || got != in {
		t.Errorf("collapseLogs must decline when the result would not be shorter (ok=%v, changed=%v)", ok, got != in)
	}
	if got, ok := logCollapse(in); ok || got != in {
		t.Errorf("logCollapse must decline too (ok=%v, changed=%v)", ok, got != in)
	}
}

// TestLogCollapseWrapperRejectsAnyRewriteThatBreaksTheContract: whatever the
// rewrite returns, logCollapse hands back the original unless the result is
// strictly shorter and carries the idempotence prefix.
func TestLogCollapseWrapperRejectsAnyRewriteThatBreaksTheContract(t *testing.T) {
	in := lcJoin(lcHead, lcRepeat("retrying the build cache lookup", 60), lcTail)
	saved := logsCollapseImpl
	defer func() { logsCollapseImpl = saved }()
	for name, impl := range map[string]func(string) (string, bool){
		"longer":           func(c string) (string, bool) { return logsPrefix + c, true },
		"same length":      func(c string) (string, bool) { return logsPrefix + c[len(logsPrefix):], true },
		"shorter unmarked": func(c string) (string, bool) { return c[:len(c)/2], true },
		"declined":         func(c string) (string, bool) { return c, false },
	} {
		logsCollapseImpl = impl
		if got, ok := logCollapse(in); ok || got != in {
			t.Errorf("%s: the wrapper must return the original (ok=%v)", name, ok)
		}
	}
	logsCollapseImpl = func(c string) (string, bool) { return logsPrefix + "ok]", true }
	if got, ok := logCollapse(in); !ok || got != logsPrefix+"ok]" {
		t.Errorf("a shorter, marked rewrite must pass through (ok=%v)", ok)
	}
}

func TestLogCollapseKeepsCRLFLinesVerbatim(t *testing.T) {
	run := "retrying the build cache lookup"
	in := strings.Join(append(append(append([]string{}, lcHead...), lcRepeat(run, 40)...), lcTail...), "\r\n") + "\r\n"
	got, ok := logCollapse(in)
	if !ok {
		t.Fatal("a CRLF body must collapse like an LF body")
	}
	if !strings.Contains(got, lcHead[0]+"\r\n") {
		t.Errorf("a kept line lost its carriage return:\n%q", got)
	}
	if !strings.Contains(got, "[... 39 similar lines folded ...]") {
		t.Errorf("a CRLF run was not folded with the right count:\n%q", got)
	}
}

func TestLogCollapseKeepsMultibyteLinesIntact(t *testing.T) {
	line := "télémétrie: dépôt synchronisé — 数据已同步 ✓"
	in := lcJoin(lcHead, lcRepeat(line, 40), lcTail)
	got, ok := logCollapse(in)
	if !ok || !strings.Contains(got, line+"\n") {
		t.Fatalf("multibyte run must fold and keep its representative: ok=%v", ok)
	}
}

// --- the properties every rung must have ------------------------------------

func TestLogCollapseDeterministic(t *testing.T) {
	bodies := []string{lrTestLog(60), lcSubtestLog(60), lrServerLog(150), lrProgressLog(120), lrRepeatLog(80), lcJoin(lcHead, lcAccessRun(60), lcTail)}
	for i, b := range bodies {
		o1, ok1 := logCollapse(b)
		o2, ok2 := logCollapse(b)
		if o1 != o2 || ok1 != ok2 {
			t.Errorf("body %d: two calls differ", i)
		}
	}
}

func TestLogCollapseIdempotent(t *testing.T) {
	for i, b := range []string{lcSubtestLog(60), lrServerLog(150), lrProgressLog(120), lrRepeatLog(80)} {
		once, ok := logCollapse(b)
		if !ok {
			t.Errorf("body %d: expected the rung to fire", i)
			continue
		}
		if !isLogCollapsed(once) {
			t.Errorf("body %d: output not recognised as collapsed", i)
		}
		twice, ok2 := logCollapse(once)
		if ok2 || twice != once {
			t.Errorf("body %d: second application changed the body (ok=%v)", i, ok2)
		}
		if !IsLogCollapseArtifact(once) {
			t.Errorf("body %d: a collapsed body must count as a log-collapse artifact", i)
		}
		if IsCompactionArtifact(once) {
			t.Errorf("body %d: IsCompactionArtifact must not change with the rung off or on", i)
		}
	}
}

func TestLogCollapseReturnsTheOriginalWhenNothingFolds(t *testing.T) {
	var distinct []string
	for i := 0; i < 80; i++ {
		distinct = append(distinct, fmt.Sprintf("line %c%c carries its own words and stays", 'a'+i/26, 'a'+i%26))
	}
	in := lcJoin(distinct)
	if got, ok := logCollapse(in); ok || got != in {
		t.Errorf("a body with no run must come back untouched: ok=%v changed=%v", ok, got != in)
	}
}

func TestLogCollapseFailsClosedOnUnexpectedShapes(t *testing.T) {
	run := lcRepeat("retrying the build cache lookup", 60)
	foldable := lcJoin(lcHead, run, lcTail)
	if _, ok := logCollapse(foldable); !ok {
		t.Fatal("control: the foldable body must fold")
	}
	jsonNums := make([]int, 300)
	for i := range jsonNums {
		jsonNums[i] = i
	}
	pretty, _ := json.MarshalIndent(jsonNums, "", "  ")
	block, ok := gcf.Compact(func() string {
		var objs []string
		for i := 0; i < 60; i++ {
			objs = append(objs, fmt.Sprintf(`{"id":%d,"name":"item","state":"ready"}`, i))
		}
		return "[" + strings.Join(objs, ",") + "]"
	}())
	if !ok {
		t.Fatal("control: gcf must encode the array")
	}
	cases := map[string]string{
		"empty":                     "",
		"single line":               strings.Repeat("x", 2000),
		"two lines":                 strings.Repeat("x", 1000) + "\n" + strings.Repeat("y", 1000),
		"below the size floor":      lcJoin([]string{"s a", "s b", "s c", "s d", "s e", "s f", "s g", "s h"}, lcRepeat("ab 7", 60), []string{"e a", "e b", "e c", "e d"}),
		"invalid utf-8":             foldable + "\xff\xfe broken tail\n",
		"nul byte":                  foldable + "binary\x00blob\n",
		"valid json document":       string(pretty),
		"gcf block present":         foldable + block,
		"already skeletonised":      skeletonPrefix + "9000 chars]\n" + foldable,
		"already elided":            elisionMarker(9000) + "\n" + foldable,
		"already deduped":           dedupeMarker("call-9", 9000) + "\n" + foldable,
		"already collapsed":         logsPrefix + "9000 chars, 100 lines]\n" + foldable,
		"over the size ceiling":     strings.Repeat("retrying the build cache lookup\n", (logsMaxChars/32)+10),
		"only newlines and spaces":  strings.Repeat(" \n", 500),
		"one long line then blanks": strings.Repeat("z", 3000) + strings.Repeat("\n", 30),
	}
	for name, in := range cases {
		got, ok := logCollapse(in)
		if ok || got != in {
			t.Errorf("%s: must fail closed to the original (ok=%v, changed=%v)", name, ok, got != in)
		}
	}
}

func TestLogCollapseFailsClosedOnAPanic(t *testing.T) {
	foldable := lcJoin(lcHead, lcRepeat("retrying the build cache lookup", 60), lcTail)
	saved := logsCollapseImpl
	defer func() { logsCollapseImpl = saved }()
	logsCollapseImpl = func(string) (string, bool) { panic("unexpected shape") }
	got, ok := logCollapse(foldable)
	if ok || got != foldable {
		t.Errorf("a panic inside the rewrite must return the original: ok=%v changed=%v", ok, got != foldable)
	}
}

func TestLogCollapseWordingStaysOutOfTheSignalVocabulary(t *testing.T) {
	// The drop rung refuses a unit whose body still matches signalLine (the
	// FORCE_PRESERVE residue test). The rung's own header, markers and footer
	// must never match, or every collapsed body would become undroppable.
	for i, b := range []string{lrRepeatLog(80), lrProgressLog(120), lcJoin(lcHead, lcAccessRun(80), lcTail)} {
		if signalLine.MatchString(b) {
			t.Fatalf("fixture %d already carries signal text", i)
		}
		got, ok := logCollapse(b)
		if !ok {
			t.Fatalf("fixture %d should collapse", i)
		}
		if signalLine.MatchString(got) {
			t.Errorf("fixture %d: the collapsed body matches signalLine:\n%s", i, got)
		}
	}
}

// lcFuzzBody builds a deterministic pseudo-random log body from a small
// vocabulary of routine and signal lines (a fixed LCG: no math/rand versioning).
func lcFuzzBody(seed uint64) string {
	state := seed*2862933555777941757 + 3037000493
	next := func() uint64 {
		state = state*6364136223846793005 + 1442695040888963407
		return state >> 33
	}
	vocab := []string{
		"GET /a 200 %dms served from the worker pool",
		"GET /b 404 %dms served from the worker pool",
		"cache hit key=%x for the compiled module",
		"ERROR boom code=%d in the upstream call",
		"warning: slow call took %dms to answer",
		"ok  \tpkg/%d\t0.311s",
		"",
		"   indented line %d of the report",
		"plain text line without any digits at all",
		`{"seq":%d,"event":"tick"}`,
		"FAIL",
		"downloading chunk %d of 99",
	}
	n := 20 + int(next()%200)
	var lines []string
	for len(lines) < n {
		v := vocab[next()%uint64(len(vocab))]
		reps := 1 + int(next()%7)
		for r := 0; r < reps; r++ {
			if strings.Contains(v, "%") {
				lines = append(lines, fmt.Sprintf(v, int(next()%900)+1))
			} else {
				lines = append(lines, v)
			}
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestLogCollapseProperties(t *testing.T) {
	fired := 0
	for seed := uint64(1); seed <= 300; seed++ {
		in := lcFuzzBody(seed)
		out, ok := logCollapse(in)
		out2, ok2 := logCollapse(in)
		if out != out2 || ok != ok2 {
			t.Fatalf("seed %d: not deterministic", seed)
		}
		if !ok {
			if out != in {
				t.Fatalf("seed %d: a declined body must be returned untouched", seed)
			}
			continue
		}
		fired++
		if len(out) >= len(in) {
			t.Fatalf("seed %d: collapsed body is not strictly shorter (%d >= %d)", seed, len(out), len(in))
		}
		again, okAgain := logCollapse(out)
		if okAgain || again != out {
			t.Fatalf("seed %d: not idempotent", seed)
		}
		var want, have []string
		for _, l := range strings.Split(strings.TrimSuffix(in, "\n"), "\n") {
			if signalLine.MatchString(l) {
				want = append(want, l)
			}
		}
		for _, l := range strings.Split(out, "\n") {
			if signalLine.MatchString(l) {
				have = append(have, l)
			}
		}
		if strings.Join(want, "\n") != strings.Join(have, "\n") {
			t.Fatalf("seed %d: signal lines changed", seed)
		}
		head := strings.Split(strings.TrimSuffix(in, "\n"), "\n")
		outLines := strings.Split(out, "\n")
		for i := 0; i < 8 && i < len(head); i++ {
			if outLines[1+i] != head[i] {
				t.Fatalf("seed %d: head line %d altered", seed, i)
			}
		}
	}
	if fired < 100 {
		t.Errorf("the rung fired on only %d of 300 generated bodies; the generator no longer exercises it", fired)
	}
}

// --- the rung inside the ladder -------------------------------------------------

func lcIsRaw(m Msg, original Msg) bool { return m.Content == original.Content }

func TestLogCollapseRungFoldsOlderBodiesOnlyAndKeepsPairing(t *testing.T) {
	msgs := lrTranscript(lrServerLog(150), lrServerLog(160), lrServerLog(170))
	budget := estimateTokens(msgs) * 6 / 10
	out := compact(context.Background(), msgs, budget, 2, 3, lcLogOptsOn)
	if len(out) != len(msgs) {
		t.Fatalf("the rung must never drop a message: %d -> %d", len(msgs), len(out))
	}
	for i := range msgs {
		if out[i].Role != msgs[i].Role || out[i].ToolCallID != msgs[i].ToolCallID || out[i].IsError != msgs[i].IsError || len(out[i].ToolCalls) != len(msgs[i].ToolCalls) {
			t.Errorf("message %d: a non-content field changed: %+v -> %+v", i, msgs[i], out[i])
		}
	}
	pairing(t, out)
	// Preamble (0..2) and the recent window (the last 2 messages) are untouched.
	for _, i := range []int{0, 1, 2, len(msgs) - 2, len(msgs) - 1} {
		if out[i].Content != msgs[i].Content {
			t.Errorf("protected message %d changed", i)
		}
	}
	// The two older tool bodies were folded; the signal lines inside survive.
	for _, i := range []int{4, 6} {
		if !isLogCollapsed(out[i].Content) {
			t.Errorf("older tool body %d was not folded:\n%.200s", i, out[i].Content)
		}
		for _, want := range []string{"WARN  slow request", "ERROR upstream connection refused"} {
			if !strings.Contains(out[i].Content, want) {
				t.Errorf("tool body %d lost its line %q", i, want)
			}
		}
	}
	if estimateTokens(out) > budget {
		t.Errorf("the rung alone should have reached the budget: %d > %d", estimateTokens(out), budget)
	}

	// Under a budget no ladder can meet, the preamble and the recent window are
	// still untouched (the ladder's window, not the rung, decides what is older).
	tight := compact(context.Background(), msgs, 1, 2, 3, lcLogOptsOn)
	for _, i := range []int{0, 1, 2} {
		if tight[i].Content != msgs[i].Content {
			t.Errorf("tight budget: preamble message %d changed", i)
		}
	}
	for k := 1; k <= 2; k++ {
		if tight[len(tight)-k].Content != msgs[len(msgs)-k].Content {
			t.Errorf("tight budget: recent message %d from the end changed", k)
		}
	}
}

func TestLogCollapseRungLeavesTheProtectedPrefixAlone(t *testing.T) {
	// A command-output tool message INSIDE the protected preamble must survive
	// even under pressure; only the older tool body after it is a candidate.
	// The budget is what folding that one body buys, so no later rung runs, and
	// the rung works oldest-first, so a rung that reached into the prefix would
	// fold the prefix message before it ever reached the candidate.
	pre := lrServerLog(200)
	msgs := []Msg{
		{Role: "system", Content: "You are a coding agent."},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "pre-1", Name: "run", Args: `{}`}}},
		{Role: "tool", ToolCallID: "pre-1", Content: pre},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call-1", Name: "run", Args: `{}`}}},
		{Role: "tool", ToolCallID: "call-1", Content: lrServerLog(190)},
		{Role: "assistant", Content: "reading it"},
	}
	folded, ok := logCollapse(msgs[4].Content)
	if !ok {
		t.Fatal("control: the older body must fold")
	}
	probe := append([]Msg(nil), msgs...)
	probe[4].Content = folded
	out := compact(context.Background(), msgs, estimateTokens(probe), 1, 3, lcLogOptsOn)
	if out[2].Content != pre {
		t.Error("a tool message inside the protected prefix was rewritten")
	}
	if out[4].Content != folded {
		t.Error("the older tool body should have been folded")
	}
}

func TestLogCollapseRungRespectsPins(t *testing.T) {
	msgs := lrTranscript(lrServerLog(150), lrServerLog(160), lrServerLog(170))
	folded, ok := logCollapse(msgs[6].Content)
	if !ok {
		t.Fatal("control: the second body must fold")
	}
	probe := append([]Msg(nil), msgs...)
	probe[6].Content = folded
	opts := compactOpts{LogCollapse: true, Pinned: map[string]bool{"call-1": true}}
	out := compact(context.Background(), msgs, estimateTokens(probe), 2, 3, opts)
	if out[4].Content != msgs[4].Content {
		t.Error("a pinned tool body was rewritten")
	}
	if out[6].Content != folded {
		t.Error("the unpinned older body should have been folded")
	}
}

func TestLogsRungClampsItsWindow(t *testing.T) {
	msgs := lrTranscript(lrServerLog(150), lrServerLog(160))
	out := append([]Msg(nil), msgs...)
	logsRung(out, -7, len(out)+50, 1, nil) // out-of-range window must clamp, not panic
	if !isLogCollapsed(out[4].Content) || !isLogCollapsed(out[6].Content) {
		t.Error("a clamped window should still reach the tool bodies")
	}
	for _, i := range []int{0, 1, 2, 3, 5} {
		if out[i].Content != msgs[i].Content {
			t.Errorf("non-tool message %d was rewritten", i)
		}
	}
	logsRung(nil, 0, 0, 1, nil)
	logsRung(out, 5, 2, 1, nil) // inverted window: nothing to do
}

func TestLogCollapseRungStopsAtTheFirstFit(t *testing.T) {
	// Five distinct older bodies; set the budget to what folding ONLY the
	// oldest buys. The rung runs oldest-first and stops at the first fit, so
	// the newer bodies must stay byte-identical (KV-prefix friendly: the edit
	// point moves forward, never scatters).
	msgs := lrTranscript(lrServerLog(150), lrServerLog(160), lrServerLog(170), lrServerLog(180), lrServerLog(190))
	probe := make([]Msg, len(msgs))
	copy(probe, msgs)
	folded, ok := logCollapse(probe[4].Content)
	if !ok {
		t.Fatal("control: the oldest body must fold")
	}
	probe[4].Content = folded
	budget := estimateTokens(probe)
	out := compact(context.Background(), msgs, budget, 2, 3, lcLogOptsOn)
	if out[4].Content != folded {
		t.Error("the oldest body was not folded exactly as logCollapse folds it")
	}
	for _, i := range []int{6, 8, 10, 12, 13} {
		if !lcIsRaw(out[i], msgs[i]) {
			t.Errorf("message %d changed although the first fold already fit", i)
		}
	}
}

func TestLogCollapseOffIsTheDefaultLadder(t *testing.T) {
	for _, fx := range lrSyntheticFixtures() {
		a := compact(context.Background(), fx.msgs, fx.budget, fx.keep, fx.prot, compactOpts{})
		b := compact(context.Background(), fx.msgs, fx.budget, fx.keep, fx.prot, compactOpts{LogCollapse: false})
		if lrDigest(a) != lrDigest(b) {
			t.Errorf("%s: an explicit off differs from the zero value", fx.name)
		}
		for i := range a {
			if isLogCollapsed(a[i].Content) {
				t.Errorf("%s: the default ladder produced a collapsed body", fx.name)
			}
		}
	}
}

func TestLogCollapseLadderIdempotentAndFloorMonotonic(t *testing.T) {
	ladders := map[string]compactOpts{
		"logs only":         {LogCollapse: true},
		"gcf+skel+logs":     {GCF: true, Skeleton: true, LogCollapse: true},
		"skeleton+logs":     {Skeleton: true, LogCollapse: true},
		"logs with a pin":   {LogCollapse: true, Pinned: map[string]bool{"call-1": true}},
		"production+logs+p": {GCF: true, Skeleton: true, LogCollapse: true, Pinned: map[string]bool{"call-2": true}},
	}
	fixtures := lcLadderFixtures(t)
	for name, opts := range ladders {
		for _, fx := range fixtures {
			once := compact(context.Background(), fx.msgs, fx.budget, fx.keep, fx.prot, opts)
			twice := compact(context.Background(), once, fx.budget, fx.keep, fx.prot, opts)
			if lrDigest(once) != lrDigest(twice) {
				t.Errorf("%s / %s: compacting twice at one budget changed the transcript", name, fx.name)
			}
			// ADR 0016 section 5: a transcript compacted at a hard budget stays
			// exactly as compacted when re-compacted at a gentler one.
			if estimateTokens(once) <= fx.budget {
				for _, gentler := range []int{estimateTokens(once), fx.budget * 3 / 2} {
					again := compact(context.Background(), once, gentler, fx.keep, fx.prot, opts)
					if lrDigest(once) != lrDigest(again) {
						t.Errorf("%s / %s: a gentler re-compaction (budget %d) moved a turn back up the ladder", name, fx.name, gentler)
					}
				}
			}
			if estimateTokens(once) > estimateTokens(fx.msgs) {
				t.Errorf("%s / %s: the ladder made the transcript longer", name, fx.name)
			}
			// The rung only rewrites Content, so it can add no orphan beyond what
			// the same ladder without it already produces (the legacy drop rung
			// can orphan a result when the keep-recent window starts between a
			// call and its result, as two synthetic fixtures do on the base ladder).
			without := opts
			without.LogCollapse = false
			base := compact(context.Background(), fx.msgs, fx.budget, fx.keep, fx.prot, without)
			if lcOrphans(once) > lcOrphans(base) {
				t.Errorf("%s / %s: the rung added orphaned tool results (%d vs %d without it)", name, fx.name, lcOrphans(once), lcOrphans(base))
			}
		}
	}
}

func TestLogCollapseRungKeepsTheBuriedErrorInTheMiniCorpus(t *testing.T) {
	for _, fx := range lrMiniCorpus(t) {
		if fx.name != "mini/lg-1" && fx.name != "mini/tt-1" {
			continue
		}
		fx = lcNamed(fx, "run")
		out := compact(context.Background(), fx.msgs, fx.budget, fx.keep, fx.prot, lcLogOptsOn)
		var body string
		for _, m := range out {
			if m.Role == "tool" {
				body = m.Content
			}
		}
		if !isLogCollapsed(body) {
			t.Errorf("%s: the rung did not engage on the shipped log fixture", fx.name)
		}
		if fx.name == "mini/lg-1" && !strings.Contains(body, "ERROR") && !strings.Contains(strings.ToLower(body), "fail") {
			t.Errorf("%s: the buried failure line was lost", fx.name)
		}
	}
}

// TestLogCollapseRungOnlyFoldsCommandOutput: a body produced by a file read, a
// search, a fetch, an unknown tool or no recorded call at all is content the
// model asked for or a shape the rung does not know; the ladder with the rung
// on must return exactly what the ladder without it returns.
func TestLogCollapseRungOnlyFoldsCommandOutput(t *testing.T) {
	for _, producer := range []string{"read_file", "search_files", "web_fetch", "github_api", "bash", ""} {
		fx := lcNamed(lrSyntheticFixtures()[2], producer) // server-only/60: bodies the rung folds when named "run"
		off := compact(context.Background(), fx.msgs, fx.budget, fx.keep, fx.prot, compactOpts{})
		on := compact(context.Background(), fx.msgs, fx.budget, fx.keep, fx.prot, lcLogOptsOn)
		if lrDigest(off) != lrDigest(on) {
			t.Errorf("producer %q: the rung rewrote a body it must not touch", producer)
		}
	}
	for _, producer := range []string{"run", "run_shell"} {
		fx := lcNamed(lrSyntheticFixtures()[2], producer)
		off := compact(context.Background(), fx.msgs, fx.budget, fx.keep, fx.prot, compactOpts{})
		on := compact(context.Background(), fx.msgs, fx.budget, fx.keep, fx.prot, lcLogOptsOn)
		if lrDigest(off) == lrDigest(on) {
			t.Errorf("producer %q: the rung should fold command output", producer)
		}
	}
	// A result with no matching call in the transcript is also left alone.
	msgs := lrTranscript(lrServerLog(150), lrServerLog(160), lrServerLog(170))
	for i := range msgs {
		msgs[i].ToolCalls = nil
	}
	budget := estimateTokens(msgs) * 6 / 10
	off := compact(context.Background(), msgs, budget, 2, 3, compactOpts{})
	on := compact(context.Background(), msgs, budget, 2, 3, lcLogOptsOn)
	if lrDigest(off) != lrDigest(on) {
		t.Error("a result with no recorded call must not be folded")
	}
}

// TestLogCollapseMeasuredSavings logs (and asserts nothing about) the token
// effect on the shipped fixtures, measured with the ladder's own estimator.
// The adoption verdict belongs to the measurement gate, not to this test.
func TestLogCollapseMeasuredSavings(t *testing.T) {
	fixtures := lcLadderFixtures(t)
	t.Logf("%-28s %7s %7s %7s %7s %7s", "fixture", "before", "base", "base+L", "prod", "prod+L")
	for _, fx := range fixtures {
		before := estimateTokens(fx.msgs)
		run := func(o compactOpts) int {
			return estimateTokens(compact(context.Background(), fx.msgs, fx.budget, fx.keep, fx.prot, o))
		}
		t.Logf("%-28s %7d %7d %7d %7d %7d", fx.name, before,
			run(compactOpts{}), run(compactOpts{LogCollapse: true}),
			run(compactOpts{GCF: true, Skeleton: true}), run(compactOpts{GCF: true, Skeleton: true, LogCollapse: true}))
	}
	for _, fx := range fixtures {
		for _, m := range fx.msgs {
			if m.Role != "tool" || len(m.Content) < 1000 {
				continue
			}
			if out, ok := logCollapse(m.Content); ok {
				t.Logf("body %-26s %6d -> %6d chars (%.1f%%)", fx.name, len(m.Content), len(out), 100*float64(len(out))/float64(len(m.Content)))
			}
		}
	}
}

// lcOrphans counts tool results whose call is absent from the transcript.
func lcOrphans(msgs []Msg) int {
	calls := map[string]bool{}
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			calls[c.ID] = true
		}
	}
	n := 0
	for _, m := range msgs {
		if m.Role == "tool" && !calls[m.ToolCallID] {
			n++
		}
	}
	return n
}

// TestLogCollapseIsDiscriminatedByTheGoldenFixtures keeps the default-ladder
// digests honest: they only prove "off changes nothing" if the rung, once on,
// would have changed something on the same fixtures.
func TestLogCollapseIsDiscriminatedByTheGoldenFixtures(t *testing.T) {
	changed := 0
	fixtures := lcLadderFixtures(t)
	for _, fx := range fixtures {
		off := compact(context.Background(), fx.msgs, fx.budget, fx.keep, fx.prot, compactOpts{})
		on := compact(context.Background(), fx.msgs, fx.budget, fx.keep, fx.prot, compactOpts{LogCollapse: true})
		if lrDigest(off) != lrDigest(on) {
			changed++
		}
	}
	if changed < 6 {
		t.Errorf("the rung changes only %d of %d golden fixtures; the pinned digests would not notice it leaking in", changed, len(fixtures))
	}
}

// --- review fixes: the default ladder, colour, language errors, sizes, ids -------

// TestDefaultLadderTreatsALogCollapseLookalikeAsRawContent: with the rung off,
// a body that merely opens with the rung's header is ordinary content. The
// ladder must dedupe and elide it exactly as it would any other body and name
// its true size; IsCompactionArtifact must not claim it. The expected values
// here come from the ladder's own rules (dedupeMarker, elisionMarker), not from
// the rung.
func TestDefaultLadderTreatsALogCollapseLookalikeAsRawContent(t *testing.T) {
	body := lrLookalikeHeader + lrServerLog(150)
	if IsCompactionArtifact(body) {
		t.Fatal("IsCompactionArtifact must stay the ladder's own three products; a quoted header is not one")
	}
	if isDeduped(body) || isElided(body) || isSkeletonized(body) {
		t.Fatal("fixture control: the lookalike must not be any other rung's product")
	}

	// Two identical copies: the older one is deduped, whatever it says on line 1.
	msgs := lrTranscript(body, body, lrServerLog(170))
	budget := estimateTokens(msgs) * 6 / 10
	out := compact(context.Background(), msgs, budget, 2, 3, compactOpts{})
	if want := dedupeMarker("call-2", len(body)); out[4].Content != want {
		t.Errorf("older identical copy should be a dedupe reference:\n got %.120q\nwant %.120q", out[4].Content, want)
	}

	// Distinct copies under a budget nothing meets: every older body is elided
	// and the marker reports the real length, not the 4096 the header claims.
	distinct := lrTranscript(lrLookalikeHeader+lrServerLog(150), lrLookalikeHeader+lrTestLog(80), lrServerLog(170))
	tight := compact(context.Background(), distinct, 1, 2, 3, compactOpts{})
	for _, i := range []int{4, 6} {
		if want := elisionMarker(len(distinct[i].Content)); !strings.HasPrefix(tight[i].Content, want) {
			t.Errorf("message %d: elide marker should name the true size:\n got %.120q\nwant prefix %q", i, tight[i].Content, want)
		}
	}
}

func TestLogsKeepSeesThroughTerminalColour(t *testing.T) {
	keep := []string{
		"\x1b[31mERROR\x1b[0m connection reset by peer",
		"\x1b[1;33mWARN\x1b[0m  slow request took 2300ms",
		"\x1b[31mFAIL\x1b[0m\texample.com/widget\t0.412s",
		"\x1b[31m--- FAIL: TestWidget (0.01s)\x1b[0m",
		"\x1b[90m    at com.example.Widget.render(Widget.java:88)\x1b[0m",
		"\x1b[1mTraceback (most recent call last):\x1b[0m",
		"\x1b[0m",
		"\x1b[0m   ",
	}
	for _, l := range keep {
		if !logsKeep(l) {
			t.Errorf("a coloured problem or blank line must be kept verbatim: %q", l)
		}
	}
	for _, l := range []string{"\x1b[32mGET\x1b[0m /health 200 4ms", "\x1b[36mdownloading\x1b[0m module.tgz 12/100 KB"} {
		if logsKeep(l) {
			t.Errorf("a coloured routine line must stay foldable: %q", l)
		}
	}

	// In a run: forty near-identical coloured errors between routine runs all
	// survive, and the routine lines around them still fold.
	var mid []string
	for i := 0; i < 40; i++ {
		mid = append(mid, fmt.Sprintf("\x1b[31mERROR\x1b[0m request %d dropped by the proxy", i))
	}
	in := lcJoin(lcHead, lcAccessRun(30), mid, lcAccessRun(30), lcTail)
	got, ok := logCollapse(in)
	if !ok {
		t.Fatal("the routine runs around the errors must still fold")
	}
	if n := strings.Count(got, "dropped by the proxy"); n != 40 {
		t.Errorf("want all 40 coloured error lines kept, got %d", n)
	}
}

func TestLogsKeepRecognisesLanguageErrorNamesAndFailureShapes(t *testing.T) {
	keep := []string{
		"TypeError: x is undefined",
		"java.lang.NullPointerException",
		"Exception in thread \"main\" java.lang.IllegalStateException: closed",
		"E   AssertionError: assert 1 == 2",
		"KeyError: 'x'",
		"ValueError: invalid literal for int()",
		"UnboundLocalError: referenced before assignment",
		"Segmentation fault (core dumped)",
		"Killed",
		"exit status 1",
		"exit status 137",
		"npm ERR! code ELIFECYCLE",
	}
	for _, l := range keep {
		if !logsKeep(l) {
			t.Errorf("failure line must be kept verbatim: %q", l)
		}
	}
	routine := []string{
		"registering the ErrorHandler route",
		"loaded ExceptionMapper table",
		"exit status 0",
		"GET /health 200 4ms",
	}
	for _, l := range routine {
		if logsKeep(l) {
			t.Errorf("routine line must stay foldable: %q", l)
		}
	}

	// A parametrised failure: forty near-identical assertion lines between
	// routine runs all survive.
	var mid []string
	for i := 0; i < 40; i++ {
		mid = append(mid, fmt.Sprintf("AssertionError: expected 1 got %d in case %d of the suite", i, i))
	}
	in := lcJoin(lcHead, lcAccessRun(30), mid, lcAccessRun(30), lcTail)
	got, ok := logCollapse(in)
	if !ok {
		t.Fatal("the routine runs around the failures must still fold")
	}
	if n := strings.Count(got, "AssertionError: expected 1 got"); n != 40 {
		t.Errorf("want all 40 assertion lines kept, got %d", n)
	}
}

// TestLogCollapseElideMarkerReportsTheOriginalSize: a body the rung folded and
// a later rung elides is announced at the size it had BEFORE the fold: the
// marker discloses what was lost, and what was lost is the whole original.
func TestLogCollapseElideMarkerReportsTheOriginalSize(t *testing.T) {
	msgs := lrTranscript(lrServerLog(150), lrServerLog(160), lrServerLog(170))
	for name, opts := range map[string]compactOpts{
		"logs":          {LogCollapse: true},
		"logs+skeleton": {Skeleton: true, LogCollapse: true},
		"production":    {GCF: true, Skeleton: true, LogCollapse: true},
	} {
		out := compact(context.Background(), msgs, 1, 2, 3, opts)
		for _, i := range []int{4, 6} {
			if want := elisionMarker(len(msgs[i].Content)); !strings.HasPrefix(out[i].Content, want) {
				t.Errorf("%s: message %d: elide marker should name the original size:\n got %.100q\nwant prefix %q", name, i, out[i].Content, want)
			}
		}
	}
}

// TestLogCollapseRungIgnoresAResultWhoseCallIdIsReused: a tool-call id declared
// more than once in the transcript does not say which call produced a result,
// so the rung must not guess: a file read must never be folded because a later
// call happened to reuse its id for a command.
func TestLogCollapseRungIgnoresAResultWhoseCallIdIsReused(t *testing.T) {
	msgs := []Msg{
		{Role: "system", Content: "You are a coding agent."},
		{Role: "user", Content: "objective: read the log, then run the build"},
		{Role: "user", Content: "exemplar"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "0", Name: "read_file", Args: `{"path":"server.log"}`}}},
		{Role: "tool", ToolCallID: "0", Content: lrServerLog(150)},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "0", Name: "run", Args: `{"cmd":"make"}`}}},
		{Role: "tool", ToolCallID: "0", Content: lrServerLog(160)},
		{Role: "assistant", Content: "reading both"},
	}
	out := append([]Msg(nil), msgs...)
	logsRung(out, 3, len(out)-1, 1, nil)
	for _, i := range []int{4, 6} {
		if out[i].Content != msgs[i].Content {
			t.Errorf("message %d: a result under a reused call id was rewritten", i)
		}
	}
	// Control: the same two bodies under distinct ids, the second a command,
	// fold the command's body only.
	fixed := append([]Msg(nil), msgs...)
	fixed[5] = Msg{Role: "assistant", ToolCalls: []ToolCall{{ID: "1", Name: "run", Args: `{"cmd":"make"}`}}}
	fixed[6] = Msg{Role: "tool", ToolCallID: "1", Content: msgs[6].Content}
	out = append([]Msg(nil), fixed...)
	logsRung(out, 3, len(out)-1, 1, nil)
	if out[4].Content != fixed[4].Content {
		t.Error("control: a file read was rewritten")
	}
	if !isLogCollapsed(out[6].Content) {
		t.Error("control: the command's body should fold under a unique id")
	}
}
