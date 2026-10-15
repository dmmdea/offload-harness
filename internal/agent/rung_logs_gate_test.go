package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/contextbudget"
	"github.com/dmmdea/offload-harness/internal/sandbox"
)

// These tests pin the rung's producer gate. The rung folds a command tool's
// result only when the call is a build or test invocation: a search listing, a
// data file read through cat or sed, and a linter's diagnostics are content the
// model asked for line by line, and a fold would lose lines that differ in a
// number only.

func lcShellCall(command string) ToolCall {
	b, _ := json.Marshal(map[string]string{"command": command})
	return ToolCall{ID: "c1", Name: "run_shell", Args: string(b)}
}

func lcRunCall(command string, args ...string) ToolCall {
	b, _ := json.Marshal(map[string]any{"command": command, "args": args})
	return ToolCall{ID: "c1", Name: "run", Args: string(b)}
}

// --- the command scan ---------------------------------------------------------

func TestLogsGateQualifiesBuildAndTestShellCommands(t *testing.T) {
	for _, cmd := range []string{
		"go test ./...",
		"go test -count=1 -run 'TestA|TestB' ./internal/agent/",
		"go build ./...",
		"go vet ./...",
		"/usr/local/go/bin/go test ./...",
		"cargo build",
		"cargo build --release",
		"cargo test",
		"cargo check --all-targets",
		"cargo +nightly test",
		"npm test",
		"npm t",
		"npm run build",
		"npm run test",
		"pytest",
		"pytest -q tests/",
		"python -m pytest -x",
		"python3 -m pytest",
		"python -m unittest",
		"python3 -m unittest discover -s tests",
		"make",
		"make test",
		"CGO_ENABLED=0 go test ./...",
		"go test ./... 2>&1",
		"go test ./... > out.txt 2>&1",
		"go test ./... &> out.txt",
		"go test -run 'a;b' ./...",
		`go test -run "a && b" ./...`,
		// A pipe into a pass-through filter or a search program still carries the
		// build's own output.
		"go test ./... | tail -50",
		"go test ./... 2>&1 | head -200",
		"go test ./... |& tee out.txt",
		"go test ./... | grep -v PASS",
		"go test ./... | sort | uniq -c",
		"go test ./... | cat | wc -l",
		"go test ./... | sed -n 1,40p | cut -c1-120",
		"go test ./... | awk '{print $1}'",
		"go test ./... | rg FAIL",
		// The stated limit (docs, rung_logs.go): a later stage is judged by its
		// program name alone, so a filter handed a file operand, which reads that
		// file and not the pipe, still qualifies. `tee out.log` is why operands
		// cannot just be refused.
		"go test ./... | cat data.csv",
		"go test ./... | head -n 5 data.csv",
		"go test ./... | tee out.log",
	} {
		if !logsCallIsBuild(lcShellCall(cmd)) {
			t.Errorf("run_shell %q must qualify", cmd)
		}
	}
}

func TestLogsGateRefusesEveryOtherShellCommand(t *testing.T) {
	for _, cmd := range []string{
		// search, file reads and listings
		"grep -rn foo .",
		"rg -n foo",
		"ag foo",
		"ack foo",
		"git grep -n foo",
		"git log --oneline",
		"cat data.csv",
		"sed -n 1,200p report.txt",
		"head -n 200 data.json",
		"ls -la",
		"find . -name '*.go'",
		// linters and formatters
		"flake8 .",
		"ruff check",
		"golangci-lint run",
		"eslint .",
		"shellcheck -f gcc build.sh",
		"gofmt -l .",
		"go fmt ./...",
		// build-tool subcommands that are not a build or a test
		"go run main.go",
		"go mod tidy",
		"go get example.com/x",
		"cargo run",
		"cargo fmt",
		"npm install",
		"npm run lint",
		"npm run start",
		"python script.py",
		"python -m flake8",
		"node server.js",
		"echo go test",
		"cd sub && go test ./...",
		"env go test ./...",
		"",
		"   ",
		// chaining
		"go test ./... && flake8 .",
		"go test ./... ; flake8 .",
		"go test ./... || flake8 .",
		"go test ./... & flake8 .",
		"go test ./...\nflake8 .",
		"go test ./...\rflake8 .",
		"(go test ./...)",
		// a pipe into anything but a pass-through filter or a search program
		"go test ./... | xargs flake8",
		"go test ./... | flake8 -",
		"go test ./... | tail -5 | xargs rm",
		"make | xargs rm",
		"go test ./... | python filter.py",
		"go test ./... |",
		"go test ./... | | cat",
		// a quote left open is not a command the scan can read
		"go test -run 'a ./...",
		`go test -run "a ./...`,
		"go test ./... | grep 'FAIL",
	} {
		if logsCallIsBuild(lcShellCall(cmd)) {
			t.Errorf("run_shell %q must NOT qualify", cmd)
		}
	}
}

