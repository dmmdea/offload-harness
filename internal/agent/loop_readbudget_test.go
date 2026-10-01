package agent

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The D-103 fixtures. The incident they reproduce (a live delegation, 2026-10-01
// 00:16): one 12,005-character context document, a seat whose probe fell back
// to the 8,192-token window (so the loop's per-result cap was 1,090 characters),
// twelve steps, and read_file called eight times on that one document, each
// result 1,090 characters or fewer, before the ninth call was refused
// "now DISABLED" - the document could not be read to its end by any seat, because
// eight results of 1,090 characters hold 8,720 of its 12,005.

// incidentCap is the per-result cap the incident seat ran under.
const incidentCap = 1090

// writeIncidentDoc writes a document of `lines` lines of 94 characters each
// (95 bytes with the newline) under root, so a ten-line page comes back from
// read_file at about 1,060 characters: under incidentCap, like the incident's
// own reads.
func writeIncidentDoc(t *testing.T, root, name string, lines int) {
	t.Helper()
	var b strings.Builder
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&b, "%03d %s\n", i, strings.Repeat("x", 90))
	}
	if err := os.WriteFile(filepath.Join(root, name), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// countedReadFile is the REAL read_file tool over root with its executions
// counted, so a test sees both what the loop let through and what the tool
// returned.
func countedReadFile(t *testing.T, root string, execs *int) []Tool {
	t.Helper()
	tools, err := ReadOnlyTools(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rf := findTool(tools, "read_file")
	if rf == nil {
		t.Fatal("read_file tool missing")
	}
	inner := rf.Exec
	rf.Exec = func(ctx context.Context, args string) (string, error) {
		*execs++
		return inner(ctx, args)
	}
	return tools
}

// pageCall is one assistant turn asking for `limit` lines of path from `offset`.
func pageCall(id, path string, offset, limit int) Completion {
	args := fmt.Sprintf(`{"path":%s,"offset":%d,"limit":%d}`, jsonStr(path), offset, limit)
	return Completion{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc(id, "read_file", args)}}, FinishReason: "tool_calls"}
}

func finalAnswer(text string) Completion {
	return Completion{Msg: Msg{Role: "assistant", Content: text}, FinishReason: "stop"}
}

// toolResultsMention reports whether any tool message in the transcript
// contains s.
func toolResultsMention(msgs []Msg, s string) bool {
	for _, m := range msgs {
		if m.Role == "tool" && strings.Contains(m.Content, s) {
			return true
		}
	}
	return false
}

// TestLoopPagedReadsOfOneFileAreNotRefusedByCallCount is the incident (D-103),
// red first against the call-counting cap: a seat paging through ONE document in
// small reads was refused its ninth read_file although every result together
// held less than one 8K window. Eleven pages (every step but the forced final
// one) of one file must all execute, none refused, read_file still offered on
// every tool step, and the run must end with the seat's own answer.
func TestLoopPagedReadsOfOneFileAreNotRefusedByCallCount(t *testing.T) {
	root := t.TempDir()
	writeIncidentDoc(t, root, "ctx.txt", 126) // 126 x 95 = 11,970 bytes: the incident's 12 KB document
	execs := 0
	tools := countedReadFile(t, root, &execs)

	const pages = 11 // twelve steps, the last of them the forced final answer
	var script []Completion
	for i := 0; i < pages; i++ {
		script = append(script, pageCall(fmt.Sprintf("c%d", i+1), "ctx.txt", 1+10*i, 10))
	}
	script = append(script, finalAnswer("read it"))
	client := &fakeClient{script: script}

	res, err := NewLoop(client, tools, 12).WithToolResultCap(incidentCap).Run(context.Background(), "read the context document")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if execs != pages {
		t.Errorf("read_file executed %d times on %d pages of one file, want %d - a call count refused a seat that had read %d characters", execs, pages, pages, readChars(res))
	}
	if toolResultsMention(res.Transcript, "DISABLED") || toolResultsMention(res.Transcript, "NOT executed") {
		t.Errorf("a page of a file already being read was refused:\n%s", firstRefusal(res.Transcript))
	}
	for i := 0; i < pages; i++ {
		if !containsName(client.seenSpecs[i], "read_file") {
			t.Errorf("read_file was withdrawn before step %d of %d", i+1, pages)
			break
		}
	}
	for _, e := range res.Effects {
		if e.ObsChars > incidentCap {
			t.Fatalf("fixture error: step %d returned %d characters, over the incident's %d cap; the pages must stay under it", e.Step, e.ObsChars, incidentCap)
		}
	}
	if res.StopReason != "done" || res.Output != "read it" {
		t.Errorf("stop=%q output=%q, want the seat's own answer", res.StopReason, res.Output)
	}
}

