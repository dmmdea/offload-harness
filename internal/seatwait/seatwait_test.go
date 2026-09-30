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

// ADR 0066: the seat gate. Waiting out a busy answer only makes sense while the seat
// serves; a 5xx from llama-swap on a seat whose engine died or cannot start is the
// seat's failure, and sleeping the contention budget on it (90 s by default) only
// delays the run's typed outcome. Every client of the budget shares the gate — the
// loop's chat calls and the structured re-pack alike.
func TestSeatGateRefusesTheWaitWhileTheSeatIsNotServing(t *testing.T) {
	var asked []int
	down := true
	b := NewBudget(60).WithSeatGate(func(status int) bool { asked = append(asked, status); return down })

	if d, ok := b.NextFor(500, ""); ok || d != 0 {
		t.Fatalf("NextFor(500) = %v ok=%v, want the wait refused while the seat is not serving", d, ok)
	}
	if b.Spent() != 0 || b.Attempts() != 0 {
		t.Fatalf("spent %v attempts %d: a refused wait is not spent", b.Spent(), b.Attempts())
	}
	if b.LastStatus() != 500 {
		t.Fatalf("LastStatus = %d, want the 500 the seat answered remembered for the reason", b.LastStatus())
	}
	// 429 is contention by definition (llama-swap's concurrency limit): the seat is
	// never asked about it, and the wait is granted.
	if d, ok := b.NextFor(429, "1"); !ok || d != time.Second {
		t.Fatalf("NextFor(429) = %v ok=%v, want the 1 s wait granted without asking the seat", d, ok)
	}
	for _, s := range asked {
		if s == 429 {
			t.Fatalf("the seat gate was asked about a 429: %v", asked)
		}
	}
	// The seat serves again: the ordinary wait resumes.
	down = false
	if d, ok := b.NextFor(503, ""); !ok || d <= 0 {
		t.Fatalf("NextFor(503) = %v ok=%v, want the ordinary wait once the seat serves", d, ok)
	}
	// A status of 0 (Next) is not a busy answer: the gate is not asked.
	n := len(asked)
	if _, ok := b.Next(""); !ok || len(asked) != n {
		t.Fatalf("Next asked the seat gate (%v)", asked[n:])
	}
}

// A budget that never waits (a disabled or zero budget) still reports why the
// failure stands, and the gate cannot make a nil budget wait.
func TestSeatGateOnABudgetThatNeverWaits(t *testing.T) {
	b := NewBudget(-1).WithSeatGate(func(int) bool { return true })
	if _, ok := b.NextFor(500, ""); ok {
		t.Fatal("a disabled budget waited")
	}
	if b.LastStatus() != 500 {
		t.Fatalf("LastStatus = %d", b.LastStatus())
	}
	var nilBudget *Budget
	if nilBudget.WithSeatGate(func(int) bool { return false }) != nil {
		t.Fatal("WithSeatGate on a nil budget must stay nil")
	}
	if _, ok := nilBudget.NextFor(500, ""); ok {
		t.Fatal("a nil budget waited")
	}
	// No gate installed: NextFor is exactly what it was.
	plain := NewBudget(10)
	if d, ok := plain.NextFor(500, ""); !ok || d != time.Second {
		t.Fatalf("plain NextFor(500) = %v ok=%v", d, ok)
	}
}