// TestLogsGateScansQuotesTheWayAShellDoes: single quotes take no backslash
// escape, so the quote in 'a\' closes at the second quote and the ";" after it
// chains; double quotes do take one.
func TestLogsGateScansQuotesTheWayAShellDoes(t *testing.T) {
	if logsCallIsBuild(lcShellCall(`go test 'a\' ; flake8 .`)) {
		t.Error(`a ";" after a single-quoted backslash chains: the command must NOT qualify`)
	}
	if logsCallIsBuild(lcShellCall(`go test 'a\' ; flake8 .' x`)) {
		t.Error(`the quote after a single-quoted backslash is real: a later quote must not swallow the ";"`)
	}
	if logsCallIsBuild(lcShellCall(`go test -run a\; ./... ; flake8 .`)) {
		t.Error("an escaped ';' hides only itself: the second ';' chains")
	}
	if !logsCallIsBuild(lcShellCall(`go test -run a\;b ./...`)) {
		t.Error("a backslash-escaped ';' is not a separator")
	}
	if !logsCallIsBuild(lcShellCall(`go test -run "a\"; b" ./...`)) {
		t.Error(`an escaped quote inside double quotes does not close them: the ";" stays quoted`)
	}
	if logsCallIsBuild(lcShellCall(`go test -run "a\\" ; flake8 .`)) {
		t.Error(`a double-quoted escaped backslash leaves the closing quote real: the ";" chains`)
	}
	if logsCallIsBuild(lcShellCall("go test ./...\r; flake8 .")) || logsCallIsBuild(lcShellCall("go test ./...\rflake8 .")) {
		t.Error("a lone carriage return separates commands")
	}
}

func TestLogsGateQualifiesBuildAndTestRunCalls(t *testing.T) {
	for _, c := range []ToolCall{
		lcRunCall("go", "test", "./..."),
		lcRunCall("go", "build", "./..."),
		lcRunCall("go", "vet", "./..."),
		lcRunCall("cargo", "build"),
		lcRunCall("cargo", "test", "--release"),
		lcRunCall("cargo", "check"),
		lcRunCall("npm", "test"),
		lcRunCall("npm", "t"),
		lcRunCall("npm", "run", "build"),
		lcRunCall("pytest"),
		lcRunCall("pytest", "-q"),
		lcRunCall("python", "-m", "pytest"),
		lcRunCall("python3", "-m", "pytest", "-x"),
		lcRunCall("python", "-m", "unittest"),
		lcRunCall("python3", "-m", "unittest", "discover"),
		lcRunCall(" go ", "test"),
	} {
		if !logsCallIsBuild(c) {
			t.Errorf("run %s must qualify", c.Args)
		}
	}
	for _, c := range []ToolCall{
		lcRunCall("go", "run", "main.go"),
		lcRunCall("go", "mod", "tidy"),
		lcRunCall("go"),
		lcRunCall("gofmt", "-l", "."),
		lcRunCall("cargo", "run"),
		lcRunCall("npm", "install"),
		lcRunCall("npm", "run", "lint"),
		lcRunCall("python", "script.py"),
		lcRunCall("python", "-m", "flake8"),
		lcRunCall("node", "server.js"),
		lcRunCall("git", "grep", "-n", "foo"),
		lcRunCall("git", "log"),
		lcRunCall("grep", "-rn", "foo", "."),
		lcRunCall("flake8", "."),
		lcRunCall(""),
		// the run tool takes a bare program and its arguments: a shell line in
		// `command` is not a program, and an operator in an argument is data
		lcRunCall("go test ./..."),
		lcRunCall("make"),
	} {
		if logsCallIsBuild(c) {
			t.Errorf("run %s must NOT qualify", c.Args)
		}
	}
}