// readChars is what the model received from read_file in a run: the
// transcript's own count, the same figure the effect ledger records as
// obs_chars.
func readChars(res Result) int {
	n := 0
	for _, e := range res.Effects {
		if e.Tool == "read_file" && e.Status != EffectNone {
			n += e.ObsChars
		}
	}
	return n
}

func firstRefusal(msgs []Msg) string {
	for _, m := range msgs {
		if m.Role == "tool" && strings.Contains(m.Content, "NOT executed") {
			return m.Content
		}
	}
	return ""
}

// toolResultFor is the text of the tool message that answers call id.
func toolResultFor(msgs []Msg, id string) string {
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == id {
			return m.Content
		}
	}
	return ""
}

// bigReadFile is a stand-in read_file whose every result is far over any cap, so
// what the loop charges is decided by the trim and not by the tool.
func bigReadFile(execs *int) []Tool {
	return []Tool{{ToolSpec: ToolSpec{Name: "read_file"}, Exec: func(_ context.Context, _ string) (string, error) {
		*execs++
		return strings.Repeat("A", 100000), nil
	}}}
}

// TestLoopReadBudgetNeverRefusesOneMaximalReadPerStep pins the size of the
// default against the incident's own numbers: six of its eight results were
// EXACTLY the 1,090-character cap, so a budget of "the count cap in characters"
// (8 x 1,090 = 8,720) is spent by the eighth read and refuses the ninth, which is
// the incident again. The default is sized to the steps (12 x 1,090 = 13,080): a
// seat that reads one maximal result per step, as often as the steps allow, is
// never refused.
func TestLoopReadBudgetNeverRefusesOneMaximalReadPerStep(t *testing.T) {
	execs := 0
	const pages = 11 // twelve steps, the last of them the forced final answer
	var script []Completion
	for i := 0; i < pages; i++ {
		script = append(script, pageCall(fmt.Sprintf("c%d", i+1), "ctx.txt", 1+10*i, 10))
	}
	script = append(script, finalAnswer("read it"))
	client := &fakeClient{script: script}

	loop := NewLoop(client, bigReadFile(&execs), 12).WithToolResultCap(incidentCap)
	if got, want := loop.readBudgetChars(), 12*incidentCap; got != want {
		t.Fatalf("default budget = %d, want %d (12 steps x the %d cap)", got, want, incidentCap)
	}
	res, err := loop.Run(context.Background(), "read the context document")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if execs != pages || readChars(res) != pages*incidentCap {
		t.Errorf("read_file ran %d times for %d characters, want %d times for %d (every result at the cap, none refused)", execs, readChars(res), pages, pages*incidentCap)
	}
	if toolResultsMention(res.Transcript, "NOT executed") {
		t.Errorf("a read was refused:\n%s", firstRefusal(res.Transcript))
	}
}

