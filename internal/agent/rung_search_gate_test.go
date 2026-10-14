package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The rung is gated on the call that produced a body: a body is thinned only if
// it came from the harness's own search tool (grouped form) or from a shell/run
// call whose command is a grep-family program (flat form). A linter prints the
// same "path:line:col:text" lines, and every one of its diagnostics matters.
// ---------------------------------------------------------------------------

// linterBody renders n diagnostics over nfiles files in the flake8/ruff/gcc shape.
func linterBody(n, nfiles int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "src/mod%02d.py:%d:%d: E501 line too long (%d > 79 characters)\n", i%nfiles, 10+i, 80+i%7, 90+i)
	}
	return strings.TrimRight(b.String(), "\n")
}

// gateMsgs is [system, objective, call, result, filler call, filler result,
// filler call, filler result]: the call under test sits in the older span and
// the two trailing turns are the kept-recent tail (rungKeep = 2 messages).
func gateMsgs(call ToolCall, body string, withCall bool) []Msg {
	msgs := []Msg{
		{Role: "system", Content: "You are a coding agent."},
		{Role: "user", Content: rungTask},
	}
	if withCall {
		msgs = append(msgs, Msg{Role: "assistant", ToolCalls: []ToolCall{call}})
	} else {
		msgs = append(msgs, Msg{Role: "assistant", Content: "looking"})
	}
	msgs = append(msgs, Msg{Role: "tool", ToolCallID: "g1", Content: body})
	msgs = append(msgs,
		Msg{Role: "assistant", ToolCalls: []ToolCall{{ID: "g2", Name: "read_file", Args: `{"path":"a.go"}`}}},
		Msg{Role: "tool", ToolCallID: "g2", Content: "short file"},
	)
	return msgs
}

// gateOutcome runs the rung with a budget nothing can meet and reports whether the
// body of the call under test was rewritten.
func gateOutcome(t *testing.T, call ToolCall, body string, withCall bool) (thinned bool, got string) {
	t.Helper()
	msgs := gateMsgs(call, body, withCall)
	searchRung(msgs, rungProtected, len(msgs)-rungKeep, 1, nil)
	got = msgs[3].Content
	return got != body, got
}

func shellCall(command string) ToolCall {
	return ToolCall{ID: "g1", Name: "run_shell", Args: fmt.Sprintf(`{"command":%q}`, command)}
}

func runCall(command string, args ...string) ToolCall {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = fmt.Sprintf("%q", a)
	}
	return ToolCall{ID: "g1", Name: "run", Args: fmt.Sprintf(`{"command":%q,"args":[%s]}`, command, strings.Join(quoted, ","))}
}

func gateSearchCall() ToolCall {
	return ToolCall{ID: "g1", Name: "search_files", Args: `{"pattern":"timeout"}`}
}

func TestSearchRungLeavesLinterOutputFromANonSearchCallAlone(t *testing.T) {
	body := prodShell(1, linterBody(80, 12), "")
	if _, ok := searchRungBody(body, rungQuery()); !ok {
		t.Fatal("fixture: this linter output has the listing shape and must be one the shape check would thin, or this test proves nothing")
	}
	for _, cmd := range []string{
		"flake8 .",
		"flake8 --max-line-length=79 src",
		"ruff check .",
		"shellcheck -f gcc scripts/*.sh",
		"rubocop --format emacs",
		"golangci-lint run --out-format=line-number",
		"mypy src",
		"pylint src",
		"cd src && grep -rn x .", // the command's first word is not a search program
		"echo grep",
		"grepx -rn timeout .",
		"git status",
		"git log --grep=timeout",
		"egrep -rn timeout .",
	} {
		if thinned, got := gateOutcome(t, shellCall(cmd), body, true); thinned {
			t.Errorf("run_shell %q: its diagnostics were thinned:\n%.200s", cmd, got)
		}
	}
	// The direct-exec tool: the same rule, on its command and first argument.
	for _, c := range []ToolCall{
		runCall("python", "-m", "flake8", "."),
		runCall("python3", "-m", "ruff", "check", "."),
		runCall("git", "log", "--grep=timeout"),
		runCall("git", "status"),
		runCall("git"),
		runCall("go", "vet", "./..."),
	} {
		if thinned, got := gateOutcome(t, c, body, true); thinned {
			t.Errorf("run %s: its diagnostics were thinned:\n%.200s", c.Args, got)
		}
	}
	// Other tools whose result happens to have the shape.
	for _, name := range []string{"read_file", "list_dir", "github_api", "browse", "web_search", "update_plan"} {
		c := ToolCall{ID: "g1", Name: name, Args: `{"path":"x"}`}
		if thinned, _ := gateOutcome(t, c, linterBody(80, 12), true); thinned {
			t.Errorf("%s: a body from a non-search tool was thinned", name)
		}
	}
}