func TestLogsGateReadsOnlyTheArgumentsItKnows(t *testing.T) {
	cases := []struct {
		name string
		call ToolCall
		want bool
	}{
		{"shell, command key", ToolCall{Name: "run_shell", Args: `{"command":"go test ./..."}`}, true},
		{"shell, other key", ToolCall{Name: "run_shell", Args: `{"cmd":"go test ./..."}`}, false},
		{"shell, malformed JSON", ToolCall{Name: "run_shell", Args: `{"command":`}, false},
		{"shell, not an object", ToolCall{Name: "run_shell", Args: `"go test ./..."`}, false},
		{"shell, empty args", ToolCall{Name: "run_shell", Args: ``}, false},
		{"shell, empty object", ToolCall{Name: "run_shell", Args: `{}`}, false},
		{"run, command key", ToolCall{Name: "run", Args: `{"command":"go","args":["test"]}`}, true},
		{"run, other key", ToolCall{Name: "run", Args: `{"cmd":"go","args":["test"]}`}, false},
		{"run, no args", ToolCall{Name: "run", Args: `{"command":"pytest"}`}, true},
		{"run, malformed JSON", ToolCall{Name: "run", Args: `{"command":"go"`}, false},
		{"run, non-string arguments", ToolCall{Name: "run", Args: `{"command":"go","args":[1,2]}`}, false},
		{"run, empty args", ToolCall{Name: "run", Args: ``}, false},
		{"another tool with a build command", ToolCall{Name: "read_file", Args: `{"command":"go test ./..."}`}, false},
		{"search_files", ToolCall{Name: "search_files", Args: `{"pattern":"go test"}`}, false},
		{"a tool with no name", ToolCall{Name: "", Args: `{"command":"go test ./..."}`}, false},
	}
	for _, c := range cases {
		if got := logsCallIsBuild(c.call); got != c.want {
			t.Errorf("%s: logsCallIsBuild = %v, want %v", c.name, got, c.want)
		}
	}
}

// --- the rung --------------------------------------------------------------------

// lcListing builds n lines of the form format, filled with the line number.
func lcListing(format string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, format, i, i)
		b.WriteByte('\n')
	}
	return b.String()
}

// lcOlderBody is a transcript with one older unit (the call and its body, at
// indices 3 and 4), a second older unit and a recent turn. logsRung is run on
// the window [3, 5) alone, so only the first unit can change.
func lcOlderBody(call ToolCall, body string) []Msg {
	msgs := lcTranscript(body, lrServerLog(170))
	call.ID = "call-1"
	msgs[3].ToolCalls = []ToolCall{call}
	return msgs
}

func lcFolds(call ToolCall, body string) bool {
	msgs := lcOlderBody(call, body)
	out := append([]Msg(nil), msgs...)
	logsRung(out, 3, 5, 1, nil)
	return out[4].Content != msgs[4].Content
}

func TestLogCollapseRungLeavesSearchFileAndLinterOutputAlone(t *testing.T) {
	bodies := []struct {
		what string
		call ToolCall
		body string
		// shaped is true for a listing whose lines are file:line diagnostics: the
		// line shape keeps them whatever the call, so the control that the fold
		// would shorten the body does not apply.
		shaped bool
	}{
		{"grep -rn listing", lcShellCall("grep -rn handle ."), lcListing("internal/pkg/file%d.go:%d: result := handle(req)", 90), true},
		{"grep -rl listing", lcShellCall("grep -rl handle ."), lcListing("internal/pkg/file%d_%d.go", 90), false},
		{"rg -n listing", lcShellCall("rg -n handle"), lcListing("internal/pkg/file%d.go:%d:    return handle(req)", 90), true},
		{"git grep listing through run", lcRunCall("git", "grep", "-n", "handle"), lcListing("internal/pkg/file%d.go:%d:    return handle(req)", 90), true},
		{"cat of a data file", lcShellCall("cat users.csv"), lcListing("alice,%d,active,%d,eu-west", 90), false},
		{"sed of a data file", lcShellCall("sed -n 1,200p users.csv"), lcListing("alice,%d,active,%d,eu-west", 90), false},
		{"flake8 output", lcShellCall("flake8 ."), lcListing("app/mod%d.py:%d:80: E501 line too long (95 > 79 characters)", 90), true},
		{"a linter's summary lines", lcShellCall("ruff check"), lcListing("app/mod%d.py: %d fixable files, style only", 90), false},
		{"a build piped into xargs", lcShellCall("go test ./... | xargs flake8"), lcListing("app/mod%d.py: unit %d", 90), false},
		{"a build chained to a linter", lcShellCall("go test ./... && flake8 ."), lcListing("app/mod%d.py: unit %d", 90), false},
	}
	for _, c := range bodies {
		if _, ok := logCollapse(c.body); !ok && !c.shaped {
			t.Fatalf("%s: control: the body is one the fold would shorten, or this test proves nothing", c.what)
		}
		if lcFolds(c.call, c.body) {
			t.Errorf("%s: the rung folded a result it must not touch", c.what)
		}
		// The ladder with the rung on returns what the ladder without it returns.
		msgs := lcOlderBody(c.call, c.body)
		budget := estimateTokens(msgs) * 6 / 10
		off := compact(context.Background(), msgs, budget, 2, 3, compactOpts{})
		on := compact(context.Background(), msgs, budget, 2, 3, lcLogOptsOn)
		if lrDigest(off) != lrDigest(on) {
			t.Errorf("%s: the ladder with the rung on differs from the ladder with it off", c.what)
		}
	}
}

