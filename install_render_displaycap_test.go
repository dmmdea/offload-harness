package main

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// audit-yaml and the display card (display-cap-unseen). A llama-swap file states no VRAM footprint, so the audit
// cannot judge an entry against gpu_display_cap_pct. It does stop passing in silence over the entries that may sit on
// the card the driver flags as the display card: proxy: seats, entries with no device pin, entries pinned onto it.
// One aggregated `note:` line, never a violation, never an exit code, and no config is read.

// displayDevs is a three-card box (three different models, so A-87 has nothing to say about an index) whose card 1
// has a monitor attached.
func displayDevs() []gpuprobe.Device {
	return []gpuprobe.Device{
		{Index: 0, UUID: boxCardA, Name: "NVIDIA GeForce RTX 5060 Ti", TotalGiB: 15.9, FreeGiB: 15},
		{Index: 1, UUID: boxCardB, Name: "NVIDIA GeForce RTX 5070 Ti", TotalGiB: 15.9, FreeGiB: 14, DisplayAttached: true},
		{Index: 2, UUID: boxCardC, Name: "NVIDIA GeForce RTX 4090", TotalGiB: 15.9, FreeGiB: 15},
	}
}

const (
	unseenProxyEntry = "  vllm-seat:\n    ttl: 300\n    proxy: http://192.0.2.10:18797\n    cmd: /opt/seats/run.sh\n    cmdStop: /opt/seats/stop.sh\n"
	unseenPlainEntry = "  free-run:\n    ttl: 300\n    cmd: /bin/llama-server -m m.gguf -ngl 99 --port ${PORT}\n"
)

func unseenPinned(uuid string) string {
	return "  on-card:\n    ttl: 300\n    env: [\"CUDA_VISIBLE_DEVICES=" + uuid + "\", CUDA_DEVICE_ORDER=PCI_BUS_ID]\n    cmd: /bin/llama-server -m m.gguf -ngl 99 --port ${PORT}\n"
}

var unseenNoteLine = regexp.MustCompile(`(?m)^note: .*display-cap-unseen \(card 1\): .*$`)

// countCardTableReads replaces the card-table seam with one that counts its calls.
func countCardTableReads(t *testing.T, devs []gpuprobe.Device, err error) *int {
	t.Helper()
	n := 0
	prev := renderCardTable
	renderCardTable = func() ([]gpuprobe.Device, error) { n++; return devs, err }
	t.Cleanup(func() { renderCardTable = prev })
	return &n
}

func auditText(t *testing.T, text string, flags ...string) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "llama-swap.yaml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	var runErr error
	out := captureStdout(t, func() { runErr = runAuditYAML(append(flags, path)) })
	return out, runErr
}

func TestAuditYAMLNamesTheEntriesItCannotPlaceOnTheDisplayCardAndNeverFails(t *testing.T) {
	yamlText := "models:\n" + unseenProxyEntry + unseenPlainEntry + unseenPinned(boxCardB)
	out, err := auditYAMLWithTable(t, displayDevs(), nil, yamlText)
	if err != nil || !strings.Contains(out, ": OK") || strings.Contains(out, "violation(s)") {
		t.Fatalf("the note is not a violation and changes no exit code; err=%v:\n%s", err, out)
	}
	if n := len(unseenNoteLine.FindAllString(out, -1)); n != 1 {
		t.Fatalf("exactly one display-cap-unseen note line, got %d:\n%s", n, out)
	}
	line := unseenNoteLine.FindString(out)
	for _, want := range []string{"vllm-seat", "free-run", "on-card", "60", "9.5 GiB", "gpu status", "no VRAM footprint"} {
		if !strings.Contains(line, want) {
			t.Errorf("the note must contain %q:\n%s", want, line)
		}
	}
	// The positive control: the same file on a box whose driver flags no display card is silent about it.
	headless := displayDevs()
	headless[1].DisplayAttached = false
	out, err = auditYAMLWithTable(t, headless, nil, yamlText)
	if err != nil || !strings.Contains(out, ": OK") || strings.Contains(out, "display-cap-unseen") {
		t.Errorf("no display card, no note; err=%v:\n%s", err, out)
	}
}

// The note rides along with real findings without changing them: the violation count and the exit status are the
// rules' own.
func TestAuditYAMLNoteDoesNotCountAsAFinding(t *testing.T) {
	bad := "models:\n" + unseenProxyEntry + strings.Replace(unseenPlainEntry, "ttl: 300", "ttl: 0", 1)
	out, err := auditYAMLWithTable(t, displayDevs(), nil, bad)
	if err == nil || !strings.Contains(err.Error(), "1 finding(s)") || !strings.Contains(out, "1 violation(s)") {
		t.Fatalf("the one ttl violation is the one finding; err=%v:\n%s", err, out)
	}
	if !unseenNoteLine.MatchString(out) {
		t.Errorf("the note is printed beside it:\n%s", out)
	}
}

// nvidia-smi is run for a file that pins a device OR has a proxy: seat, once per file, and for nothing else.
func TestAuditYAMLReadsTheCardTableOnceAndOnlyWhereItHasSomethingToSay(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want int
	}{
		{"a proxy seat alone", "models:\n" + unseenProxyEntry, 1},
		{"a pin alone", pinnedYAML(boxCardA), 1},
		{"a pin and a proxy seat share one read", "models:\n" + unseenProxyEntry + unseenPinned(boxCardA), 1},
		{"neither", "models:\n" + unseenPlainEntry, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := countCardTableReads(t, displayDevs(), nil)
			if _, err := auditText(t, tc.text); err != nil {
				t.Fatalf("audit-yaml: %v", err)
			}
			if *reads != tc.want {
				t.Errorf("the card table was read %d times, want %d", *reads, tc.want)
			}
		})
	}
}

