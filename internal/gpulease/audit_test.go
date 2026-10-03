package gpulease

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBinary writes a file that is big enough to be taken for a harness binary. It
// carries the format signature only when aware, and the build marker when version != "".
func fakeBinary(t *testing.T, dir, name string, aware bool, version string) string {
	t.Helper()
	body := []byte(strings.Repeat("\x00MZ-padding-", 200))
	if aware {
		body = append(body, []byte("\x00"+FormatSignature+"\x00")...)
	}
	if version != "" {
		body = append(body, []byte("\x00offload-build-version="+version+"\x00")...)
	}
	body = append(body, []byte(strings.Repeat("tail", 100))...)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, body, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func findItem(r AuditReport, path string) *AuditItem {
	for i := range r.Items {
		if r.Items[i].Path == path {
			return &r.Items[i]
		}
	}
	return nil
}

func TestDoctorFlagsNonFormatAwareBinary(t *testing.T) {
	dir := t.TempDir()
	old := fakeBinary(t, dir, "local-offload-wrapper.exe", false, "")
	cur := fakeBinary(t, dir, "local-offload.exe", true, "9.9.9")
	bak := fakeBinary(t, dir, "local-offload.exe.bak-swap-20260101-000000", false, "")
	r := Audit(AuditOptions{Roots: []string{dir}})

	if r.Green {
		t.Fatalf("a host with a binary that predates the per-epoch fence must not be green: %+v", r)
	}
	if it := findItem(r, old); it == nil || it.Aware || it.Kind != KindBinary {
		t.Fatalf("the old wrapper copy must be reported not format-aware: %+v", it)
	}
	if it := findItem(r, bak); it == nil || it.Aware {
		t.Fatalf("a node-swap backup (what a rollback restores) must be audited: %+v", it)
	}
	it := findItem(r, cur)
	if it == nil || !it.Aware || it.Version != "9.9.9" {
		t.Fatalf("the current binary is aware and reports its build marker: %+v", it)
	}
	if len(r.Reasons) < 2 {
		t.Fatalf("each offender is a reason: %v", r.Reasons)
	}
}

func TestDoctorGreenWhenEveryCopyIsAware(t *testing.T) {
	dir := t.TempDir()
	a := fakeBinary(t, dir, "local-offload.exe", true, "1.2.3")
	r := Audit(AuditOptions{Roots: []string{dir}, SelfPath: a})
	if !r.Green || len(r.Reasons) != 0 {
		t.Fatalf("one aware binary and nothing else is green: %+v", r)
	}
}

// Fail closed: finding nothing is not an audit.
func TestDoctorIsNotGreenWhenNoBinaryIsFound(t *testing.T) {
	r := Audit(AuditOptions{Roots: []string{t.TempDir()}})
	if r.Green {
		t.Fatal("zero binaries found is not a green audit")
	}
	if len(r.Reasons) == 0 {
		t.Fatal("it must say why")
	}
}

func TestDoctorIsNotGreenOnAScanError(t *testing.T) {
	dir := t.TempDir()
	fakeBinary(t, dir, "local-offload.exe", true, "")
	r := Audit(AuditOptions{Roots: []string{dir, filepath.Join(dir, "does-not-exist")}})
	if r.Green {
		t.Fatal("a root that could not be read is an unaudited part of the host")
	}
}

func TestDoctorFlagsNodeReaderWithoutPerEpochFence(t *testing.T) {
	dir := t.TempDir()
	fakeBinary(t, dir, "local-offload.exe", true, "")
	renderDir := filepath.Join(dir, "render")
	if err := os.MkdirAll(renderDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(renderDir, "gpu-lock.mjs")
	if err := os.WriteFile(bad, []byte("export function checkInheritedLease(l){ return true }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := Audit(AuditOptions{Roots: []string{dir}})
	it := findItem(r, bad)
	if it == nil || it.Kind != KindNodeReader || it.Aware {
		t.Fatalf("a gpu-lock.mjs without the signature is a reader that predates the fence: %+v", it)
	}
	if r.Green {
		t.Fatal("an un-fenced Node reader keeps the host off green")
	}
	// The same file carrying the signature is fine.
	good := "// " + FormatSignature + "\nexport function checkInheritedLease(l){ return true }\n"
	if err := os.WriteFile(bad, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := Audit(AuditOptions{Roots: []string{dir}}); !r.Green {
		t.Fatalf("an aware reader must pass: %+v", r)
	}
}

// A running image that is not under any scan root is still audited (a fleet node or MCP
// server started from a binary that has since been replaced or moved).
func TestDoctorAuditsRunningImagesAndLeaseHolders(t *testing.T) {
	root := t.TempDir()
	fakeBinary(t, root, "local-offload.exe", true, "")
	elsewhere := t.TempDir()
	old := fakeBinary(t, elsewhere, "local-offload-wrapper.exe", false, "")
	r := Audit(AuditOptions{Roots: []string{root}, Images: []ProcImage{{PID: 4242, Path: old, ReadPath: old, Why: "lease holder"}}})
	it := findItem(r, old)
	if it == nil || it.Aware || len(it.PIDs) != 1 || it.PIDs[0] != 4242 {
		t.Fatalf("a running image off the scan roots must be audited with its pid: %+v", it)
	}
	if r.Green {
		t.Fatal("a running old image is not green")
	}
	// A failed process listing is itself an unaudited part of the host.
	r = Audit(AuditOptions{Roots: []string{root}, ImagesErr: os.ErrPermission})
	if r.Green {
		t.Fatal("an unreadable process table must not read as 'no running images'")
	}
}

func TestAuditSkipsNonBinaries(t *testing.T) {
	dir := t.TempDir()
	fakeBinary(t, dir, "local-offload.exe", true, "")
	for _, n := range []string{"local-offload.md", "local-offload-notes.json", "offload-harness.log"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("not a binary, mentions nothing"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := Audit(AuditOptions{Roots: []string{dir}})
	if len(r.Items) != 1 || !r.Green {
		t.Fatalf("only the binary is an item: %+v", r.Items)
	}
}

func TestWriteReaderAuditMarker(t *testing.T) {
	m, _ := newTestManager(t)
	if m.ReaderAuditResult() != "absent" {
		t.Fatalf("no marker yet: %s", m.ReaderAuditResult())
	}
	green := AuditReport{Green: true, Items: []AuditItem{{Path: "x", Kind: KindBinary, Aware: true}}}
	if err := m.WriteReaderAudit(green); err != nil {
		t.Fatal(err)
	}
	if !m.readerAuditGreen() || m.ReaderAuditResult() != "green" {
		t.Fatal("a green report must write the green marker")
	}
	red := AuditReport{Green: false, Reasons: []string{"x is not format-aware"}}
	if err := m.WriteReaderAudit(red); err != nil {
		t.Fatal(err)
	}
	if m.readerAuditGreen() || m.ReaderAuditResult() != "red" {
		t.Fatal("a red report must REVOKE a green marker, never leave a stale one")
	}
}

// Files are audited by name without walking anything (the PATH entries: a recursive walk
// of a system directory is both slow and pointless).
func TestAuditTakesNamedFiles(t *testing.T) {
	dir := t.TempDir()
	old := fakeBinary(t, dir, "local-offload-fleet.exe", false, "")
	r := Audit(AuditOptions{Files: []string{old}})
	it := findItem(r, old)
	if it == nil || it.Aware || it.Kind != KindBinary || r.Green {
		t.Fatalf("a named old binary is audited and fails: %+v", r)
	}
	missing := Audit(AuditOptions{Files: []string{filepath.Join(dir, "gone.exe")}})
	if missing.Green {
		t.Fatal("a named file that cannot be read is not green")
	}
	if len(missing.Reasons) == 0 || !strings.Contains(strings.Join(missing.Reasons, "\n"), "could not be scanned") {
		t.Fatalf("the finding must say the file could not be scanned, not that it predates the fence: %v", missing.Reasons)
	}
}

func TestIsHarnessBinaryName(t *testing.T) {
	for name, want := range map[string]bool{
		"local-offload.exe": true, "local-offload-wrapper.exe": true, "Local-Offload.EXE": true, "offload-harness.exe": true,
		"local-offload.exe.bak-swap-20260101": true, "local-offload": true,
		"local-offload.md": false, "local-offload.log": false, "other.exe": false, "local-offload-notes.json": false,
	} {
		if got := IsHarnessBinaryName(name, 1<<20); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
	if IsHarnessBinaryName("local-offload.exe", 10) {
		t.Error("a ten-byte file is not a binary")
	}
}

// The audit looks for FormatSignature in render/gpu-lock.mjs; the shipped file must
// carry it, or `gpu doctor` would call the repository's own reader un-fenced (and a typo
// on either side would silently do the opposite for a stale copy).
func TestShippedNodeReaderCarriesTheFormatSignature(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "render", "gpu-lock.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), FormatSignature) {
		t.Fatalf("render/gpu-lock.mjs does not carry %q", FormatSignature)
	}
	r := Audit(AuditOptions{Files: []string{filepath.Join("..", "..", "render", "gpu-lock.mjs")}})
	if it := findItem(r, filepath.Join("..", "..", "render", "gpu-lock.mjs")); it == nil || !it.Aware {
		t.Fatalf("the audit must judge the shipped reader aware: %+v", r.Items)
	}
}

// The scan reads in 1 MiB chunks; a signature that straddles a chunk boundary must still
// be found, or a binary would be judged by where its bytes happen to fall.
func TestScanFindsASignatureAcrossAChunkBoundary(t *testing.T) {
	for _, offset := range []int{(1 << 20) - 1, (1 << 20) - 10, (1 << 20) - len(FormatSignature)/2, 1 << 20, (3 << 20) - 5} {
		body := make([]byte, 3<<20+1024)
		copy(body[offset:], FormatSignature)
		p := filepath.Join(t.TempDir(), "local-offload.exe")
		if err := os.WriteFile(p, body, 0o755); err != nil {
			t.Fatal(err)
		}
		r := Audit(AuditOptions{Files: []string{p}})
		if len(r.Items) != 1 || !r.Items[0].Aware {
			t.Fatalf("signature at offset %d not found: %+v", offset, r.Items)
		}
	}
	// And the control: no signature anywhere is not aware.
	p := filepath.Join(t.TempDir(), "local-offload.exe")
	if err := os.WriteFile(p, make([]byte, 3<<20), 0o755); err != nil {
		t.Fatal(err)
	}
	if r := Audit(AuditOptions{Files: []string{p}}); r.Items[0].Aware {
		t.Fatal("an all-zero file is not aware")
	}
}