func TestLogCollapseRungStillFoldsBuildAndTestOutput(t *testing.T) {
	log := lcSubtestLog(80)
	for _, c := range []ToolCall{
		lcShellCall("go test -v ./widget/..."),
		lcShellCall("go test -v ./widget/... 2>&1 | tail -3000"),
		lcShellCall("make test"),
		lcShellCall("cargo test"),
		lcShellCall("npm run build"),
		lcShellCall("pytest -q"),
		lcShellCall("npm t"),
		lcShellCall("python -m unittest discover"),
		lcShellCall("python3 -m unittest -v"),
		lcRunCall("go", "test", "-v", "./widget/..."),
		lcRunCall("cargo", "build"),
		lcRunCall("npm", "test"),
		lcRunCall("npm", "t"),
		lcRunCall("python", "-m", "unittest"),
		lcRunCall("pytest"),
	} {
		if !lcFolds(c, log) {
			t.Errorf("%s %s: the rung must still fold a build or test log", c.Name, c.Args)
		}
	}
	// And through the whole ladder: the older test log folds, the older listing
	// beside it does not.
	msgs := lcTranscript(lcListing("internal/pkg/file%d_%d.go", 90), log, lrServerLog(170))
	msgs[3].ToolCalls = []ToolCall{{ID: "call-1", Name: "run_shell", Args: `{"command":"grep -rl handle ."}`}}
	probe := append([]Msg(nil), msgs...)
	folded, ok := logCollapse(msgs[6].Content)
	if !ok {
		t.Fatal("control: the test log must fold")
	}
	probe[6].Content = folded
	out := compact(context.Background(), msgs, estimateTokens(probe), 2, 3, lcLogOptsOn)
	if out[4].Content != msgs[4].Content {
		t.Error("the grep listing was folded")
	}
	if out[6].Content != folded {
		t.Error("the go test output was not folded")
	}
}

// TestLogCollapseRungRefusesEmptyCallIds: a call and a result that both carry
// an empty id say nothing about each other, so the pair is not made.
func TestLogCollapseRungRefusesEmptyCallIds(t *testing.T) {
	body := lrServerLog(150)
	empty := func(callID, resultID string) []Msg {
		return []Msg{
			{Role: "system", Content: "You are a coding agent."},
			{Role: "user", Content: "objective: find out why the widget build is red"},
			{Role: "user", Content: "exemplar"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: callID, Name: "run_shell", Args: lcShellArgs}}},
			{Role: "tool", ToolCallID: resultID, Content: body},
			{Role: "assistant", Content: "reading it"},
		}
	}
	for _, c := range []struct{ callID, resultID string }{{"", ""}, {"call-1", ""}, {"", "call-1"}} {
		msgs := empty(c.callID, c.resultID)
		out := append([]Msg(nil), msgs...)
		logsRung(out, 3, 5, 1, nil)
		if out[4].Content != msgs[4].Content {
			t.Errorf("call id %q and result id %q: the pair was made and the body folded", c.callID, c.resultID)
		}
	}
	msgs := empty("call-1", "call-1")
	out := append([]Msg(nil), msgs...)
	logsRung(out, 3, 5, 1, nil)
	if !isLogCollapsed(out[4].Content) {
		t.Error("control: a call and result under one real id must pair and fold")
	}
}

// --- the real shell framing and the truncation markers -----------------------------

// lcFramedStdout is a long, mostly routine stdout.
func lcFramedStdout(n int) string {
	var b strings.Builder
	b.WriteString("go test -v ./widget/...\n")
	for i := 0; i < n; i++ {
		b.WriteString(lcAccess(i))
		b.WriteByte('\n')
	}
	return b.String()
}

