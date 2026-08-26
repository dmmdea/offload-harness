package modelaffinity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// testBase gives every test its own base key. The registry is process-global
// on purpose (one llama-swap is one shared resource however many clients exist),
// so tests must not share a key or they observe each other's traffic.
func testBase(t *testing.T) string { return "http://" + t.Name() + ".invalid:11436" }

// waitQueueDepth blocks until exactly n callers are parked on base. It is how a
// test proves a waiter has REACHED the gate before the test advances — without
// it, "the queued call went second" can pass because the second caller had not
// arrived yet, which is a test that cannot fail.
func waitQueueDepth(t *testing.T, base string, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if queueDepth(base) == n {
			return
		}
		time.Sleep(200 * time.Microsecond)
	}
	t.Fatalf("queue depth on %s never reached %d (last=%d)", base, n, queueDepth(base))
}

// waitInFlight blocks until at least one admission is live on base.
func waitInFlight(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if inFlight(base) > 0 {
			return
		}
		time.Sleep(200 * time.Microsecond)
	}
	t.Fatalf("no admission ever went in flight on %s", base)
}

func mustAdmit(t *testing.T, base, model string) Ticket {
	t.Helper()
	tk, err := Admit(context.Background(), base, model, 5*time.Second)
	if err != nil {
		t.Fatalf("Admit(%s,%s): %v", base, model, err)
	}
	return tk
}

// Same model, same base: the requests must OVERLAP. llama-swap already queues
// same-model requests harmlessly; serialising them here would be a pure
// regression. The barrier IS the assertion — every goroutine has to be inside
// its admission at the same instant, so a serialising gate cannot pass by
// simply finishing quickly.
func TestSameModelSameBaseRunsConcurrently(t *testing.T) {
	base := testBase(t)
	const n = 4
	entered := make(chan struct{}, n)
	hold := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tk, err := Admit(context.Background(), base, "same-model", 5*time.Second)
			if err != nil {
				t.Errorf("Admit: %v", err)
				return
			}
			entered <- struct{}{}
			<-hold
			tk.Release()
		}()
	}
	for i := 0; i < n; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			close(hold)
			wg.Wait()
			t.Fatalf("only %d of %d same-model admissions overlapped: the gate serialised same-model traffic", i, n)
		}
	}
	close(hold)
	wg.Wait()
}

// A different model on a base that has in-flight requests must WAIT for those
// to drain. The order channel records real entry and exit, so the assertion is
// on observed sequence, not on elapsed time.
func TestDifferentModelWaitsForInFlightDrain(t *testing.T) {
	base := testBase(t)
	order := make(chan string, 4)
	aIn := make(chan struct{})
	aHold := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		tk := mustAdmit(t, base, "model-A")
		order <- "A-in"
		close(aIn)
		<-aHold
		order <- "A-out"
		tk.Release()
	}()
	<-aIn

	wg.Add(1)
	go func() {
		defer wg.Done()
		tk, err := Admit(context.Background(), base, "model-B", 5*time.Second)
		if err != nil {
			t.Errorf("B Admit: %v", err)
			return
		}
		order <- "B-in"
		tk.Release()
	}()
	waitQueueDepth(t, base, 1) // B has provably reached the gate and parked
	close(aHold)
	wg.Wait()
	close(order)

	var got []string
	for s := range order {
		got = append(got, s)
	}
	want := []string{"A-in", "A-out", "B-in"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v (a different model entered before the in-flight batch drained)", got, want)
	}
}

// Two llama-swap instances do not contend. resolveEndpoint can hand two models
// two different bases; keying the gate on the model alone would serialise lanes
// that never conflicted. The barrier is again the assertion.
func TestDifferentBasesDoNotBlockEachOther(t *testing.T) {
	b1 := testBase(t) + "/one"
	b2 := testBase(t) + "/two"
	entered := make(chan string, 2)
	hold := make(chan struct{})
	var wg sync.WaitGroup
	for _, pair := range [][2]string{{b1, "model-A"}, {b2, "model-B"}} {
		wg.Add(1)
		go func(base, model string) {
			defer wg.Done()
			tk, err := Admit(context.Background(), base, model, 5*time.Second)
			if err != nil {
				t.Errorf("Admit(%s): %v", base, err)
				return
			}
			entered <- base
			<-hold
			tk.Release()
		}(pair[0], pair[1])
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			close(hold)
			wg.Wait()
			t.Fatalf("only %d of 2 admissions on DIFFERENT bases overlapped: the gate is not keyed on the resolved base", i)
		}
	}
	close(hold)
	wg.Wait()
}

