//go:build windows

package fleetnode

import (
	"path/filepath"
	"syscall"
	"testing"
)

// blockRemoval makes os.RemoveAll of jobDir fail: the context doc is held open
// by a handle that does not share DELETE, the way a process still reading a
// file keeps it from being removed. The handle is opened with CreateFile and an
// explicit share mode so the test says what it needs; os.Open would hold the
// file the same way, because the syscall package opens files with
// FILE_SHARE_READ|FILE_SHARE_WRITE and never FILE_SHARE_DELETE.
func blockRemoval(t *testing.T, jobDir string) (release func()) {
	t.Helper()
	name, err := syscall.UTF16PtrFromString(filepath.Join(jobDir, "context", "doc.txt"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("opening the doc without share-delete: %v", err)
	}
	return func() { _ = syscall.CloseHandle(h) }
}