func TestLogCollapseKeepsTheShellFramingOfARealResult(t *testing.T) {
	stderr := "widget_test.go:88: got 3, want 4\nFAIL\texample.com/widget\t0.412s\n"
	body := formatShellResult(sandbox.Result{ExitCode: 1, Stdout: lcFramedStdout(400), Stderr: stderr})
	got, ok := logCollapse(body)
	if !ok {
		t.Fatal("a real run_shell result with a long routine stdout must fold")
	}
	inner := strings.TrimPrefix(got, strings.SplitN(got, "\n", 2)[0]+"\n")
	if !strings.HasPrefix(inner, "exit=1\n--- stdout ---\n") {
		t.Errorf("the exit line and the stdout separator must open the body:\n%.200s", inner)
	}
	if n := strings.Count(got, "\n--- stderr ---\n"); n != 1 {
		t.Errorf("the stderr separator must survive exactly once, got %d", n)
	}
	for _, want := range []string{"widget_test.go:88: got 3, want 4", "FAIL\texample.com/widget\t0.412s"} {
		if !strings.Contains(got, want) {
			t.Errorf("a stderr line was lost: %q", want)
		}
	}
	// The same result with a killed-by-signal exit line.
	killed := formatShellResult(sandbox.Result{ExitCode: 137, Signal: 9, Stdout: lcFramedStdout(400), Stderr: ""})
	out, ok := logCollapse(killed)
	if !ok || !strings.Contains(out, "exit=137 (killed by signal 9)\n--- stdout ---\n") {
		t.Errorf("the killed-by-signal exit line must survive (ok=%v)", ok)
	}
}

func TestLogCollapseKeepsTheTruncationMarkerOfAClippedResult(t *testing.T) {
	// stdout beyond the shell tool's cap is clipped and ends in its marker.
	stdout := lcFramedStdout(1500)
	if len(stdout) < shellOutputCap+1024 {
		t.Fatalf("control: stdout must exceed the cap: %d <= %d", len(stdout), shellOutputCap)
	}
	body := formatShellResult(sandbox.Result{ExitCode: 2, Stdout: stdout, Stderr: "build failed: exit status 2\n"})
	marker := "\n…(truncated)\n--- stderr ---\n"
	if !strings.Contains(body, marker) {
		t.Fatalf("control: the clipped result must carry the marker before the stderr separator")
	}
	got, ok := logCollapse(body)
	if !ok {
		t.Fatal("a clipped result must fold")
	}
	if !strings.Contains(got, marker) {
		t.Errorf("the truncation marker and the stderr separator that follows it must stay adjacent and verbatim:\n%s", got[len(got)-400:])
	}
	if n := strings.Count(got, "…(truncated)"); n != 1 {
		t.Errorf("the truncation marker must survive exactly once, got %d", n)
	}
	if !strings.Contains(got, "build failed: exit status 2") {
		t.Error("the stderr line was lost")
	}
}

func TestLogCollapseKeepsTheTrimMarkerOfATrimmedResult(t *testing.T) {
	// The loop trims a result that is over the context cap to its head and tail
	// around a marker line before it is stored.
	stderr := "widget_test.go:88: got 3, want 4\nFAIL\texample.com/widget\t0.412s\n"
	body := formatShellResult(sandbox.Result{ExitCode: 1, Stdout: lcFramedStdout(900), Stderr: stderr})
	trimmed, did := contextbudget.Trim(body, 30000)
	if !did {
		t.Fatal("control: the body must exceed the trim cap")
	}
	const marker = "[...content elided to fit local model context...]"
	if !strings.Contains(trimmed, marker) {
		t.Fatal("control: the trimmed result must carry the marker")
	}
	got, ok := logCollapse(trimmed)
	if !ok {
		t.Fatal("a trimmed result must fold")
	}
	if n := strings.Count(got, marker); n != 1 {
		t.Errorf("the trim marker must survive exactly once, got %d", n)
	}
	if !strings.Contains(got, "\n"+marker+"\n") {
		t.Errorf("the trim marker must stay on a line of its own")
	}
	inner := strings.TrimPrefix(got, strings.SplitN(got, "\n", 2)[0]+"\n")
	if !strings.HasPrefix(inner, "exit=1\n--- stdout ---\n") {
		t.Errorf("the exit line must still open the body:\n%.120s", inner)
	}
	for _, want := range []string{"widget_test.go:88: got 3, want 4", "--- stderr ---"} {
		if !strings.Contains(got, want) {
			t.Errorf("a line was lost: %q", want)
		}
	}
}

// --- digit runs, identifiers and diagnostics --------------------------------------

