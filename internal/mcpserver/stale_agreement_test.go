package mcpserver

// stale_agreement_test.go: the two staleness signals of an MCP server say one thing (0.184.0 integration review).
//
// offload_status's process block (stale_image.go) says the installed bytes differ from the image the process started
// from; the stale-binary guard (stalebinary.go) said the size or modification time of the file moved. A redeploy of
// the same build rewrites the file (a new modification time) and leaves its bytes alone: stale_image read false while
// the same answer carried a stale_binary note. The guard's start stamp was also taken in New, after the config had
// been loaded and the pipeline opened, which re-opened the start-up window the capture closed (D-36). The guard now
// starts from the identity the process captured first thing, and a moved stamp is settled by the bytes.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
)

// bothSignals asks the door a caller makes first (offload_status, section brief) and returns what it says about the
// image: process.stale_image (nil when the key is absent) and the stale_binary note ("" when none).
func bothSignals(t *testing.T, s *Server) (staleImage any, note string) {
	t.Helper()
	res, err := s.handleStatus(context.Background(), callReq(`{"section":"brief"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	block, _ := m["process"].(map[string]any)
	if block == nil {
		t.Fatalf("the brief carries no process block (keys %v)", staleKeys(m))
	}
	note, _ = m["stale_binary"].(string)
	return block["stale_image"], note
}

// serverOnInstalledImage builds a server that started from an installed image at a fresh path: it holds that
// startup identity, and its guard watches the file at the path (the watch is built before any swap, as a capture is).
func serverOnInstalledImage(t *testing.T, version string) (*Server, buildinfo.Startup, string) {
	t.Helper()
	captureStaleLog(t)
	path := t.TempDir() + "/local-offload.exe"
	st := installImage(t, path, version)
	s := delegateTestServer(t, staleLocalRun())
	s.startup = &st
	s.exe = newExeWatch(func() (exeStamp, error) { return stampOf(path) })
	return s, st, path
}

// failHashes makes every read of the installed image fail until the test ends.
func failHashes(t *testing.T) {
	t.Helper()
	real := hashInstalledImage
	hashInstalledImage = func(string) (string, error) { return "", errors.New("the file is held by another process") }
	t.Cleanup(func() { hashInstalledImage = real })
}

// TestTheSameBytesWithANewerStampAreNotStaleOnEitherSignal: a redeploy that rewrote the file with equal bytes is not a
// different image. stale_image said false; stale_binary said "changed on disk".
func TestTheSameBytesWithANewerStampAreNotStaleOnEitherSignal(t *testing.T) {
	s, _, path := serverOnInstalledImage(t, "0.183.0")
	swapImage(t, path, imageBody("0.183.0"))
	staleImage, note := bothSignals(t, s)
	if staleImage != false {
		t.Fatalf("setup: process.stale_image = %v for equal bytes, want false", staleImage)
	}
	if note != "" {
		t.Fatalf("stale_binary says %q while process.stale_image is false for the same bytes: the two signals disagree", note)
	}
}

// TestDifferentBytesAreStaleOnBothSignals: the other half of the definition, so a fix cannot silence the guard.
func TestDifferentBytesAreStaleOnBothSignals(t *testing.T) {
	s, _, path := serverOnInstalledImage(t, "0.183.0")
	swapImage(t, path, imageBody("0.184.0"))
	staleImage, note := bothSignals(t, s)
	if staleImage != true {
		t.Fatalf("process.stale_image = %v after a swap to other bytes, want true", staleImage)
	}
	if !strings.Contains(note, "changed on disk") || !strings.Contains(strings.ToLower(note), "restart") {
		t.Fatalf("stale_binary = %q beside stale_image true, want the changed-on-disk note", note)
	}
}

// TestTheStaleBinaryNoteNamesTheInstalledVersionWhenItIsKnown: the guard shares the installed check with the process
// block, which reads the build marker of the installed file (never by running it), so the note can say what to expect.
func TestTheStaleBinaryNoteNamesTheInstalledVersionWhenItIsKnown(t *testing.T) {
	s, _, path := serverOnInstalledImage(t, "0.183.0")
	swapImage(t, path, imageBody("0.184.0"))
	_, note := bothSignals(t, s)
	if !strings.Contains(note, "0.184.0") {
		t.Fatalf("stale_binary = %q, want it to name the installed build 0.184.0 the process block carries", note)
	}
}

// TestAnImageThatCannotBeComparedKeepsTheStampAndSaysSo: when the bytes cannot be read, the changed stamp is the only
// evidence there is, and a guard against a silent failure does not go quiet on it; the note says what it rests on.
func TestAnImageThatCannotBeComparedKeepsTheStampAndSaysSo(t *testing.T) {
	s, _, path := serverOnInstalledImage(t, "0.183.0")
	swapImage(t, path, imageBody("0.183.0"))
	failHashes(t)

	staleImage, note := bothSignals(t, s)
	if staleImage != nil {
		t.Fatalf("an unreadable installed file answered process.stale_image = %v, want the key absent", staleImage)
	}
	if !strings.Contains(note, "changed on disk") || !strings.Contains(note, "could not be compared") {
		t.Fatalf("stale_binary = %q, want the changed-on-disk note that says the bytes could not be compared", note)
	}
}

// TestAStampMovedByAHeldFileClearsOnceTheBytesCanBeRead: a failed read is a moment, not a verdict (the retry the process
// block makes), so the note a lock caused goes when the next call reads equal bytes.
func TestAStampMovedByAHeldFileClearsOnceTheBytesCanBeRead(t *testing.T) {
	s, _, path := serverOnInstalledImage(t, "0.183.0")
	swapImage(t, path, imageBody("0.183.0"))
	real := hashInstalledImage
	fail := true
	hashInstalledImage = func(p string) (string, error) {
		if fail {
			fail = false
			return "", errors.New("the file is held by another process")
		}
		return real(p)
	}
	t.Cleanup(func() { hashInstalledImage = real })

	// The guard alone, so that the one failing read is its own (the process block makes a read of its own).
	if note := s.staleBinaryNote(buildinfo.Version); note == "" {
		t.Fatal("setup: the first call, whose read failed, carries no stale_binary note")
	}
	if note := s.staleBinaryNote(buildinfo.Version); note != "" {
		t.Fatalf("second call: stale_binary = %q, want none once the bytes read equal", note)
	}
	if staleImage, note := bothSignals(t, s); staleImage != false || note != "" {
		t.Fatalf("third call: stale_image = %v, stale_binary = %q, want false and none", staleImage, note)
	}
}

// TestAProcessWhoseOwnHashIsUnknownKeepsTheStamp: the process could not hash itself at start, so there is nothing to
// compare the installed bytes with. The stamp stands and the note says so.
func TestAProcessWhoseOwnHashIsUnknownKeepsTheStamp(t *testing.T) {
	s, st, path := serverOnInstalledImage(t, "0.183.0")
	st.SHA256 = ""
	s.startup = &st
	swapImage(t, path, imageBody("0.183.0"))
	_, note := bothSignals(t, s)
	if !strings.Contains(note, "changed on disk") || !strings.Contains(note, "could not be compared") {
		t.Fatalf("stale_binary = %q, want the stamp's note that says the bytes could not be compared", note)
	}
}

// TestTheGuardStartsFromTheIdentityTheProcessCapturedNotFromWhenTheServerWasBuilt: New runs after the config is loaded
// and the pipeline opened. A deploy that lands between the capture (the first thing runMCP does) and New used to be
// read as the image the process started from, and the guard accused nothing for the life of the process.
func TestTheGuardStartsFromTheIdentityTheProcessCapturedNotFromWhenTheServerWasBuilt(t *testing.T) {
	captureStaleLog(t)
	path := t.TempDir() + "/local-offload.exe"
	st := installImage(t, path, "0.183.0")
	old := startupIdentity
	startupIdentity = func() (buildinfo.Startup, bool) { return st, true }
	t.Cleanup(func() { startupIdentity = old })

	swapImage(t, path, imageBody("0.184.0")) // the deploy lands after the capture and before New

	s := delegateTestServer(t, staleLocalRun())
	if note := s.staleBinaryNote(buildinfo.Version); !strings.Contains(note, "changed on disk") {
		t.Fatalf("stale_binary = %q for a swap between the capture and New, want the changed-on-disk note", note)
	}
	if state := s.exe.guardState(); state != "" {
		t.Fatalf("the guard is not watching: %q", state)
	}
}

// TestASwapOfEqualBytesBetweenTheCaptureAndNewIsNotStale: the arbitration holds for a guard seeded from the capture too.
func TestASwapOfEqualBytesBetweenTheCaptureAndNewIsNotStale(t *testing.T) {
	captureStaleLog(t)
	path := t.TempDir() + "/local-offload.exe"
	st := installImage(t, path, "0.183.0")
	old := startupIdentity
	startupIdentity = func() (buildinfo.Startup, bool) { return st, true }
	t.Cleanup(func() { startupIdentity = old })

	swapImage(t, path, imageBody("0.183.0"))

	s := delegateTestServer(t, staleLocalRun())
	s.startup = &st
	if staleImage, note := bothSignals(t, s); staleImage != false || note != "" {
		t.Fatalf("stale_image = %v, stale_binary = %q for equal bytes, want false and none", staleImage, note)
	}
}

// TestAServerWithNoCapturedIdentityStartsTheGuardFromItsOwnFile: a process that captured nothing (the test seam, a
// short CLI call) keeps the guard's old start: its own file, now.
func TestAServerWithNoCapturedIdentityStartsTheGuardFromItsOwnFile(t *testing.T) {
	old := startupIdentity
	startupIdentity = func() (buildinfo.Startup, bool) { return buildinfo.Startup{}, false }
	t.Cleanup(func() { startupIdentity = old })
	s := delegateTestServer(t, staleLocalRun())
	if !s.exe.known {
		t.Fatal("a server with no captured identity could not read its own executable")
	}
	if note := s.staleBinaryNote(buildinfo.Version); note != "" {
		t.Fatalf("a server on the executable it started from carries the note %q", note)
	}
}

// TestAGonePathIsStaleWithoutComparingAnything: there is nothing to read at a path that is gone (an OS that unlinks the
// old file), so the guard says so and no hash is attempted; process.stale_image gives no verdict there (the installed
// file cannot be read), and the note does not claim its bytes were compared or could not be.
func TestAGonePathIsStaleWithoutComparingAnything(t *testing.T) {
	s, _, path := serverOnInstalledImage(t, "0.183.0")
	hashes := 0
	real := hashInstalledImage
	hashInstalledImage = func(p string) (string, error) { hashes++; return real(p) }
	t.Cleanup(func() { hashInstalledImage = real })
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	staleImage, note := bothSignals(t, s)
	if staleImage != nil {
		t.Fatalf("process.stale_image = %v for a file that is gone, want the key absent", staleImage)
	}
	if !strings.Contains(note, "no longer at its path") || strings.Contains(note, "compared") {
		t.Fatalf("stale_binary = %q, want the note that the file is gone, with no claim about its bytes", note)
	}
	// The process block's own check hashes nothing either: the stat fails first. No read of the gone file anywhere.
	if hashes != 0 {
		t.Fatalf("a gone file was hashed %d time(s)", hashes)
	}
}

// The watch itself asks for no comparison when the path is gone.
func TestAGonePathAsksForNoComparison(t *testing.T) {
	exe := &staleMovableExe{cur: staleExeThen}
	w := newExeWatch(exe.read)
	exe.set(exeStamp{Missing: true}, nil)
	asked := 0
	note := w.staleNoteWith(buildinfo.Version, func() imageCheck { asked++; return imageCheck{} })
	if asked != 0 || !strings.Contains(note, "no longer") {
		t.Fatalf("asked for %d comparison(s), note %q: a path that is gone is stale without one", asked, note)
	}
}
