package gpulease

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// The deterministic form of TestAHeldLeaseNeverReadsAbsentWhileItsRecordIsRewritten. While a rename replaces a record, the system
// holds a handle on the old entry that has DELETE access and shares only read and write. Windows checks sharing both ways: an open
// whose own share mode lacks FILE_SHARE_DELETE collides with a handle that has DELETE access, and fails with
// ERROR_SHARING_VIOLATION. os.Open and os.ReadFile open that way. The test holds such a handle on a record and reads it: the plain
// read fails (the control, so the test cannot pass by no longer modelling the race), and the read the lease code uses does not.
func TestARecordReadSucceedsWhileAnotherHandleHoldsDeleteAccessOnIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "1.json")
	if err := os.WriteFile(path, []byte(`{"epoch":1}`), 0o666); err != nil {
		t.Fatal(err)
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(p, windows.DELETE, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("opening the record for delete access: %v", err)
	}
	defer syscall.CloseHandle(h)

	if _, err := os.ReadFile(path); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("control: os.ReadFile of a record with a delete-access handle open returned %v, want ERROR_SHARING_VIOLATION: the test no longer models the race", err)
	}
	got, err := readRecordFile(path)
	if err != nil {
		t.Fatalf("reading a record while another handle holds delete access on it: %v", err)
	}
	if string(got) != `{"epoch":1}` {
		t.Errorf("read %q, want the record", got)
	}
}
