package mediacap

// The bounds on the fingerprint cache (fingerprint.go, F59 fix round): a wait that ends, a read that is given up on, a read
// in flight that the cache bound never drops, and the width of what the sample does not see.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gatedHash is a hash that holds the named files until release, counting how often each file was asked for, so a test can
// stall a read the way a dying disk or a hung share does.
type gatedHash struct {
	gate  chan struct{}
	once  sync.Once
	hold  func(name string) bool
	mu    sync.Mutex
	calls map[string]int
}

func newGated(t *testing.T, hold func(name string) bool) (*Fingerprinter, *gatedHash) {
	t.Helper()
	g := &gatedHash{gate: make(chan struct{}), hold: hold, calls: map[string]int{}}
	t.Cleanup(g.release) // a read still held when the test ends must not outlive it
	f := NewFingerprinterWith(func(path string, size int64) (string, error) {
		name := filepath.Base(path)
		g.mu.Lock()
		g.calls[name]++
		g.mu.Unlock()
		if g.hold(name) {
			<-g.gate
		}
		return FingerprintFile(path, size)
	})
	return f, g
}

func (g *gatedHash) release() { g.once.Do(func() { close(g.gate) }) }

func (g *gatedHash) count(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[name]
}

func smallWeight(t *testing.T, dir, name string, fill byte) (string, int64) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, bytes.Repeat([]byte{fill}, 3000), 0o644); err != nil {
		t.Fatal(err)
	}
	return p, 3000
}

// within runs fn and fails the test if it has not returned in d: a wait that never ends must fail here, not hang the run.
func within(t *testing.T, d time.Duration, what string, fn func() string) string {
	t.Helper()
	ch := make(chan string, 1)
	go func() { ch <- fn() }()
	select {
	case s := <-ch:
		return s
	case <-time.After(d):
		t.Fatalf("%s did not return in %s", what, d)
		return ""
	}
}

// AWAIT HAS A DEADLINE. A caller on a request path (the 412 re-check, the door) must not hang on a read that is stalled or
// queued behind one: it is answered pending when the bound runs out, and the read it gave up on is not lost.
func TestAwaitIsBoundedAndAnswersPendingWhenItRunsOut(t *testing.T) {
	f, g := newGated(t, func(string) bool { return true })
	f.awaitMax = 80 * time.Millisecond
	p, n := smallWeight(t, t.TempDir(), "w.safetensors", 1)
	start := time.Now()
	if got := within(t, 5*time.Second, "Await of a stalled read", func() string { return f.Await(p, n) }); got != FingerprintPending {
		t.Fatalf("Await = %q, want %q when the bound runs out", got, FingerprintPending)
	}
	if el := time.Since(start); el < f.awaitMax/2 {
		t.Errorf("Await answered after %s: it must wait for the bound (%s) before it gives up", el, f.awaitMax)
	}
	g.release()
	f.awaitMax = 10 * time.Second
	if got := f.Await(p, n); !IsFingerprint(got) {
		t.Errorf("Await after the read was let go = %q, want the hash", got)
	}
	if c := g.count("w.safetensors"); c != 1 {
		t.Errorf("the file was read %d times: giving up on a wait must not drop or repeat the read", c)
	}
}

// ONE deadline covers the whole call, the retry-at-once included. The first read fails after 320 ms of a 400 ms bound; the
// retry it asks for then stalls. Sharing the deadline ends the call near 400 ms; a second full wait for the retry would end
// near 720 ms. (Timing-based: the margin on either side of the 560 ms threshold is 160 ms.)
func TestAwaitsDeadlineCoversItsRetry(t *testing.T) {
	var calls atomic.Int32
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	f := NewFingerprinterWith(func(path string, size int64) (string, error) {
		if calls.Add(1) == 1 {
			time.Sleep(320 * time.Millisecond)
			return "", errors.New("device not ready")
		}
		<-gate
		return FingerprintFile(path, size)
	})
	f.awaitMax = 400 * time.Millisecond
	p, n := smallWeight(t, t.TempDir(), "w.safetensors", 1)
	start := time.Now()
	if got := within(t, 5*time.Second, "Await whose retry stalls", func() string { return f.Await(p, n) }); got != FingerprintPending {
		t.Errorf("Await = %q, want pending: the retry was still running when the bound ran out", got)
	}
	if el := time.Since(start); el > 560*time.Millisecond {
		t.Errorf("Await took %s: the retry got a wait of its own instead of what was left of the one bound (%s)", el, f.awaitMax)
	}
	if calls.Load() != 2 {
		t.Errorf("%d reads, want the failed one and its retry", calls.Load())
	}
}

