package main

// Foreign GPU memory holders (register D-1xx-4, 2026-09-23). These tests pin
// the pure classification/formatting logic (denylist, MiB floor, sort,
// message shape) with synthetic PDH-shaped input — the live PDH/toolhelp
// reads and the nvidia-smi fallback are best-effort by design (see
// gpu_foreign.go's package comment) and are not reproducible on a CI box
// without a GPU, so they are exercised through this pure seam instead.

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/fleetnode"
)

// THE HEADLINE CASE, shaped exactly like the incident: DaVinci Resolve at
// 1,450 MiB must be reported; dwm.exe (the OS compositor) and llama-server.exe
// (the agent seat, harness-owned) must not, whatever their own MiB; a
// resident under the noise floor must not, however it is named.
func TestClassifyForeignFiltersHarnessOwnedAndSmallResidents(t *testing.T) {
	byPID := []fleetnode.ProcessDedicated{
		{PID: 9416, MiB: 1450}, // Resolve — must be reported
		{PID: 100, MiB: 320},   // dwm — OS compositor, never a finding
		{PID: 200, MiB: 6176},  // llama-server — the agent seat itself
		{PID: 300, MiB: 10},    // trivial — below the noise floor
		{PID: 400, MiB: 200},   // unnamed process, above the floor — must be reported
	}
	names := map[int]string{
		9416: `C:\Program Files\Blackmagic Design\DaVinci Resolve\Resolve.exe`,
		100:  "dwm.exe",
		200:  "llama-server.exe",
		300:  "SomeTinyThing.exe",
		// 400 deliberately has no entry: an unresolved name must still surface.
	}

	got := classifyForeign(byPID, names)
	if len(got) != 2 {
		t.Fatalf("classifyForeign = %+v, want exactly 2 entries (Resolve and the unnamed pid 400)", got)
	}
	// Sorted MiB descending: Resolve first.
	if got[0].PID != 9416 || got[0].MiB != 1450 || got[0].Name != "Resolve.exe" {
		t.Fatalf("first entry = %+v, want Resolve pid 9416 1450 MiB", got[0])
	}
	if got[1].PID != 400 || got[1].MiB != 200 {
		t.Fatalf("second entry = %+v, want the unnamed pid 400 at 200 MiB", got[1])
	}
	for _, h := range got {
		if h.PID == 100 || h.PID == 200 || h.PID == 300 {
			t.Fatalf("a harness/OS-owned or sub-floor resident leaked into the foreign list: %+v", h)
		}
	}
}

// The denylist match is case-insensitive and tolerates the ".exe" suffix
// either way, since PDH's process name and Windows' own casing conventions
// are not guaranteed uniform.
func TestIsHarnessOwnedMatchesCaseInsensitivelyWithOrWithoutExeSuffix(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"dwm.exe", true},
		{"DWM.EXE", true},
		{`C:\Windows\System32\dwm.exe`, true},
		{"llama-server.exe", true},
		{"offload-harness.exe", true},
		{"Resolve.exe", false},
		{"chrome.exe", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isHarnessOwned(c.name); got != c.want {
			t.Errorf("isHarnessOwned(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// The message shape is pinned exactly, including the "WARNING foreign GPU
// memory holders:" prefix and the "<name> pid <pid> <mib> MiB" per-entry
// form (the task's own worked example), and an empty list must render
// nothing at all — never a "no foreign holders" line nobody asked for.
func TestFormatForeignWarningShape(t *testing.T) {
	if got := formatForeignWarning(nil); got != "" {
		t.Fatalf("formatForeignWarning(nil) = %q, want empty", got)
	}
	got := formatForeignWarning([]ForeignGPUHolder{
		{Name: "Resolve.exe", PID: 9416, MiB: 1450},
		{Name: "chrome.exe", PID: 21044, MiB: 210},
	})
	want := "WARNING foreign GPU memory holders: Resolve.exe pid 9416 1450 MiB, chrome.exe pid 21044 210 MiB"
	if got != want {
		t.Fatalf("formatForeignWarning = %q, want %q", got, want)
	}
	if !strings.Contains(got, "pid 9416") || !strings.Contains(got, "1450 MiB") {
		t.Fatalf("formatForeignWarning dropped the pid/MiB detail an operator reads: %q", got)
	}
}

// classifyForeign must never touch anything below the noise floor — a small
// desktop resident (an icon renderer, a thumbnail codec) is normal on every
// box and would turn every `gpu reserve` into a wall of warnings nobody
// reads.
func TestClassifyForeignRespectsTheNoiseFloor(t *testing.T) {
	byPID := []fleetnode.ProcessDedicated{{PID: 1, MiB: foreignMinMiB - 1}}
	if got := classifyForeign(byPID, map[int]string{1: "tiny.exe"}); len(got) != 0 {
		t.Fatalf("a resident 1 MiB under the floor was reported: %+v", got)
	}
	byPID = []fleetnode.ProcessDedicated{{PID: 1, MiB: foreignMinMiB}}
	if got := classifyForeign(byPID, map[int]string{1: "atfloor.exe"}); len(got) != 1 {
		t.Fatalf("a resident exactly AT the floor must be reported: %+v", got)
	}
}
