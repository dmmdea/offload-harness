package gpulease

// An operator's release must not be undone by a restamp that read the record first. A whole-node restamp reads the record,
// writes a replacement beside it and renames the replacement over it, all under the epoch lock; the whole-node release
// (`gpu release`) removed the claim WITHOUT the lock, so a release that landed between the restamp's read and its rename was
// followed by the rename, which put the record back: the lease the operator had just ended was held again, and the wrapper that
// held it saw nothing wrong on its next heartbeat. The wrapper's own tree record (a restamp right after its command starts)
// made the window reachable: a `gpu release` a few milliseconds after the command started could be reversed.

import (
	"testing"
	"time"
)

func TestAReleaseCannotBeUndoneByAWholeNodeRestampThatReadTheRecordFirst(t *testing.T) {
	m, _ := newTestManager(t)
	l, err := m.TryAcquire(ClassText, Options{Reason: "wrapper", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	restampWholeHook = func() {
		restampWholeHook = nil
		// The operator's release arrives now, after the restamp read the record and before it writes it back.
		go func() { _, _ = m.ReleaseByEpoch(0); close(released) }()
		select {
		case <-released: // an unlocked release finishes at once: the rename below will put the record back
		case <-time.After(400 * time.Millisecond): // a release that waits for the epoch lock is still waiting
		}
	}
	t.Cleanup(func() { restampWholeHook = nil })

	_ = l.Restamp(func(*Meta) {})
	select {
	case <-released:
	case <-time.After(10 * time.Second):
		t.Fatal("the release never finished")
	}
	if info := m.Inspect(); info.Held {
		t.Fatalf("the operator's release was undone by the restamp that was in flight: %+v", info)
	}
}