func TestAuditYAMLDisplayCapUnseenHonoursOtherBox(t *testing.T) {
	proxyOnly := "models:\n" + unseenProxyEntry
	reads := countCardTableReads(t, displayDevs(), nil)
	out, err := auditText(t, proxyOnly, "--other-box")
	if err != nil || !strings.Contains(out, ": OK") {
		t.Fatalf("--other-box skips and fails nothing; err=%v:\n%s", err, out)
	}
	if !regexp.MustCompile(`(?m)^note: .*display-cap-unseen.*not applied.*--other-box`).MatchString(out) {
		t.Errorf("a silent pass would read as a clean audit; the output must say the note did not run and why:\n%s", out)
	}
	if unseenNoteLine.MatchString(out) || *reads != 0 {
		t.Errorf("the other box's file is not compared with this box's cards (reads=%d):\n%s", *reads, out)
	}

	// A file that pins keeps the existing skip line, and the same line says the note is skipped with it.
	out, err = auditText(t, pinnedYAML(boxCardA), "--other-box")
	if err != nil || !strings.Contains(out, "pin rules not applied") || !strings.Contains(out, "display-cap-unseen") {
		t.Errorf("the pin rules' skip line also names the note; err=%v:\n%s", err, out)
	}
	// A file with neither is silent about both.
	out, err = auditText(t, "models:\n"+unseenPlainEntry, "--other-box")
	if err != nil || strings.Contains(out, "display-cap-unseen") || strings.Contains(out, "pin rules") {
		t.Errorf("nothing to skip, nothing said; err=%v:\n%s", err, out)
	}
	// The positive control: without the flag the same proxy file gets its note.
	out, err = auditYAMLWithTable(t, displayDevs(), nil, proxyOnly)
	if err != nil || !unseenNoteLine.MatchString(out) {
		t.Errorf("control: no flag, the note is printed; err=%v:\n%s", err, out)
	}
}

func TestAuditYAMLSaysWhenTheCardTableCouldNotBeReadForAProxyOnlyFile(t *testing.T) {
	out, err := auditYAMLWithTable(t, nil, errors.New("nvidia-smi: not found"), "models:\n"+unseenProxyEntry)
	if err != nil || !strings.Contains(out, ": OK") {
		t.Fatalf("a missing card table must not change the exit status; err=%v:\n%s", err, out)
	}
	if !strings.Contains(out, "no card table") || !strings.Contains(out, "display-cap-unseen") || !strings.Contains(out, "nvidia-smi: not found") {
		t.Errorf("a silent pass would read as a clean audit; the output must say the note did not run and why:\n%s", out)
	}
	// A file that pins keeps the existing words and names the note too.
	out, err = auditYAMLWithTable(t, nil, errors.New("nvidia-smi: not found"), pinnedYAML(boxCardA))
	if err != nil || !strings.Contains(out, "no card table: pin rules not applied") || !strings.Contains(out, "display-cap-unseen") {
		t.Errorf("the pin rules' words stay and the note is named; err=%v:\n%s", err, out)
	}
}

func TestAuditYAMLNamesAnIndexPinWithoutPCIOrderAsUnmappable(t *testing.T) {
	yamlText := "models:\n  by-index:\n    ttl: 300\n    env: [\"CUDA_VISIBLE_DEVICES=1\"]\n    cmd: /bin/llama-server -m m.gguf -ngl 99 --port ${PORT}\n"
	out, err := auditYAMLWithTable(t, displayDevs(), nil, yamlText)
	if err != nil || !strings.Contains(out, ": OK") {
		t.Fatalf("an index on three different cards is not an A-87 finding; err=%v:\n%s", err, out)
	}
	line := unseenNoteLine.FindString(out)
	if !strings.Contains(line, "by-index") || !strings.Contains(line, "cannot be mapped") {
		t.Errorf("the index with no CUDA_DEVICE_ORDER=PCI_BUS_ID is named as unmappable, not guessed:\n%s", out)
	}
	// With PCI order the same index IS the display card's: named as pinned onto it, not as unmappable.
	withOrder := strings.Replace(yamlText, `"CUDA_VISIBLE_DEVICES=1"`, `"CUDA_VISIBLE_DEVICES=1", CUDA_DEVICE_ORDER=PCI_BUS_ID`, 1)
	out, _ = auditYAMLWithTable(t, displayDevs(), nil, withOrder)
	line = unseenNoteLine.FindString(out)
	if !strings.Contains(line, "by-index (card 1)") || strings.Contains(line, "cannot be mapped") {
		t.Errorf("control: under PCI_BUS_ID index 1 is the display card:\n%s", out)
	}
}

func TestAuditYAMLSaysWhenTheReadingCouldNotSayWhichCardIsTheMonitors(t *testing.T) {
	devs := displayDevs()
	for i := range devs {
		devs[i].AttachedUnknown = true
	}
	out, err := auditYAMLWithTable(t, devs, nil, "models:\n"+unseenProxyEntry)
	if err != nil || !strings.Contains(out, "display-cap-unseen") || !strings.Contains(out, "could not say which card the monitor is on") {
		t.Errorf("an unknown reading is said, not judged; err=%v:\n%s", err, out)
	}
	if strings.Contains(out, "vllm-seat") {
		t.Errorf("no entry is named on a reading that cannot say which card is the monitor's:\n%s", out)
	}
}
