package gpulease

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The audit's discovery is by CONTENT as well as by name (finding of the P3 review: a
// wrapper copy under any other name, or the agent binary, read the lease directory and was
// never listed). A harness binary that links the lease reader carries its package import
// path in its symbol tables, so a big executable that carries one is judged by the format
// signature whatever it is called.
const (
	leaseReaderMarker = "github.com/dmmdea/offload-harness/internal/gpulease"
	lockReaderMarker  = "github.com/dmmdea/offload-harness/internal/gpulock"
)

// fakeLeaseReader writes a 2 MiB executable-shaped file that carries marker (when not
// empty) and the format signature (when aware).
func fakeLeaseReader(t *testing.T, dir, name, marker string, aware bool) string {
	t.Helper()
	body := bytes.Repeat([]byte("\x00MZ-padding-"), (2<<20)/12)
	if marker != "" {
		body = append(body, []byte("\x00"+marker+".(*Manager).Inspect\x00")...)
	}
	if aware {
		body = append(body, []byte("\x00"+FormatSignature+"\x00")...)
	}
	body = append(body, []byte(strings.Repeat("tail", 100))...)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, body, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A wrapper copy made for another repository can be called anything.
func TestAuditFindsAHarnessBinaryUnderAnyName(t *testing.T) {
	dir := t.TempDir()
	renamed := fakeLeaseReader(t, dir, "media-wrapper.exe", leaseReaderMarker, false)
	backup := fakeLeaseReader(t, dir, "film-runner.exe.bak-swap-20260101-000000", leaseReaderMarker, false)
	unrelated := fakeLeaseReader(t, dir, "some-other-tool.exe", "", false)

	r := Audit(AuditOptions{Roots: []string{dir}})
	for _, p := range []string{renamed, backup} {
		it := findItem(r, p)
		if it == nil || it.Kind != KindBinary || it.Aware {
			t.Fatalf("%s carries the lease reader and not the signature: it must be listed as not aware: %+v", p, it)
		}
	}
	if findItem(r, unrelated) != nil {
		t.Fatal("a big executable that carries no harness package path is not a harness binary and is not listed")
	}
	if r.Green {
		t.Fatalf("a renamed pre-format copy keeps the host off green: %+v", r)
	}
	if !strings.Contains(strings.Join(r.Reasons, "\n"), renamed) {
		t.Fatalf("the offender is named in the findings: %v", r.Reasons)
	}

	// The same copy carrying the signature is judged aware, whatever it is called.
	aware := fakeLeaseReader(t, t.TempDir(), "media-wrapper.exe", leaseReaderMarker, true)
	r = Audit(AuditOptions{Roots: []string{filepath.Dir(aware)}})
	if it := findItem(r, aware); it == nil || !it.Aware || !r.Green {
		t.Fatalf("an aware renamed copy is green: %+v", r)
	}
}

// Both packages that read the lease are identity markers: gpulock is the read-only view the
// pipeline takes, and it once carried a reader of its own.
func TestAuditIdentityMarkersCoverBothLeaseReaderPackages(t *testing.T) {
	for _, marker := range []string{leaseReaderMarker, lockReaderMarker} {
		dir := t.TempDir()
		p := fakeLeaseReader(t, dir, "wrapper.exe", marker, false)
		if it := findItem(Audit(AuditOptions{Roots: []string{dir}}), p); it == nil || it.Aware {
			t.Errorf("a binary carrying %q must be found and judged: %+v", marker, it)
		}
	}
}

// The scan reads in 1 MiB chunks; an identity marker that straddles a boundary must still
// be found, or a binary would be listed or not by where its bytes happen to fall.
func TestAuditFindsAnIdentityMarkerAcrossAChunkBoundary(t *testing.T) {
	for _, offset := range []int{(1 << 20) - 1, (1 << 20) - 10, (1 << 20) - len(leaseReaderMarker)/2, 1 << 20, (2 << 20) - 5} {
		body := make([]byte, 3<<20)
		copy(body[offset:], leaseReaderMarker)
		dir := t.TempDir()
		p := filepath.Join(dir, "wrapper.exe")
		if err := os.WriteFile(p, body, 0o755); err != nil {
			t.Fatal(err)
		}
		if it := findItem(Audit(AuditOptions{Roots: []string{dir}}), p); it == nil {
			t.Fatalf("an identity marker at offset %d was not found", offset)
		}
	}
}

// The agent binary links the lease reader (modelaffinity reads the lease before a seat
// load) and is deployed as local-agent: it is audited by name, and a running one by image.
func TestIsHarnessBinaryNameCoversTheAgentBinary(t *testing.T) {
	for name, want := range map[string]bool{
		"local-agent.exe": true, "Local-Agent.EXE": true, "local-agent": true,
		"local-agent.exe.bak-swap-20260101": true, "local-agent.md": false, "local-agent.log": false,
	} {
		if got := IsHarnessBinaryName(name, 1<<20); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
	dir := t.TempDir()
	old := fakeBinary(t, dir, "local-agent.exe", false, "")
	if it := findItem(Audit(AuditOptions{Roots: []string{dir}}), old); it == nil || it.Aware {
		t.Fatalf("an agent binary that predates the fence must be listed as not aware: %+v", it)
	}
}

// FAIL CLOSED on what cannot be examined: an entry whose stat fails is a finding, not a
// silent skip.
func TestAuditReportsAnEntryItCouldNotStat(t *testing.T) {
	dir := t.TempDir()
	fakeBinary(t, dir, "local-offload.exe", true, "")
	bad := filepath.Join(dir, "local-offload-copy.exe")
	if err := os.WriteFile(bad, make([]byte, 4096), 0o755); err != nil {
		t.Fatal(err)
	}
	old := statEntry
	statEntry = func(d fs.DirEntry) (fs.FileInfo, error) {
		if d.Name() == "local-offload-copy.exe" {
			return nil, os.ErrPermission
		}
		return old(d)
	}
	t.Cleanup(func() { statEntry = old })

	r := Audit(AuditOptions{Roots: []string{dir}})
	if r.Green {
		t.Fatal("an entry that could not be examined may be a harness binary: not green")
	}
	if !strings.Contains(strings.Join(r.Reasons, "\n"), bad) {
		t.Fatalf("the finding must name the entry: %v", r.Reasons)
	}
}

// A candidate that cannot be OPENED to be told apart from a harness binary is a finding too.
func TestAuditReportsAnExecutableItCouldNotRead(t *testing.T) {
	dir := t.TempDir()
	fakeBinary(t, dir, "local-offload.exe", true, "")
	p := fakeLeaseReader(t, dir, "locked-wrapper.exe", leaseReaderMarker, false)
	old := openAudited
	openAudited = func(path string) (*os.File, error) {
		if path == p {
			return nil, os.ErrPermission
		}
		return old(path)
	}
	t.Cleanup(func() { openAudited = old })

	r := Audit(AuditOptions{Roots: []string{dir}})
	if r.Green || !strings.Contains(strings.Join(r.Reasons, "\n"), p) {
		t.Fatalf("an executable that could not be read may be a harness binary: %v", r.Reasons)
	}
	if findItem(r, p) != nil {
		t.Fatal("a file that could not be read is a finding, not an item judged aware or not")
	}
}

// What the walk did not enter is listed (not a finding: node_modules and .git are never
// where a binary is kept), so a gap is visible instead of being a silent pass.
func TestAuditSaysWhatItDidNotSearch(t *testing.T) {
	dir := t.TempDir()
	fakeBinary(t, dir, "local-offload.exe", true, "")
	mk := func(parts ...string) string {
		p := filepath.Join(append([]string{dir}, parts...)...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	nodeModules := mk("node_modules")
	gitDir := mk(".git")
	deep := mk("a", "b", "c", "d") // four levels below the root
	deepBinary := fakeBinary(t, deep, "local-offload-deep.exe", false, "")
	three := mk("x", "y", "z") // three levels: inside the default cap
	threeBinary := fakeBinary(t, three, "local-offload-three.exe", true, "")

	r := Audit(AuditOptions{Roots: []string{dir}})
	all := strings.Join(r.NotSearched, "\n")
	for _, want := range []string{nodeModules, gitDir, deep} {
		if !strings.Contains(all, want) {
			t.Errorf("%s was not entered and must be listed as not searched:\n%s", want, all)
		}
	}
	if strings.Contains(all, three) {
		t.Errorf("a directory the walk entered must not be listed as skipped:\n%s", all)
	}
	if findItem(r, deepBinary) != nil {
		t.Error("the default cap does not reach four levels down")
	}
	if it := findItem(r, threeBinary); it == nil || !it.Aware {
		t.Errorf("the default cap reaches three levels below a root: %+v", it)
	}
	if !r.Green {
		t.Fatalf("a gap that is listed is not a finding: %v", r.Reasons)
	}

	// Widening the cap reaches it, and the red verdict follows.
	r = Audit(AuditOptions{Roots: []string{dir}, MaxDepth: 6})
	if it := findItem(r, deepBinary); it == nil || it.Aware || r.Green {
		t.Fatalf("with a deeper cap the old copy is found and fails the audit: %+v", r)
	}
	if strings.Contains(strings.Join(r.NotSearched, "\n"), deep) {
		t.Errorf("a directory that was searched is not listed as skipped: %v", r.NotSearched)
	}
}
