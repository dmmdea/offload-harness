package fleetnode

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestParseSmiMemory locks the nvidia-smi CSV parse: MiB in, GiB out, tolerant
// of whitespace and CRLF (nvidia-smi on Windows emits \r\n). A total <= 0 is an
// error — the contract treats vram_total_gb <= 0 as a failed probe, so the
// parser refuses to produce a zero-total snapshot rather than letting one leak
// into /fleet/health.
func TestParseSmiMemory(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantTotal float64
		wantUsed  float64
		wantErr   bool
	}{
		{"plain", "16384, 1234", 16.0, 1234.0 / 1024, false},
		{"crlf", "16384, 1234\r\n", 16.0, 1234.0 / 1024, false},
		{"lf", "16384, 2048\n", 16.0, 2.0, false},
		{"extra whitespace", "  16384 ,  512  ", 16.0, 0.5, false},
		{"zero used", "8192, 0", 8.0, 0, false},
		{"multi-gpu takes first line", "16384, 1024\r\n8192, 512\r\n", 16.0, 1.0, false},
		{"empty", "", 0, 0, true},
		{"one field", "16384", 0, 0, true},
		{"three fields", "16384, 1, 2", 0, 0, true},
		{"non-numeric total", "N/A, 1234", 0, 0, true},
		{"non-numeric used", "16384, [N/A]", 0, 0, true},
		{"zero total refused", "0, 0", 0, 0, true},
		{"negative used refused", "16384, -5", 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			total, used, err := ParseSmiMemory(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSmiMemory(%q) = (%v, %v, nil), want error", tc.in, total, used)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSmiMemory(%q) error: %v", tc.in, err)
			}
			if total != tc.wantTotal || used != tc.wantUsed {
				t.Errorf("ParseSmiMemory(%q) = (%v, %v), want (%v, %v)", tc.in, total, used, tc.wantTotal, tc.wantUsed)
			}
		})
	}
}

// waitFor polls until cond() or the deadline. Sampler tests are timing-based by
// nature (goroutine + ticker); the injected runner keeps them GPU-free.
func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestStartGlobalSampler locks the snapshot pipeline: injected runner output is
// parsed and published (Free = Total - Used), Load reports ok, and the sampler
// keeps refreshing on its interval.
func TestStartGlobalSampler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	calls := 0
	run := func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return "16384, 4096", nil
	}
	s := StartGlobalSampler(ctx, 10*time.Millisecond, run)

	snap, ok := s.Load()
	if !ok {
		t.Fatal("Load() not ok after start — the initial sample must publish before StartGlobalSampler returns")
	}
	if snap.TotalGiB != 16.0 || snap.FreeGiB != 12.0 {
		t.Errorf("snapshot = %+v, want Total 16 Free 12", snap)
	}
	if snap.At.IsZero() {
		t.Error("snapshot At is zero, want a real timestamp")
	}

	if !waitFor(t, 2*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return calls >= 3 }) {
		t.Fatalf("sampler did not keep sampling: %d calls", calls)
	}
}

// TestStartGlobalSamplerErrorKeepsLast locks the never-emit-zeros rule: runner
// errors and unparseable output leave the last good snapshot in place, and a
// sampler that has never succeeded reports !ok rather than a zero snapshot.
func TestStartGlobalSamplerErrorKeepsLast(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	fail := false
	calls := 0
	run := func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if fail {
			return "", errors.New("nvidia-smi exploded")
		}
		return "16384, 1024", nil
	}
	s := StartGlobalSampler(ctx, 10*time.Millisecond, run)
	if _, ok := s.Load(); !ok {
		t.Fatal("initial sample should have published")
	}
	mu.Lock()
	fail = true
	base := calls
	mu.Unlock()
	if !waitFor(t, 2*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return calls >= base+2 }) {
		t.Fatal("sampler stopped sampling after errors")
	}
	snap, ok := s.Load()
	if !ok || snap.TotalGiB != 16.0 || snap.FreeGiB != 15.0 {
		t.Errorf("after runner failures snapshot = %+v ok=%v, want last good (16, 15) true", snap, ok)
	}
}

// TestStartGlobalSamplerNeverOK locks the all-failures path: Load stays !ok.
func TestStartGlobalSamplerNeverOK(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := StartGlobalSampler(ctx, 10*time.Millisecond, func() (string, error) {
		return "", errors.New("no GPU")
	})
	if snap, ok := s.Load(); ok {
		t.Errorf("Load() = %+v ok=true, want !ok when no sample ever succeeded", snap)
	}
}

// TestStartGlobalSamplerStops locks ctx cancellation: no further runner calls
// after cancel (modulo one in-flight tick).
func TestStartGlobalSamplerStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	calls := 0
	run := func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return "16384, 1024", nil
	}
	StartGlobalSampler(ctx, 5*time.Millisecond, run)
	cancel()
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	after := calls
	mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	final := calls
	mu.Unlock()
	if final > after+1 { // allow at most one racing tick around cancel
		t.Errorf("sampler kept running after cancel: %d -> %d calls", after, final)
	}
}
