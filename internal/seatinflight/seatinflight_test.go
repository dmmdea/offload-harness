package seatinflight

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestBeginCountEnd(t *testing.T) {
	d := t.TempDir()
	ArmAt(d)
	t.Cleanup(Disarm)
	alive := func(int) bool { return true }

	endA := Begin("Agent-Pool")
	endB := Begin("agent-pool")
	endC := Begin("qwen3.5-9b-agent")
	got := Count(d, time.Now(), alive)
	if got["agent-pool"] != 2 || got["qwen3.5-9b-agent"] != 1 {
		t.Fatalf("Count = %v, want agent-pool 2 (case folded), qwen3.5-9b-agent 1", got)
	}
	endA()
	endA() // idempotent
	endC()
	if got := Count(d, time.Now(), alive); got["agent-pool"] != 1 || got["qwen3.5-9b-agent"] != 0 {
		t.Fatalf("after two ends Count = %v", got)
	}
	endB()
	if entries, _ := os.ReadDir(d); len(entries) != 0 {
		t.Fatalf("%d marker(s) left after every end", len(entries))
	}
}

// TestCountDropsDeadAndStaleMarkers: a marker a crashed process left behind
// is removed, and one older than MaxAge is not believed even when its pid
// reads alive (pids recycle).
func TestCountDropsDeadAndStaleMarkers(t *testing.T) {
	d := t.TempDir()
	now := time.Now()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(d, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("dead.json", `{"model":"m","pid":111,"started_ms":`+itoa(now.UnixMilli())+`}`)
	write("stale.json", `{"model":"m","pid":222,"started_ms":`+itoa(now.Add(-2*MaxAge).UnixMilli())+`}`)
	write("live.json", `{"model":"m","pid":222,"started_ms":`+itoa(now.UnixMilli())+`}`)
	write("partial.json", `{"model":`)
	write("other.txt", `ignored`)
	got := Count(d, now, func(pid int) bool { return pid == 222 })
	if got["m"] != 1 {
		t.Fatalf("Count = %v, want only the live marker", got)
	}
	for _, gone := range []string{"dead.json", "stale.json"} {
		if _, err := os.Stat(filepath.Join(d, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s was not removed", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(d, "partial.json")); err != nil {
		t.Fatal("a marker being written was removed")
	}
}

func TestUnarmedIsNoop(t *testing.T) {
	Disarm()
	end := Begin("m")
	end()
	if got := Count("", time.Now(), nil); len(got) != 0 {
		t.Fatalf("Count on no dir = %v", got)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// TestArmIsOffWhenTheFeatureIs: modelaffinity.Admit is the gate every text
// call passes, so a box that does not run the seat watcher must not pay a
// file create and remove per request.
func TestArmIsOffWhenTheFeatureIs(t *testing.T) {
	t.Cleanup(Disarm)
	if err := Arm(t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	if d := Dir(); d != "" {
		t.Fatalf("Dir = %q with the feature off, want unarmed", d)
	}
	end := Begin("agent-pool")
	end()
	if err := Arm(t.TempDir(), true); err != nil {
		t.Fatal(err)
	}
	if Dir() == "" {
		t.Fatal("Arm(enabled) left the register unarmed")
	}
}

// TestRemoveRetriesAfterASharingViolation: on Windows os.ReadFile opens a
// marker without FILE_SHARE_DELETE, so the watcher's own 2 s read collides
// with a request ending and DeleteFile fails. Dropping that error hid direct
// traffic for an hour (review finding, 2026-09-22).
func TestRemoveRetriesAfterASharingViolation(t *testing.T) {
	d := t.TempDir()
	ArmAt(d)
	t.Cleanup(Disarm)
	var mu sync.Mutex
	fails := 2
	real := removeFn
	removeFn = func(path string) error {
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 {
			fails--
			return errors.New("ERROR_SHARING_VIOLATION")
		}
		return real(path)
	}
	t.Cleanup(func() { removeFn = real })

	end := Begin("agent-pool")
	end()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(Count(d, time.Now(), func(int) bool { return true })) == 0 {
			return
		}
		time.Sleep(removeBackoff)
	}
	t.Fatal("a marker whose first removals failed was never removed")
}
