package vllmseat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The markers seat_fg.stale-mp.tests.sh extracts the launcher's cleanup by. A move or a rename that loses them must
// fail loudly here, not turn the behavioral test into a test of an empty block.
const (
	reapBlockBegin  = "# >>> port refusals and the dead generation's cleanup"
	reapBlockEnd    = "# <<< port refusals and the dead generation's cleanup"
	reapSectionStop = "# --- reaping ends here"
)

func readSeatStop(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "vllm-seat", "seat_stop.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// TestLauncherReapsTheDeadGenerationBeforeItTouchesTheMPServerOrTheCards pins WHERE the crash cleanup sits in
// seat_fg.sh. A crashed generation's MP server and engine workers must be gone before the launcher stops the MP unit,
// waits for the cards (the VRAM precheck only warns, so workers still holding VRAM would fail the start two minutes
// later) and starts the new MP server; the launcher must still become `vllm serve` by exec (nothing of it survives a
// crash to clean up after it — the cleanup is at the NEXT start and in the stub's crash exit, never in a resident
// process); and the foreign-holder refusal must be the last thing in the block, so no cleanup path can skip it.
func TestLauncherReapsTheDeadGenerationBeforeItTouchesTheMPServerOrTheCards(t *testing.T) {
	s := readLauncher(t)
	begin := strings.Index(s, reapBlockBegin)
	end := strings.Index(s, reapBlockEnd)
	if begin < 0 || end < 0 || begin >= end {
		t.Fatalf("the launcher's marked cleanup block is gone or inverted (begin %d, end %d); seat_fg.stale-mp.tests.sh extracts it by these lines", begin, end)
	}
	block := s[begin:end]
	stopMP := strings.Index(s, `systemctl stop "$MP_UNIT"`)
	vram := strings.Index(s, "# VRAM precheck")
	startMP := strings.Index(s, `systemd-run --unit="$MP_UNIT"`)
	serve := strings.Index(s, `exec "$VENV/bin/vllm" serve`)
	if serve < 0 {
		t.Fatal("the launcher no longer execs into vllm serve: the swap-out reaping and the crash-cleanup design (no resident process) both rest on it")
	}
	if stopMP < 0 || vram < 0 || startMP < 0 {
		t.Fatalf("the launcher lost a landmark: MP unit stop %d, VRAM precheck %d, MP start %d", stopMP, vram, startMP)
	}
	if !(end < stopMP && stopMP < vram && vram < startMP && startMP < serve) {
		t.Fatalf("the crash cleanup must come first: block ends %d, MP unit stop %d, VRAM precheck %d, MP start %d, exec %d", end, stopMP, vram, startMP, serve)
	}
	for _, must := range []string{
		// what triggers it: no engine of THIS stack alive, and a leftover of any of the three kinds
		`if ! pgrep -f "vllm serve .*--port $PORT" >/dev/null 2>&1; then`,
		`grep -q ":$MP_HTTP_PORT " && left=`,
		`pgrep -f "lmcache server .*--port $MP_PORT( |\$)"`,
		`ps -eo args= 2>/dev/null | grep -q '^VLLM::'`,
		// what it runs: the stack's own stop script, once
		`/seat_stop.sh" "$CFG"`,
	} {
		if !strings.Contains(block, must) {
			t.Errorf("the launcher's cleanup block lost %q", must)
		}
	}
	if strings.Count(block, "seat_stop.sh\" \"$CFG\"") != 1 {
		t.Error("the launcher must run the stack's seat_stop.sh exactly once per start")
	}
	// An engine process is found by the start of its command line: `pgrep -f VLLM::` also matches any wrapper whose
	// arguments merely mention the name (a shell running this very check).
	if strings.Contains(block, "pgrep -f 'VLLM::") || strings.Contains(block, `pgrep -f "VLLM::`) {
		t.Error("engine processes must be looked for by the start of their command line, never pgrep -f VLLM::")
	}
	// The refusal on a foreign holder is what the cleanup must never be able to skip.
	refusal := strings.LastIndex(block, `if ss -ltnp 2>/dev/null | grep -q ":$MP_HTTP_PORT "; then`)
	if refusal < 0 || !strings.Contains(block[refusal:], "REFUSING to start — the MP HTTP port") {
		t.Error("the foreign-holder refusal on the MP HTTP port is not the last thing in the cleanup block")
	}
}

// TestSeatEnvDocumentsTheCrashCleanupKnobs keeps the two optional knobs of the crash cleanup honest in both directions:
// the scripts read them under exactly these names with the documented default, and the seat.env template (the file an
// operator actually opens) lists them, commented out, so a knob nobody can find is not a knob.
func TestSeatEnvDocumentsTheCrashCleanupKnobs(t *testing.T) {
	env, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "vllm-seat", "windows-wsl", "seat.env"))
	if err != nil {
		t.Fatal(err)
	}
	e := strings.ReplaceAll(string(env), "\r\n", "\n")
	for _, tc := range []struct{ knob, read, script string }{
		{"SEAT_REAP_WAIT_SEC", `WAIT="${SEAT_REAP_WAIT_SEC:-10}"`, readSeatStop(t)},
		{"SEAT_MP_PORT_WAIT_SEC", `MP_PORT_WAIT="${SEAT_MP_PORT_WAIT_SEC:-10}"`, readLauncher(t)},
	} {
		if !strings.Contains(tc.script, tc.read) {
			t.Errorf("the launcher no longer reads %s with a default of 10 (%q missing)", tc.knob, tc.read)
		}
		if !strings.Contains(e, "\n#"+tc.knob+"=10\n") {
			t.Errorf("seat.env does not list %s (commented out, default 10) beside the other optional knobs", tc.knob)
		}
	}
}

