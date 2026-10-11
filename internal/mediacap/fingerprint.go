package mediacap

// The content fingerprint of a weight file (F59, ADR 0082 Amendment 2).
//
// WHY. A family's recipe used to identify a weight file by its name and byte size. Two files that share both and differ
// in content (a same-named re-release with the same tensor layout, a full-length copy that was altered, one node's file
// replaced in place) digested alike, so a delegator could send a job to a node that renders other pixels. The recipe now
// carries a fingerprint of each present file, and a file whose fingerprint is not known yet is a NAMED MISS, never a
// match by size.
//
// WHAT IT IS. A SAMPLED hash: sha256 over the scheme tag, the file size and fingerprintBlocks blocks of fingerprintBlock
// bytes at evenly spaced offsets (the first and the last block always among them); a file no larger than the sample is
// hashed whole. fingerprintBlock and fingerprintBlocks are CHOSEN, not measured against a requirement: 64 MiB at most per
// file, so a family of four files reads 256 MiB at most and there is no persisted cache to keep honest. After a restart a
// family is pending for the seconds its files take to read, not the minutes a whole-file hash takes. MEASURED on one
// workstation (2026-10-10, its local model store, page cache not controlled, so read these as warm-cache figures: a cold
// disk or a network share can be slower, and that was not measured): 0.17 to 0.87 s per file for files of 0.36 to 42 GB. For
// comparison a FULL sha256 ran at 389 MB/s on one core there (3.8 s for a 1.47 GB file), which is about 50 s for a 20 GB
// checkpoint and about 110 s for a 42 GB one (extrapolated from that rate, not run). The whole-file hash was rejected on
// that time and on the window it opens after every release or model change, in which a node cannot be matched. That the
// window matters is a JUDGEMENT, not a measurement. RAM was not the reason: a streaming hash holds one 1 MiB buffer.
//
// WHAT IT SEES. Any wholesale difference in weights: a retrain, a fine-tune that touches the weights throughout, a
// re-quantisation of the same size, a different build with the same layout (two real 42 GB transformer builds that share a
// size differed in 58 of the 64 sample blocks), and a copy whose tail is missing or zero-filled. WHAT IT DOES NOT SEE:
//   - A contiguous region between two blocks. The blocks are evenly spaced, so the stretch between two of them is about a
//     sixty-third of the file less one block: roughly 0.3 GB of a 20 GB file and 0.65 GB of a 42 GB one
//     (TestTheUnsampledStretchBetweenTwoBlocksIsAboutASixtyThirdOfTheFile pins those widths). Every byte of it can differ
//     and the fingerprint is the same: a layer-wise merge that changes a few transformer blocks, a VAE or text encoder baked
//     into a checkpoint. An operator who swaps such a file compares whole-file hashes (sha256sum) on the nodes.
//   - A file replaced by one with the same size AND the same modification time (the cache key; a copy that preserves times
//     does it).
//
// A full hash would see the first and not the second, and costs the minutes above.
//
// WHEN IT IS COMPUTED. Never on the path of /fleet/health: Peek answers from the cache and, on a miss, starts the hash in
// the background (this process reads one weight file at a time; another process, such as ComfyUI loading a checkpoint, is
// not coordinated with) and answers FingerprintPending. AwaitContext (and Await, which is it with no caller context) is for
// a caller that must have the answer (the delegator's own recipe, the node's 412 re-check): it waits for the same single
// worker, but for at most awaitMax or until its caller is gone, and then answers FingerprintPending, so a request names a
// file that is not hashed yet and does not hang on it. A read that does not finish in readMax is GIVEN UP ON: its entry
// settles unreadable and the worker is free for the other files. The read itself cannot be cancelled, so it runs on until
// the disk lets it return; that file is not read again meanwhile, so a hung disk costs one goroutine and one handle per stuck
// file and not a pile. It is logged once. Until readMax passes, a stalled read holds the worker and every other file stays
// pending. The cache is in memory, keyed by path, size and modification time, and shared by the process
// (SharedFingerprints), so the node's health, its admission check and a delegator in the same process hash a file once. A
// restart hashes again.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// The two states a fingerprint takes besides a hash. A recipe file with either is never a match.
const (
	// FingerprintPending: the file is being (or is about to be) hashed on this machine.
	FingerprintPending = "pending"
	// FingerprintUnreadable: the file is there but its content could not be read, or it changed while it was read.
	FingerprintUnreadable = "unreadable"
)

