package core

import (
	"context"
	"testing"
)

func TestMarkWorkingCallsTheMark(t *testing.T) {
	n := 0
	ctx := WithWorkingMark(context.Background(), func() { n++ })
	MarkWorking(ctx)
	MarkWorking(context.Background()) // no mark: ignored
	MarkWorking(nil)                  //nolint:staticcheck // a nil context is tolerated
	if n != 1 {
		t.Fatalf("mark called %d times, want 1", n)
	}
	if WithWorkingMark(context.Background(), nil) != context.Background() {
		t.Fatal("a nil mark must leave the context alone")
	}
}