// TestLoopReadBudgetSpentRefusesAndWithdrawsReadFile: when the characters read
// reach the budget the next read_file call is refused with its own text (the
// budget and what was read), the tool is withdrawn from every later Chat while the
// other tools stay offered, and the forced final step still asks for the answer.
func TestLoopReadBudgetSpentRefusesAndWithdrawsReadFile(t *testing.T) {
	root := t.TempDir()
	writeIncidentDoc(t, root, "ctx.txt", 126)
	execs := 0
	tools := countedReadFile(t, root, &execs)
	var script []Completion
	for i := 0; i < 5; i++ {
		script = append(script, pageCall(fmt.Sprintf("c%d", i+1), "ctx.txt", 1+10*i, 10))
	}
	script = append(script, finalAnswer("what I read"))
	client := &fakeClient{script: script}

	// A ten-line page is about 1,060 characters: the third read reaches 3,000.
	res, err := NewLoop(client, tools, 6).WithToolResultCap(incidentCap).WithReadBudgetChars(3000).Run(context.Background(), "read it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if execs != 3 {
		t.Fatalf("read_file ran %d times, want 3: the budget (3,000) is spent by the third page", execs)
	}
	spent := toolResultFor(res.Transcript, "c4")
	for _, want := range []string{"NOT executed", "read budget (3000)", "DISABLED", fmt.Sprintf("returned %d characters", readChars(res))} {
		if !strings.Contains(spent, want) {
			t.Errorf("the refusal %q does not say %q", spent, want)
		}
	}
	if got := toolResultFor(res.Transcript, "c5"); !strings.Contains(got, "disabled") {
		t.Errorf("a call after the withdrawal got %q, want the tool-disabled refusal", got)
	}
	// c4 is answered on Chat 4 (index 3) and c5 on Chat 5: from Chat 5 (index 4)
	// on, read_file is gone.
	for i := 4; i < len(client.seenSpecs); i++ {
		if containsName(client.seenSpecs[i], "read_file") {
			t.Errorf("read_file still offered on Chat %d after the budget was spent: %v", i+1, specNames(client.seenSpecs[i]))
		}
	}
	if !containsName(client.seenSpecs[4], "list_dir") {
		t.Errorf("the other tools must stay offered, got %v", specNames(client.seenSpecs[4]))
	}
	if res.StopReason != "done" || !strings.Contains(res.StopNote, "forced final answer") || len(client.seenSpecs[5]) != 0 {
		t.Errorf("stop=%q note=%q finalSpecs=%v, want done through the forced final step with no tools", res.StopReason, res.StopNote, specNames(client.seenSpecs[5]))
	}
}

// TestLoopReadBudgetIsChargedInCallOrderWithinAStep: five reads requested in ONE
// completion are charged one after another, so the budget refuses the fourth
// and the fifth finds the tool already withdrawn. This is the many-reads-in-one-
// step case the default budget exists for.
func TestLoopReadBudgetIsChargedInCallOrderWithinAStep(t *testing.T) {
	root := t.TempDir()
	writeIncidentDoc(t, root, "ctx.txt", 126)
	execs := 0
	tools := countedReadFile(t, root, &execs)
	var calls []ToolCall
	for i := 0; i < 5; i++ {
		calls = append(calls, tc(fmt.Sprintf("c%d", i+1), "read_file", fmt.Sprintf(`{"path":"ctx.txt","offset":%d,"limit":10}`, 1+10*i)))
	}
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: calls}, FinishReason: "tool_calls"},
		finalAnswer("done"),
	}}
	res, err := NewLoop(client, tools, 6).WithToolResultCap(incidentCap).WithReadBudgetChars(3000).Run(context.Background(), "read it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if execs != 3 {
		t.Fatalf("read_file ran %d times, want 3", execs)
	}
	if got := toolResultFor(res.Transcript, "c4"); !strings.Contains(got, "read budget") {
		t.Errorf("the fourth read got %q, want the read-budget refusal", got)
	}
	if got := toolResultFor(res.Transcript, "c5"); !strings.Contains(got, "disabled") {
		t.Errorf("the fifth read got %q, want the tool-disabled refusal", got)
	}
}