const (
	// fingerprintScheme is hashed first, so a change of what is sampled can never collide with an older fingerprint.
	fingerprintScheme = "fp1"
	// fingerprintBlock and fingerprintBlocks are CHOSEN (see the header): 64 x 1 MiB.
	fingerprintBlock  = 1 << 20
	fingerprintBlocks = 64
	// fingerprintRetry is how long a failed read is remembered before the next Peek tries again (CHOSEN; health's own
	// memo is 60 s, so a file that was busy is looked at again on the next refresh after this).
	fingerprintRetry = 30 * time.Second
	// fingerprintAwaitMax is how long Await waits before it answers pending (CHOSEN). A cold family of four files read in a
	// few seconds on the one workstation measured, so 20 s leaves several times that for a slower disk, and still ends well
	// inside the minutes a dispatch or a render is allowed.
	fingerprintAwaitMax = 20 * time.Second
	// fingerprintReadMax is how long one read may run before it is given up on (CHOSEN): 64 MiB at 1 MiB/s, a disk far
	// slower than anything the measured files came from, so a read this long is a stalled one and not a slow one.
	fingerprintReadMax = 60 * time.Second
	// fingerprintMaxEntries bounds the cache (CHOSEN): a node holds a few dozen weight files, so more only ever means
	// files that were replaced or removed. Past it the older half of the settled entries goes, least recently used first.
	fingerprintMaxEntries = 1024
)

