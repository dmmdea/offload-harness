package agent

import (
	"context"
	"testing"
)

// TestWithoutExemplarsRemovesTheFewShotFromTheTranscript: after WithProfile installed
// the edit profile's exemplars, WithoutExemplars must leave the transcript as
// system -> objective with no exemplar message in between, while the profile's
// tuned system prompt and tool narrowing stay in force.
func TestWithoutExemplarsRemovesTheFewShotFromTheTranscript(t *testing.T) {
	full := mkTools("list_dir", "read_file", "search_files", "edit_file", "write_file", "update_plan")
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	l := NewLoop(client, full, 3).WithSystem("SYS-PROMPT")
	p, err := LookupProfile("edit")
	if err != nil {
		t.Fatalf("LookupProfile(edit): %v", err)
	}
	l.WithProfile(p).WithoutExemplars()
	if _, err := l.Run(context.Background(), "OBJECTIVE-TEXT"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(client.seen) == 0 {
		t.Fatal("client never called")
	}
	msgs := client.seen[0]
	for i, m := range msgs {
		for _, ex := range p.Exemplars {
			if m.Role == ex.Role && m.Content == ex.Content && m.Content != "" {
				t.Fatalf("exemplar message %q still present at transcript[%d]", m.Content, i)
			}
		}
	}
	if msgs[0].Role != "system" || msgs[0].Content != p.System {
		t.Fatalf("profile system prompt lost: role=%q content=%.40q", msgs[0].Role, msgs[0].Content)
	}
	if msgs[len(msgs)-1].Content != "OBJECTIVE-TEXT" {
		t.Fatalf("objective must be the last user message, got %.60q", msgs[len(msgs)-1].Content)
	}
}