func TestLogCollapseNeverFoldsLinesThatDifferInAnIdentifier(t *testing.T) {
	var ids []string
	for _, name := range []string{"a1", "b2", "c3", "d4", "e5", "f6", "g7", "h8", "i9", "j10", "k11", "l12"} {
		ids = append(ids, "undefined: "+name)
	}
	in := lcJoin(lcHead, ids, lcTail)
	if got, ok := logCollapse(in); ok || got != in {
		t.Errorf("twelve different undefined identifiers must all stay: ok=%v changed=%v", ok, got != in)
	}
	var algos []string
	for _, name := range []string{"md5", "sha1", "crc32", "sha256", "adler32", "blake2b", "xxh64", "fnv1a", "murmur3", "sha512"} {
		algos = append(algos, "unsupported digest: "+name+" requested by the caller")
	}
	if got, ok := logCollapse(lcJoin(lcHead, algos, lcTail)); ok {
		t.Errorf("lines that name different algorithms must not fold: %q", got)
	}
	// Control: lines that differ only in a digit run still fold.
	var same []string
	for i := 0; i < 20; i++ {
		same = append(same, fmt.Sprintf("worker%d finished unit %d of the build graph", i, i*3))
	}
	if _, ok := logCollapse(lcJoin(lcHead, same, lcTail)); !ok {
		t.Error("control: lines that differ only in digit runs must fold")
	}
}

func TestLogsKeepRecognisesFileLineDiagnostics(t *testing.T) {
	keep := []string{
		"main.go:12:5: declared and not used: v",
		"pkg/sub/a.go:7:1: missing return",
		"    widget_test.go:88: got 3, want 4",
		"\twidget_test.go:88: got 3, want 4",
		"src/lib.rs:3:9",
		"  --> src/main.rs:12:5",
		"tests/test_widget.py:41: in test_render",
		`C:\work\widget\a.go:3:4: declared and not used: v`,
		"./a.go:3:4: declared and not used: v",
		"a.c:10:3: note: previous declaration is here",
		"src/lib.rs:3",
		"pkg/a.go:12:5",
		"\x1b[1m main.go:12:5: declared and not used: v",
	}
	for _, l := range keep {
		if !logsKeep(l) {
			t.Errorf("a file:line diagnostic must be kept verbatim: %q", l)
		}
	}
	fold := []string{
		"12:30:45 up 3 days",
		"127.0.0.1:18080: accepted a connection",
		"0.0.0.0:8080: bind",
		"listening on 127.0.0.1:18080",
		"GET http://localhost:8080/health 200",
		"build:12: ok then",
		"retry 3:4: waiting for the cache",
		"took 12:30 to build",
		"2026-10-14T09:12:01.123Z INFO  req=0badc0de GET /health 200 4ms",
		"downloading module-3.tgz 12/1024 KB",
	}
	for _, l := range fold {
		if logsKeep(l) {
			t.Errorf("not a diagnostic, must stay foldable: %q", l)
		}
	}
}

func TestLogCollapseNeverFoldsDistinctDiagnostics(t *testing.T) {
	for _, format := range []string{
		"pkg/a.go:%d:5: declared and not used: v%d",
		"    widget_test.go:%d: got %d, want 4",
		"  --> src/main.rs:%d:%d",
	} {
		var diag []string
		for i := 0; i < 20; i++ {
			diag = append(diag, fmt.Sprintf(format, 10+i, i))
		}
		in := lcJoin(lcHead, diag, lcTail)
		if got, ok := logCollapse(in); ok || got != in {
			t.Errorf("%q: distinct diagnostics must all stay: ok=%v changed=%v", format, ok, got != in)
		}
		// Beside a run that does fold, every diagnostic is still there.
		got, ok := logCollapse(lcJoin(lcHead, diag, lcRepeat("retrying the build cache lookup", 40), lcTail))
		if !ok {
			t.Fatalf("%q: control: the routine run must fold", format)
		}
		for _, l := range diag {
			if !strings.Contains(got, l+"\n") {
				t.Errorf("%q: a diagnostic was folded away: %q", format, l)
			}
		}
	}
}

// --- the last line of a run --------------------------------------------------------

