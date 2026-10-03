package modelaffinity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// A text admission that waits out its budget behind a lease whose owner is long gone is
// told so: which lease, what is wrong with it, what is running and the command that frees
// it. "a media job holds the GPU" answered none of that, and the incident it came from
// held a card for hours behind a label that read healthy.
func TestLeaseErrorNamesAnOrphanedHolderAndTheTakeoverCommand(t *testing.T) {
	m := armLease(t)
	// Any owner that no process has: a tracked session with a pid that is not running.
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", TTL: time.Hour, Command: "python film.py",
		Owner: gpulease.Owner{Session: "sess-gone", PID: 2000000000, Tracked: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	gpulease.SetDefaultOrphanGrace(time.Nanosecond) // the owner has been gone "long enough" the moment it is first seen
	t.Cleanup(func() { gpulease.SetDefaultOrphanGrace(0) })
	m.ObserveOwner(m.Inspect()) // the first observer stamps the moment
	time.Sleep(5 * time.Millisecond)

	err = awaitCard(context.Background(), "http://127.0.0.1:1", "agent-pool", time.Now().Add(-time.Second))
	var le *LeaseError
	if !errors.As(err, &le) {
		t.Fatalf("want *LeaseError, got %v", err)
	}
	msg := le.Error()
	for _, want := range []string{"gpu-lease timeout", "held-orphaned", "session sess-gone", "film.py", "gpu takeover --epoch"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal lacks %q: %s", want, msg)
		}
	}
	if !strings.Contains(msg, "timeout") {
		t.Fatal("the pinned classifier word must stay")
	}

	// The same sentence rides the refusal a CANCELLED caller gets (its own ctx ended the
	// wait before the budget did): both exits of the wait loop carry it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = awaitCard(ctx, "http://127.0.0.1:1", "agent-pool", time.Now().Add(time.Minute))
	if !errors.As(err, &le) || !errors.Is(err, context.Canceled) || !strings.Contains(le.Error(), "held-orphaned") {
		t.Fatalf("a cancelled wait must carry the standing too: %v", err)
	}
}

// A healthy holder's refusal is exactly what it was: no advice appended.
func TestLeaseErrorOfAHealthyHolderIsUnchanged(t *testing.T) {
	m := armLease(t)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	err = awaitCard(context.Background(), "http://127.0.0.1:1", "agent-pool", time.Now().Add(-time.Second))
	var le *LeaseError
	if !errors.As(err, &le) {
		t.Fatalf("want *LeaseError, got %v", err)
	}
	if strings.Contains(le.Error(), "takeover") || strings.Contains(le.Error(), "held-") {
		t.Fatalf("a healthy holder is not escalated: %s", le.Error())
	}
}

// The text gate is a read-only inspector: its refusal says what the lease's recorded
// standing is, and never stamps the orphan marker (a write, and the epoch lock) from a path
// that polls once a second and may run without write access to the lease directory.
func TestTextGateRefusalNeverStampsTheOrphanMarker(t *testing.T) {
	m := armLease(t)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "film", TTL: time.Hour, Command: "python film.py",
		Owner: gpulease.Owner{Session: "sess-gone", PID: 2000000000, Tracked: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	gpulease.SetDefaultOrphanGrace(time.Nanosecond)
	t.Cleanup(func() { gpulease.SetDefaultOrphanGrace(0) })

	err = awaitCard(context.Background(), "http://127.0.0.1:1", "agent-pool", time.Now().Add(-time.Second))
	var le *LeaseError
	if !errors.As(err, &le) {
		t.Fatalf("want *LeaseError, got %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(m.Root(), "gpu", "lease"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "orphan.") {
			t.Fatalf("the text gate's refusal stamped %s: it is a read-only inspector", e.Name())
		}
	}
	// Once a status surface has recorded the moment, the gate reads it.
	_ = m.Standing(m.Inspect(), 0)
	time.Sleep(5 * time.Millisecond)
	err = awaitCard(context.Background(), "http://127.0.0.1:1", "agent-pool", time.Now().Add(-time.Second))
	if !errors.As(err, &le) || !strings.Contains(le.Error(), "held-orphaned") {
		t.Fatalf("the gate must read the recorded moment: %v", err)
	}
}
