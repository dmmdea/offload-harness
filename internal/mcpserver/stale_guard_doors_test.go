package mcpserver

// stale_guard_doors_test.go: the delegating doors say when the stale-binary guard is not watching (0.184.0 integration
// review, silent failure hunt). The guard exists for agent_delegate and offload_research, the doors whose behaviour a release
// changes, and offload_status was the only place that said "not watching" (stale_binary_guard). A server whose guard is blind
// answered the delegating doors with no note, which from outside reads as a fresh server: the failure the guard was built to
// stop, in the one case it cannot see.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// guardDoor runs one delegating door and returns its decoded answer.
type guardDoor struct {
	name string
	call func(t *testing.T, s *Server) map[string]any
}

func guardDoors() []guardDoor {
	return []guardDoor{
		{"agent_delegate", func(t *testing.T, s *Server) map[string]any {
			t.Helper()
			res, err := s.handleAgentDelegate(context.Background(), callReq(staleDelegateArgs))
			if err != nil {
				t.Fatal(err)
			}
			return decodeResult(t, res)
		}},
		{"offload_research", func(t *testing.T, s *Server) map[string]any {
			t.Helper()
			res, err := s.handleResearch(context.Background(), callReq(staleResearchArgs))
			if err != nil {
				t.Fatal(err)
			}
			return decodeResult(t, res)
		}},
	}
}

func TestAnInactiveGuardSaysSoOnTheDelegatingDoors(t *testing.T) {
	for _, d := range guardDoors() {
		t.Run(d.name, func(t *testing.T) {
			captureStaleLog(t)
			s := staleResearchServer(t)
			s.exe = newExeWatchFrom(exeStamp{}, errors.New("no executable path"), nil)
			m := d.call(t, s)
			guard, _ := m["stale_binary_guard"].(string)
			if !strings.HasPrefix(guard, "inactive:") || !strings.Contains(guard, "no executable path") {
				t.Fatalf("%s: stale_binary_guard = %q (keys %v), want it to say the guard is inactive and why", d.name, guard, staleKeys(m))
			}
			if m["stale_binary"] != nil {
				t.Fatalf("%s: an inactive guard accused: %v", d.name, m["stale_binary"])
			}
		})
	}
}

// TestAGuardThatCannotReadItsStampSaysSoOnTheDelegatingDoorsWhileItLasts: the stat that fails later is blind for as long as it
// fails, and the same call that found it blind says so (the state is read after the note's own read of the stamp).
func TestAGuardThatCannotReadItsStampSaysSoOnTheDelegatingDoorsWhileItLasts(t *testing.T) {
	for _, d := range guardDoors() {
		t.Run(d.name, func(t *testing.T) {
			captureStaleLog(t)
			exe := &staleMovableExe{cur: staleExeThen}
			s := staleResearchServer(t)
			s.exe = newExeWatch(exe.read)
			exe.set(exeStamp{}, errors.New("permission denied"))
			m := d.call(t, s)
			guard, _ := m["stale_binary_guard"].(string)
			if !strings.HasPrefix(guard, "unreadable:") || !strings.Contains(guard, "permission denied") {
				t.Fatalf("%s: stale_binary_guard = %q (keys %v), want the failure while it lasts", d.name, guard, staleKeys(m))
			}
			exe.set(staleExeThen, nil)
			if m := d.call(t, s); m["stale_binary_guard"] != nil {
				t.Fatalf("%s: stale_binary_guard %v after the stamp could be read again", d.name, m["stale_binary_guard"])
			}
		})
	}
}

// TestAWatchingGuardAddsNothingToTheDelegatingDoors: the key exists only when the guard is not watching.
func TestAWatchingGuardAddsNothingToTheDelegatingDoors(t *testing.T) {
	for _, d := range guardDoors() {
		t.Run(d.name, func(t *testing.T) {
			captureStaleLog(t)
			exe := &staleMovableExe{cur: staleExeThen}
			s := staleResearchServer(t)
			s.exe = newExeWatch(exe.read)
			if m := d.call(t, s); m["stale_binary_guard"] != nil || m["stale_binary"] != nil {
				t.Fatalf("%s: a watching guard on an unchanged image put stale_binary_guard %v / stale_binary %v on the answer", d.name, m["stale_binary_guard"], m["stale_binary"])
			}
		})
	}
}
