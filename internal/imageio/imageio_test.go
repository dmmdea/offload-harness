package imageio

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalPNG is a valid 1x1 PNG (magic header + IHDR/IDAT/IEND).
var minimalPNG = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, // PNG signature
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52, // IHDR length + type
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, // 1x1
	0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53, 0xDE, // bit depth/color + CRC
	0x00, 0x00, 0x00, 0x0C, 0x49, 0x44, 0x41, 0x54, // IDAT length + type
	0x08, 0xD7, 0x63, 0xF8, 0xCF, 0xC0, 0x00, 0x00, 0x00, 0x03, 0x00, 0x01, // data
	0x18, 0xDD, 0x8D, 0xB4, // IDAT CRC
	0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82, // IEND
}

func TestLoadImageB64_FilePNG(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tiny.png")
	if err := os.WriteFile(p, minimalPNG, 0o644); err != nil {
		t.Fatal(err)
	}
	uri, err := LoadImageB64(p, 6000000)
	if err != nil {
		t.Fatalf("LoadImageB64: %v", err)
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(uri, prefix) {
		t.Fatalf("uri = %q, want prefix %q", uri, prefix)
	}
	// round-trips back to the original bytes.
	dec, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(uri, prefix))
	if err != nil {
		t.Fatalf("decode b64: %v", err)
	}
	if string(dec) != string(minimalPNG) {
		t.Errorf("decoded bytes differ from source")
	}
}

func TestLoadImageB64_DataURIPassthrough(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(minimalPNG)
	in := "data:image/png;base64," + b64
	out, err := LoadImageB64(in, 6000000)
	if err != nil {
		t.Fatalf("LoadImageB64: %v", err)
	}
	if out != in {
		t.Errorf("passthrough changed the URI: %q != %q", out, in)
	}
}

func TestLoadImageB64_OversizeFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.png")
	// header is a valid PNG, but total exceeds the cap.
	big := append(append([]byte{}, minimalPNG...), make([]byte, 1000)...)
	if err := os.WriteFile(p, big, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadImageB64(p, 100); err == nil {
		t.Errorf("expected oversize error, got nil")
	}
}

func TestLoadImageB64_RemoteURLRejected(t *testing.T) {
	if _, err := LoadImageB64("http://example.com/cat.png", 6000000); err == nil {
		t.Errorf("expected remote-URL rejection, got nil")
	}
	if _, err := LoadImageB64("https://example.com/cat.png", 6000000); err == nil {
		t.Errorf("expected remote-URL rejection (https), got nil")
	}
}

func TestLoadImageB64_NonImageFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(p, []byte("just some text, not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadImageB64(p, 6000000); err == nil {
		t.Errorf("expected non-image error, got nil")
	}
}

func TestLoadImageB64_NonImageDataURIRejected(t *testing.T) {
	if _, err := LoadImageB64("data:text/plain;base64,aGVsbG8=", 6000000); err == nil {
		t.Errorf("expected non-image data URI rejection, got nil")
	}
}

func TestLoadImageB64_EmptyBase64PayloadRejected(t *testing.T) {
	// A well-formed image data URI header but NO payload after ";base64," is
	// useless (decodes to nothing) and must be rejected.
	if _, err := LoadImageB64("data:image/png;base64,", 6000000); err == nil {
		t.Errorf("expected empty-payload data URI rejection, got nil")
	}
}

func TestLoadImageB64_NoLimit(t *testing.T) {
	// maxBytes <= 0 means no size limit: a valid-but-large image still loads.
	dir := t.TempDir()
	p := filepath.Join(dir, "big.png")
	big := append(append([]byte{}, minimalPNG...), make([]byte, 50000)...)
	if err := os.WriteFile(p, big, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadImageB64(p, 0); err != nil {
		t.Errorf("maxBytes<=0 should impose no limit, got error: %v", err)
	}
}
