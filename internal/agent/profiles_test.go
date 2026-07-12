package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// specNameSet returns the advertised tool names in a loop's spec list as a set.
func specNameSet(l *Loop) map[string]bool {
	m := make(map[string]bool, len(l.specs))
	for _, s := range l.specs {
		m[s.Name] = true
	}
	return m
}

// mkTools builds no-op executor tools for the given names (for wiring tests).
func mkTools(names ...string) []Tool {
	out := make([]Tool, 0, len(names))
	for _, n := range names {
		out = append(out, Tool{
			ToolSpec: ToolSpec{Name: n, Description: n, Schema: json.RawMessage(`{"type":"object"}`)},
			Exec:     func(_ context.Context, _ string) (string, error) { return "ok", nil },
		})
	}
	return out
}

// I-2: every registered profile's exemplars must form COMPLETE tool cycles — an
// assistant message carrying ToolCalls must be immediately followed by tool-role
// messages covering ALL of that assistant's ToolCall.IDs. A dangling assistant
// tool_calls with no matching tool result violates strict --jinja templates and
// can 400 the server.
func TestProfileExemplarsAreCompleteToolCycles(t *testing.T) {
	for name, p := range profileRegistry {
		if len(p.Exemplars) == 0 {
			continue // general: no exemplars, nothing to validate.
		}
		ex := p.Exemplars
		for i, m := range ex {
			if m.Role != "assistant" || len(m.ToolCalls) == 0 {
				continue
			}
			// Every ToolCall must have a non-empty ID (so the tool result can match it).
			want := map[string]bool{}
			for _, c := range m.ToolCalls {
				if c.ID == "" {
					t.Errorf("profile %q exemplar[%d]: assistant ToolCall %q has empty ID", name, i, c.Name)
					continue
				}
				want[c.ID] = true
			}
			// The IMMEDIATELY following messages must be tool-role results covering
			// every one of this assistant's ToolCall.IDs.
			covered := map[string]bool{}
			for j := i + 1; j < len(ex) && ex[j].Role == "tool"; j++ {
				covered[ex[j].ToolCallID] = true
			}
			for id := range want {
				if !covered[id] {
					t.Errorf("profile %q exemplar[%d]: assistant ToolCall id %q has NO matching following tool-role result", name, i, id)
				}
			}
		}
	}
}

// (a) LookupProfile returns the documented edit subset; unknown → error listing valid names.
func TestLookupProfileEditSubset(t *testing.T) {
	p, err := LookupProfile("edit")
	if err != nil {
		t.Fatalf("LookupProfile(edit): %v", err)
	}
	want := []string{"list_dir", "read_file", "search_files", "edit_file", "write_file", "update_plan"}
	got := map[string]bool{}
	for _, n := range p.Tools {
		got[n] = true
	}
	if len(got) != len(want) {
		t.Fatalf("edit Tools = %v, want %v", p.Tools, want)
	}
	for _, n := range want {
		if !got[n] {
			t.Errorf("edit profile missing tool %q (got %v)", n, p.Tools)
		}
	}
}

func TestLookupProfileUnknownErrorsListingValidNames(t *testing.T) {
	_, err := LookupProfile("nope")
	if err == nil {
		t.Fatal("LookupProfile(nope) = nil error, want error")
	}
	// The error must list the valid profile names so the CLI user can recover.
	for _, name := range []string{"general", "edit", "build", "research", "github"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not list valid profile %q", err.Error(), name)
		}
	}
}

// (b) WithProfile narrows the advertised specs to ONLY the profile subset present,
// and a profile listing a tool NOT already enabled does NOT add it (narrow-only invariant).
func TestWithProfileNarrowsSpecsToSubset(t *testing.T) {
	// Loop has the full set (superset of the edit profile plus web tools).
	full := mkTools("list_dir", "read_file", "search_files", "edit_file", "write_file",
		"update_plan", "web_search", "web_fetch", "run_shell", "offload_summarize")
	l := NewLoop(&fakeClient{}, full, 5)
	p, err := LookupProfile("edit")
	if err != nil {
		t.Fatalf("LookupProfile(edit): %v", err)
	}
	l.WithProfile(p)

	got := specNameSet(l)
	wantOnly := []string{"list_dir", "read_file", "search_files", "edit_file", "write_file", "update_plan"}
	if len(got) != len(wantOnly) {
		t.Fatalf("after WithProfile(edit), specs = %v, want only %v", got, wantOnly)
	}
	for _, n := range wantOnly {
		if !got[n] {
			t.Errorf("edit profile should advertise %q but does not (got %v)", n, got)
		}
	}
	// Tools NOT in the profile must be gone from both specs and executor map.
	for _, n := range []string{"web_search", "web_fetch", "run_shell", "offload_summarize"} {
		if got[n] {
			t.Errorf("edit profile should NOT advertise %q", n)
		}
		if _, ok := l.tools[n]; ok {
			t.Errorf("edit profile should have removed executor %q", n)
		}
	}
}