// TestLoopReadBudgetChargesWhatTheModelReceivesNotWhatTheToolReturned: the
// charge is the length after the loop-boundary trim, which is what the
// transcript holds and the effect ledger records as obs_chars. A tool that
// returns 100,000 characters into a 1,000-character cap costs 1,000.
func TestLoopReadBudgetChargesWhatTheModelReceivesNotWhatTheToolReturned(t *testing.T) {
	execs := 0
	var script []Completion
	for i := 0; i < 5; i++ {
		script = append(script, pageCall(fmt.Sprintf("c%d", i+1), "big.txt", 1+10*i, 10))
	}
	script = append(script, finalAnswer("done"))
	client := &fakeClient{script: script}
	res, err := NewLoop(client, bigReadFile(&execs), 8).WithToolResultCap(1000).WithReadBudgetChars(2500).Run(context.Background(), "read it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Charged at 1,000 each: 1,000 / 2,000 / 3,000, so the fourth is refused.
	// Charged at what the tool returned (100,000), the second would be.
	if execs != 3 || readChars(res) != 3000 {
		t.Errorf("read_file ran %d times for %d characters, want 3 times for 3000", execs, readChars(res))
	}
}

// TestLoopFirstReadOfEachFileStillCountsAgainstTheCap: the cap counts FILES. Eight
// distinct files, then a page of the first (not counted), then a ninth distinct
// file (refused): nine executions, and the refusal says how many calls there
// really were (ten), not the eight the cap counted.
func TestLoopFirstReadOfEachFileStillCountsAgainstTheCap(t *testing.T) {
	execs := 0
	tools := []Tool{{ToolSpec: ToolSpec{Name: "read_file"}, Exec: func(_ context.Context, _ string) (string, error) {
		execs++
		return "contents", nil
	}}}
	read := func(id, args string) Completion {
		return Completion{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc(id, "read_file", args)}}, FinishReason: "tool_calls"}
	}
	var script []Completion
	for i := 1; i <= 8; i++ {
		script = append(script, read(fmt.Sprintf("f%d", i), fmt.Sprintf(`{"path":"f%d.md"}`, i)))
	}
	script = append(script, read("page", `{"path":"f1.md","offset":2}`), read("f9", `{"path":"f9.md"}`), finalAnswer("done"))
	res, err := NewLoop(&fakeClient{script: script}, tools, 14).Run(context.Background(), "map the repo")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if execs != 9 {
		t.Errorf("read_file ran %d times, want 9: eight files and one page of the first", execs)
	}
	refused := toolResultFor(res.Transcript, "f9")
	if !strings.Contains(refused, "DISABLED") || !strings.Contains(refused, "called 10 times") {
		t.Errorf("the ninth file got %q, want the same-name refusal naming 10 calls", refused)
	}
}

// TestLoopPagesOfAFileSpelledTwoWaysAreOneFile: "ctx.txt" and "./ctx.txt" are one
// path. With the cap at one file, a page asked for under the other spelling is
// still a page.
func TestLoopPagesOfAFileSpelledTwoWaysAreOneFile(t *testing.T) {
	root := t.TempDir()
	writeIncidentDoc(t, root, "ctx.txt", 126)
	execs := 0
	tools := countedReadFile(t, root, &execs)
	client := &fakeClient{script: []Completion{
		pageCall("c1", "ctx.txt", 1, 10), pageCall("c2", "./ctx.txt", 11, 10), pageCall("c3", "ctx.txt", 21, 10),
		finalAnswer("done"),
	}}
	res, err := NewLoop(client, tools, 8).WithMaxSameTool(1).Run(context.Background(), "read it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if execs != 3 || toolResultsMention(res.Transcript, "NOT executed") {
		t.Errorf("read_file ran %d times (want 3), refusal: %q", execs, firstRefusal(res.Transcript))
	}
}