// IsFingerprint reports whether s is a real fingerprint (a sha256 in hex) and not a state or garbage.
func IsFingerprint(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// FingerprintFunc answers the fingerprint of the weight file at path whose size is size: a hash, or FingerprintPending
// or FingerprintUnreadable. It is how ImageRecipeWith reads content, so a caller picks the blocking behaviour and a
// test supplies its own.
type FingerprintFunc func(path string, size int64) string

// fingerprintOffsets are the starts of the sampled blocks of a file of size bytes (larger than the sample): evenly spaced,
// the first at 0 and the last ending at the end of the file.
func fingerprintOffsets(size int64) []int64 {
	offs := make([]int64, fingerprintBlocks)
	for i := range offs {
		offs[i] = int64(i) * (size - fingerprintBlock) / (fingerprintBlocks - 1)
	}
	return offs
}

// FingerprintFile computes the fingerprint of the file at path, which must be size bytes long. It reads up to 64 MiB
// and is for a cache that runs it off the request path (Fingerprinter) and for a test.
func FingerprintFile(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if st.IsDir() || st.Size() != size {
		return "", fmt.Errorf("%s is not %d bytes", path, size)
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%d\n", fingerprintScheme, size)
	buf := make([]byte, fingerprintBlock)
	if size <= int64(fingerprintBlock)*fingerprintBlocks {
		if n, err := io.CopyBuffer(h, io.LimitReader(f, size), buf); err != nil || n != size {
			return "", fmt.Errorf("short read of %s: %d of %d bytes: %v", path, n, size, err)
		}
	} else {
		for _, off := range fingerprintOffsets(size) {
			if n, err := f.ReadAt(buf, off); n != len(buf) {
				return "", fmt.Errorf("short read of %s at %d: %d bytes: %v", path, off, n, err)
			}
			h.Write(buf)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fpKey is what a cached fingerprint is valid for: the file as it was stat-ed.
type fpKey struct {
	path  string
	size  int64
	mtime int64
}

// fpEntry is one file's fingerprint in flight or settled. done is closed when it settles.
type fpEntry struct {
	done chan struct{}
	fp   string // a hash; "" when the read failed
	at   time.Time
	used uint64 // the cache's tick at the last time it was asked for (guarded by Fingerprinter.mu)
}

func (e *fpEntry) settled() bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

// Fingerprinter caches weight-file fingerprints and hashes them one at a time.
type Fingerprinter struct {
	hash func(path string, size int64) (string, error)
	now  func() time.Time
	// sem is the one worker: this process reads one weight file at a time, whoever asked.
	sem chan struct{}
	gen atomic.Uint64
	max int
	// awaitMax and readMax are fingerprintAwaitMax and fingerprintReadMax; a test shortens them.
	awaitMax, readMax time.Duration

	mu    sync.Mutex
	m     map[fpKey]*fpEntry
	tick  uint64
	stuck map[string]bool // paths with a read given up on that has not returned yet (guarded by mu)
}

// NewFingerprinter builds a Fingerprinter that reads the real files.
func NewFingerprinter() *Fingerprinter { return NewFingerprinterWith(FingerprintFile) }

// NewFingerprinterWith is NewFingerprinter over a different hash, for a test that must count, delay or fail the read.
func NewFingerprinterWith(hash func(path string, size int64) (string, error)) *Fingerprinter {
	return &Fingerprinter{hash: hash, now: time.Now, sem: make(chan struct{}, 1), max: fingerprintMaxEntries,
		awaitMax: fingerprintAwaitMax, readMax: fingerprintReadMax, m: map[fpKey]*fpEntry{}, stuck: map[string]bool{}}
}

var shared = NewFingerprinter()

// SharedFingerprints is the process-wide Fingerprinter. A fingerprint is a property of the file, not of a server or a
// config, so one cache serves the node's health, its 412 re-check and a delegator running in the same process.
func SharedFingerprints() *Fingerprinter { return shared }

// Generation counts the reads that have settled. A caller that memoises what Peek told it can tell that a pending
// answer may have become a hash.
func (f *Fingerprinter) Generation() uint64 { return f.gen.Load() }

// Peek answers from the cache and never reads the file's content on the caller's goroutine. A file not yet hashed
// starts a background read and answers FingerprintPending; the hash is there on a later call. A read that failed is
// remembered for fingerprintRetry (FingerprintUnreadable), then tried again.
func (f *Fingerprinter) Peek(path string, size int64) string {
	e, ok := f.entry(path, size, false)
	if !ok {
		return FingerprintUnreadable
	}
	if !e.settled() {
		return FingerprintPending
	}
	if e.fp != "" {
		return e.fp
	}
	if f.now().Sub(e.at) < fingerprintRetry {
		return FingerprintUnreadable
	}
	f.entry(path, size, true)
	return FingerprintPending
}

// Await is AwaitContext for a caller with no context of its own: it still ends at awaitMax.
func (f *Fingerprinter) Await(path string, size int64) string {
	return f.AwaitContext(context.Background(), path, size)
}

// AwaitContext answers with the file's fingerprint, reading it when it is not cached (behind any read already running). It
// answers FingerprintUnreadable for a file it cannot read, and FingerprintPending when the read has not settled by the
// time awaitMax has passed or ctx is done: the read goes on and a later call finds its result. One deadline covers the
// whole call, the retry-at-once below included.
func (f *Fingerprinter) AwaitContext(ctx context.Context, path string, size int64) string {
	e, ok := f.entry(path, size, false)
	if !ok {
		return FingerprintUnreadable
	}
	timer := time.NewTimer(f.awaitMax)
	defer timer.Stop()
	wait := func(e *fpEntry) bool {
		if e.settled() {
			return true // a cached answer is given even to a caller that is gone: select would pick among ready cases at random
		}
		select {
		case <-e.done:
			return true
		case <-ctx.Done():
		case <-timer.C:
			log.Printf("mediacap: gave up waiting %s for the fingerprint of %s: its read is queued or stalled (it goes on; ask again)", f.awaitMax, filepath.Base(path))
		}
		return false
	}
	if !wait(e) {
		return FingerprintPending
	}
	if e.fp == "" {
		// A failed read is asked again at once: this caller needs an answer, and a failure is cheap.
		if e, ok = f.entry(path, size, true); !ok {
			return FingerprintUnreadable
		}
		if !wait(e) {
			return FingerprintPending
		}
		if e.fp == "" {
			return FingerprintUnreadable
		}
	}
	return e.fp
}

// entry returns the cache entry for the file as it is now, starting its read when there is none (or, with retry, when
// the one there failed). ok is false when the file cannot be stat-ed or is not size bytes long, which no read can fix.
func (f *Fingerprinter) entry(path string, size int64, retry bool) (e *fpEntry, ok bool) {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || st.Size() != size {
		return nil, false
	}
	k := fpKey{path: path, size: size, mtime: st.ModTime().UnixNano()}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tick++
	if e = f.m[k]; e != nil && !(retry && e.settled() && e.fp == "") {
		e.used = f.tick
		return e, true
	}
	// A new reading of this path supersedes the ones taken of its earlier size or time.
	for old := range f.m {
		if old.path == path && old != k {
			delete(f.m, old)
		}
	}
	if len(f.m) >= f.max {
		f.evictLocked()
	}
	e = &fpEntry{done: make(chan struct{}), used: f.tick}
	f.m[k] = e
	go f.read(k, e)
	return e, true
}

// evictLocked drops the older half of the settled entries, least recently used first. A read in flight is never dropped.
func (f *Fingerprinter) evictLocked() {
	type aged struct {
		k    fpKey
		used uint64
	}
	var settled []aged
	for k, e := range f.m {
		if e.settled() {
			settled = append(settled, aged{k, e.used})
		}
	}
	sort.Slice(settled, func(i, j int) bool { return settled[i].used < settled[j].used })
	for _, a := range settled[:(len(settled)+1)/2] {
		delete(f.m, a.k)
	}
}

// read hashes one file behind the single worker and settles its entry. The result is kept only if the file is still the
// one the key describes: a file that grew or was replaced while it was read (a copy in progress) is a failed read.
func (f *Fingerprinter) read(k fpKey, e *fpEntry) {
	f.sem <- struct{}{}
	fp, err := f.hashBounded(k)
	<-f.sem
	if err == nil {
		if st, serr := os.Stat(k.path); serr != nil || st.Size() != k.size || st.ModTime().UnixNano() != k.mtime {
			err = fmt.Errorf("%s changed while it was read", k.path)
		}
	}
	if err == nil {
		e.fp = fp
	}
	e.at = f.now()
	close(e.done) // e.fp and e.at are written before this, and read only after it
	f.gen.Add(1)
}

// hashBounded runs the hash for at most readMax. A read that is still running then is GIVEN UP ON, not cancelled (a read
// blocked in the kernel cannot be): the caller settles the entry as failed and frees the worker, the path is marked stuck so
// that no second read of it starts while the first is out, and the goroutine clears the mark when its read finally returns.
func (f *Fingerprinter) hashBounded(k fpKey) (string, error) {
	f.mu.Lock()
	stuck := f.stuck[k.path]
	f.mu.Unlock()
	if stuck {
		return "", fmt.Errorf("an earlier read of %s is still stuck", k.path)
	}
	type result struct {
		fp  string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		fp, err := f.hashGuarded(k)
		ch <- result{fp, err} // before the lock below: see the timeout case
		f.mu.Lock()
		delete(f.stuck, k.path)
		f.mu.Unlock()
	}()
	timer := time.NewTimer(f.readMax)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.fp, r.err
	case <-timer.C:
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case r := <-ch: // it returned in the instant the timer fired
		return r.fp, r.err
	default:
	}
	// Marked under the lock the goroutine takes to clear it, after it has sent: it cannot clear the mark before it is set.
	f.stuck[k.path] = true
	log.Printf("mediacap: the read of %s has not finished in %s and was given up on; its fingerprint is unreadable until a read of it returns, and the other weight files are read meanwhile", filepath.Base(k.path), f.readMax)
	return "", fmt.Errorf("the read of %s did not finish in %s", k.path, f.readMax)
}

// hashGuarded is the hash, with a panic in it turned into a failed read: a worker that never settled its entry would
// leave every Await on that file waiting for good.
func (f *Fingerprinter) hashGuarded(k fpKey) (fp string, err error) {
	defer func() {
		if r := recover(); r != nil {
			fp, err = "", fmt.Errorf("fingerprint of %s: %v", k.path, r)
		}
	}()
	return f.hash(k.path, k.size)
}