// TestLogCollapseDoesNotFoldARunWhoseFirstAndLastLineCostAsMuchAsTheRun: keeping
// the last line as well changes what a fold saves, so the per-run check counts
// both lines kept. Three 25-byte lines cost 78 bytes; first line, marker and last
// line cost 84.
func TestLogCollapseDoesNotFoldARunWhoseFirstAndLastLineCostAsMuchAsTheRun(t *testing.T) {
	mid := []string{"step output line number 1", "step output line number 2", "step output line number 3"}
	for i := 0; i < 10; i++ {
		mid = append(mid, fmt.Sprintf("distinct middle line number %c of the transcript body", 'a'+i))
	}
	if got, ok := logCollapse(lcJoin(lcHead, mid, lcTail)); ok {
		t.Errorf("a run that would not shrink once its last line is kept was folded:\n%s", got)
	}
	got, ok := logCollapse(lcJoin(lcHead, mid, lcRepeat("retrying the build cache lookup", 40), lcTail))
	if !ok {
		t.Fatal("control: the long run should fold")
	}
	if !strings.Contains(got, strings.Join(mid[:3], "\n")+"\n") {
		t.Errorf("the three short lines were folded:\n%s", got)
	}
}

func TestLogCollapseKeepsTheFirstAndLastLineOfARun(t *testing.T) {
	var prog []string
	for i := 1; i <= 60; i++ {
		prog = append(prog, fmt.Sprintf("  downloading module-a.tgz %d/60 KB", i))
	}
	in := lcJoin(lcHead, prog, lcTail)
	got, ok := logCollapse(in)
	if !ok {
		t.Fatal("a 60-line progress run must fold")
	}
	want := prog[0] + "\n[... 58 similar lines folded ...]\n" + prog[59] + "\n"
	if !strings.Contains(got, want) {
		t.Errorf("a run must fold to its first line, a count of N-2 and its last line:\n%s", got)
	}
	if !strings.Contains(got, "(58 lines hidden;") {
		t.Errorf("the footer must count the hidden lines as N-2:\n%s", got)
	}
	// A run of three folds one line, and the marker says so in the singular.
	long := lcLongLine
	three := []string{fmt.Sprintf(long, 5), fmt.Sprintf(long, 6), fmt.Sprintf(long, 7)}
	got, ok = logCollapse(lcJoin(lcHead, three, []string{"between", "the", "windows", "we", "check"}, lcTail))
	if !ok || !strings.Contains(got, "[... 1 similar line folded ...]\n") {
		t.Errorf("a run of three must say '1 similar line folded':\n%s", got)
	}
}

// --- both rungs on -------------------------------------------------------------------

// lcBothRungsOpts is every optional rung that exists: the log rung and the
// search rung. The log rung runs first (compaction.go): it keeps every
// distinct line, the search rung drops most of a listing.
func lcBothRungsOpts() compactOpts { return compactOpts{LogCollapse: true, SearchResults: true} }

// lcBothRungsTranscript is the transcript the two rungs meet in: two older
// listings (a `grep -rl` one of bare paths, which no search rung parses and no
// log rung claims; a `grep -rn` one of path:line:text hits, the shape the search
// rung thins) and an older `go test` log, all inside the ladder's window, then
// one recent log. The folded build log and the thinned hits listing are
// returned beside it, built by the rungs' own functions, so a test can set a
// budget that needs exactly one rung's edit or both.
func lcBothRungsTranscript(t *testing.T) (msgs []Msg, foldedBuild, thinnedHits string) {
	t.Helper()
	paths := lcListing("internal/pkg/file%d_%d.go", 90)
	// Line numbers start at 1: a match on line 0 is no match the search rung accepts,
	// and one such line would make the whole listing a shape it leaves alone.
	var hb strings.Builder
	for i := 0; i < 90; i++ {
		fmt.Fprintf(&hb, "internal/pkg/file%d.go:%d:    return handle(req)\n", i, i+1)
	}
	hits := hb.String()
	msgs = lcTranscript(paths, hits, lcSubtestLog(100), lrServerLog(170))
	msgs[3].ToolCalls = []ToolCall{{ID: "call-1", Name: "run_shell", Args: `{"command":"grep -rl handle ."}`}}
	msgs[5].ToolCalls = []ToolCall{{ID: "call-2", Name: "run_shell", Args: `{"command":"grep -rn handle ."}`}}
	if _, ok := logCollapse(paths); !ok {
		t.Fatal("control: the paths listing is one the fold would shorten")
	}
	if _, ok := searchRungBodyForm(paths, nil, searchFormAny); ok {
		t.Fatal("control: the paths listing is not one the search rung parses")
	}
	foldedBuild, ok := logCollapse(msgs[8].Content)
	if !ok {
		t.Fatal("control: the build log must fold")
	}
	thinnedHits, ok = searchRungBodyForm(hits, searchQueryTerms(msgs[2].Content), searchFormFlat)
	if !ok {
		t.Fatal("control: the grep -rn listing must be one the search rung thins")
	}
	return msgs, foldedBuild, thinnedHits
}

