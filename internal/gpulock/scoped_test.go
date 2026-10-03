package gpulock

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// WaitFreeScoped reads the lease through the caller's own narrowing: a lease that does not
// touch the caller's cards is not one to wait for, and the held rule stays gpulease's.
func TestWaitFreeScopedReadsTheLeasesThatMatterToTheCaller(t *testing.T) {
	lock := writeLease(t, os.Getpid(), nil)

	// Unnarrowed, the live lease is held, and the wait is spent.
	begin := time.Now()
	if info := WaitFree(context.Background(), lock, 80*time.Millisecond, 10*time.Millisecond); !info.Held {
		t.Fatal("a live whole-node lease is held")
	}
	if time.Since(begin) < 80*time.Millisecond {
		t.Fatal("the unnarrowed wait must be spent in full")
	}

	// Narrowed to nothing (a lease on another card), it is free at once.
	begin = time.Now()
	none := func(string) gpulease.Info { return gpulease.Info{} }
	if info := WaitFreeScoped(context.Background(), lock, 5*time.Second, 10*time.Millisecond, none); info.Held {
		t.Fatalf("a lease that does not touch the caller's cards is not held for it: %+v", info)
	}
	if time.Since(begin) > time.Second {
		t.Fatal("a free card must not wait")
	}

	// The reader is handed the lease directory this package resolved.
	var got string
	WaitFreeScoped(context.Background(), lock, 0, time.Millisecond, func(dir string) gpulease.Info { got = dir; return gpulease.Info{} })
	if got != lock {
		t.Fatalf("the reader reads the directory it was given, got %q want %q", got, lock)
	}
}