func TestSearchRungStillThinsGrepOutputFromAGrepCall(t *testing.T) {
	flat := fixtureFlat(benignHits(51, 16, 80))
	body := prodShell(0, flat+"\n", "")
	if _, ok := searchRungBody(body, rungQuery()); !ok {
		t.Fatal("fixture: must be a thinnable flat listing")
	}
	for _, cmd := range []string{
		"grep -rn timeout .",
		"grep -rnE 'fetch|dial' internal",
		"rg -n timeout",
		"rg --line-number timeout internal",
		"ag timeout",
		"ack timeout",
		"git grep -n timeout",
		"git grep -n timeout -- '*.go'",
		"  grep -rn timeout .",
		"/usr/bin/grep -rn timeout .",
		"grep -rn timeout . | head -200",
	} {
		thinned, got := gateOutcome(t, shellCall(cmd), body, true)
		if !thinned || !strings.Contains(got, "lines elided") {
			t.Errorf("run_shell %q: a grep listing must still be thinned", cmd)
		}
	}
	for _, c := range []ToolCall{
		runCall("rg", "-n", "timeout"),
		runCall("grep", "-rn", "timeout", "."),
		runCall("git", "grep", "-n", "timeout"),
	} {
		thinned, got := gateOutcome(t, c, body, true)
		if !thinned || !strings.Contains(got, "lines elided") {
			t.Errorf("run %s: a grep listing must still be thinned", c.Args)
		}
	}
	// Bare stdout (no framing) from a grep call is the same listing.
	if thinned, _ := gateOutcome(t, shellCall("rg -n timeout"), flat, true); !thinned {
		t.Error("unframed grep output must still be thinned")
	}
}

func TestSearchRungStillThinsTheHarnessSearchTool(t *testing.T) {
	body := prodGrouped(benignHits(52, 20, 90), true)
	thinned, got := gateOutcome(t, gateSearchCall(), body, true)
	if !thinned || !strings.Contains(got, "lines elided") {
		t.Error("a search_files listing must still be thinned")
	}
}

func TestSearchRungLeavesABodyWithNoMatchingCallAlone(t *testing.T) {
	grouped := prodGrouped(benignHits(53, 20, 90), true)
	flat := prodShell(0, fixtureFlat(benignHits(54, 16, 80))+"\n", "")

	// No assistant call at all in front of the result.
	for name, body := range map[string]string{"grouped": grouped, "flat": flat} {
		if thinned, _ := gateOutcome(t, gateSearchCall(), body, false); thinned {
			t.Errorf("%s: a body with no originating call was thinned", name)
		}
	}
	// A call exists, but under another id.
	other := ToolCall{ID: "elsewhere", Name: "search_files", Args: `{"pattern":"timeout"}`}
	if thinned, _ := gateOutcome(t, other, grouped, true); thinned {
		t.Error("a result whose id matches no call was thinned")
	}
	other = ToolCall{ID: "elsewhere", Name: "run_shell", Args: `{"command":"grep -rn timeout ."}`}
	if thinned, _ := gateOutcome(t, other, flat, true); thinned {
		t.Error("a result whose id matches no call was thinned")
	}
	// A result with no id at all.
	msgs := gateMsgs(gateSearchCall(), grouped, true)
	msgs[3].ToolCallID = ""
	searchRung(msgs, rungProtected, len(msgs)-rungKeep, 1, nil)
	if msgs[3].Content != grouped {
		t.Error("a result with an empty id was thinned")
	}
	// The same id used by two different calls is ambiguous, so it fails closed.
	msgs = gateMsgs(gateSearchCall(), grouped, true)
	msgs = append([]Msg{msgs[0], msgs[1], {Role: "assistant", ToolCalls: []ToolCall{{ID: "g1", Name: "read_file", Args: `{"path":"x"}`}}}}, msgs[2:]...)
	searchRung(msgs, rungProtected, len(msgs)-rungKeep, 1, nil)
	if msgs[4].Content != grouped {
		t.Error("an id claimed by two different calls must not be trusted")
	}
}

