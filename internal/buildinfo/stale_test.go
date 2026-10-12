package buildinfo

// stale_test.go: the build hash a long-lived process stamps on its rows is the hash of the image it STARTED from.
//
// BuildSHA256 used to hash the executable lazily, on its first call, by the path os.Executable returns. A long-lived
// process (the MCP server) whose first delegation came after a rename-swap deploy hashed the NEW file at that path
// and stamped every row harness_build_sha256 of a build it was not running (D-36). CaptureStartup hashes once, at
// start; the tests choose the image path through the executable seam, so nothing here depends on what os.Executable
// answers after a swap on any OS.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func sumOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// useImage points the executable seam at a fresh file holding body and resets the latches, so each test starts as a
// new process would. It restores the production seam when the test ends.
func useImage(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "image.bin")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	oldExe := executable
	executable = func() (string, error) { return path, nil }
	resetForTest()
	t.Cleanup(func() {
		executable = oldExe
		resetForTest()
	})
	return path
}

// resetForTest returns the package to the state of a process that has not hashed anything or captured anything.
func resetForTest() {
	buildOnce = sync.Once{}
	buildSHA = ""
	buildImage = image{}
	startupMu.Lock()
	startup = nil
	startupMu.Unlock()
}

// The defect: capture over file A, overwrite the file with B, ask for the hash. The lazy hash returned B's.
func TestBuildSHA256IsTheImageCapturedAtStartNotAtFirstUse(t *testing.T) {
	path := useImage(t, "image A, the build this process started from")
	CaptureStartup()
	if err := os.WriteFile(path, []byte("image B, a rename-swap deployed it afterwards"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := BuildSHA256(), sumOf([]byte("image A, the build this process started from")); got != want {
		t.Fatalf("BuildSHA256 after a swap = %s, want the hash of the image captured at start, %s (the swapped file hashes to %s)",
			got, want, sumOf([]byte("image B, a rename-swap deployed it afterwards")))
	}
}

// The startup record carries the same hash the rows will carry, the path it was read from and that file's stat.
func TestCaptureStartupRecordsTheImageItHashed(t *testing.T) {
	path := useImage(t, "image A, the build this process started from")
	st := CaptureStartup()
	if st.Version != Version {
		t.Errorf("Version = %q, want the compiled-in %q", st.Version, Version)
	}
	if want := sumOf([]byte("image A, the build this process started from")); st.SHA256 != want || BuildSHA256() != want {
		t.Errorf("SHA256 = %q and BuildSHA256 = %q, both want %q", st.SHA256, BuildSHA256(), want)
	}
	if st.Path != path {
		t.Errorf("Path = %q, want the image path %q", st.Path, path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size != fi.Size() || !st.ModTime.Equal(fi.ModTime()) {
		t.Errorf("stat = (%d, %v), want the file's own (%d, %v)", st.Size, st.ModTime, fi.Size(), fi.ModTime())
	}
	got, ok := StartupIdentity()
	if !ok || got != st {
		t.Errorf("StartupIdentity = (%+v, %v), want the captured %+v", got, ok, st)
	}
}

// One capture per process: a second call returns the first answer, even over a different image.
func TestCaptureStartupIsOnceAndIdempotent(t *testing.T) {
	path := useImage(t, "image A, the build this process started from")
	first := CaptureStartup()
	if err := os.WriteFile(path, []byte("image B, a rename-swap deployed it afterwards"), 0o755); err != nil {
		t.Fatal(err)
	}
	second := CaptureStartup()
	if second != first {
		t.Fatalf("a second CaptureStartup answered %+v, want the first answer %+v", second, first)
	}
	if first.SHA256 == "" {
		t.Fatalf("the capture hashed nothing: %+v", first)
	}
}

// Guard: a short-lived CLI process never captures anything and keeps the lazy path, so a hook invocation does not
// hash the whole executable. With no capture there is no startup identity either.
func TestWithoutACaptureTheHashIsLazyAndThereIsNoStartupIdentity(t *testing.T) {
	path := useImage(t, "image A, the build this process started from")
	if _, ok := StartupIdentity(); ok {
		t.Fatal("a process that never captured reports a startup identity")
	}
	if err := os.WriteFile(path, []byte("image B, a rename-swap deployed it afterwards"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := BuildSHA256(), sumOf([]byte("image B, a rename-swap deployed it afterwards")); got != want {
		t.Fatalf("the lazy path hashed %s at first use, want the file as it was then, %s", got, want)
	}
}

// Guard: an image that cannot be hashed leaves the hash ABSENT ("" is unknown, never a value), and the capture still
// says where it looked.
func TestCaptureStartupOfAnUnreadableImageIsAbsentNotInvented(t *testing.T) {
	path := useImage(t, "image A, the build this process started from")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	st := CaptureStartup()
	if st.SHA256 != "" || BuildSHA256() != "" {
		t.Fatalf("an unreadable image produced a hash: %+v / %q", st, BuildSHA256())
	}
	if st.Path != path || st.Version != Version {
		t.Fatalf("the capture lost where it looked or the version: %+v", st)
	}
}