// AwaitContext ends with its caller: a request whose client is gone stops waiting for the hash.
func TestAwaitContextStopsWaitingWhenItsCallerIsGone(t *testing.T) {
	f, g := newGated(t, func(string) bool { return true })
	f.awaitMax = time.Hour
	p, n := smallWeight(t, t.TempDir(), "w.safetensors", 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(40 * time.Millisecond); cancel() }()
	if got := within(t, 5*time.Second, "AwaitContext of a cancelled caller", func() string { return f.AwaitContext(ctx, p, n) }); got != FingerprintPending {
		t.Fatalf("AwaitContext = %q, want pending", got)
	}
	g.release()
	if got := f.Await(p, n); !IsFingerprint(got) {
		t.Errorf("the read that was waited on settles for the next caller: %q", got)
	}
	if c := g.count("w.safetensors"); c != 1 {
		t.Errorf("read %d times, want once", c)
	}
}

// A fingerprint that is already cached is answered even to a caller that is gone: the select between "settled" and "caller
// gone" is random when both are ready, so without a look at the entry first a cache hit would answer pending half the time.
func TestACachedFingerprintIsAnsweredEvenToAGoneCaller(t *testing.T) {
	var hashes atomic.Int32
	f := countingFingerprinter(&hashes)
	p, n := smallWeight(t, t.TempDir(), "w.safetensors", 1)
	want := f.Await(p, n)
	if !IsFingerprint(want) {
		t.Fatalf("Await = %q", want)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 60; i++ {
		if got := f.AwaitContext(ctx, p, n); got != want {
			t.Fatalf("call %d: AwaitContext of a cached file for a gone caller = %q, want the cached hash", i, got)
		}
	}
	if hashes.Load() != 1 {
		t.Errorf("%d hashes, want 1", hashes.Load())
	}
}

// THE SINGLE WORKER'S CONSEQUENCE, PINNED: while one read holds the worker, a wait for ANOTHER file also runs out. It
// ends in pending at the bound, not in a hang, and that file is read once the worker is free.
func TestAWaitForAnotherFileQueuedBehindAStalledReadEndsAtTheBound(t *testing.T) {
	f, g := newGated(t, func(name string) bool { return name == "slow.safetensors" })
	f.awaitMax = 80 * time.Millisecond
	f.readMax = time.Hour
	dir := t.TempDir()
	slow, sn := smallWeight(t, dir, "slow.safetensors", 1)
	other, on := smallWeight(t, dir, "other.safetensors", 2)
	f.Peek(slow, sn)
	waitFor(t, func() bool { return g.count("slow.safetensors") == 1 }) // the stalled read holds the worker
	if got := within(t, 5*time.Second, "Await of a file behind a stalled read", func() string { return f.Await(other, on) }); got != FingerprintPending {
		t.Errorf("Await = %q, want pending", got)
	}
	g.release()
	f.awaitMax = 10 * time.Second
	if got := f.Await(other, on); !IsFingerprint(got) {
		t.Errorf("once the worker is free the queued file is read: %q", got)
	}
}

// A READ THAT NEVER FINISHES IS GIVEN UP ON. It settles unreadable after readMax and frees the worker, so the node's
// other files are read and a stalled file does not leave every family pending for good; asking for the stalled file again
// does not pile a second read onto the first, and it is read again once the first one has returned.
func TestAStalledReadIsGivenUpOnAndDoesNotPinTheOtherFiles(t *testing.T) {
	f, g := newGated(t, func(name string) bool { return name == "stuck.safetensors" })
	f.readMax = 60 * time.Millisecond
	dir := t.TempDir()
	stuck, sn := smallWeight(t, dir, "stuck.safetensors", 1)
	other, on := smallWeight(t, dir, "other.safetensors", 2)

	if got := f.Peek(stuck, sn); got != FingerprintPending {
		t.Fatalf("first Peek = %q", got)
	}
	if got := within(t, 5*time.Second, "Await of a file queued behind a stuck read", func() string { return f.Await(other, on) }); !IsFingerprint(got) {
		t.Fatalf("the other file = %q: a stuck read must free the worker after readMax", got)
	}
	waitFor(t, func() bool { return f.Peek(stuck, sn) == FingerprintUnreadable })
	for i := 0; i < 3; i++ {
		if got := f.Await(stuck, sn); got != FingerprintUnreadable {
			t.Errorf("Await of the stuck file = %q, want unreadable", got)
		}
	}
	if c := g.count("stuck.safetensors"); c != 1 {
		t.Fatalf("the stuck file was read %d times: a read still stuck is not started again", c)
	}

	g.release()
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.stuck) == 0 })
	if got := f.Await(stuck, sn); !IsFingerprint(got) {
		t.Errorf("once the first read has returned the file is read again: %q", got)
	}
	if c := g.count("stuck.safetensors"); c != 2 {
		t.Errorf("%d reads of the stuck file, want 2", c)
	}
}

