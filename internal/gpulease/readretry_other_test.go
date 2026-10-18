//go:build !windows

package gpulease

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Off Windows a permission error on an open is a real, lasting refusal: a file with no read bit for the caller. POSIX has no
// delete-pending state (rename(2) is atomic and an open descriptor never holds a rename up), so no wait can make that open pass,
// and the Windows wait for the instant of a rename (readretry_test.go) is pure cost here. Measured on a Linux host, a mode-000
// record: a read took about 108 ms where the plain read it replaced took microseconds, Leases() cost 108 ms per such record, and an
// acquirer that reads the records under the epoch lock took 4.3 s with eight of them, long enough for a second acquirer to give
// up on the lock. This test pins the other half: the open is made once and the refusal is returned at once.

// TestOffWindowsAPermissionErrorIsReturnedAtOnce: one open, then the error, for both readers of a lease file, with the refusal
// supplied by the seam (deterministic) and with a real file the caller cannot read.
func TestOffWindowsAPermissionErrorIsReturnedAtOnce(t *testing.T) {
	readers := map[string]func(path string) error{
		"readSharedFile": func(path string) error { _, err := readSharedFile(path); return err },
		"readRecordFile": func(path string) error { _, err := readRecordFile(path); return err },
	}
	for name, read := range readers {
		t.Run(name+"/refused by the seam", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "1.json")
			if err := os.WriteFile(path, []byte("{}"), 0o666); err != nil {
				t.Fatal(err)
			}
			calls := failingOpens(t, 1<<30, os.ErrPermission)
			start := time.Now()
			err := read(path)
			took := time.Since(start)
			if !errors.Is(err, os.ErrPermission) {
				t.Fatalf("returned %v, want the permission error", err)
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("made %d opens, want 1 (took %v)", got, took)
			}
		})
		t.Run(name+"/unreadable file", func(t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a mode-000 file")
			}
			path := filepath.Join(t.TempDir(), "1.json")
			if err := os.WriteFile(path, []byte("{}"), 0o666); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(path, 0o666) })
			calls := failingOpens(t, 0, nil)
			start := time.Now()
			err := read(path)
			took := time.Since(start)
			if !errors.Is(err, os.ErrPermission) {
				t.Fatalf("returned %v, want the permission error", err)
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("made %d opens, want 1 (took %v)", got, took)
			}
		})
	}
}
