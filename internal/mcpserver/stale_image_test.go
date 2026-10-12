package mcpserver

// stale_image_test.go: offload_status's process block (D-36).
//
// A rename-swap deploy replaces the installed binary while every MCP server already running keeps the image it
// started from. The block names the image this process runs (the version and build hash it stamps on its rows) and
// compares it with the file now at the path it started from: stale_image true when the installed bytes differ,
// false when they are equal, and absent with a note when the installed file cannot be read (never "fresh" on a
// failure). The tests choose both files, so nothing depends on what os.Executable answers after a swap on any OS.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// imageBody is a fake build: padding, then the marker a real binary carries.
func imageBody(version string) []byte {
	return []byte("fake build padding\x00" + gpulease.BuildMarkerPrefix + version + "\x00tail")
}

// installImage writes an installed build at path and returns the startup identity of a process that began from it.
func installImage(t *testing.T, path, version string) buildinfo.Startup {
	t.Helper()
	body := imageBody(version)
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return buildinfo.Startup{Version: version, SHA256: hex.EncodeToString(sum[:]), Path: path, Size: fi.Size(), ModTime: fi.ModTime()}
}

// swapImage replaces the installed file the way a deploy does: other bytes (or the same ones), a later mtime.
func swapImage(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

// processBlock asks offload_status for the process block by name.
func processBlock(t *testing.T, s *Server) map[string]any {
	t.Helper()
	got := decodeObject(t, statusCall(t, s, json.RawMessage(`{"section":"process"}`)))
	block, _ := got["process"].(map[string]any)
	if block == nil {
		t.Fatalf("section process returned no object block: %v", got)
	}
	return block
}

func TestStaleImageIsTrueWhenTheInstalledBuildIsAnotherVersion(t *testing.T) {
	for _, tc := range []struct{ name, running, installed string }{
		{"a roll-forward deploy", "0.182.0", "0.183.0"},
		{"a rollback", "0.183.0", "0.182.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := statusFixture(t)
			path := filepath.Join(t.TempDir(), "local-offload.exe")
			st := installImage(t, path, tc.running)
			s.startup = &st
			swapImage(t, path, imageBody(tc.installed))
			block := processBlock(t, s)
			if block["stale_image"] != true {
				t.Errorf("stale_image = %v, want true after the installed build became %s: %v", block["stale_image"], tc.installed, block)
			}
			if block["installed_version"] != tc.installed {
				t.Errorf("installed_version = %v, want %s", block["installed_version"], tc.installed)
			}
			if block["version"] != tc.running || block["build_sha256"] != st.SHA256 {
				t.Errorf("the block must keep naming the image this process started from (%s, %s): %v", tc.running, st.SHA256, block)
			}
		})
	}
}

func TestStaleImageIsFalseForTheSameBytes(t *testing.T) {
	s, _, _ := statusFixture(t)
	path := filepath.Join(t.TempDir(), "local-offload.exe")
	st := installImage(t, path, "0.183.0")
	s.startup = &st
	if block := processBlock(t, s); block["stale_image"] != false || block["installed_version"] != "0.183.0" {
		t.Fatalf("an untouched installed image must read fresh: %v", block)
	}
	// A redeploy of the same build rewrites the file (a new mtime) and leaves the bytes alone: still not stale.
	swapImage(t, path, imageBody("0.183.0"))
	if block := processBlock(t, s); block["stale_image"] != false || block["installed_version"] != "0.183.0" {
		t.Fatalf("a rewrite with equal bytes must read fresh: %v", block)
	}
}

func TestStaleImageIsUnknownWhenTheInstalledFileCannotBeRead(t *testing.T) {
	s, _, _ := statusFixture(t)
	path := filepath.Join(t.TempDir(), "local-offload.exe")
	st := installImage(t, path, "0.183.0")
	s.startup = &st
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	block := processBlock(t, s)
	if _, present := block["stale_image"]; present {
		t.Fatalf("an unreadable installed file must not answer stale_image either way: %v", block)
	}
	note, _ := block["note"].(string)
	if !strings.Contains(note, "cannot be read") || !strings.Contains(note, path) {
		t.Errorf("the note must say the installed image cannot be read and name it, got %q", note)
	}
	if block["version"] != "0.183.0" || block["build_sha256"] != st.SHA256 {
		t.Errorf("the identity this process runs is still reported: %v", block)
	}
}

