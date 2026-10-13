package mcpserver

// stale_reason_test.go: a note that says the bytes could not be compared says why (0.184.0 integration review, silent
// failure hunt). The reason (a lock on the freshly renamed file, "Access is denied") was kept on offload_status's process
// block and dropped from the stale_binary note and from the one line the guard logs, so the answer and the log said "could not
// be compared" and nothing a reader could act on.

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
)

// TestTheNoteSaysWhyTheBytesCouldNotBeCompared: the failure is named in the stale_binary note and in the log line.
func TestTheNoteSaysWhyTheBytesCouldNotBeCompared(t *testing.T) {
	s, _, path := serverOnInstalledImage(t, "0.183.0")
	logged := captureStaleLog(t) // after the server is built: serverOnInstalledImage installs a capture of its own
	swapImage(t, path, imageBody("0.183.0"))
	failHashes(t) // "the file is held by another process"

	note := s.staleBinaryNote(buildinfo.Version)
	if !strings.Contains(note, "could not be compared") {
		t.Fatalf("setup: stale_binary = %q, want the note that says the bytes could not be compared", note)
	}
	const reason = "the file is held by another process"
	if !strings.Contains(note, reason) {
		t.Fatalf("stale_binary = %q: it says the bytes could not be compared and drops the reason %q", note, reason)
	}
	if !strings.Contains(logged.String(), reason) {
		t.Fatalf("log %q: the one line the guard logs drops the reason %q", logged.String(), reason)
	}
}
