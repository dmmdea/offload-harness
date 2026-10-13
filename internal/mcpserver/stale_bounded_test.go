package mcpserver

// stale_bounded_test.go: the comparison of the installed file's bytes does not hold an answer (0.184.0 integration review,
// silent failure hunt). Settling a moved stamp by the bytes puts a full read of the installed executable (its hash and its
// build marker, two passes over about 60 MB) on the answer path of agent_delegate and offload_research, after every result
// exists. A read that stalls (an antivirus scan of the freshly renamed file, a slow mount) held the finished results past the
// call's deadline, and a read that failed was retried by every later answer. The wait is bounded: past it the answer carries
// the moved stamp and says the read did not finish, the read goes on in the background, and the next call uses its result.

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
)

// stalledHashes makes every read of the installed image wait for release (and counts the reads), for at most max: a
// stalled read that would never return must not hang the test run when a fix is missing. The cleanup lets the read go and
// waits for it, before the seam is restored, so no read outlives its test.
func stalledHashes(t *testing.T, max time.Duration) (release func(), reads *atomic.Int64) {
	t.Helper()
	real := hashInstalledImage
	realWait := installedReadWait
	gate := make(chan struct{})
	var once sync.Once
	var running sync.WaitGroup
	reads = new(atomic.Int64)
	hashInstalledImage = func(p string) (string, error) {
		running.Add(1)
		defer running.Done()
		reads.Add(1)
		select {
		case <-gate:
		case <-time.After(max):
		}
		return real(p)
	}
	installedReadWait = 100 * time.Millisecond
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(func() { hashInstalledImage, installedReadWait = real, realWait })
	t.Cleanup(func() { release(); running.Wait() })
	return release, reads
}

// TestAStalledReadOfTheInstalledImageDoesNotHoldTheAnswer: the stale_binary note comes back inside the wait, carries the moved
// stamp, and says the bytes were not compared and why.
func TestAStalledReadOfTheInstalledImageDoesNotHoldTheAnswer(t *testing.T) {
	s, _, path := serverOnInstalledImage(t, "0.183.0")
	swapImage(t, path, imageBody("0.183.0"))
	stalledHashes(t, 1500*time.Millisecond)

	began := time.Now()
	note := s.staleBinaryNote(buildinfo.Version)
	if took := time.Since(began); took > 1*time.Second {
		t.Fatalf("the answer waited %s for a read of the installed image that was stalled (the wait is %s)", took, installedReadWait)
	}
	if !strings.Contains(note, "changed on disk") || !strings.Contains(note, "could not be compared") || !strings.Contains(note, "did not finish") {
		t.Fatalf("stale_binary = %q, want the moved stamp's note that says the read of the bytes did not finish", note)
	}
}

// TestAStalledReadIsOneReadNoMatterHowManyAnswersWaitOnIt: the answers that come while a read is in flight do not start another,
// and the read's result, once it lands, settles the next answer without a second pass over the file.
func TestAStalledReadIsOneReadNoMatterHowManyAnswersWaitOnIt(t *testing.T) {
	s, _, path := serverOnInstalledImage(t, "0.183.0")
	swapImage(t, path, imageBody("0.183.0"))
	release, reads := stalledHashes(t, 1500*time.Millisecond)

	for i := 0; i < 3; i++ {
		if note := s.staleBinaryNote(buildinfo.Version); !strings.Contains(note, "did not finish") {
			t.Fatalf("call %d while the read is stalled: stale_binary = %q, want the note that the read did not finish", i, note)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("%d answers during one stalled read started %d reads of the installed image, want 1", 3, n)
	}

	release()
	deadline := time.Now().Add(5 * time.Second)
	for {
		note := s.staleBinaryNote(buildinfo.Version)
		if note == "" {
			break // equal bytes: not stale, settled by the read that finished in the background
		}
		if time.Now().After(deadline) {
			t.Fatalf("the finished read never settled the answer: stale_binary = %q", note)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("the installed image was read %d times, want the one read the answers shared", n)
	}
}

// TestAStalledReadAlsoDoesNotHoldTheStatusDoor: offload_status's process block shares the check, so it shares the bound: no
// verdict (the key is absent) and a note, inside the wait.
func TestAStalledReadAlsoDoesNotHoldTheStatusDoor(t *testing.T) {
	s, _, path := serverOnInstalledImage(t, "0.183.0")
	swapImage(t, path, imageBody("0.183.0"))
	stalledHashes(t, 1500*time.Millisecond)

	began := time.Now()
	staleImage, _ := bothSignals(t, s)
	if took := time.Since(began); took > 1*time.Second {
		t.Fatalf("offload_status waited %s for a read of the installed image that was stalled", took)
	}
	if staleImage != nil {
		t.Fatalf("process.stale_image = %v while the read had not finished, want the key absent", staleImage)
	}
}