// TestLoopCallsThatAreNotPagesOfAReadFileStillCount: the paging exemption is for
// a new page of a file that was READ. A path that does not exist, a call with no
// path and a call whose path is not a string are never "being read", so ten
// distinct ones are refused at the ninth, as the count cap always did: a seat
// hammering a bad path with a different offset each time must not get past it.
// (Arguments that are not JSON at all never get this far: the loop treats them as
// a cut tool call, D-114.)
func TestLoopCallsThatAreNotPagesOfAReadFileStillCount(t *testing.T) {
	cases := map[string]func(k int) string{
		"a path that does not exist": func(k int) string { return fmt.Sprintf(`{"path":"missing.txt","offset":%d}`, k) },
		"no path":                    func(k int) string { return fmt.Sprintf(`{"offset":%d}`, k) },
		"a path of the wrong type":   func(k int) string { return fmt.Sprintf(`{"path":123,"offset":%d}`, k) },
	}
	for name, argsFor := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeIncidentDoc(t, root, "ctx.txt", 126)
			execs := 0
			tools := countedReadFile(t, root, &execs)
			var script []Completion
			for k := 1; k <= 10; k++ {
				script = append(script, Completion{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc(fmt.Sprintf("c%d", k), "read_file", argsFor(k))}}, FinishReason: "tool_calls"})
			}
			script = append(script, finalAnswer("done"))
			res, err := NewLoop(&fakeClient{script: script}, tools, 12).Run(context.Background(), "read it")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if execs != 8 {
				t.Errorf("read_file ran %d times, want 8 (the count cap, unchanged for calls that are not pages of a read file)", execs)
			}
			if got := toolResultFor(res.Transcript, "c9"); !strings.Contains(got, "DISABLED") {
				t.Errorf("the ninth call got %q, want the same-name refusal", got)
			}
		})
	}
}

// TestLoopAnExactRepeatOfAPageIsStillRefusedAndCounted: the exact-repeat refusal
// and the D-49 withdrawal are untouched by the budget. The same page asked for
// again is refused and not re-run; asked for a third time the tool is withdrawn
// with the answer-now turn; and, with the cap at two files, exact repeats still
// count against it.
func TestLoopAnExactRepeatOfAPageIsStillRefusedAndCounted(t *testing.T) {
	t.Run("refused, then withdrawn on the second refusal (D-49)", func(t *testing.T) {
		root := t.TempDir()
		writeIncidentDoc(t, root, "ctx.txt", 126)
		execs := 0
		tools := countedReadFile(t, root, &execs)
		client := &fakeClient{script: []Completion{
			pageCall("c1", "ctx.txt", 1, 10), pageCall("c2", "ctx.txt", 11, 10),
			pageCall("c3", "ctx.txt", 11, 10), pageCall("c4", "ctx.txt", 11, 10),
			finalAnswer("done"),
		}}
		res, err := NewLoop(client, tools, 8).Run(context.Background(), "read it")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if execs != 2 {
			t.Errorf("read_file ran %d times, want 2: the repeats of page 2 must not run", execs)
		}
		if got := toolResultFor(res.Transcript, "c3"); !strings.Contains(got, "already called") {
			t.Errorf("the first repeat got %q, want the exact-repeat refusal", got)
		}
		if got := toolResultFor(res.Transcript, "c4"); !strings.Contains(got, "withdrawn") {
			t.Errorf("the second repeat got %q, want the D-49 withdrawal", got)
		}
		if containsName(client.seenSpecs[4], "read_file") {
			t.Errorf("read_file still offered after the D-49 withdrawal: %v", specNames(client.seenSpecs[4]))
		}
	})
	t.Run("repeats count against the cap", func(t *testing.T) {
		root := t.TempDir()
		writeIncidentDoc(t, root, "ctx.txt", 126)
		execs := 0
		tools := countedReadFile(t, root, &execs)
		// cap 2: the first read counts 1, the new page 0, the repeat 2, the second repeat 3 > 2.
		client := &fakeClient{script: []Completion{
			pageCall("c1", "ctx.txt", 1, 10), pageCall("c2", "ctx.txt", 11, 10),
			pageCall("c3", "ctx.txt", 11, 10), pageCall("c4", "ctx.txt", 11, 10),
			finalAnswer("done"),
		}}
		res, err := NewLoop(client, tools, 8).WithMaxSameTool(2).Run(context.Background(), "read it")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if got := toolResultFor(res.Transcript, "c3"); !strings.Contains(got, "already called") {
			t.Errorf("the first repeat got %q, want the exact-repeat refusal", got)
		}
		if got := toolResultFor(res.Transcript, "c4"); !strings.Contains(got, "DISABLED") || !strings.Contains(got, "called 4 times") {
			t.Errorf("the second repeat got %q, want the same-name refusal naming 4 calls: repeats count against the cap", got)
		}
	})
}

