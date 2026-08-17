package mediahash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustDigest(t *testing.T, path string, maxFull int64) string {
	t.Helper()
	id, err := Digest(path, maxFull)
	if err != nil {
		t.Fatalf("Digest(%s): %v", path, err)
	}
	if !id.OK() {
		t.Fatalf("Digest(%s) returned no error but no identity", path)
	}
	return id.Digest
}

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
	if mustDigest(t, a, 0) != mustDigest(t, b, 0) {
		t.Fatalf("identical content produced different digests:\n  %s\n  %s", mustDigest(t, a, 0), mustDigest(t, b, 0))
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
	before := mustDigest(t, p, 0)

	if err := os.WriteFile(p, []byte("BBBBBBBBBBBBBBBB"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if mustDigest(t, p, 0) == before {
		t.Fatal("a same-size, same-mtime content replacement kept the digest — the cache would serve the OLD file's result")
	}
}

func TestTouchDoesNotChangeTheDigest(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "x.wav", []byte("stable content"))
	before := mustDigest(t, p, 0)
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	if mustDigest(t, p, 0) != before {
		t.Fatal("touch changed the digest; an unchanged file must keep its identity")
	}
}

// Sampled and full digests of the same file must never be confused, or enabling
// the option once would silently revalidate against a weaker identity.
func TestSampledAndFullDigestsAreDistinguishable(t *testing.T) {
	dir := t.TempDir()
	body := make([]byte, 1<<20)
	for i := range body {
		body[i] = byte(i)
	}
	p := write(t, dir, "big.bin", body)

	full := mustDigest(t, p, 0)
	samp := mustDigest(t, p, 1024)
	if full == samp {
		t.Fatal("sampled and full digests collided")
	}
	if !strings.Contains(full, ":sha256:") {
		t.Errorf("full digest does not name its mode: %s", full)
	}
	if !strings.Contains(samp, ":sampled:") {
		t.Errorf("sampled digest does not name its mode: %s", samp)
	}
	if mustDigest(t, p, 1024) != samp {
		t.Error("sampled digest is not stable across calls")
	}
}

// THE LOAD-BEARING CONTRACT. An unidentifiable input must produce an ERROR, never
// a digest.
//
// An earlier version returned a synthetic `media:staterr:<hash(path+err)>`
// string so callers never had to branch. Callers then used it as a cache key —
// which is a PATH key, exactly what this package abolishes. A transient read
// failure wrote a durable entry that a different file at the same path later hit.
func TestUnidentifiableInputsReturnAnErrorNotADigest(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ name, path string }{
		{"missing", filepath.Join(dir, "nope.wav")},
		{"empty path", ""},
		{"directory", dir},
	} {
		id, err := Digest(tc.path, 0)
		if err == nil {
			t.Errorf("%s: got identity %q with no error — it would be used as a cache key", tc.name, id.Digest)
		}
		if id.OK() {
			t.Errorf("%s: identity must be unusable on error, got %q", tc.name, id.Digest)
		}
	}
}

// Two different missing files must not be distinguishable-but-usable either:
// the point is that NEITHER yields a key.
func TestNoErrorPathYieldsAUsableKey(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{filepath.Join(dir, "a.wav"), filepath.Join(dir, "b.wav")} {
		if id, err := Digest(p, 0); err == nil || id.OK() {
			t.Fatalf("%s produced a usable key (%q, err=%v)", p, id.Digest, err)
		}
	}
	// Once the file exists it must produce a real digest — a transient failure
	// must not be sticky.
	real := write(t, dir, "a.wav", []byte("now it exists"))
	if id, err := Digest(real, 0); err != nil || !strings.Contains(id.Digest, ":sha256:") {
		t.Fatalf("digest did not recover once readable: %q err=%v", id.Digest, err)
	}
}

// Sampled mode must not read the same bytes repeatedly. For any file at or under
// one window all three offsets collapse to 0, and the previous version hashed it
// three times — costing 3x the I/O of the full hash it exists to avoid, for
// identical exactness. Any maxFullBytes below 8 MiB reached that range.
func TestSampledOffsetsAreDeduplicated(t *testing.T) {
	cases := []struct {
		size int64
		want int
	}{
		{1, 1},
		{sampleWindow - 1, 1},
		{sampleWindow, 1},
		// Just over one window: head starts at 0, the middle still rounds to 0,
		// and only the tail differs — 2 distinct reads, not 3. Asserting 3 here
		// would be asserting a duplicate read, which is the bug under test.
		{sampleWindow + 1, 2},
		{2 * sampleWindow, 3},
		{3 * sampleWindow, 3},
	}
	for _, c := range cases {
		got := dedupeOffsets(c.size)
		if len(got) != c.want {
			t.Errorf("size=%d: %d offsets %v, want %d distinct", c.size, len(got), got, c.want)
		}
		seen := map[int64]bool{}
		for _, o := range got {
			if seen[o] {
				t.Errorf("size=%d: duplicate offset %d in %v", c.size, o, got)
			}
			seen[o] = true
			if o < 0 {
				t.Errorf("size=%d: negative offset %d", c.size, o)
			}
		}
	}
}