func TestSearchRungRequiresTheFormTheCallProduces(t *testing.T) {
	grouped := prodGrouped(benignHits(55, 20, 90), true)
	flat := fixtureFlat(benignHits(56, 16, 80))
	// search_files prints the grouped form only; a flat listing from it is not its output.
	if thinned, _ := gateOutcome(t, gateSearchCall(), flat, true); thinned {
		t.Error("a flat listing from search_files was thinned")
	}
	// A grep-family program prints the flat form only.
	if thinned, _ := gateOutcome(t, shellCall("grep -rn timeout ."), grouped, true); thinned {
		t.Error("a grouped listing from a grep command was thinned")
	}
}

// ---------------------------------------------------------------------------
// The footer example in the docs is a footer the code can print.
// ---------------------------------------------------------------------------

// The documented example: a 100-match search_files listing over 20 files keeps the
// rung's caps (30 lines from at most 15 files) and elides the rest.
const documentedFooter = "[... 70 of 100 lines elided, 5 of 20 files elided ...]"

func documentedFooterListing() string {
	return prodGrouped(benignHits(61, 20, 100), false)
}

func TestSearchRungFooterExampleIsReachableAndPinned(t *testing.T) {
	out := mustThin(t, documentedFooterListing(), rungQuery())
	lines := strings.Split(out, "\n")
	if got := lines[len(lines)-1]; got != documentedFooter {
		t.Fatalf("the footer of the documented 100-line, 20-file listing is %q, want %q", got, documentedFooter)
	}
	// The example follows from the caps: it is not a number to remember.
	want := fmt.Sprintf("[... %d of 100 lines elided, %d of 20 files elided ...]", 100-searchRungMaxLines, 20-searchRungMaxFiles)
	if documentedFooter != want {
		t.Errorf("documented footer %q does not follow from the caps (%d lines, %d files): %q", documentedFooter, searchRungMaxLines, searchRungMaxFiles, want)
	}
}

var docFooterPattern = regexp.MustCompile(`\[\.\.\. (\d+) of (\d+) lines elided, (\d+) of (\d+) files elided \.\.\.\]`)

// checkDocFooters requires the documented example and holds every footer shown in
// text to what the caps allow: at most searchRungMaxLines lines and
// searchRungMaxFiles files survive, so a listing of F files elides at least
// F-searchRungMaxFiles of them.
func checkDocFooters(t *testing.T, name, text string) {
	t.Helper()
	if !strings.Contains(text, documentedFooter) {
		t.Errorf("%s does not show the footer %q", name, documentedFooter)
	}
	for _, m := range docFooterPattern.FindAllStringSubmatch(text, -1) {
		var el, tot, fel, ftot int
		fmt.Sscan(m[1], &el)
		fmt.Sscan(m[2], &tot)
		fmt.Sscan(m[3], &fel)
		fmt.Sscan(m[4], &ftot)
		if tot-el > searchRungMaxLines || ftot-fel > searchRungMaxFiles || el > tot || fel > ftot {
			t.Errorf("%s: footer %q cannot be printed (at most %d lines and %d files are kept)", name, m[0], searchRungMaxLines, searchRungMaxFiles)
		}
	}
}

func TestSearchRungFooterExampleAppearsInTheDocs(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "systems", "coding-agent.md"))
	if err != nil {
		t.Fatal(err)
	}
	checkDocFooters(t, "docs/systems/coding-agent.md", string(doc))
	// The fragment is consumed at release; check it while it exists.
	frags, _ := filepath.Glob(filepath.Join("..", "..", "changes", "unreleased", "*compaction-rung-search*.md"))
	for _, f := range frags {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		checkDocFooters(t, f, string(b))
	}
}

