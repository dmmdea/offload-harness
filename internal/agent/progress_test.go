package agent

import (
	"context"
	"testing"
)

func TestProgressFromContext(t *testing.T) {
	if ProgressFromContext(context.Background()) != nil {
		t.Fatal("no progress func installed, want nil")
	}
	var got []int
	ctx := ContextWithProgress(context.Background(), func(n int) { got = append(got, n) })
	fn := ProgressFromContext(ctx)
	if fn == nil {
		t.Fatal("progress func not carried")
	}
	fn(3)
	fn(7)
	if len(got) != 2 || got[1] != 7 {
		t.Fatalf("callbacks = %v, want [3 7]", got)
	}
	// a derived context still carries it (the loop wraps stepCtx several times)
	if ProgressFromContext(ContextWithoutThinking(ctx)) == nil {
		t.Fatal("lost through a derived context")
	}
	if ProgressFromContext(ContextWithProgress(ctx, nil)) != nil {
		t.Fatal("nil must remove an inherited func")
	}
}
