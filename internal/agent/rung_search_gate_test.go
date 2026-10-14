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
