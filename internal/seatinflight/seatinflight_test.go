package seatinflight

import (
	"os"
	"path/filepath"
	"strconv"
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