// A READ IN FLIGHT IS NEVER DROPPED by the bound on the cache, however many files are asked for at once: the entry
// waiters hold is the one in the map, so every file is read once.
func TestReadsInFlightAreNeverEvictedByTheBound(t *testing.T) {
	f, g := newGated(t, func(string) bool { return true })
	f.max = 2
	dir := t.TempDir()
	var names []string
	for i := 0; i < 5; i++ {
		name := string(rune('a'+i)) + ".safetensors"
		p, n := smallWeight(t, dir, name, byte(i))
		if got := f.Peek(p, n); got != FingerprintPending {
			t.Fatalf("Peek(%s) = %q", name, got)
		}
		names = append(names, name)
	}
	f.mu.Lock()
	held := len(f.m)
	f.mu.Unlock()
	if held != len(names) {
		t.Errorf("the cache holds %d of %d reads in flight: the bound (%d) must not drop a read that has not settled", held, len(names), f.max)
	}
	g.release()
	for _, name := range names {
		p := filepath.Join(dir, name)
		waitFor(t, func() bool { return IsFingerprint(f.Peek(p, 3000)) })
	}
	for _, name := range names {
		if c := g.count(name); c != 1 {
			t.Errorf("%s was read %d times, want once", name, c)
		}
	}
}

// THE NAMED LIMIT, AS WIDE AS IT IS. The blocks are evenly spaced, so the stretch between two of them is about a sixty-third
// of the file less a block: every byte in it can differ and the fingerprint is unchanged. The docs quote this width.
func TestTheUnsampledStretchBetweenTwoBlocksIsAboutASixtyThirdOfTheFile(t *testing.T) {
	for _, tc := range []struct {
		size           int64
		minGap, maxGap int64
		quoted         string
	}{
		{20_000_000_000, 300_000_000, 320_000_000, "0.3 GB of a 20 GB file"},
		{42_000_000_000, 640_000_000, 670_000_000, "0.65 GB of a 42 GB file"},
	} {
		offs := fingerprintOffsets(tc.size)
		var widest int64
		for i := 1; i < len(offs); i++ {
			if g := offs[i] - offs[i-1] - fingerprintBlock; g > widest {
				widest = g
			}
		}
		if widest < tc.minGap || widest > tc.maxGap {
			t.Errorf("size %d: the widest unsampled stretch is %d bytes, the docs quote %s", tc.size, widest, tc.quoted)
		}
	}
}

// A whole stretch between two blocks can be rewritten without the fingerprint noticing (a few transformer blocks changed
// by a layer-wise merge, a baked-in VAE), and one byte over its edge on either side is seen.
func TestAContiguousStretchBetweenTwoBlocksIsNotSeenAndItsEdgesAre(t *testing.T) {
	size := int64(80<<20) + 12345
	p := bigFile(t, size)
	base := mustSample(t, p)
	offs := fingerprintOffsets(size)
	from, to := offs[20]+fingerprintBlock, offs[21]
	if to-from < 100_000 {
		t.Fatalf("premise: the stretch between samples 20 and 21 is %d bytes", to-from)
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xAB}, int(to-from)), from); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if mustSample(t, p) != base {
		t.Errorf("all %d bytes between two sampled blocks changed and the fingerprint moved: the docs name this as the limit", to-from)
	}
	for _, edge := range []int64{from - 1, to} {
		poke(t, p, edge, 0xCD)
		if mustSample(t, p) == base {
			t.Errorf("a changed byte at %d, the edge of the stretch, is inside a block and must be seen", edge)
		}
		poke(t, p, edge, 0)
	}
}
