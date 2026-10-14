package mcpserver

// stale_share_test.go: the stale server's read of the installed image does not block a rename of it (D-36).
//
// With the notice on every door, the first call of ANY door on a stale server hashes the freshly installed file (about 0.3 s
// for 60 MB). Go's os.Open on Windows grants no FILE_SHARE_DELETE, so a rollback or a second swap landing while that read
// is open fails on a sharing violation, and node-swap's holder diagnosis does not see a stale server (its image path was
// renamed away) and ends in "no process holds it".

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTheInstalledImageIsOpenedWithoutBlockingARename: the opener the hash reads the installed file through lets the file be
// renamed aside while it is open.
func TestTheInstalledImageIsOpenedWithoutBlockingARename(t *testing.T) {
	p := filepath.Join(t.TempDir(), "local-offload.exe")
	if err := os.WriteFile(p, imageBody("0.184.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := installedOpen(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(p, p+".bak-rollback"); err != nil {
		t.Fatalf("a rollback could not rename the installed image aside while a stale server held it open: %v", err)
	}
}

// TestTheHashOfTheInstalledImageOpensThroughThatOpener: the default hash reads through installedOpen, so the test above is about
// the open the hash really makes.
func TestTheHashOfTheInstalledImageOpensThroughThatOpener(t *testing.T) {
	p := filepath.Join(t.TempDir(), "local-offload.exe")
	if err := os.WriteFile(p, imageBody("0.184.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	real := installedOpen
	opens := 0
	installedOpen = func(path string) (*os.File, error) { opens++; return real(path) }
	t.Cleanup(func() { installedOpen = real })
	sum, err := hashInstalledImage(p)
	if err != nil || len(sum) != 64 {
		t.Fatalf("hashInstalledImage = %q, %v", sum, err)
	}
	if opens != 1 {
		t.Fatalf("the hash opened the file %d times through installedOpen, want once", opens)
	}
}