// Content differing ONLY outside the sampled windows is the sampled mode's known
// blind spot. Pin it as a measured limitation rather than a claim.
func TestSampledModeBlindSpotIsDocumentedByTest(t *testing.T) {
	dir := t.TempDir()
	n := 40 << 20 // larger than the three 8 MiB windows can cover
	a := make([]byte, n)
	for i := range a {
		a[i] = byte(i)
	}
	b := append([]byte(nil), a...)
	b[9<<20] ^= 0xFF // between the head and middle windows

	pa := write(t, dir, "a.bin", a)
	pb := write(t, dir, "b.bin", b)

	if mustDigest(t, pa, 0) == mustDigest(t, pb, 0) {
		t.Fatal("FULL digests collided for different content — that would be a real hash failure")
	}
	if mustDigest(t, pa, 1<<20) != mustDigest(t, pb, 1<<20) {
		t.Skip("sampled windows happened to cover the mutated byte; the blind spot is offset-dependent")
	}
	t.Log("CONFIRMED known limitation: sampled mode cannot see content outside its windows — this is why it is opt-in and never the default")
}

// --- Ident.Unchanged: the TOCTOU detector itself ---
//
// These exist because the detector had NO direct coverage. The pipeline's gate
// tests inject a `mediaUnchanged` seam, so they exercise what the pipeline does
// with the answer but never the logic that produces it — replacing the whole of
// Unchanged with `return true` left the entire suite green. That is the same
// shape as the round-3 finding on this branch ("the gate tests never constructed
// a Pipeline"), one layer down: a load-bearing claim in CHANGELOG.md and
// docs/systems/offload-pipeline.md ("the file is re-stat'ed after the consuming
// read and compared against what the digest saw") rested on untested code.

func TestUnchangedIsTrueForAnUntouchedFile(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "steady.wav", []byte("bytes that do not move"))
	id, err := Digest(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !id.Unchanged(p) {
		t.Fatal("Unchanged reported a difference on a file nothing touched — every call would be treated as unidentifiable and nothing would ever cache")
	}
}

func TestUnchangedDetectsASizeChange(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "grow.wav", []byte("short"))
	id, err := Digest(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The append case: the digest covers a prefix, ffmpeg would read more.
	if err := os.WriteFile(p, []byte("short plus considerably more"), 0o600); err != nil {
		t.Fatal(err)
	}
	if id.Unchanged(p) {
		t.Fatal("a file that grew after hashing passed the re-stat — the transcript of the LONGER audio would be stored under sha256(prefix)")
	}
}

func TestUnchangedDetectsAnMtimeChangeAtEqualSize(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "swap.wav", []byte("take one"))
	id, err := Digest(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Same LENGTH, different content — the replacement case the digest exists to
	// catch, distinguished here only by mtime.
	if err := os.WriteFile(p, []byte("take two"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Force a distinct mtime: the (size, mtime) detector is explicitly documented
	// as blind to a same-size overwrite inside one tick, so pinning it is what
	// makes this test about the comparison rather than about clock granularity.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	if id.Unchanged(p) {
		t.Fatal("a same-size replacement with a moved mtime passed the re-stat — take one's transcript would be cached under take two's digest")
	}
}

func TestUnchangedIsFalseWhenTheFileIsGone(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "vanish.wav", []byte("here for now"))
	id, err := Digest(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if id.Unchanged(p) {
		t.Fatal("a deleted file passed the re-stat — a failed stat must never read as agreement")
	}
}

func TestUnchangedIsFalseOnAnIdentWithNoIdentity(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "real.wav", []byte("a real file"))
	// The zero Ident is what Digest returns alongside an error. It must never
	// validate against anything: `Ident{}.Unchanged(p)` returning true would make
	// a failed digest look like a confirmed identity and re-open the path-keyed
	// false hit this package exists to remove.
	var zero Ident
	if zero.Unchanged(p) {
		t.Fatal("the zero Ident validated against a real file — a failed digest would be treated as a confirmed identity")
	}
}
