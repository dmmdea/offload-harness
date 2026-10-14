//go:build windows

package gpulease

// openshared_windows_test.go: the control for the premise of OpenShared (D-36).
//
// The tests in openshared_test.go show a rename succeeding while OpenShared holds the file. That only means something if a plain
// os.Open does NOT allow it, which is the fact ADR 0085 and openshared_windows.go state. This test keeps that fact checked: it
// is about the operating system and the Go toolchain, not about the harness, so it has no mutant in production code. If a
// toolchain ever starts granting FILE_SHARE_DELETE in os.Open it goes red, and that is the signal to revisit OpenShared and the
// Windows facts of the ADR rather than a defect to fix here.

import (
	"os"
	"testing"
)

// TestAPlainOsOpenBlocksTheRenameThatOpenSharedAllows is the control: the same file, the same rename, the other opener.
func TestAPlainOsOpenBlocksTheRenameThatOpenSharedAllows(t *testing.T) {
	p := writeImageFile(t, "local-offload.exe")
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(p, p+".bak-rollback"); err == nil {
		t.Fatal("os.Open no longer blocks a rename of the file it holds open: this toolchain grants FILE_SHARE_DELETE there, so the " +
			"premise of OpenShared and of the Windows facts in ADR 0085 no longer holds; revisit both")
	}
}
