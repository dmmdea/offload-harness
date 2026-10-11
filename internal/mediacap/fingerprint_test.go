package mediacap

// The content fingerprint and its cache (fingerprint.go, F59).

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// bigFile writes a file larger than the sample (so it is hashed by sampling) of size bytes, zero-filled, and returns its
// path. It is sparse where the file system allows it.
func bigFile(t *testing.T, size int64) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "big.safetensors")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func poke(t *testing.T, path string, off int64, b byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte{b}, off); err != nil {
		t.Fatal(err)
	}
}

func mustSample(t *testing.T, path string) string {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := FingerprintFile(path, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

// A file no larger than the sample is hashed whole: a change of any one byte, the last included, is seen.
func TestASmallFileIsHashedWhole(t *testing.T) {
	p := filepath.Join(t.TempDir(), "small.safetensors")
	if err := os.WriteFile(p, bytes.Repeat([]byte{7}, 100_000), 0o644); err != nil {
		t.Fatal(err)
	}
	a := mustSample(t, p)
	if !IsFingerprint(a) {
		t.Fatalf("fingerprint = %q, want a sha256 in hex", a)
	}
	if again := mustSample(t, p); again != a {
		t.Error("the fingerprint of an unchanged file is stable")
	}
	for _, off := range []int64{0, 54_321, 99_999} {
		poke(t, p, off, 8)
		if mustSample(t, p) == a {
			t.Errorf("a changed byte at %d of a small file must change the fingerprint", off)
		}
		poke(t, p, off, 7)
	}
}

// A larger file is sampled: a change inside a sampled block (the first, the last, any between) is seen, and so is a
// difference in size alone. THE NAMED LIMIT is pinned too: a byte that falls between the sample points is not seen, and
// the docs say so.
func TestALargeFileIsSampledAndTheLimitIsPinned(t *testing.T) {
	size := int64(80<<20) + 12345
	p := bigFile(t, size)
	base := mustSample(t, p)
	offs := fingerprintOffsets(size)
	if len(offs) != fingerprintBlocks || offs[0] != 0 || offs[len(offs)-1]+fingerprintBlock != size {
		t.Fatalf("the samples must start at the first byte and end at the last: first %d, last ends at %d of %d", offs[0], offs[len(offs)-1]+fingerprintBlock, size)
	}
	for i := 1; i < len(offs); i++ {
		if offs[i] < offs[i-1]+fingerprintBlock {
			t.Fatalf("sample %d overlaps the one before it", i)
		}
	}
	for _, i := range []int{0, 1, 31, 62, 63} {
		off := offs[i] + 100
		poke(t, p, off, 9)
		if mustSample(t, p) == base {
			t.Errorf("a changed byte inside sample %d (offset %d) must change the fingerprint", i, off)
		}
		poke(t, p, off, 0)
	}
	gap := offs[0] + fingerprintBlock + 7
	if gap >= offs[1] {
		t.Fatal("no gap between the first two samples to test with")
	}
	poke(t, p, gap, 9)
	if mustSample(t, p) != base {
		t.Error("a byte between two samples is the named limit: the fingerprint does not see it")
	}
	poke(t, p, gap, 0)

	longer := bigFile(t, size+1)
	if mustSample(t, longer) == base {
		t.Error("the size is part of the fingerprint: a file one byte longer with the same sampled bytes is another file")
	}
}

// The path is not part of the fingerprint: the same bytes in two places are one file, which is what lets two machines
// agree.
func TestTheFingerprintIsOfTheBytesNotThePath(t *testing.T) {
	a, b := bigFile(t, 70<<20), bigFile(t, 70<<20)
	if mustSample(t, a) != mustSample(t, b) {
		t.Fatal("two files of the same size and bytes at different paths have one fingerprint")
	}
}

func countingFingerprinter(hashes *atomic.Int32) *Fingerprinter {
	return NewFingerprinterWith(func(path string, size int64) (string, error) {
		hashes.Add(1)
		return FingerprintFile(path, size)
	})
}

// A cache hit skips hashing; a file whose size or modification time moved is read again, and the old reading goes.
func TestACacheHitSkipsHashing(t *testing.T) {
	var hashes atomic.Int32
	f := countingFingerprinter(&hashes)
	p := filepath.Join(t.TempDir(), "w.safetensors")
	if err := os.WriteFile(p, bytes.Repeat([]byte{1}, 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	first := f.Await(p, 5000)
	if !IsFingerprint(first) {
		t.Fatalf("Await = %q", first)
	}
	for i := 0; i < 3; i++ {
		if got := f.Await(p, 5000); got != first {
			t.Errorf("a repeat answered %q, want the cached %q", got, first)
		}
		if got := f.Peek(p, 5000); got != first {
			t.Errorf("Peek of a cached file answered %q, want the hash", got)
		}
	}
	if n := hashes.Load(); n != 1 {
		t.Fatalf("hashed %d times, want once", n)
	}

	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}
	f.Await(p, 5000)
	if n := hashes.Load(); n != 2 {
		t.Errorf("a file with a new modification time is read again: %d hashes, want 2", n)
	}
	if err := os.WriteFile(p, bytes.Repeat([]byte{2}, 6000), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := f.Await(p, 6000); got == first || !IsFingerprint(got) {
		t.Errorf("a rewritten file has a new fingerprint: %q", got)
	}
	if n := hashes.Load(); n != 3 {
		t.Errorf("hashed %d times, want 3", n)
	}
	f.mu.Lock()
	n := len(f.m)
	f.mu.Unlock()
	if n != 1 {
		t.Errorf("the cache keeps one reading per path, has %d", n)
	}
}

// Peek never waits for a read: with the hash blocked it answers pending at once, and it answers the hash once the read
// has settled. The node's health reads through Peek.
func TestPeekNeverWaitsForTheHash(t *testing.T) {
	gate := make(chan struct{})
	f := NewFingerprinterWith(func(path string, size int64) (string, error) {
		<-gate
		return FingerprintFile(path, size)
	})
	p := filepath.Join(t.TempDir(), "w.safetensors")
	if err := os.WriteFile(p, bytes.Repeat([]byte{1}, 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { done <- f.Peek(p, 5000) }()
	select {
	case got := <-done:
		if got != FingerprintPending {
			t.Fatalf("Peek = %q, want pending while the read is blocked", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Peek waited for the hash")
	}
	if got := f.Peek(p, 5000); got != FingerprintPending {
		t.Errorf("a second Peek during the read = %q, want pending", got)
	}
	gen := f.Generation()
	close(gate)
	waitFor(t, func() bool { return f.Generation() > gen })
	if got := f.Peek(p, 5000); !IsFingerprint(got) {
		t.Errorf("Peek after the read settled = %q, want the hash", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// One worker: the files of a family are read one at a time, and a Peek and an Await of one file share one read.
func TestReadsAreOneAtATimeAndSharedPerFile(t *testing.T) {
	var running, peak, hashes atomic.Int32
	f := NewFingerprinterWith(func(path string, size int64) (string, error) {
		hashes.Add(1)
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		running.Add(-1)
		return FingerprintFile(path, size)
	})
	dir := t.TempDir()
	var paths []string
	for i := 0; i < 4; i++ {
		p := filepath.Join(dir, string(rune('a'+i))+".safetensors")
		if err := os.WriteFile(p, bytes.Repeat([]byte{byte(i)}, 3000), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	var wg sync.WaitGroup
	for _, p := range paths {
		f.Peek(p, 3000)
		wg.Add(1)
		go func() { defer wg.Done(); f.Await(p, 3000) }()
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Errorf("%d reads ran at once, want one at a time", peak.Load())
	}
	if hashes.Load() != int32(len(paths)) {
		t.Errorf("%d reads for %d files: a Peek and an Await of one file must share the read", hashes.Load(), len(paths))
	}
}

// A read that fails is unreadable, is remembered for fingerprintRetry and is then tried again; an Await tries again at once.
func TestAFailedReadIsUnreadableThenRetried(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	f := NewFingerprinterWith(func(path string, size int64) (string, error) {
		if fail.Load() {
			return "", errors.New("device not ready")
		}
		return FingerprintFile(path, size)
	})
	var tick atomic.Int64 // the clock the cache reads, moved by the test while its worker reads it too
	tick.Store(time.Now().UnixNano())
	f.now = func() time.Time { return time.Unix(0, tick.Load()) }
	advance := func(d time.Duration) { tick.Add(int64(d)) }
	p := filepath.Join(t.TempDir(), "w.safetensors")
	if err := os.WriteFile(p, bytes.Repeat([]byte{1}, 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := f.Peek(p, 5000); got != FingerprintPending {
		t.Fatalf("first Peek = %q", got)
	}
	waitFor(t, func() bool { return f.Peek(p, 5000) == FingerprintUnreadable })
	advance(fingerprintRetry / 2)
	if got := f.Peek(p, 5000); got != FingerprintUnreadable {
		t.Errorf("inside the retry window a failure is remembered: %q", got)
	}
	fail.Store(false)
	advance(fingerprintRetry)
	if got := f.Peek(p, 5000); got != FingerprintPending {
		t.Errorf("after the window the read is tried again: %q", got)
	}
	waitFor(t, func() bool { return IsFingerprint(f.Peek(p, 5000)) })

	fail.Store(true)
	q := filepath.Join(filepath.Dir(p), "q.safetensors")
	if err := os.WriteFile(q, bytes.Repeat([]byte{2}, 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := f.Await(q, 5000); got != FingerprintUnreadable {
		t.Errorf("Await of a file that cannot be read = %q", got)
	}
	fail.Store(false)
	if got := f.Await(q, 5000); !IsFingerprint(got) {
		t.Errorf("Await asks again at once, and the file reads now: %q", got)
	}
}

// A file that is missing, a directory, or not the size the caller saw is unreadable: nothing is hashed.
func TestAMissingOrChangedFileIsUnreadable(t *testing.T) {
	var hashes atomic.Int32
	f := countingFingerprinter(&hashes)
	dir := t.TempDir()
	if got := f.Await(filepath.Join(dir, "gone.safetensors"), 10); got != FingerprintUnreadable {
		t.Errorf("a file that is not there = %q", got)
	}
	if got := f.Await(dir, 0); got != FingerprintUnreadable {
		t.Errorf("a directory = %q", got)
	}
	p := filepath.Join(dir, "w.safetensors")
	if err := os.WriteFile(p, bytes.Repeat([]byte{1}, 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := f.Peek(p, 4999); got != FingerprintUnreadable {
		t.Errorf("a file that is not the size the caller saw = %q", got)
	}
	if hashes.Load() != 0 {
		t.Errorf("nothing was readable, %d hashes ran", hashes.Load())
	}
}

// A file that changes while it is being read (a copy in progress) is a failed read: its half-written content must not be
// cached as the file's fingerprint.
func TestAFileThatChangesWhileItIsReadIsNotCached(t *testing.T) {
	p := filepath.Join(t.TempDir(), "w.safetensors")
	if err := os.WriteFile(p, bytes.Repeat([]byte{1}, 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	var touches atomic.Int64
	f := NewFingerprinterWith(func(path string, size int64) (string, error) {
		fp, err := FingerprintFile(path, size)
		later := time.Now().Add(time.Duration(touches.Add(1)) * time.Hour) // a different time on every read, whatever the clock's resolution
		_ = os.Chtimes(path, later, later)                                 // the writer touched it during the read
		return fp, err
	})
	if got := f.Await(p, 5000); got != FingerprintUnreadable {
		t.Errorf("a file that changed during its read = %q, want unreadable", got)
	}
}

// A hash that panics settles as a failed read: it must not leave every Await of that file waiting for good.
func TestAPanickingHashSettlesAsUnreadable(t *testing.T) {
	f := NewFingerprinterWith(func(string, int64) (string, error) { panic("boom") })
	p := filepath.Join(t.TempDir(), "w.safetensors")
	if err := os.WriteFile(p, bytes.Repeat([]byte{1}, 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { done <- f.Await(p, 5000) }()
	select {
	case got := <-done:
		if got != FingerprintUnreadable {
			t.Errorf("Await = %q, want unreadable", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Await never returned")
	}
}

func TestIsFingerprint(t *testing.T) {
	good := strings.Repeat("ab", 32)
	for s, want := range map[string]bool{good: true, "": false, FingerprintPending: false, FingerprintUnreadable: false, good[:63]: false, good + "0": false, strings.Repeat("zz", 32): false} {
		if IsFingerprint(s) != want {
			t.Errorf("IsFingerprint(%q) = %v, want %v", s, !want, want)
		}
	}
}

// Past its bound the cache drops the files asked for least recently, never the ones in use: a node's few dozen weight
// files stay, and a churn of replaced files does not evict the warm ones.
func TestTheCacheDropsTheLeastRecentlyUsedPastItsBound(t *testing.T) {
	var hashes atomic.Int32
	f := countingFingerprinter(&hashes)
	f.max = 8
	dir := t.TempDir()
	var paths []string
	for i := 0; i < 12; i++ {
		p := filepath.Join(dir, string(rune('a'+i))+".safetensors")
		if err := os.WriteFile(p, bytes.Repeat([]byte{byte(i)}, 2000+i), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	hot := paths[0]
	f.Await(hot, 2000)
	for i := 1; i < 12; i++ {
		f.Await(paths[i], int64(2000+i))
		f.Await(hot, 2000) // asked for again between the others
	}
	before := hashes.Load()
	f.Await(hot, 2000)
	if hashes.Load() != before {
		t.Error("the file in use was dropped by the bound")
	}
	f.mu.Lock()
	n := len(f.m)
	f.mu.Unlock()
	if n > f.max {
		t.Errorf("the cache holds %d entries past its bound of %d", n, f.max)
	}
}
