package gpulease

// openshared_test.go: reading a harness image must not stand in the way of a rename of it (D-36).
//
// On Windows a rename of a file, or a delete of it, needs FILE_SHARE_DELETE on every handle already open against it, and Go's
// os.Open grants only FILE_SHARE_READ|FILE_SHARE_WRITE. A stale MCP server reads the installed executable (its hash, its build
// marker) on the first call after a deploy, and with the notice on every door that is any call, so a rollback or a second swap
// landing in that window failed with a sharing violation and node-swap's holder diagnosis could not see the cause.

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeImageFile(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("fake build padding\x00"+BuildMarkerPrefix+"9.9.9\x00tail"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestOpenSharedLetsTheFileBeRenamedWhileItIsOpen: the first step of a rollback or a second swap is a rename of the installed
// file aside; it must succeed while a reader holds the file open, and the reader goes on reading the bytes it opened.
func TestOpenSharedLetsTheFileBeRenamedWhileItIsOpen(t *testing.T) {
	p := writeImageFile(t, "local-offload.exe")
	f, err := OpenShared(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	aside := p + ".bak-rollback"
	if err := os.Rename(p, aside); err != nil {
		t.Fatalf("the installed image could not be renamed aside while a reader had it open: %v", err)
	}
	b, err := io.ReadAll(f)
	if err != nil || len(b) == 0 {
		t.Fatalf("the open handle stopped reading after the rename: %d bytes, %v", len(b), err)
	}
	if _, err := os.Stat(aside); err != nil {
		t.Fatalf("the renamed file is not where it was moved: %v", err)
	}
}

// TestOpenSharedReportsAMissingFileAsOne: a caller tells "no such file" from a read failure by errors.Is(err, fs.ErrNotExist).
func TestOpenSharedReportsAMissingFileAsOne(t *testing.T) {
	_, err := OpenShared(filepath.Join(t.TempDir(), "gone.exe"))
	if err == nil || !os.IsNotExist(err) {
		t.Fatalf("OpenShared(missing) = %v, want an error os.IsNotExist reports", err)
	}
}

// TestTheAuditReaderDoesNotBlockARenameEither: ReadBuildMarkerVersion (the stale-image check's reader and gpu doctor's) opens its
// file through openAudited; the opener the audit uses by default is the sharing one.
func TestTheAuditReaderDoesNotBlockARenameEither(t *testing.T) {
	p := writeImageFile(t, "local-offload.exe")
	f, err := openAudited(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(p, p+".bak-rollback"); err != nil {
		t.Fatalf("the installed image could not be renamed aside while the audit reader had it open: %v", err)
	}
}

// TestTheBuildMarkerStillReadsThroughTheSharedOpen: the sharing open changes nothing about what the reader finds.
func TestTheBuildMarkerStillReadsThroughTheSharedOpen(t *testing.T) {
	p := writeImageFile(t, "local-offload.exe")
	v, err := ReadBuildMarkerVersion(p)
	if err != nil || v != "9.9.9" {
		t.Fatalf("ReadBuildMarkerVersion = %q, %v, want 9.9.9", v, err)
	}
}

// longPathImage writes an image file whose path is longer than MAX_PATH (260): a deep tree under a scan root, or a long backup
// path, is what the audit and the stale-image check can be pointed at. The file is made through os calls. A Windows host whose
// policy does not allow a path that long (LongPathsEnabled off) cannot build one at all: the test is skipped there, since
// os.Open would not open it either (Go 1.23+ marks the process long-path aware and adds no prefix of its own).
func longPathImage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for len(dir) < 330 {
		dir = filepath.Join(dir, strings.Repeat("d", 60))
	}
	p := filepath.Join(dir, "local-offload.exe")
	err := os.MkdirAll(dir, 0o755)
	if err == nil {
		err = os.WriteFile(p, []byte("fake build padding\x00"+BuildMarkerPrefix+"9.9.9\x00tail"), 0o755)
	}
	if err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("this host cannot create a %d-character path (long paths are not enabled): %v", len(p), err)
		}
		t.Fatalf("cannot build the %d-character path: %v", len(p), err)
	}
	return p
}

// TestOpenSharedOpensAPathLongerThanMaxPath: the sharing open takes the path as os.Open does, so a long path that os.Open read
// is read here too, and through the audit's opener, and the share bit survives it (the file can be renamed while it is open).
// A parity pin: on the toolchain this repository builds with, os.Open adds no extended-length prefix of its own (the runtime
// marks the process long-path aware), so OpenShared has nothing to mirror; if that ever changes this test goes red first.
func TestOpenSharedOpensAPathLongerThanMaxPath(t *testing.T) {
	p := longPathImage(t)
	if len(p) < 261 {
		t.Fatalf("setup: the path is %d characters, want more than MAX_PATH", len(p))
	}
	for name, open := range map[string]func(string) (*os.File, error){"OpenShared": OpenShared, "openAudited": openAudited} {
		f, err := open(p)
		if err != nil {
			t.Fatalf("%s(%d-character path): %v", name, len(p), err)
		}
		b, err := io.ReadAll(f)
		f.Close()
		if err != nil || len(b) == 0 {
			t.Fatalf("%s read %d bytes, %v", name, len(b), err)
		}
	}
	f, err := OpenShared(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(p, p+".bak-rollback"); err != nil {
		t.Fatalf("the long-path image could not be renamed aside while a reader had it open: %v", err)
	}
}
