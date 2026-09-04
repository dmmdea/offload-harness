package seatwait

import (
	"context"
	"testing"
	"time"
)

func TestLadderAndBudgetAreCumulativeAcrossCalls(t *testing.T) {
	b := NewBudget(10) // 1+2+4 = 7 fits; +8 = 15 does not
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		d, ok := b.Next("")
		if !ok || d != want {
			t.Fatalf("call %d: %v ok=%v want %v", i, d, ok, want)
		}
	}
	if _, ok := b.Next(""); ok {
		t.Fatal("fourth wait must exceed the 10 s budget")
	}
	if b.Spent() != 7*time.Second || b.Attempts() != 3 {
		t.Fatalf("spent %v attempts %d", b.Spent(), b.Attempts())
	}
}

func TestLadderCapsAtFifteenSeconds(t *testing.T) {
	b := NewBudget(300)
	var got []time.Duration
	for i := 0; i < 7; i++ {
		d, ok := b.Next("")
		if !ok {
			t.Fatalf("attempt %d refused inside a 300 s budget", i)
		}
		got = append(got, d)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 15 * time.Second, 15 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("attempt %d: %v want %v", i, got[i], want[i])
		}
	}
}

func TestRetryAfterCountsAgainstTheBudget(t *testing.T) {
	b := NewBudget(3)
	for i := 0; i < 3; i++ {
		if d, ok := b.Next("1"); !ok || d != time.Second {
			t.Fatalf("call %d: %v %v", i, d, ok)
		}
	}
	if _, ok := b.Next("1"); ok {
		t.Fatal("a server saying Retry-After: 1 forever must still run out of budget")
	}
}

func TestRetryAfterBeyondTheBudgetIsRefusedNotSlept(t *testing.T) {
	b := NewBudget(30)
	if _, ok := b.Next("600"); ok {
		t.Fatal("Retry-After 600 on a 30 s budget must refuse")
	}
	if b.Spent() != 0 {
		t.Fatalf("a refused reservation must not be charged: %v", b.Spent())
	}
}

func TestDisabledAndAbsentBudgetsNeverWait(t *testing.T) {
	if _, ok := NewBudget(-1).Next("1"); ok {
		t.Fatal("negative config disables the wait")
	}
	if _, ok := FromContext(context.Background()).Next(""); ok {
		t.Fatal("no budget in ctx = no wait")
	}
	var nilB *Budget
	if _, ok := nilB.Next(""); ok || nilB.Spent() != 0 {
		t.Fatal("nil receiver never waits")
	}
}

func TestContextRoundTrip(t *testing.T) {
	b := NewBudget(0)
	ctx := WithBudget(context.Background(), b)
	if FromContext(ctx) != b {
		t.Fatal("FromContext must return the attached budget")
	}
	if d, ok := b.Next(""); !ok || d != time.Second {
		t.Fatalf("default budget first sleep: %v %v", d, ok)
	}
}

func TestRetryableClasses(t *testing.T) {
	cases := []struct {
		code int
		body string
		want bool
	}{
		{429, `{"error":{"code":"concurrency_limit","src":"llama-swap"}}`, true},
		{429, ``, true},
		{503, `[qwen3.8-27b] process is not ready`, true},
		{503, `service unavailable`, false},
		{500, `{"error":{"message":"unspecific error: health check timed out after 120s","src":"llama-swap"}}`, true},
		{500, `CUDA error: out of memory`, false},
		{502, `upstream closed`, false},
		{502, ``, true},
		{502, `  `, true},
		{400, `context length exceeded`, false},
		{200, ``, false},
	}
	for _, c := range cases {
		if got := Retryable(c.code, c.body); got != c.want {
			t.Fatalf("%d %q: got %v want %v", c.code, c.body, got, c.want)
		}
	}
}

func TestSleepHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Sleep(ctx, time.Minute); err == nil {
		t.Fatal("a cancelled ctx must end the sleep with its error")
	}
	if err := Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestCausedTimeoutNeedsCausation(t *testing.T) {
	b := NewBudget(90)
	if b.CausedTimeout(time.Minute) {
		t.Fatal("no busy answer ever: not a contention timeout")
	}
	b.NextFor(429, "1") // one early 429, 1 s, resolved
	if b.CausedTimeout(10 * time.Minute) {
		t.Fatal("1 s of waiting does not explain a 10-minute wall")
	}
	if !b.CausedTimeout(2 * time.Second) {
		t.Fatal("1 s of waiting IS half of a 2 s wall")
	}
	// a sleep in flight at expiry always counts
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		_ = b.Sleep(ctx, time.Minute)
		done <- true
	}()
	time.Sleep(50 * time.Millisecond)
	if !b.Sleeping() || !b.CausedTimeout(10*time.Minute) {
		t.Fatal("a sleep in flight when the wall expires is contention, whatever the ratio")
	}
	cancel()
	<-done
	if b.Sleeping() {
		t.Fatal("the in-flight flag must clear when the sleep ends")
	}
}