func TestWithProfileCannotGrantToolNotEnabled(t *testing.T) {
	// Loop is missing write_file/edit_file/run_shell etc. — only read tools present.
	l := NewLoop(&fakeClient{}, mkTools("list_dir", "read_file", "search_files"), 5)
	// The build profile lists run_shell + edit_file + write_file, none of which are enabled.
	p, err := LookupProfile("build")
	if err != nil {
		t.Fatalf("LookupProfile(build): %v", err)
	}
	l.WithProfile(p)

	got := specNameSet(l)
	// Only the intersection (list_dir, read_file, search_files) may remain; the
	// profile CANNOT add run_shell/edit_file/write_file that the caps never enabled.
	for _, n := range []string{"run_shell", "edit_file", "write_file", "update_plan"} {
		if got[n] {
			t.Errorf("narrow-only violated: profile added %q that was not enabled (got %v)", n, got)
		}
		if _, ok := l.tools[n]; ok {
			t.Errorf("narrow-only violated: executor %q added", n)
		}
	}
}

// (d) general profile (empty Tools) leaves specs unchanged.
func TestWithProfileGeneralLeavesSpecsUnchanged(t *testing.T) {
	full := mkTools("list_dir", "read_file", "search_files", "web_search", "web_fetch")
	l := NewLoop(&fakeClient{}, full, 5)
	before := specNameSet(l)
	p, err := LookupProfile("general")
	if err != nil {
		t.Fatalf("LookupProfile(general): %v", err)
	}
	l.WithProfile(p)
	after := specNameSet(l)
	if len(after) != len(before) {
		t.Fatalf("general changed specs: before %v after %v", before, after)
	}
	for n := range before {
		if !after[n] {
			t.Errorf("general dropped tool %q", n)
		}
	}
}

// (c) Exemplars are injected as user/assistant messages AFTER the system message
// and BEFORE the objective, in the msgs the client sees.
func TestWithProfileInjectsExemplarsAfterSystemBeforeObjective(t *testing.T) {
	full := mkTools("list_dir", "read_file", "search_files", "edit_file", "write_file", "update_plan")
	// One-step script: model immediately answers (no tool call), so Run does one Chat.
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	l := NewLoop(client, full, 3).WithSystem("SYS-PROMPT")
	p, err := LookupProfile("edit")
	if err != nil {
		t.Fatalf("LookupProfile(edit): %v", err)
	}
	if len(p.Exemplars) < 2 {
		t.Fatalf("edit profile should ship >=2 exemplar messages, got %d", len(p.Exemplars))
	}
	l.WithProfile(p)

	if _, err := l.Run(context.Background(), "OBJECTIVE-TEXT"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(client.seen) == 0 {
		t.Fatal("client never called")
	}
	msgs := client.seen[0]

	// Locate the indices of: system, first exemplar, objective. NOTE: the edit
	// profile carries a tuned System, so WithProfile legitimately REPLACES the
	// "SYS-PROMPT" content — match on Role, not the original content.
	sysIdx, exemplarStart, objIdx := -1, -1, -1
	for i, m := range msgs {
		if m.Role == "system" && sysIdx == -1 {
			sysIdx = i
		}
		if m.Content == "OBJECTIVE-TEXT" && objIdx == -1 {
			objIdx = i
		}
	}
	if sysIdx == -1 {
		t.Fatal("system message not found in transcript")
	}
	if objIdx == -1 {
		t.Fatal("objective message not found in transcript")
	}
	// Exemplars must appear as the messages immediately after system.
	exemplarStart = sysIdx + 1
	if exemplarStart+len(p.Exemplars) > len(msgs) {
		t.Fatalf("not enough messages for exemplars after system")
	}
	// The exemplar block must match the profile's Exemplars, and precede the objective.
	if exemplarStart+len(p.Exemplars) > objIdx {
		t.Fatalf("exemplars (start %d, len %d) not before objective (idx %d)", exemplarStart, len(p.Exemplars), objIdx)
	}
	for j, want := range p.Exemplars {
		gotMsg := msgs[exemplarStart+j]
		if gotMsg.Role != want.Role || gotMsg.Content != want.Content {
			t.Errorf("exemplar[%d] = {%q, %q}, want {%q, %q}", j, gotMsg.Role, gotMsg.Content, want.Role, want.Content)
		}
	}
	// Sanity: at least one exemplar is role=user and one is role=assistant.
	var sawUser, sawAssistant bool
	for _, m := range p.Exemplars {
		if m.Role == "user" {
			sawUser = true
		}
		if m.Role == "assistant" {
			sawAssistant = true
		}
	}
	if !sawUser || !sawAssistant {
		t.Errorf("exemplars must include both a user and an assistant message (user=%v assistant=%v)", sawUser, sawAssistant)
	}
}