// A status call must not hash a whole executable every time (245 to 335 ms under load, 0.183.0): the installed file
// is hashed only once its stat has moved from the one the process saw, and once per stat.
func TestStaleImageHashesTheInstalledFileOnlyWhenItsStatMoved(t *testing.T) {
	s, _, _ := statusFixture(t)
	path := filepath.Join(t.TempDir(), "local-offload.exe")
	st := installImage(t, path, "0.182.0")
	s.startup = &st
	hashes := 0
	real := hashInstalledImage
	hashInstalledImage = func(p string) (string, error) { hashes++; return real(p) }
	t.Cleanup(func() { hashInstalledImage = real })

	for i := 0; i < 3; i++ {
		processBlock(t, s)
	}
	if hashes != 0 {
		t.Fatalf("an installed file whose stat is the one the process started from was hashed %d time(s)", hashes)
	}
	swapImage(t, path, imageBody("0.183.0"))
	for i := 0; i < 3; i++ {
		if block := processBlock(t, s); block["stale_image"] != true {
			t.Fatalf("call %d after the swap: %v", i, block)
		}
	}
	if hashes != 1 {
		t.Fatalf("three calls after one swap hashed the installed file %d time(s), want once", hashes)
	}
}

// The block is in the full answer, not only asked for by name, and says the same thing there.
func TestOffloadStatusProcessBlockCarriesStaleImage(t *testing.T) {
	s, _, _ := statusFixture(t)
	path := filepath.Join(t.TempDir(), "local-offload.exe")
	st := installImage(t, path, "0.182.0")
	s.startup = &st
	swapImage(t, path, imageBody("0.183.0"))
	full := decodeObject(t, statusCall(t, s, nil))
	block, _ := full["process"].(map[string]any)
	if block == nil || block["stale_image"] != true || block["installed_version"] != "0.183.0" || block["version"] != "0.182.0" {
		t.Fatalf("the full answer's process block must carry the stale image: %v", full["process"])
	}
}

// The fixture holds a fixed identity: the block is that identity and never the test binary's own.
func TestStatusProcessBlockIsTheInjectedIdentityNeverTheTestBinarys(t *testing.T) {
	s, _, _ := statusFixture(t)
	block := processBlock(t, s)
	if block["version"] != fixtureImageVersion || block["build_sha256"] != fixtureImageSHA() {
		t.Fatalf("the block must be the injected identity (%s, %s): %v", fixtureImageVersion, fixtureImageSHA(), block)
	}
	if block["version"] == buildinfo.Version || block["build_sha256"] == buildinfo.BuildSHA256() {
		t.Fatalf("the block carries the test binary's own identity: %v", block)
	}
}

// A server that holds no identity (not Run, as the test seam builds it) has no process block, like pair; asked for by
// name the block is empty, never null.
func TestStatusProcessBlockIsAbsentWithoutAnIdentity(t *testing.T) {
	s, _, _ := statusFixture(t)
	s.startup = nil
	if _, present := decodeObject(t, statusCall(t, s, nil))["process"]; present {
		t.Fatal("a server with no startup identity published a process block in the full answer")
	}
	got := decodeObject(t, statusCall(t, s, json.RawMessage(`{"section":"process"}`)))
	if block, ok := got["process"].(map[string]any); !ok || len(block) != 0 {
		t.Fatalf("asked by name with no identity held the block must be {}, got %v", got["process"])
	}
}

// Run takes the identity: the version/sha pair offload_status reports is the pair a row written by this process carries.
func TestRunHoldsTheStartupIdentityOfThisProcess(t *testing.T) {
	s, _, _ := statusFixture(t)
	s.startup = nil
	s.holdStartup()
	block := processBlock(t, s)
	if block["version"] != buildinfo.Version {
		t.Errorf("process.version = %v, want the compiled-in %s", block["version"], buildinfo.Version)
	}
	if sha, _ := block["build_sha256"].(string); sha == "" || sha != buildinfo.BuildSHA256() {
		t.Errorf("process.build_sha256 = %v, want the hash rows carry, %s", block["build_sha256"], buildinfo.BuildSHA256())
	}
}

// A failed read is a moment, not a verdict. An antivirus or indexer lock on the freshly renamed file makes ONE read
// fail, and the file's stat does not change afterwards, so a failure cached against the stat would read "unknown" for
// the life of the process while the file is readable and differs: the deploy this block exists to report on.
func TestStaleImageRetriesAfterATransientHashFailure(t *testing.T) {
	s, _, _ := statusFixture(t)
	path := filepath.Join(t.TempDir(), "local-offload.exe")
	st := installImage(t, path, "0.182.0")
	s.startup = &st
	swapImage(t, path, imageBody("0.183.0"))
	real := hashInstalledImage
	fail := true
	hashes := 0
	hashInstalledImage = func(p string) (string, error) {
		hashes++
		if fail {
			fail = false
			return "", errors.New("the process cannot access the file because it is being used by another process")
		}
		return real(p)
	}
	t.Cleanup(func() { hashInstalledImage = real })

	first := processBlock(t, s)
	if _, present := first["stale_image"]; present {
		t.Fatalf("a failed read must not answer either way: %v", first)
	}
	if note, _ := first["note"].(string); !strings.Contains(note, "cannot be read") {
		t.Errorf("the failed read must be said, got note %q", note)
	}
	second := processBlock(t, s)
	if second["stale_image"] != true || second["installed_version"] != "0.183.0" {
		t.Fatalf("the file is readable on the next call and differs: the answer must be stale_image true, got %v", second)
	}
	if _, present := second["note"]; present {
		t.Errorf("a clean read carries no note: %v", second)
	}
	// The retry itself, not only its outcome: a latched failure that left an empty hash behind would also read "stale".
	if hashes != 2 {
		t.Errorf("the failed hash is retried once and the good one kept: the installed file was hashed %d time(s), want 2", hashes)
	}
	processBlock(t, s)
	if hashes != 2 {
		t.Errorf("a successful hash is cached against the stat: hashed %d time(s) after a third call, want 2", hashes)
	}
}

