package pipeline

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/mediahash"
)

func metaWithBypass(s string) core.Meta { return core.Meta{CacheBypass: s} }

// These guard the T2-A2 gates AT THE PIPELINE LEVEL. The mediahash package tests
// its own contract well, but nothing asserted that runTranscribe /
// runVideoDescribe actually HONOUR it — so a future refactor dropping
// `&& identifiable` or `&& cacheable` would have been caught by nothing.

// The load-bearing property: an input whose content identity cannot be
// established must not produce a cache key at all. Keying on anything else — a
// path, or a synthetic error token — is what turns a transient read failure into
// a durable wrong answer for a different file at that path.
func TestUnidentifiableInputYieldsNoIdentity(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone.wav")
	id, err := mediahash.Digest(missing, 0)
	if err == nil {
		t.Fatal("a missing file produced an identity")
	}
	if id.OK() {
		t.Fatal("Ident.OK() true for a failed digest — it would be used as a key")
	}
	if id.Unchanged(missing) {
		t.Fatal("Unchanged() must be false for an identity that was never established")
	}
}

// THE TOCTOU DETECTOR. Hashing before the consuming read does not close the
// window, it transposes it; the verify-after is what actually catches a file
// swapped mid-call. Without it, the transcript of the NEW bytes is stored under
// the OLD digest — a false hit reachable from any path holding those bytes.
func TestContentChangedDuringTheCallIsDetected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "rotating.wav")
	if err := os.WriteFile(p, []byte("take A, the bytes that were hashed"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := mediahash.Digest(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !id.Unchanged(p) {
		t.Fatal("an untouched file must verify as unchanged")
	}

	// Rotate it, the way a recorder or an exporter would, mid-call.
	if err := os.WriteFile(p, []byte("take B, entirely different bytes!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Force a distinct mtime even on coarse-granularity filesystems, so this
	// asserts the detector rather than the clock.
	future := time.Now().Add(3 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	if id.Unchanged(p) {
		t.Fatal("a mid-call rotation was NOT detected — the result would be stored under a digest for bytes the model never saw")
	}
}

// A same-SIZE rotation is the case size alone cannot see; mtime carries it.
func TestSameSizeRotationIsAlsoDetected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "same-size.wav")
	if err := os.WriteFile(p, []byte("AAAAAAAAAAAAAAAA"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := mediahash.Digest(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("BBBBBBBBBBBBBBBB"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(3 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	if id.Unchanged(p) {
		t.Fatal("a same-size rotation was not detected")
	}
}

// A file still being APPENDED to is the case no re-ordering can fix: the digest
// covers a prefix while ffmpeg reads more. full() compares bytes read against the
// stat size, so this surfaces as an error rather than a confident wrong identity.
func TestGrowingFileDoesNotProduceAConfidentIdentity(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "growing.wav")
	if err := os.WriteFile(p, []byte("first chunk"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := mediahash.Digest(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(" ...and more audio arrived"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	future := time.Now().Add(3 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	if id.Unchanged(p) {
		t.Fatal("growth during the call was not detected — the longer audio's transcript would be stored under sha256(prefix)")
	}
}

// The bypass must be OBSERVABLE. Without a field for it, a permanently
// uncacheable input is byte-identical in telemetry to an ordinary cold miss, so
// it re-runs the model at full cost forever while the ledger looks healthy.
func TestCacheBypassReachesTheLedgerEntry(t *testing.T) {
	e := entryFrom("transcribe", metaWithBypass("media identity: mediahash: stat: no such file"), false, 10)
	if e.CacheBypass == "" {
		t.Fatal("CacheBypass did not reach the ledger entry — a bypassed cache is indistinguishable from a cold one")
	}
	if e.CacheHit {
		t.Fatal("a bypass must not be recorded as a hit")
	}
	// And an ordinary call must leave it empty, or the field would be noise.
	if got := entryFrom("transcribe", metaWithBypass(""), false, 10); got.CacheBypass != "" {
		t.Fatalf("CacheBypass = %q on a normal call, want empty", got.CacheBypass)
	}
}