func TestSearchRungDoesNotPairTwoEmptyIDs(t *testing.T) {
	// A call and a result that both carry no id are not a pair: nothing says this
	// result came from this call, so it is left alone.
	grouped := prodGrouped(benignHits(57, 20, 90), true)
	call := gateSearchCall()
	call.ID = ""
	msgs := gateMsgs(call, grouped, true)
	msgs[3].ToolCallID = ""
	searchRung(msgs, rungProtected, len(msgs)-rungKeep, 1, nil)
	if msgs[3].Content != grouped {
		t.Error("a result and a call that both have an empty id were paired and thinned")
	}
}

// ---------------------------------------------------------------------------
// A command that chains another command after the search prints that command's
// output too, so the body is no longer the search's alone.
// ---------------------------------------------------------------------------

func TestSearchRungLeavesAChainedCommandAlone(t *testing.T) {
	body := prodShell(1, linterBody(80, 12), "")
	for _, cmd := range []string{
		"grep -n x f; flake8 .",
		"grep -q x f || ruff check .",
		"grep x f && flake8 .",
		"grep x f;flake8 .",
		"grep x f&&flake8 .",
		"grep x f||flake8 .",
		"grep x f & flake8 .",
		"grep -rn timeout . | head && flake8 .",
		"grep -rn 'a' . ; grep -rn \"b\" . ; flake8 .",
		"grep -rn timeout .\nflake8 .",
		"grep -rn \\\"x . ; flake8 .", // an escaped quote does not open a quoted span
		"git grep -n x; ruff check .",
		"rg -n x && flake8 .",
	} {
		if thinned, got := gateOutcome(t, shellCall(cmd), body, true); thinned {
			t.Errorf("run_shell %q: a chained command's output was thinned:\n%.200s", cmd, got)
		}
	}
}

func TestSearchRungKeepsPipesAndQuotedSeparatorsAsSearches(t *testing.T) {
	flat := fixtureFlat(benignHits(58, 16, 80))
	body := prodShell(0, flat+"\n", "")
	for _, cmd := range []string{
		"grep x f | head",
		"grep -rn timeout . | sort | uniq",
		"grep -rn timeout . |& head",
		`grep -rn "a;b" .`,
		`grep -rn 'a;b' .`,
		`grep -rn "a&&b" .`,
		`grep -rn 'a||b' .`,
		`grep -rn "say \"x\";y" .`,
		`grep -rnE "a|b" .`,
		`grep -rn a\;b .`,
		"grep -rn timeout . 2>&1",
		"grep -rn timeout . >/dev/null 2>&1 | head",
		"grep -rn timeout . &>/dev/null",
		"grep -rn timeout . &> f",
		"grep -rn timeout . >&2",
		"grep -rn timeout . <&3",
	} {
		thinned, got := gateOutcome(t, shellCall(cmd), body, true)
		if !thinned || !strings.Contains(got, "lines elided") {
			t.Errorf("run_shell %q: a plain search must still be thinned", cmd)
		}
	}
}

// ---------------------------------------------------------------------------
// Pins for the pairing and the form the run tool prints.
// ---------------------------------------------------------------------------

func TestSearchRungRunToolPrintsTheFlatFormOnly(t *testing.T) {
	grouped := prodGrouped(benignHits(59, 20, 90), true)
	for _, c := range []ToolCall{
		runCall("grep", "-rn", "timeout", "."),
		runCall("rg", "-n", "timeout"),
		runCall("git", "grep", "-n", "timeout"),
	} {
		if thinned, _ := gateOutcome(t, c, grouped, true); thinned {
			t.Errorf("run %s: a grouped listing from a grep command was thinned", c.Args)
		}
	}
}

func TestSearchRungTrimsTheRunCommand(t *testing.T) {
	flat := prodShell(0, fixtureFlat(benignHits(60, 16, 80))+"\n", "")
	for _, c := range []ToolCall{
		runCall("  grep", "-rn", "timeout", "."),
		runCall("\tgrep", "-rn", "timeout", "."),
		runCall(" git ", "grep", "-n", "timeout"),
	} {
		thinned, got := gateOutcome(t, c, flat, true)
		if !thinned || !strings.Contains(got, "lines elided") {
			t.Errorf("run %s: a command with surrounding space must still qualify", c.Args)
		}
	}
}