// The same for the build marker, and a marker that cannot be read is said, not silently left out: installed_version
// is absent, and the note says why. The hash that did succeed is kept (a 60 MB read is not repeated for it).
func TestStaleImageRetriesAfterATransientMarkerFailureAndSaysSoMeanwhile(t *testing.T) {
	s, _, _ := statusFixture(t)
	path := filepath.Join(t.TempDir(), "local-offload.exe")
	st := installImage(t, path, "0.182.0")
	s.startup = &st
	swapImage(t, path, imageBody("0.183.0"))
	hashes := 0
	realHash, realMark := hashInstalledImage, readInstalledVersion
	hashInstalledImage = func(p string) (string, error) { hashes++; return realHash(p) }
	failMark := true
	readInstalledVersion = func(p string) (string, error) {
		if failMark {
			failMark = false
			return "", errors.New("read error at the build marker")
		}
		return realMark(p)
	}
	t.Cleanup(func() { hashInstalledImage, readInstalledVersion = realHash, realMark })

	first := processBlock(t, s)
	if first["stale_image"] != true {
		t.Errorf("the hash was read, so the comparison is made: %v", first)
	}
	if _, present := first["installed_version"]; present {
		t.Errorf("an unreadable marker leaves installed_version out: %v", first)
	}
	if note, _ := first["note"].(string); !strings.Contains(note, "build marker") {
		t.Errorf("the missing installed_version must be explained, got note %q", note)
	}
	second := processBlock(t, s)
	if second["installed_version"] != "0.183.0" || second["stale_image"] != true {
		t.Fatalf("the marker is readable on the next call: %v", second)
	}
	if _, present := second["note"]; present {
		t.Errorf("a clean read carries no note: %v", second)
	}
	if hashes != 1 {
		t.Errorf("only the failed read is retried: the installed file was hashed %d time(s), want once", hashes)
	}
}

// A process whose own start-up hash failed has no build_sha256 to publish, and says so on EVERY path, the fast one
// (the installed file's stat is the one the process started from) included: its ledger rows then carry no
// harness_build_sha256 either, and nothing else on the status surface tells the reader why.
func TestProcessBlockSaysWhenItsOwnStartupHashIsUnknown(t *testing.T) {
	s, _, _ := statusFixture(t)
	path := filepath.Join(t.TempDir(), "local-offload.exe")
	st := installImage(t, path, "0.183.0")
	st.SHA256 = "" // the self-hash failed at start, after the stat was taken
	s.startup = &st
	block := processBlock(t, s)
	if _, present := block["build_sha256"]; present {
		t.Fatalf("an unknown hash is absent, never invented: %v", block)
	}
	note, _ := block["note"].(string)
	if !strings.Contains(note, "harness_build_sha256") || !strings.Contains(note, "self-hash") {
		t.Fatalf("the missing hash must be explained (the self-hash failed, rows carry no harness_build_sha256), got note %q in %v", note, block)
	}
}

// offload_status's description tells a caller to start with section:"brief", and CLAUDE.md makes it the first call of
// every delegating session: a stale server must be visible THERE, not only in the full answer or when asked for by name.
func TestStatusBriefCarriesTheStaleImage(t *testing.T) {
	s, _, _ := statusFixture(t)
	path := filepath.Join(t.TempDir(), "local-offload.exe")
	st := installImage(t, path, "0.182.0")
	s.startup = &st
	swapImage(t, path, imageBody("0.183.0"))
	brief := decodeObject(t, statusCall(t, s, json.RawMessage(`{"section":"brief"}`)))
	block, _ := brief["process"].(map[string]any)
	if block == nil || block["stale_image"] != true || block["installed_version"] != "0.183.0" || block["version"] != "0.182.0" {
		t.Fatalf("the brief must carry the process block with the stale image: %v", brief["process"])
	}
	// A server that holds no identity has no block there either (absent, as in the full answer), never a null.
	s.startup = nil
	if _, present := decodeObject(t, statusCall(t, s, json.RawMessage(`{"section":"brief"}`)))["process"]; present {
		t.Fatal("a server with no startup identity published a process block in the brief")
	}
}