// TestLoopWithoutReadBudgetCountsEveryReadCall: the opt-out is the loop as it
// was. The incident's shape is refused at the ninth read again.
func TestLoopWithoutReadBudgetCountsEveryReadCall(t *testing.T) {
	root := t.TempDir()
	writeIncidentDoc(t, root, "ctx.txt", 126)
	execs := 0
	tools := countedReadFile(t, root, &execs)
	var script []Completion
	for i := 0; i < 10; i++ {
		script = append(script, pageCall(fmt.Sprintf("c%d", i+1), "ctx.txt", 1+10*i, 10))
	}
	script = append(script, finalAnswer("done"))
	res, err := NewLoop(&fakeClient{script: script}, tools, 12).WithToolResultCap(incidentCap).WithoutReadBudget().Run(context.Background(), "read it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if execs != 8 || !strings.Contains(toolResultFor(res.Transcript, "c9"), "DISABLED") {
		t.Errorf("read_file ran %d times, want 8 and the ninth refused: without the budget every call counts", execs)
	}
}

// TestLoopPagingExemptionIsReadFileOnly: another tool that happens to take a
// path and an offset is still counted by call.
func TestLoopPagingExemptionIsReadFileOnly(t *testing.T) {
	execs := 0
	tools := []Tool{{ToolSpec: ToolSpec{Name: "peek_file"}, Exec: func(_ context.Context, _ string) (string, error) {
		execs++
		return "contents", nil
	}}}
	var script []Completion
	for k := 1; k <= 10; k++ {
		script = append(script, Completion{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc(fmt.Sprintf("c%d", k), "peek_file", fmt.Sprintf(`{"path":"a.md","offset":%d}`, k))}}, FinishReason: "tool_calls"})
	}
	script = append(script, finalAnswer("done"))
	if _, err := NewLoop(&fakeClient{script: script}, tools, 12).Run(context.Background(), "read it"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if execs != 8 {
		t.Errorf("peek_file ran %d times, want 8: only read_file is metered in characters", execs)
	}
}

// TestLoopSetupReplayIsNotChargedToTheReadBudget: a replayed read_file spends
// none of the model's budget (setup.go: the model's own budget under the
// breakers stays whole). Budget 1,200: the replay (about 1,060) plus the model's
// first two pages run, and the third is refused; had the replay been charged, the
// model's second page would have been.
func TestLoopSetupReplayIsNotChargedToTheReadBudget(t *testing.T) {
	root := t.TempDir()
	writeIncidentDoc(t, root, "ctx.txt", 126)
	execs := 0
	tools := countedReadFile(t, root, &execs)
	client := &fakeClient{script: []Completion{
		pageCall("c1", "ctx.txt", 11, 10), pageCall("c2", "ctx.txt", 21, 10), pageCall("c3", "ctx.txt", 31, 10),
		finalAnswer("done"),
	}}
	loop := NewLoop(client, tools, 8).WithToolResultCap(incidentCap).WithReadBudgetChars(1200).
		WithSetupActions(setupActs(`read_file {"path":"ctx.txt","limit":10}`))
	res, err := loop.Run(context.Background(), "read it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if execs != 3 { // the replay + two model pages
		t.Errorf("read_file ran %d times, want 3 (the replay and two pages): the replay was charged to the model's budget", execs)
	}
	if got := toolResultFor(res.Transcript, "c3"); !strings.Contains(got, "read budget") {
		t.Errorf("the third page got %q, want the read-budget refusal", got)
	}
}