func TestSearchRungAmbiguityNeedsTheNameAndTheArgsToAgree(t *testing.T) {
	grouped := prodGrouped(benignHits(61, 20, 90), true)
	flat := prodShell(0, fixtureFlat(benignHits(62, 16, 80))+"\n", "")
	// two reuses an id of the call under test; the result sits under that id.
	with := func(first, second ToolCall, body string) string {
		msgs := []Msg{
			{Role: "system", Content: "You are a coding agent."},
			{Role: "user", Content: rungTask},
			{Role: "assistant", ToolCalls: []ToolCall{first}},
			{Role: "assistant", ToolCalls: []ToolCall{second}},
			{Role: "tool", ToolCallID: "g1", Content: body},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "g2", Name: "read_file", Args: `{"path":"a.go"}`}}},
			{Role: "tool", ToolCallID: "g2", Content: "short file"},
		}
		searchRung(msgs, rungProtected, len(msgs)-rungKeep, 1, nil)
		return msgs[4].Content
	}
	grep := ToolCall{ID: "g1", Name: "run_shell", Args: `{"command":"grep -rn timeout ."}`}
	lint := ToolCall{ID: "g1", Name: "run_shell", Args: `{"command":"flake8 ."}`}
	// Same name, different args: ambiguous, whichever comes last.
	for _, pair := range [][2]ToolCall{{lint, grep}, {grep, lint}} {
		if got := with(pair[0], pair[1], flat); got != flat {
			t.Errorf("same name, different args (%s then %s): the id is ambiguous and the body must be left alone", pair[0].Args, pair[1].Args)
		}
	}
	// Same args, different name: ambiguous, whichever comes last.
	args := `{"command":"grep -rn timeout .","pattern":"timeout"}`
	shell := ToolCall{ID: "g1", Name: "run_shell", Args: args}
	search := ToolCall{ID: "g1", Name: "search_files", Args: args}
	read := ToolCall{ID: "g1", Name: "read_file", Args: args}
	for _, pair := range [][2]ToolCall{{shell, search}, {search, shell}, {read, search}, {search, read}} {
		for _, body := range []string{grouped, flat} {
			if got := with(pair[0], pair[1], body); got != body {
				t.Errorf("same args, different name (%s then %s): the id is ambiguous and the body must be left alone", pair[0].Name, pair[1].Name)
			}
		}
	}
}

func TestSearchRungCountsTwoIdenticalCallsWithOneIDAsOneCall(t *testing.T) {
	grouped := prodGrouped(benignHits(63, 20, 90), true)
	call := gateSearchCall()
	msgs := []Msg{
		{Role: "system", Content: "You are a coding agent."},
		{Role: "user", Content: rungTask},
		{Role: "assistant", ToolCalls: []ToolCall{call}},
		{Role: "assistant", ToolCalls: []ToolCall{call}}, // a replayed duplicate, byte for byte
		{Role: "tool", ToolCallID: "g1", Content: grouped},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "g2", Name: "read_file", Args: `{"path":"a.go"}`}}},
		{Role: "tool", ToolCallID: "g2", Content: "short file"},
	}
	searchRung(msgs, rungProtected, len(msgs)-rungKeep, 1, nil)
	if msgs[4].Content == grouped || !strings.Contains(msgs[4].Content, "lines elided") {
		t.Error("two identical calls under one id are one call, and its listing must still be thinned")
	}
}

// ---------------------------------------------------------------------------
// A pipeline qualifies only when every stage after the search is a pass-through
// filter or another search: a linter at the end of a pipe prints its own
// diagnostics, and none of them may be dropped.
// ---------------------------------------------------------------------------

func TestSearchRungLeavesAPipeIntoALinterAlone(t *testing.T) {
	body := prodShell(1, linterBody(80, 12), "")
	for _, cmd := range []string{
		"rg -l foo | xargs flake8",
		"grep -rn x . | xargs flake8",
		"rg --files | xargs ruff check",
		"rg foo | flake8 -",
		"grep -rn x . | head -50 | xargs flake8",
		"grep -rn x . |& xargs flake8",
		"grep -rn x . | python -m flake8 -",
		"grep -rn x . | xargs",
		"grep -rn x . | ",
		"grep -rn x . |",
		`grep -rn x . | "head" | flake8 -`,
		"grep -rn x . | FOO=1 head",
		"grep -rn x . | tr a b",
		"grep -rn x . | headx",
		"grep -rn x . | sh -c 'flake8 .'",
	} {
		if thinned, got := gateOutcome(t, shellCall(cmd), body, true); thinned {
			t.Errorf("run_shell %q: a pipe into another program was thinned:\n%.200s", cmd, got)
		}
	}
}

