package jobdir

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The marker holds this process's id as decimal text, directly inside the job
// dir, and reads back as the same id.
func TestWriteOwnerRecordsThisProcess(t *testing.T) {
	dir := t.TempDir()
	if err := WriteOwner(dir); err != nil {
		t.Fatalf("WriteOwner: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, OwnerFile))
	if err != nil {
		t.Fatalf("marker not written beside the job dir's contents: %v", err)
	}
	if got, want := strings.TrimSpace(string(raw)), strconv.Itoa(os.Getpid()); got != want {
		t.Fatalf("marker text = %q, want this process's id %q", got, want)
	}
	pid, ok := ReadOwner(dir)
	if !ok || pid != os.Getpid() {
		t.Fatalf("ReadOwner = (%d, %v), want (%d, true)", pid, ok, os.Getpid())
	}
}

// A marker that is not a usable id reads as "no owner" (ok=false), never as an
// id: the sweep must not act on garbage, and ok=false sends a directory down
// the conservative unmarked path. Whitespace around a good id is tolerated
// (a trailing newline, a CRLF from an editor).
func TestReadOwnerTable(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantPID int
		wantOK  bool
	}{
		{"plain", "4242", 4242, true},
		{"trailing newline", "4242\n", 4242, true},
		{"crlf and padding", "  4242 \r\n", 4242, true},
		{"largest 31-bit id", "2147483647", 2147483647, true},
		{"empty", "", 0, false},
		{"whitespace only", " \r\n\t ", 0, false},
		{"letters", "abc", 0, false},
		{"digits then junk", "4242abc", 0, false},
		{"two numbers", "4242 4243", 0, false},
		{"negative", "-5", 0, false},
		{"explicit plus", "+7", 0, false},
		{"zero", "0", 0, false},
		{"decimal point", "12.5", 0, false},
		{"past 31 bits", "2147483648", 0, false},
		{"far past any id", "99999999999999999999", 0, false},
		{"longer than any id", strings.Repeat("7", maxOwnerBytes+10), 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, OwnerFile), []byte(c.content), 0o644); err != nil {
				t.Fatal(err)
			}
			pid, ok := ReadOwner(dir)
			if pid != c.wantPID || ok != c.wantOK {
				t.Fatalf("ReadOwner(%q) = (%d, %v), want (%d, %v)", c.content, pid, ok, c.wantPID, c.wantOK)
			}
		})
	}
}

// No marker, a marker that is a directory, and a job dir that is gone all read
// as "no owner".
func TestReadOwnerMissingOrUnreadable(t *testing.T) {
	if pid, ok := ReadOwner(t.TempDir()); ok || pid != 0 {
		t.Fatalf("a dir without a marker = (%d, %v), want (0, false)", pid, ok)
	}
	if pid, ok := ReadOwner(filepath.Join(t.TempDir(), "gone")); ok || pid != 0 {
		t.Fatalf("a missing dir = (%d, %v), want (0, false)", pid, ok)
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, OwnerFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if pid, ok := ReadOwner(dir); ok || pid != 0 {
		t.Fatalf("a marker that is a directory = (%d, %v), want (0, false)", pid, ok)
	}
}

// A job dir that does not exist is an error the caller can act on, not a
// silent no-op that leaves the run unmarked.
func TestWriteOwnerIntoAMissingDirFails(t *testing.T) {
	if err := WriteOwner(filepath.Join(t.TempDir(), "gone")); err == nil {
		t.Fatal("WriteOwner into a missing dir must fail")
	}
}

// Writing again replaces the marker (a reused directory is re-owned, never
// left holding the previous owner's id).
func TestWriteOwnerReplacesAnExistingMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, OwnerFile), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteOwner(dir); err != nil {
		t.Fatal(err)
	}
	if pid, ok := ReadOwner(dir); !ok || pid != os.Getpid() {
		t.Fatalf("ReadOwner = (%d, %v), want this process", pid, ok)
	}
}
