package agent

import (
	"context"
	"testing"
	"time"
)

// slowClient wraps the scripted client with a per-call delay — the seat's
// generation time, which is what calls[].ms must carry.
type slowClient struct {
	inner *fakeClient
	delay time.Duration
}

func (s *slowClient) Chat(ctx context.Context, msgs []Msg, specs []ToolSpec, maxTokens int) (Completion, error) {
	time.Sleep(s.delay)
	return s.inner.Chat(ctx, msgs, specs, maxTokens)
}

// TestCallRecordCarriesTheCallWall (0.115.21, register D-03): every planner
// completion records the client-measured wall it took, on the record the
// corpus and the seat-rate store read. Without it a seat's decode rate could
// only be guessed from the run's total wall, which includes tool execution
// and the cold load.
func TestCallRecordCarriesTheCallWall(t *testing.T) {
	execs := 0
	client := &slowClient{delay: 30 * time.Millisecond, inner: &fakeClient{script: []Completion{
		searchCall("c1", "a"),
		{Msg: Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop"},
	}}}
	res, err := NewLoop(client, oneTool(&execs), 4).Run(context.Background(), "extract")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(res.Calls))
	}
	for i, c := range res.Calls {
		if c.Ms < 30 {
			t.Errorf("call %d: ms = %d, want ≥ 30 (the seat's generation time must be on the record)", i, c.Ms)
		}
		if c.Ms > 5000 {
			t.Errorf("call %d: ms = %d, want the call's own wall, not something cumulative", i, c.Ms)
		}
	}
}