// lcBudgetWith is the estimate of msgs with the bodies at the given indexes
// replaced: the budget at which exactly those edits are enough.
func lcBudgetWith(msgs []Msg, edits map[int]string) int {
	probe := append([]Msg(nil), msgs...)
	for i, body := range edits {
		probe[i].Content = body
	}
	return estimateTokens(probe)
}

// TestLogCollapseAndSearchRungsDoNotClaimTheSameOutput: a grep listing whose
// lines differ only in a number is the search rung's, not the log rung's, and
// the build output beside it is the log rung's, not the search rung's. With both
// rungs on, each edits only its own bodies, and the log rung, which runs first,
// leaves the search rung nothing to do when the fold alone fits the budget.
func TestLogCollapseAndSearchRungsDoNotClaimTheSameOutput(t *testing.T) {
	msgs, foldedBuild, thinnedHits := lcBothRungsTranscript(t)

	// The fold alone is enough: the log rung stops the ladder at the first fit,
	// so both listings stay byte for byte as the tools printed them.
	out := compact(context.Background(), msgs, lcBudgetWith(msgs, map[int]string{8: foldedBuild}), 2, 3, lcBothRungsOpts())
	for _, i := range []int{4, 6} {
		if out[i].Content != msgs[i].Content {
			t.Errorf("message %d: a budget the fold meets must leave the listing raw:\n%.200s", i, out[i].Content)
		}
	}
	if out[8].Content != foldedBuild {
		t.Errorf("the build output was not folded:\n%.200s", out[8].Content)
	}
	pairing(t, out)

	// The fold is not enough: the search rung thins the grep -rn listing, the
	// bare-path listing stays raw (neither rung claims it), and the build log
	// is the fold the log rung alone writes (the search rung left it alone).
	out = compact(context.Background(), msgs, lcBudgetWith(msgs, map[int]string{6: thinnedHits, 8: foldedBuild}), 2, 3, lcBothRungsOpts())
	if out[4].Content != msgs[4].Content {
		t.Errorf("the bare-path listing is nobody's and must stay raw:\n%.200s", out[4].Content)
	}
	if out[6].Content != thinnedHits || isLogCollapsed(out[6].Content) {
		t.Errorf("the grep -rn listing must be the search rung's thinned form, not folded:\n%.200s", out[6].Content)
	}
	if out[8].Content != foldedBuild {
		t.Errorf("the build output must be the log rung's fold, untouched by the search rung:\n%.200s", out[8].Content)
	}
	pairing(t, out)
}

// TestLogCollapseAndSearchRungsKeepEachOthersMarkersHonest: a listing the
// search rung thinned and a log the log rung folded each open with a disclosure
// line that names the size the body had. With the log rung on, its size parser
// believes the search rung's line (it is the skeleton header, which the search
// rung writes through skeletonHeader), so under a budget nothing meets the
// reactive last resort's marker names the original size of both, not the
// smaller size the rung left.
func TestLogCollapseAndSearchRungsKeepEachOthersMarkersHonest(t *testing.T) {
	msgs, foldedBuild, thinnedHits := lcBothRungsTranscript(t)
	hitsLen, buildLen := len(msgs[6].Content), len(msgs[8].Content)
	if n, ok := logsSkeletonSize(thinnedHits); !ok || n != hitsLen {
		t.Fatalf("the log rung's parser must believe the search rung's header: got %d, %v, want %d", n, ok, hitsLen)
	}
	if n, ok := logsOriginalSize(foldedBuild); !ok || n != buildLen {
		t.Fatalf("control: the fold's header must name the build log's size: got %d, %v, want %d", n, ok, buildLen)
	}
	edited := append([]Msg(nil), msgs...)
	edited[6].Content, edited[8].Content = thinnedHits, foldedBuild
	// With the log rung off, the fold's header is plain text to the default
	// ladder, so only the search rung's listing is named by its original size.
	check := func(opts compactOpts, want map[int]int) {
		t.Helper()
		out := emergencyShrinkWith(edited, 1, 3, opts)
		for i, n := range want {
			if marker := elisionMarker(n); !strings.HasPrefix(out[i].Content, marker) {
				t.Errorf("opts %+v, message %d: want a marker naming %d chars (%q), got %.120q", opts, i, n, marker, out[i].Content)
			}
		}
		pairing(t, out)
	}
	check(lcBothRungsOpts(), map[int]int{6: hitsLen, 8: buildLen})
	check(compactOpts{SearchResults: true}, map[int]int{6: hitsLen})
}
