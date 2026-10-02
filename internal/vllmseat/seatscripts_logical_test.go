package vllmseat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The seat scripts take their defaults (env file, log, venv, work directory) from the directory
// they sit in. The directory is the LOGICAL one (cd + pwd, no -P, no readlink -f), so a copy that is
// a symlink keeps the directory it is linked from and a copy installed anywhere keeps its own. These
// tests pin that form, that no script names a fixed seat directory any more, and the one line that
// must keep resolving symlinks.

const (
	seatScriptsDir = "../../setup/templates/vllm-seat"
	logicalHere    = `HERE="$(cd "$(dirname "$0")" && pwd)"`
	// The line that starts the engine-side cleanup resolves symlinks ON PURPOSE (it finds the stop
	// script beside the real file) and is pinned by launcher_reap_order_test.go too.
	stopLine = `SEAT_STOP_ATTACHED=1 bash "$(dirname "$(readlink -f "$0")")/seat_stop.sh" "${stop_args[@]}" || stop_rc=$?`
)

func readSeatScript(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(seatScriptsDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// assignmentLine returns the first line of s that starts with prefix (a shell assignment).
func assignmentLine(t *testing.T, name, s, prefix string) string {
	t.Helper()
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	t.Fatalf("%s has no line starting with %q", name, prefix)
	return ""
}

func TestSeatScriptsDeriveTheirDirectoryLogically(t *testing.T) {
	// The fixed seat directory the scripts used to carry is assembled here, inside a word, so this
	// file never spells it either.
	fixedDir := "/ro" + "ot/g7"

	for _, rel := range []string{
		"seat_fg.sh", "seat_stop.sh", "seat_fg.stale-mp.tests.sh", "lmcache-patches/repatch-lmcache-overlay.sh",
	} {
		if strings.Contains(readSeatScript(t, rel), fixedDir) {
			t.Errorf("%s still names a fixed seat directory: defaults derive from where the script sits", rel)
		}
	}

	fg := readSeatScript(t, "seat_fg.sh")
	stop := readSeatScript(t, "seat_stop.sh")

	for _, c := range []struct {
		name, body, marker string
		defaults           map[string]string // line prefix -> what the line must contain
	}{
		{"seat_fg.sh", fg, "# >>> port refusals", map[string]string{
			"CFG=": "$HERE/seat.env", "LOG=": "$HERE/seat.log", "VENV=": "$HERE/vllm-env", "WORK=": "${SEAT_WORKDIR:-$HERE}",
		}},
		{"seat_stop.sh", stop, "# --- reaping ends here", map[string]string{
			"CFG=": "$HERE/seat.env", "LOG=": "$HERE/seat.log",
		}},
	} {
		if n := strings.Count(c.body, logicalHere); n != 1 {
			t.Errorf("%s must define HERE exactly once as %s, found %d", c.name, logicalHere, n)
			continue
		}
		// HERE is defined above the marker the stale-MP test cuts at, so the part of the script it
		// extracts defines it too.
		if strings.Index(c.body, logicalHere) > strings.Index(c.body, c.marker) {
			t.Errorf("%s defines HERE below its %q marker", c.name, c.marker)
		}
		// The defaults use it, and none of them resolves symlinks.
		for prefix, want := range c.defaults {
			line := assignmentLine(t, c.name, c.body, prefix)
			if !strings.Contains(line, want) {
				t.Errorf("%s: %q must derive from HERE (want %q in it)", c.name, line, want)
			}
		}
		for _, prefix := range []string{"HERE=", "CFG=", "LOG=", "VENV=", "WORK="} {
			for _, l := range strings.Split(c.body, "\n") {
				if strings.HasPrefix(l, prefix) && (strings.Contains(l, "readlink") || strings.Contains(l, "pwd -P")) {
					t.Errorf("%s: the default %q resolves symlinks; it must be the logical path", c.name, l)
				}
			}
		}
	}

	// The port-refusal block runs alone in a scratch script that does not define HERE (and under
	// set -u), so nothing inside the marked block may use it.
	start := strings.Index(fg, "# >>> port refusals")
	end := strings.Index(fg, "# <<< port refusals")
	if start < 0 || end < start {
		t.Fatal("seat_fg.sh lost its marked port-refusal block")
	}
	for _, use := range []string{"$HERE", "${HERE", "HERE="} {
		if strings.Contains(fg[start:end], use) {
			t.Errorf("the marked port-refusal block of seat_fg.sh uses HERE (%q); the stale-MP test extracts it into a script that has none", use)
		}
	}

	if n := strings.Count(fg, stopLine); n != 1 {
		t.Errorf("seat_fg.sh must carry the symlink-resolving stop line exactly once, found %d", n)
	}
}