// TestSeatStopReapsByIdentityNeverByPort pins the rule behind "a foreign listener is refused, never reaped": the
// reaping section of seat_stop.sh picks its victims by what they ARE (a VLLM:: process with no live `vllm serve`
// ancestor; an `lmcache server` of THIS stack's MP port; this stack's unit) and never by which port they hold. It
// also pins the two escalations that make "0 orphans 60 s after the death" checkable: SIGKILL for an MP server that
// ignores SIGTERM, and the report of a reaped process that is still alive. The behavior is proven by
// seat_fg.stale-mp.tests.sh (Linux); this keeps the identity rule readable on every platform.
func TestSeatStopReapsByIdentityNeverByPort(t *testing.T) {
	s := readSeatStop(t)
	cut := strings.Index(s, reapSectionStop)
	if cut < 0 {
		t.Fatalf("seat_stop.sh lost its %q marker: seat_fg.stale-mp.tests.sh runs everything above it", reapSectionStop)
	}
	sec := s[:cut]
	for _, must := range []string{
		`MP_PAT="lmcache server .*--port $MP_PORT( |\$)"`,
		`ps -eo pid,args 2>/dev/null | awk '$2 ~ /^VLLM::/ {print $1}'`,
		"if ! has_api_ancestor",
		`systemctl stop "$MP_UNIT"`,
		`kill -KILL "$p" 2>/dev/null; MP_KILLED=`,
		"still alive ${WAIT} s after SIGKILL",
	} {
		if !strings.Contains(sec, must) {
			t.Errorf("seat_stop.sh's reaping section lost %q", must)
		}
	}
	// No victim may be selected by the port it holds: these are the ways a shell does that.
	for _, banned := range []string{"fuser", "lsof", "ss -", "netstat", "/proc/net"} {
		if strings.Contains(sec, banned) {
			t.Errorf("seat_stop.sh's reaping section uses %q: a victim picked by the port it holds would reap a foreign listener", banned)
		}
	}
	// The unit stop must not be judged by a stale answer: a unit that is already gone is no failure.
	if !strings.Contains(sec, `systemctl is-active --quiet "$MP_UNIT"`) {
		t.Error("seat_stop.sh warns about a failed unit stop even when the unit is gone (a crash cleanup finds none)")
	}
	// The stop still ends non-zero when a process of the seat survives SIGKILL, like a bound port does.
	if !strings.Contains(s[cut:], "INCOMPLETE") {
		t.Error("seat_stop.sh no longer fails when a process of the seat survives SIGKILL")
	}
}