// TestReadBudgetDefaultIsTheLargerOfTheStepsAndTheCapTimesTheResultCap pins the
// derivation (see readBudgetChars) and the overrides.
func TestReadBudgetDefaultIsTheLargerOfTheStepsAndTheCapTimesTheResultCap(t *testing.T) {
	const c = 1000
	cases := []struct {
		name string
		loop *Loop
		want int
	}{
		{"steps above the cap", NewLoop(nil, nil, 12).WithToolResultCap(c), 12 * c},
		{"steps below the cap", NewLoop(nil, nil, 4).WithToolResultCap(c), defaultMaxSameTool * c},
		{"a tighter cap, many steps", NewLoop(nil, nil, 12).WithMaxSameTool(3).WithToolResultCap(c), 12 * c},
		{"a tighter cap, few steps", NewLoop(nil, nil, 2).WithMaxSameTool(3).WithToolResultCap(c), 3 * c},
		{"a looser cap", NewLoop(nil, nil, 12).WithMaxSameTool(20).WithToolResultCap(c), 20 * c},
		{"the cap disabled counts as the default cap", NewLoop(nil, nil, 5).WithMaxSameTool(0).WithToolResultCap(c), defaultMaxSameTool * c},
		{"explicit", NewLoop(nil, nil, 12).WithToolResultCap(c).WithReadBudgetChars(500), 500},
		{"a non-positive explicit value restores the default", NewLoop(nil, nil, 12).WithToolResultCap(c).WithReadBudgetChars(500).WithReadBudgetChars(0), 12 * c},
		{"off", NewLoop(nil, nil, 12).WithToolResultCap(c).WithoutReadBudget(), 0},
		{"explicit again after off", NewLoop(nil, nil, 12).WithToolResultCap(c).WithoutReadBudget().WithReadBudgetChars(700), 700},
	}
	for _, tt := range cases {
		if got := tt.loop.readBudgetChars(); got != tt.want {
			t.Errorf("%s: budget = %d, want %d", tt.name, got, tt.want)
		}
	}
	// A result cap too large to multiply must saturate, not wrap into a tiny or negative budget.
	if got := NewLoop(nil, nil, 12).WithToolResultCap(math.MaxInt / 2).readBudgetChars(); got != math.MaxInt {
		t.Errorf("an overflowing budget = %d, want it clamped to %d", got, math.MaxInt)
	}
}

