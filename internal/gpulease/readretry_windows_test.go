package gpulease

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The control for readretry_test.go: a file that is delete-pending, which is what a rename leaves of the old entry for an instant, is
// refused to every opener with ERROR_ACCESS_DENIED, even one that shares delete. That is the failure the retry waits out, and the
// reason the seam's refusal there is a permission error. If the system ever stops answering that way this goes red, and the retry
// and its seam need another look.
func TestAnOpenOfADeletePendingFileFailsWithAccessDeniedEvenWhenItSharesDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "1.json")
	if err := os.WriteFile(path, []byte("{}"), 0o666); err != nil {
		t.Fatal(err)
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(p, windows.DELETE, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("opening the file for delete access: %v", err)
	}
	defer syscall.CloseHandle(h)
	disposition := struct{ DeleteFile uint32 }{DeleteFile: 1}
	if err := windows.SetFileInformationByHandle(windows.Handle(h), windows.FileDispositionInfo, (*byte)(unsafe.Pointer(&disposition)), uint32(unsafe.Sizeof(disposition))); err != nil {
		t.Fatalf("marking the file for deletion: %v", err)
	}

	f, err := OpenShared(path)
	if err == nil {
		f.Close()
		t.Fatal("OpenShared of a delete-pending file succeeded: the premise of the access-denied retry no longer holds")
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) || !errors.Is(err, os.ErrPermission) {
		t.Errorf("OpenShared of a delete-pending file returned %v, want ERROR_ACCESS_DENIED (a permission error)", err)
	}
}

// TestWindowsWaitsOutADeniedOpenByDefault: the wait is on in a Windows build without a test switching it on, so the gate that took it
// off for other systems (sharedOpenMeetsDeletePending) cannot have taken it off here. Three refusals, then the file.
func TestWindowsWaitsOutADeniedOpenByDefault(t *testing.T) {
	if !deniedOpenMayClear {
		t.Fatal("deniedOpenMayClear is false in a Windows build: a read would give up on the delete-pending refusal of a live lease's file")
	}
	path := filepath.Join(t.TempDir(), "hb.7")
	if err := os.WriteFile(path, []byte("424242"), 0o666); err != nil {
		t.Fatal(err)
	}
	calls := failingOpens(t, 3, os.ErrPermission)
	got, err := readSharedFile(path)
	if err != nil || string(got) != "424242" {
		t.Errorf("after three access-denied opens readSharedFile = %q, %v, want the file's content 424242", got, err)
	}
	if n := calls.Load(); n != 4 {
		t.Errorf("made %d opens, want 4 (three refused, one through)", n)
	}
}
