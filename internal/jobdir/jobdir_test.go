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
// (padding, a CRLF from an editor), but the terminating newline WriteOwner
// always writes is required: a file that stops short of it is a write caught
// in the middle, and its digits are a prefix of the real id.
func TestReadOwnerTable(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantPID int
		wantOK  bool
	}{
		{"trailing newline", "4242\n", 4242, true},
		{"no terminating newline", "4242", 0, false},
		{"crlf and padding", "  4242 \r\n", 4242, true},
		{"largest 31-bit id", "2147483647\n", 2147483647, true},
		{"empty", "", 0, false},
		{"whitespace only", " \r\n\t ", 0, false},
		{"letters", "abc\n", 0, false},
		{"digits then junk", "4242abc\n", 0, false},
		{"two numbers", "4242 4243\n", 0, false},
		{"negative", "-5\n", 0, false},
		{"explicit plus", "+7\n", 0, false},
		{"zero", "0\n", 0, false},
		{"decimal point", "12.5\n", 0, false},
		{"past 31 bits", "2147483648\n", 0, false},
		{"far past any id", "99999999999999999999\n", 0, false},
		{"longer than any id", strings.Repeat("7", maxOwnerBytes+10) + "\n", 0, false},
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

// A reader that catches a marker while it is being written must not take the
// part it sees for an owner: "41728\n" cut after four digits is the id 4172,
// another process that may well be dead, and the sweep would remove a live
// run's dir on its word. Every proper prefix reads as "no owner"; only the
// whole marker names one.
func TestReadOwnerRejectsEveryProperPrefixOfAMarker(t *testing.T) {
	const marker = "41728\n"
	for n := 0; n < len(marker); n++ {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, OwnerFile), []byte(marker[:n]), 0o644); err != nil {
			t.Fatal(err)
		}
		if pid, ok := ReadOwner(dir); ok || pid != 0 {
			t.Errorf("a marker cut after %d byte(s) (%q) = (%d, %v), want (0, false)", n, marker[:n], pid, ok)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, OwnerFile), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	if pid, ok := ReadOwner(dir); !ok || pid != 41728 {
		t.Errorf("the whole marker = (%d, %v), want (41728, true)", pid, ok)
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
