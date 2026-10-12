package gpulease

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeImage(t *testing.T, body []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "image.bin")
	if err := os.WriteFile(p, body, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// ReadBuildMarkerVersion is the audit's reader exported: the version a binary's marker states, the same string a
// doctor row shows.
func TestReadBuildMarkerVersionReadsTheMarker(t *testing.T) {
	p := writeImage(t, []byte("MZ\x00padding\x00"+BuildMarkerPrefix+"0.182.0\x00more bytes"))
	got, err := ReadBuildMarkerVersion(p)
	if err != nil || got != "0.182.0" {
		t.Fatalf("ReadBuildMarkerVersion = (%q, %v), want (\"0.182.0\", nil)", got, err)
	}
}

// A marker that straddles the audit's 1 MiB read chunk is still found: the stale-image check reads a whole
// executable, and a marker the linker put at a chunk edge must not read as "predates the marker".
func TestReadBuildMarkerVersionFindsAMarkerAcrossAChunkEdge(t *testing.T) {
	const chunk = 1 << 20
	marker := BuildMarkerPrefix + "0.183.0\x00"
	body := append(bytes.Repeat([]byte{'x'}, chunk-12), []byte(marker)...)
	body = append(body, bytes.Repeat([]byte{'y'}, 4096)...)
	got, err := ReadBuildMarkerVersion(writeImage(t, body))
	if err != nil || got != "0.183.0" {
		t.Fatalf("ReadBuildMarkerVersion across a chunk edge = (%q, %v), want (\"0.183.0\", nil)", got, err)
	}
}

// Guard: a binary without a marker is not an error; the version is simply unknown.
func TestReadBuildMarkerVersionOfABinaryWithoutAMarkerIsEmptyNotAnError(t *testing.T) {
	got, err := ReadBuildMarkerVersion(writeImage(t, []byte(strings.Repeat("no marker here ", 100))))
	if err != nil || got != "" {
		t.Fatalf("ReadBuildMarkerVersion of a bare file = (%q, %v), want (\"\", nil)", got, err)
	}
}

// An unreadable file is an error, never "no marker": the caller must not read a failure as a fresh image.
func TestReadBuildMarkerVersionOfAMissingFileIsAnError(t *testing.T) {
	if got, err := ReadBuildMarkerVersion(filepath.Join(t.TempDir(), "gone.exe")); err == nil {
		t.Fatalf("ReadBuildMarkerVersion of a missing file = (%q, nil), want an error", got)
	}
}