// TestLoopAPageThatGivesNothingNewCountsAgainstTheCap (D-103 review, finding 1):
// the paging exemption is for a page that is PROGRESS. A seat that has read a
// file once and then keeps sending pages that fail, that the tool refuses as not
// performed (wrong-typed arguments), or that begin past the end of the file is
// the near-duplicate thrash the same-tool cap exists to catch (D-48 saw a seat
// page past a document's end three times). Such a page costs a few dozen
// characters, so the read budget never reaches it; before the exemption looked
// at the result the only bound left was the step budget, and a seat got every
// step instead of eight calls.
//
// A page that gave nothing new counts against the cap once it has run, so the
// cap's overshoot is the budget's: the call that crosses the line completes. One
// read and defaultMaxSameTool such pages run (the cap is then spent), and the next
// call is refused and the tool withdrawn.
func TestLoopAPageThatGivesNothingNewCountsAgainstTheCap(t *testing.T) {
	const pages = 11 // after the first read; steps leave room for all of them
	const wantExecs = 1 + defaultMaxSameTool
	cases := []struct {
		name     string
		tools    func(t *testing.T, execs *int) []Tool
		pageArgs func(k int) string
	}{
		{
			// The file is read once, then every later call to the tool fails.
			name: "the tool fails",
			tools: func(t *testing.T, execs *int) []Tool {
				return []Tool{{ToolSpec: ToolSpec{Name: "read_file"}, Exec: func(_ context.Context, _ string) (string, error) {
					*execs++
					if *execs == 1 {
						return "1: the whole file", nil
					}
					return "", fmt.Errorf("read failed")
				}}}
			},
			pageArgs: func(k int) string { return fmt.Sprintf(`{"path":"ctx.txt","offset":%d}`, 1+k) },
		},
		{
			// A seat that re-sends a wrong-typed field: the REAL tool refuses the call as
			// not performed (decodeToolArgs), the loop records it as EffectNone and
			// charges the budget nothing at all.
			name: "the tool refuses the call as not performed",
			tools: func(t *testing.T, execs *int) []Tool {
				root := t.TempDir()
				writeIncidentDoc(t, root, "ctx.txt", 126)
				return countedReadFile(t, root, execs)
			},
			pageArgs: func(k int) string { return fmt.Sprintf(`{"path":"ctx.txt","offset":%d,"limit":3.5}`, 1+k) },
		},
		{
			// The REAL tool answers "(end of file - N lines)" to a page that starts after
			// the last line: about 27 characters.
			name: "the page begins past the end of the file",
			tools: func(t *testing.T, execs *int) []Tool {
				root := t.TempDir()
				writeIncidentDoc(t, root, "ctx.txt", 5)
				return countedReadFile(t, root, execs)
			},
			pageArgs: func(k int) string { return fmt.Sprintf(`{"path":"ctx.txt","offset":%d,"limit":10}`, 100+k) },
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			execs := 0
			tools := tt.tools(t, &execs)
			read := func(id, args string) Completion {
				return Completion{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc(id, "read_file", args)}}, FinishReason: "tool_calls"}
			}
			script := []Completion{read("c1", `{"path":"ctx.txt"}`)}
			for k := 1; k <= pages; k++ {
				script = append(script, read(fmt.Sprintf("c%d", k+1), tt.pageArgs(k)))
			}
			script = append(script, finalAnswer("done"))
			res, err := NewLoop(&fakeClient{script: script}, tools, pages+3).Run(context.Background(), "read it")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if execs != wantExecs {
				t.Errorf("read_file ran %d times, want %d: one read, then %d pages that gave nothing new, then the cap", execs, wantExecs, defaultMaxSameTool)
			}
			// c1 is the read and c2..c9 are the pages that ran; c10 is the first call the cap refuses.
			if got := toolResultFor(res.Transcript, fmt.Sprintf("c%d", wantExecs)); strings.Contains(got, "DISABLED") {
				t.Errorf("the last page that was due to run was refused: %q", got)
			}
			if got := toolResultFor(res.Transcript, fmt.Sprintf("c%d", wantExecs+1)); !strings.Contains(got, "NOT executed") || !strings.Contains(got, "DISABLED") {
				t.Errorf("the call after the cap got %q, want the same-name refusal", got)
			}
		})
	}
}

// TestPageGaveNothing is the whole rule for a page that is not progress: it did
// not complete, or it began past the end of the file. A page of lines is progress
// whatever else it carries (a continuation hint included), and the marker counts
// only at the start of the result: real lines always begin with their number.
func TestPageGaveNothing(t *testing.T) {
	cases := []struct {
		name  string
		out   string
		isErr bool
		eff   EffectStatus
		want  bool
	}{
		{"lines came back", "5: some text\n6: more text", false, EffectCommitted, false},
		{"lines and a continuation hint", "5: some text\n(showing lines 5-5 of 9; use offset=6 to continue)", false, EffectCommitted, false},
		{"the tool failed", "error: read failed", true, EffectFailed, true},
		{"the tool refused the call as not performed", "NOT performed: read_file arguments do not match the tool's schema", false, EffectNone, true},
		{"the call was abandoned at its time budget", "error: tool read_file exceeded its budget and was cancelled", true, EffectUnknown, true},
		{"an error flag on a committed call is still an error", "5: some text", true, EffectCommitted, true},
		{"the page began past the end of the file", "(end of file - 6 lines)", false, EffectCommitted, true},
		{"the marker inside a page is file text", "5: see (end of file) below", false, EffectCommitted, false},
	}
	for _, tt := range cases {
		if got := pageGaveNothing(tt.out, tt.isErr, tt.eff); got != tt.want {
			t.Errorf("%s: pageGaveNothing(%q, %v, %v) = %v, want %v", tt.name, tt.out, tt.isErr, tt.eff, got, tt.want)
		}
	}
}