// The wait is bounded, and exhausting it says what happened: which base, which
// model was wanted, which model held it, how long it waited.
func TestBoundFiresWithDistinctOutcome(t *testing.T) {
	base := testBase(t)
	hold := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tk := mustAdmit(t, base, "holder-model")
		<-hold
		tk.Release()
	}()
	waitInFlight(t, base)

	start := time.Now()
	_, err := Admit(context.Background(), base, "waiting-model", 30*time.Millisecond)
	waited := time.Since(start)
	close(hold)
	<-done

	if err == nil {
		t.Fatal("Admit succeeded while a different model held the base; the bound never fired")
	}
	var we *WaitError
	if !errors.As(err, &we) {
		t.Fatalf("err = %T (%v), want *WaitError — a generic timeout does not name what happened", err, err)
	}
	if we.Base != base || we.Want != "waiting-model" || we.Held != "holder-model" {
		t.Fatalf("WaitError = %+v, want Base=%s Want=waiting-model Held=holder-model", we, base)
	}
	if we.InFlight != 1 {
		t.Errorf("WaitError.InFlight = %d, want 1", we.InFlight)
	}
	if we.Bound <= 0 || waited < we.Bound {
		t.Errorf("waited %v with Bound %v: the bound must actually be waited out", waited, we.Bound)
	}
	msg := err.Error()
	for _, want := range []string{"model-affinity", base, "waiting-model", "holder-model"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name %q", msg, want)
		}
	}
	// Deliberate coupling, pinned here so a reword cannot silently reclassify:
	// internal/pipeline.classifyErr buckets on err.Error() substrings and files
	// anything without a known marker as "other". An exhausted affinity wait is
	// congestion, which that function spells "timeout".
	if !strings.Contains(msg, "timeout") {
		t.Errorf("error %q lacks the substring pipeline.classifyErr buckets congestion by (%q)", msg, "timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("errors.Is(err, context.DeadlineExceeded) = false; callers that branch on a deadline lose the signal")
	}
}

// A caller's own ctx firing while parked must still name the wait, rather than
// surfacing a bare context error from an unnamed place.
func TestCtxCancellationNamesTheWait(t *testing.T) {
	base := testBase(t)
	hold := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tk := mustAdmit(t, base, "holder-model")
		<-hold
		tk.Release()
	}()
	waitInFlight(t, base)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := Admit(ctx, base, "waiting-model", time.Hour)
		errCh <- err
	}()
	waitQueueDepth(t, base, 1)
	cancel()
	err := <-errCh
	close(hold)
	<-done

	var we *WaitError
	if !errors.As(err, &we) {
		t.Fatalf("err = %T (%v), want *WaitError", err, err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false for %v", err)
	}
}

// A lane that keeps losing the race must eventually get its switch. Once a
// different-model caller is parked, later SAME-model arrivals park behind it
// instead of joining the running batch — otherwise a steady stream of the
// resident model starves the switch forever.
func TestParkedSwitchIsNotStarvedByLaterSameModelArrivals(t *testing.T) {
	base := testBase(t)
	order := make(chan string, 8)
	a1In := make(chan struct{})
	a1Hold := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() { // A1 takes the base
		defer wg.Done()
		tk := mustAdmit(t, base, "model-A")
		close(a1In)
		<-a1Hold
		tk.Release()
	}()
	<-a1In

	wg.Add(1)
	go func() { // B parks behind A1
		defer wg.Done()
		tk, err := Admit(context.Background(), base, "model-B", 5*time.Second)
		if err != nil {
			t.Errorf("B Admit: %v", err)
			return
		}
		order <- "B"
		// Held long enough that an A2 which had jumped the queue would have
		// recorded itself first.
		time.Sleep(20 * time.Millisecond)
		tk.Release()
	}()
	waitQueueDepth(t, base, 1)

	wg.Add(1)
	go func() { // A2 arrives AFTER B, naming the model that is already resident
		defer wg.Done()
		tk, err := Admit(context.Background(), base, "model-A", 5*time.Second)
		if err != nil {
			t.Errorf("A2 Admit: %v", err)
			return
		}
		order <- "A2"
		tk.Release()
	}()
	waitQueueDepth(t, base, 2) // A2 provably parked too, rather than joining A1's batch

	close(a1Hold)
	wg.Wait()
	close(order)

	var got []string
	for s := range order {
		got = append(got, s)
	}
	if len(got) != 2 || got[0] != "B" {
		t.Fatalf("admission order = %v, want B before A2: a later same-model arrival jumped the parked switch", got)
	}
}

