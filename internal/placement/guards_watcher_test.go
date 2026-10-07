package placement

import (
	"strings"
	"testing"
)

// The display layer's guards decide once, at the placement; what takes a twin down when the operator
// returns is the watcher in fleet-serve. Admission therefore refuses while nothing is watching, and says
// so, on the display layer's presence guard.

func TestTheDisplayLayerDoesNotOpenWhileTheWatcherIsNotAlive(t *testing.T) {
	l, s := displayLayerAndSeat(t)
	f := admitting()
	if ok, reason, guard := LayerAdmissible(l, s, f.live(), nil); !ok {
		t.Fatalf("setup: a live watcher and an away operator admit: %q (%s)", reason, guard)
	}
	f.watcherDown = "the last heartbeat is 4m0s old, so fleet-serve has stopped checking or is not running"
	ok, reason, guard := LayerAdmissible(l, s, f.live(), nil)
	if ok || guard != "presence" {
		t.Fatalf("a dead watcher refuses on the presence guard: ok=%v guard=%q reason=%q", ok, guard, reason)
	}
	if !strings.Contains(reason, "watcher") || !strings.Contains(reason, "4m0s old") {
		t.Errorf("the refusal must say the watcher is why, and why it is not alive: %q", reason)
	}
}

// A watcher reader that is absent is no evidence of a watcher: fail closed, like every reader.
func TestAnAbsentWatcherReaderRefusesTheDisplayLayer(t *testing.T) {
	l, s := displayLayerAndSeat(t)
	live := admitting().live()
	live.WatcherAlive = nil
	ok, reason, _ := LayerAdmissible(l, s, live, nil)
	if ok || !strings.Contains(reason, "watcher") {
		t.Fatalf("no reader, no admission: ok=%v %q", ok, reason)
	}
}

// The operator at the desk is refused for that, first: the watcher is a second condition, not a
// replacement for presence.
func TestPresenceIsAskedBeforeTheWatcher(t *testing.T) {
	l, s := displayLayerAndSeat(t)
	f := admitting()
	f.pres = &Presence{Mode: "present", Known: true}
	f.watcherDown = "no heartbeat"
	_, reason, guard := LayerAdmissible(l, s, f.live(), nil)
	if guard != "presence" || !strings.Contains(reason, "operator_presence is present") || strings.Contains(reason, "watcher") {
		t.Fatalf("the operator's presence refuses first: %q (%s)", reason, guard)
	}
}

// Only the layer named display is the watcher's. A layer of another name that happens to use the
// presence guard is judged by presence alone.
func TestAnotherLayersPresenceGuardDoesNotAskForTheWatcher(t *testing.T) {
	l, s := displayLayerAndSeat(t)
	l.Name = "overflow"
	f := admitting()
	f.watcherDown = "no heartbeat"
	if ok, reason, _ := LayerAdmissible(l, s, f.live(), nil); !ok {
		t.Fatalf("the watcher watches the display layer only: %q", reason)
	}
}

// A remote row carries the node's own verdict for a guard whose reader is absent; the node asked its
// own watcher, so the delegator neither re-asks nor refuses for want of a local reader.
func TestARemoteRowsVerdictStandsForThePresenceAndWatcherGuardsTogether(t *testing.T) {
	l, s := displayLayerAndSeat(t)
	l.Guards = []string{"presence"}
	verdict := true
	if ok, reason, _ := LayerAdmissible(l, s, Live{}, &verdict); !ok {
		t.Fatalf("the node's admitting verdict stands: %q", reason)
	}
	verdict = false
	if ok, _, _ := LayerAdmissible(l, s, Live{}, &verdict); ok {
		t.Fatal("the node's refusing verdict stands")
	}
}

// The watcher calls ResidentVerdict on every check. A stale or absent heartbeat must not make it
// unload twins because of its own bookkeeping: that guard never reads the watcher.
func TestResidentVerdictNeverReadsTheWatcher(t *testing.T) {
	l, _ := displayLayerAndSeat(t)
	f := admitting()
	f.watcherDown = "no heartbeat"
	live := f.live()
	live.WatcherAlive = func() (bool, string) {
		t.Error("ResidentVerdict must not ask whether the watcher is alive")
		return false, "x"
	}
	if ok, reasons := ResidentVerdict(l, live); !ok {
		t.Fatalf("an away operator and a kept floor keep the twin: %v", reasons)
	}
}
