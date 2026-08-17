package mediahash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// THE REUSE CASE: the same bytes at a different path must produce the same
// identity. Under the old path+size+mtime scheme this always missed, which is
// precisely the reuse an artifact cache exists to capture.
func TestIdenticalContentAtDifferentPathsSharesADigest(t *testing.T) {
	dir := t.TempDir()
	body := []byte("the same audio bytes, twice")
	a := write(t, dir, "a.wav", body)
	b := write(t, dir, "sub-b.wav", body)
	// Different mtimes, to prove mtime is not an ingredient.
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(b, future, future); err != nil {
		t.Fatal(err)
	}
	if Digest(a, 0) != Digest(b, 0) {
		t.Fatalf("identical content produced different digests:\n  %s\n  %s", Digest(a, 0), Digest(b, 0))
	}
}

// THE FALSE-HIT CASE, and the reason this package exists: replacing a file at the
// same path with different content of the SAME SIZE and a preserved mtime used to
// keep the key identical, so the cache served the old file's result for the new
// one.
func TestSameSizeSameMtimeReplacementChangesTheDigest(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "take.wav", []byte("AAAAAAAAAAAAAAAA"))
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	before := Digest(p, 0)

	// Same length, different bytes; restore the original mtime exactly.
	if err := os.WriteFile(p, []byte("BBBBBBBBBBBBBBBB"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	after := Digest(p, 0)
	if before == after {
		t.Fatal("a same-size, same-mtime content replacement kept the digest — the cache would serve the OLD file's result")
	}
}

// Touching a file without changing its content must NOT change the identity.
func TestTouchDoesNotChangeTheDigest(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "x.wav", []byte("stable content"))
	before := Digest(p, 0)
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	if Digest(p, 0) != before {
		t.Fatal("touch changed the digest; an unchanged file must keep its identity")
	}
}

// Sampled and full digests of the same file must never be confused, or enabling
// the option once would silently invalidate-then-revalidate against a weaker
// identity.
func TestSampledAndFullDigestsAreDistinguishable(t *testing.T) {
	dir := t.TempDir()
	body := make([]byte, 1<<20)
	for i := range body {
		body[i] = byte(i)
	}
	p := write(t, dir, "big.bin", body)

	full := Digest(p, 0)
	samp := Digest(p, 1024) // 1 MiB file exceeds a 1 KiB threshold
	if full == samp {
		t.Fatal("sampled and full digests collided")
	}
	if !strings.Contains(full, ":sha256:") {
		t.Errorf("full digest does not name its mode: %s", full)
	}
	if !strings.Contains(samp, ":sampled:") {
		t.Errorf("sampled digest does not name its mode: %s", samp)
	}
	// Sampled must still be stable for the same file.
	if Digest(p, 1024) != samp {
		t.Error("sampled digest is not stable across calls")
	}
}

// A file that cannot be read must not collide with every other unreadable file,
// and must not look like a successful hash of empty content — a shared "error"
// key would let two different missing files serve each other's cached results.
func TestUnreadableFilesDoNotShareADigest(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "missing-a.wav")
	b := filepath.Join(dir, "missing-b.wav")
	da, db := Digest(a, 0), Digest(b, 0)
	if da == db {
		t.Fatal("two different missing files share a digest")
	}
	empty := write(t, dir, "empty.wav", nil)
	if da == Digest(empty, 0) {
		t.Fatal("a missing file and an empty file share a digest")
	}
	if !strings.HasPrefix(da, "media:staterr:") {
		t.Errorf("a missing file's digest does not name the failure: %s", da)
	}
	// And once the file appears, the digest must become the real one — a
	// transient failure must not poison the key permanently.
	real := write(t, dir, "missing-a.wav", []byte("now it exists"))
	if got := Digest(real, 0); got == da {
		t.Fatal("the digest did not recover once the file became readable")
	}
}

func TestEmptyPathAndDirectoryAreTheirOwnStates(t *testing.T) {
	if got := Digest("", 0); got != "media:none" {
		t.Errorf("empty path digest = %q", got)
	}
	dir := t.TempDir()
	if got := Digest(dir, 0); !strings.HasPrefix(got, "media:isdir:") {
		t.Errorf("directory digest = %q, want an isdir state", got)
	}
}

// Content differing ONLY outside the sampled windows is the sampled mode's known
// blind spot. Pin it as a documented limitation rather than letting a future
// reader assume sampled mode is exact.
func TestSampledModeBlindSpotIsDocumentedByTest(t *testing.T) {
	dir := t.TempDir()
	n := 40 << 20 // 40 MiB, so the three 8 MiB windows do not cover it
	a := make([]byte, n)
	for i := range a {
		a[i] = byte(i)
	}
	b := append([]byte(nil), a...)
	// Mutate a byte between the head window and the middle window.
	b[9<<20] ^= 0xFF

	pa := write(t, dir, "a.bin", a)
	pb := write(t, dir, "b.bin", b)

	if Digest(pa, 0) == Digest(pb, 0) {
		t.Fatal("FULL digests collided for different content — that would be a real hash failure")
	}
	if Digest(pa, 1<<20) != Digest(pb, 1<<20) {
		t.Skip("sampled windows happened to cover the mutated byte; the blind spot is offset-dependent")
	}
	t.Log("CONFIRMED known limitation: sampled mode cannot see content outside its windows — this is why it is opt-in and never the default")
}