func TestSearchRungKeepsAPipeThroughFiltersAndSearches(t *testing.T) {
	flat := fixtureFlat(benignHits(64, 16, 80))
	body := prodShell(0, flat+"\n", "")
	for _, cmd := range []string{
		"grep x | head",
		"grep -rn x . | tail -n 200",
		"grep -rn x . | sort | uniq",
		"grep -rn x . | wc -l",
		"grep -rn x . | cut -d: -f1-3",
		"grep -rn x . | sed 's/a/b/'",
		"grep -rn x . | awk '{print}'",
		"grep -rn x . | tee out.txt",
		"grep -rn x . | cat",
		"grep -rn x . | grep -v y",
		"rg -n x | rg y",
		"rg -n x | git grep -n y",
		"grep -rn x . | /usr/bin/head -5",
		"grep -rn x . |& tail",
		"grep -rn x . | sort | uniq | head | tee o",
	} {
		thinned, got := gateOutcome(t, shellCall(cmd), body, true)
		if !thinned || !strings.Contains(got, "lines elided") {
			t.Errorf("run_shell %q: a search through pass-through filters must still be thinned", cmd)
		}
	}
}

// ---------------------------------------------------------------------------
// Scanner behaviours worth pinning: a single quote takes no backslash escape,
// and a lone carriage return separates commands.
// ---------------------------------------------------------------------------

func TestSearchRungSingleQuotesTakeNoBackslashEscape(t *testing.T) {
	body := prodShell(1, linterBody(80, 12), "")
	// In a shell 'a\' is the word a\ : the quote closes at the second quote, so the
	// ';' after it is a real separator and flake8 runs.
	for _, cmd := range []string{
		`grep 'a\' ; flake8 .`,
		`grep -rn 'a\';flake8 .`,
		`grep -rn 'x\' . && ruff check .`,
	} {
		if thinned, got := gateOutcome(t, shellCall(cmd), body, true); thinned {
			t.Errorf("run_shell %q: the quote closes at the second quote, the separator chains, and the body must not qualify:\n%.200s", cmd, got)
		}
	}
	// A backslash inside double quotes does escape the quote, so the separator
	// stays quoted and the search is still just a search.
	flat := prodShell(0, fixtureFlat(benignHits(65, 16, 80))+"\n", "")
	if thinned, _ := gateOutcome(t, shellCall(`grep -rn "a\" ; b" .`), flat, true); !thinned {
		t.Error(`a ';' inside a double-quoted span with an escaped quote is quoted: the search must still be thinned`)
	}
}

func TestSearchRungALoneCarriageReturnSeparatesCommands(t *testing.T) {
	body := prodShell(1, linterBody(80, 12), "")
	for _, cmd := range []string{
		"grep -rn timeout .\rflake8 .",
		"grep -rn timeout . \r flake8 .",
		"grep -rn timeout .\r\nflake8 .",
	} {
		if thinned, got := gateOutcome(t, shellCall(cmd), body, true); thinned {
			t.Errorf("run_shell %q: a CR separates commands, the body must not qualify:\n%.200s", cmd, got)
		}
	}
}

// ---------------------------------------------------------------------------
// What the run tool can reach. Its allowlist is the real control: of the
// grep-family programs only git is on it, so only "git grep" can come through run;
// grep, rg, ag and ack come through run_shell. The docs say exactly this.
// ---------------------------------------------------------------------------

func TestSearchRungRunToolReachesOnlyGitGrep(t *testing.T) {
	onList := map[string]bool{}
	for _, p := range runAllowedExecutables {
		onList[p] = true
	}
	for p := range searchGrepPrograms {
		if onList[p] {
			t.Errorf("%s is now on the run allowlist: the docs say only git grep is reachable through run; update them and this pin together", p)
		}
	}
	if !onList["git"] {
		t.Error("git left the run allowlist: the docs say git grep is the one search reachable through run; update them and this pin together")
	}
}

