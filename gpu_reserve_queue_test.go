package main

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// THE INCIDENT (register D-1xx-3, 2026-10-09). acquireQueued probed the card with a
// bare TryAcquire and only queued once that probe returned ErrHeld, so a card freed
// in the window between a holder's release and the front waiter's next poll tick went
// to whichever fresh `gpu reserve` was launched in it — ahead of everyone registered.
// A recipe that chains reserves back to back won that race every batch: on the
// reference 3-card box it took card 2 as epochs 1320, 1321 and 1322 while a text
// waiter registered for 1h26m sat front of queue the whole time. This reproduces the
// shape with no timing to race: a waiter is registered and the card is FREE, and a
// fresh reserve — with --wait 0 and with a real --wait — must queue behind it instead
// of winning; once the waiter leaves, the same reserve wins at once.
func TestAFreshReserveQueuesBehindARegisteredWaiterOnAFreeCard(t *testing.T) {
	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	refresh, unregister := gpulease.RegisterSeatWaiter(filepath.Join(root, "gpu", "lease"), "transcribe voice_es.wav", nil)
	var once sync.Once
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				refresh()
			}
		}
	}()
	leave := func() {
		once.Do(func() {
			close(stop)
			<-done
			unregister()
		})
	}
	defer leave()

	opts := gpulease.Options{Reason: "krea2 batch 3", TTL: time.Hour}

	// --wait 0: one gated attempt, refused at once, naming the waiter and the flag that queues.
	if lease, err := acquireQueued(m, gpulease.ClassMedia, opts, 0); err == nil {
		_ = lease.Release()
		t.Fatal("a fresh --wait 0 reserve won a free card ahead of a registered waiter — the bare-probe defect is back")
	} else if !errors.Is(err, gpulease.ErrStillQueued) || !strings.Contains(err.Error(), "transcribe voice_es.wav") || !strings.Contains(err.Error(), "--wait") {
		t.Fatalf("the refusal must be ErrStillQueued naming the waiter ahead and the --wait hint, got: %v", err)
	}

	// With a wait: it stands in line for its whole window and still does not win the card.
	start := time.Now()
	if lease, err := acquireQueued(m, gpulease.ClassMedia, opts, 300*time.Millisecond); err == nil {
		_ = lease.Release()
		t.Fatal("a fresh reserve with --wait won a free card ahead of a registered waiter")
	} else if !errors.Is(err, gpulease.ErrStillQueued) {
		t.Fatalf("expected ErrStillQueued after the window, got: %v", err)
	}
	if waited := time.Since(start); waited < 250*time.Millisecond {
		t.Fatalf("gave up after only %s — it must queue for its whole --wait, not fail instantly", waited)
	}

	// Once the waiter leaves, the same reserve is granted at once.
	leave()
	lease, err := acquireQueued(m, gpulease.ClassMedia, opts, 0)
	if err != nil {
		t.Fatalf("after the waiter left, a fresh reserve must win the free card: %v", err)
	}
	_ = lease.Release()
}