// The property, sampled under real overlap: at no instant may two different
// models be in flight on one base, and the burst must actually overlap — a
// burst that never overlapped would pass vacuously.
func TestMixedBurstUpholdsOneModelPerBase(t *testing.T) {
	bases := []string{testBase(t) + "/a", testBase(t) + "/b", testBase(t) + "/c"}
	models := []string{"m1", "m2", "m3"}

	var mu sync.Mutex
	cur := map[string]string{} // base -> model currently in flight
	live := map[string]int{}   // base -> in-flight count
	maxLive := map[string]int{}
	var violations []string

	enter := func(base, model string) {
		mu.Lock()
		defer mu.Unlock()
		if live[base] > 0 && cur[base] != model {
			violations = append(violations, fmt.Sprintf("%s: %q entered while %d x %q was in flight", base, model, live[base], cur[base]))
		}
		cur[base] = model
		live[base]++
		if live[base] > maxLive[base] {
			maxLive[base] = live[base]
		}
	}
	exit := func(base string) {
		mu.Lock()
		defer mu.Unlock()
		live[base]--
	}

	const perCombo = 12
	var wg sync.WaitGroup
	done := make(chan struct{})
	for bi, base := range bases {
		for mi, model := range models {
			for k := 0; k < perCombo; k++ {
				wg.Add(1)
				go func(base, model string, jitter int) {
					defer wg.Done()
					time.Sleep(time.Duration(jitter%7) * 100 * time.Microsecond)
					tk, err := Admit(context.Background(), base, model, 10*time.Second)
					if err != nil {
						t.Errorf("Admit(%s,%s): %v", base, model, err)
						return
					}
					enter(base, model)
					time.Sleep(time.Duration(jitter%5) * 200 * time.Microsecond)
					exit(base)
					tk.Release()
				}(base, model, bi*100+mi*10+k)
			}
		}
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("mixed burst did not complete: deadlock or lost release")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(violations) > 0 {
		t.Fatalf("two models in flight on one base:\n  %s", strings.Join(violations, "\n  "))
	}
	overlapped := false
	for _, n := range maxLive {
		if n > 1 {
			overlapped = true
		}
	}
	if !overlapped {
		t.Fatal("no base ever had two concurrent admissions: the burst never overlapped, so it proved nothing")
	}
	for _, base := range bases {
		if queueDepth(base) != 0 || inFlight(base) != 0 {
			t.Errorf("%s left with queue=%d inflight=%d after the burst drained", base, queueDepth(base), inFlight(base))
		}
	}
}

// A withdrawn waiter must not wedge the base: after a bound fires, the next
// caller still gets in.
func TestBaseStaysUsableAfterAWaitIsAbandoned(t *testing.T) {
	base := testBase(t)
	hold := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tk := mustAdmit(t, base, "holder-model")
		<-hold
		tk.Release()
	}()
	waitInFlight(t, base)

	if _, err := Admit(context.Background(), base, "abandoned-model", 20*time.Millisecond); err == nil {
		t.Fatal("expected the bound to fire")
	}
	if d := queueDepth(base); d != 0 {
		t.Fatalf("queue depth after an abandoned wait = %d, want 0", d)
	}
	close(hold)
	<-done

	tk, err := Admit(context.Background(), base, "next-model", 2*time.Second)
	if err != nil {
		t.Fatalf("base wedged after an abandoned wait: %v", err)
	}
	tk.Release()
}

// The bound scales with what is queued ahead: a caller that must wait through
// two batches is allowed proportionally longer than one waiting through one.
// Without this, a legitimate deep queue fails callers that would have been
// served.
func TestBoundScalesWithBatchesAhead(t *testing.T) {
	base := testBase(t)
	hold := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tk := mustAdmit(t, base, "model-A")
		<-hold
		tk.Release()
	}()
	waitInFlight(t, base)

	// One waiter parks (model-B), then we ask for model-C. C must wait through
	// A's batch AND B's batch, so its bound has to be larger than a bound that
	// only covered A's.
	bErr := make(chan error, 1)
	go func() {
		tk, err := Admit(context.Background(), base, "model-B", time.Hour)
		if err == nil {
			tk.Release()
		}
		bErr <- err
	}()
	waitQueueDepth(t, base, 1)

	_, err := Admit(context.Background(), base, "model-C", 25*time.Millisecond)
	close(hold)
	<-done
	<-bErr

	var we *WaitError
	if !errors.As(err, &we) {
		t.Fatalf("err = %T (%v), want *WaitError", err, err)
	}
	if we.BatchesAhead != 2 {
		t.Errorf("BatchesAhead = %d, want 2 (A in flight + B parked)", we.BatchesAhead)
	}
	if want := 3 * 25 * time.Millisecond; we.Bound != want {
		t.Errorf("Bound = %v, want %v ((batchesAhead+1) x budget)", we.Bound, want)
	}
}