// ---------------------------------------------------------------------------
// The caps are chosen to match the values the upstream configuration example
// shows. That page never calls them defaults, so nothing here may.
// ---------------------------------------------------------------------------

var (
	capWordsPattern    = regexp.MustCompile(`(?i)max_total_matches|max_matches_per_file|max_files|search\s*compressor|\b30\s*/\s*5\s*/\s*15\b|\b30, 5 and 15\b|\b30\s+lines,\s*5\s+per\s+file\b`)
	capsAllowedWording = regexp.MustCompile(`(?i)\boff by default\b|\bby default\b|\bdefault[- ]off\b|\bdefaults? to off\b`)
	capsDefaultWord    = regexp.MustCompile(`(?i)\bdefaults?\b`)
	paragraphBreak     = regexp.MustCompile(`\n[ \t]*\n`)
	lineWrap           = regexp.MustCompile(`[ \t]*\r?\n[ \t]*(?:>|-|\*|#)?[ \t]*`)
	sentenceEnd        = regexp.MustCompile(`[.!?]+(?:\s+|$)`)
)

// capsParagraphs cuts text into the runs of lines that read as one passage. In prose
// a passage ends at a blank line. In Go source a run of // comment lines is one
// passage (the comment marker dropped) and every other line is a passage of its own,
// so a stretch of code is never read as one long sentence.
func capsParagraphs(text string, goSource bool) []string {
	if !goSource {
		return paragraphBreak.Split(text, -1)
	}
	var out []string
	var run []string
	flush := func() {
		if len(run) > 0 {
			out = append(out, strings.Join(run, "\n"))
			run = nil
		}
	}
	for _, ln := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if t := strings.TrimSpace(ln); strings.HasPrefix(t, "//") {
			run = append(run, strings.TrimPrefix(t, "//"))
			continue
		}
		flush()
		out = append(out, ln)
	}
	flush()
	return out
}

// capsDefaultSentences returns every sentence of text that names the caps and calls
// them defaults. A sentence ends at ".", "!" or "?" followed by white space, or at the
// end of its passage (see capsParagraphs); a line wrap inside a passage (with its
// list marker) is a space, so a "default" wrapped onto the next line cannot escape the
// check. The rung's own default-off wording is allowed.
func capsDefaultSentences(text string, goSource bool) []string {
	var bad []string
	for _, para := range capsParagraphs(text, goSource) {
		para = lineWrap.ReplaceAllString(para, " ")
		for _, sn := range sentenceEnd.Split(para, -1) {
			if !capWordsPattern.MatchString(sn) {
				continue
			}
			if capsDefaultWord.MatchString(capsAllowedWording.ReplaceAllString(sn, "")) {
				bad = append(bad, strings.TrimSpace(sn))
			}
		}
	}
	return bad
}

func TestCapsDefaultGuardReadsAcrossWrappedLines(t *testing.T) {
	for name, text := range map[string]string{
		"same line":         "The rung keeps 30 / 5 / 15 as its defaults.",
		"wrapped":           "The rung keeps up to 30 / 5 / 15,\nwhich are the documented\ndefaults of the idea.",
		"star list wrapped": "* keeps 30 / 5 / 15 and\n  the default.",
		"list wrapped":      "- keeps 30 lines, 5 per file and\n  15 files, the default.",
		"quote wrapped":     "> keeps 30 lines,\n> 5 per file as the default.",
		"list cap wrapped":  "- keeps 30 lines,\n- 5 per file as the default.",
		"cap phrase wrap":   "keeps 30 lines,\n5 per file as the default",
		"cap phrase plain":  "It keeps 30 lines, 5 per file, which are the defaults of the idea.",
		"default first":     "The defaults are 30 lines, 5 per file.",
	} {
		if len(capsDefaultSentences(text, false)) == 0 {
			t.Errorf("%s: the guard missed a caps sentence that says default: %q", name, text)
		}
	}
	for name, text := range map[string]string{
		"off by default":         "The rung keeps 30 / 5 / 15 and is off by default.",
		"default off":            "The search compressor caps are chosen to match an example; the rung is default-off.",
		"other sentence":         "The caps are 30 / 5 / 15.\nThe default ladder is unchanged.",
		"other sentence wrapped": "The caps are 30 lines,\n5 per file.\n\nThe golden pins the default ladder.",
		"no caps":                "The default is off.",
	} {
		if got := capsDefaultSentences(text, false); len(got) != 0 {
			t.Errorf("%s: the guard flagged a sentence it should not: %q", name, got)
		}
	}
}

func TestCapsDefaultGuardReadsGoComments(t *testing.T) {
	for name, text := range map[string]string{
		"comment wrapped":  "// the search compressor keeps\n// the default values for\n// every cap.\nx := 1",
		"indented comment": "\t// keeps 30 / 5 / 15 as\n\t// the defaults of the idea.\n",
		"string literal":   "\tt.Errorf(\"the caps are the documented defaults of the idea (30 lines, 5 per file, 15 files)\")\n",
		"crlf comment":     "// keeps 30 / 5 / 15 as\r\n// the defaults.\r\n",
	} {
		if len(capsDefaultSentences(text, true)) == 0 {
			t.Errorf("%s: the guard missed a caps sentence that says default: %q", name, text)
		}
	}
	for name, text := range map[string]string{
		"code stretch":   "hits := benignHits(18, 8, 40)\nswitch x {\ndefault:\n}\nconst a = 30 // 30 / 5 / 15\n",
		"two passages":   "// keeps 30 / 5 / 15.\n\n// The default ladder is unchanged.\n",
		"off by default": "// keeps 30 / 5 / 15; the rung is off by default.\n",
	} {
		if got := capsDefaultSentences(text, true); len(got) != 0 {
			t.Errorf("%s: the guard flagged a sentence it should not: %q", name, got)
		}
	}
}

// capsScannedFiles are the files whose sentences about the caps are read. The package's
// own tests are on the list because a test name or a failure message calls the caps
// something too; attribute marks the files that must also say where the values come from.
var capsScannedFiles = []struct {
	rel       string
	attribute bool // the file must say the caps are the values shown in the upstream configuration example
}{
	{"NOTICE", true},
	{"internal/agent/rung_search.go", true},
	{"docs/systems/coding-agent.md", true},
	{"changes/unreleased/feat-compaction-rung-search.md", true},
	{"internal/agent/rung_search_test.go", false},
}

// The scan list is the guard's reach: dropping a file from it silently weakens the guard.
func TestCapsGuardScansTheCodeTheDocsAndThePackageTests(t *testing.T) {
	scanned := map[string]bool{}
	for _, f := range capsScannedFiles {
		scanned[f.rel] = true
	}
	for _, rel := range []string{
		"NOTICE",
		"internal/agent/rung_search.go",
		"internal/agent/rung_search_test.go",
		"docs/systems/coding-agent.md",
		"changes/unreleased/feat-compaction-rung-search.md",
	} {
		if !scanned[rel] {
			t.Errorf("%s is not scanned for a caps sentence that says default", rel)
		}
	}
}

// Every sentence that names the caps is read, in the code, the notice, the docs, the
// fragment and this package's own tests; "default" in one fails, except the rung's
// own default-off wording. Where a file describes the caps it must attribute them.
func TestSearchRungCapsAreAttributedToTheConfigurationExample(t *testing.T) {
	for _, f := range capsScannedFiles {
		b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(f.rel)))
		if err != nil {
			if os.IsNotExist(err) && strings.HasPrefix(f.rel, "changes/unreleased/") {
				continue // the fragment is consumed at release
			}
			t.Fatal(err)
		}
		for _, sn := range capsDefaultSentences(string(b), strings.HasSuffix(f.rel, ".go")) {
			t.Errorf("%s: %q calls the caps defaults; say \"the values shown in the upstream configuration example\" or \"chosen to match\"", f.rel, sn)
		}
		if f.attribute && !strings.Contains(string(b), "configuration example") && !strings.Contains(string(b), "chosen to match") {
			t.Errorf("%s: must say the caps are the values shown in the upstream configuration example (or chosen to match)", f.rel)
		}
	}
}
