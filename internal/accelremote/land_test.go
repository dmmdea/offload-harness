package accelremote

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// A forwarded mask came back as 40-110 KB of base64 inside the tool's answer and no file was written, so the
// caller's out_path was silently dropped (seen 2026-11-11 driving the Axera card from the Qube). The file must
// land on the caller's box, and a node's answer must never put bytes over the caller's image, a non-PNG file
// or anything outside the image's directory (security pass on that fix).
func TestForwardedMaskLandsOnTheCallersBoxAndNowhereElse(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte("mask pixels")...)
	b64 := base64.StdEncoding.EncodeToString(png)
	call := func(result map[string]any, args map[string]any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(result)
		node := newFakeNode(t, "node-x", []string{"axera-ax650"}, string(raw))
		cfg := config.Default()
		cfg.DelegateRemotes = []string{node.srv.URL}
		out, err := Call(context.Background(), cfg, "axera-ax650", "semantic_segment", args)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	root := t.TempDir()
	dir := filepath.Join(root, "shots")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(dir, "person.jpg")
	if err := os.WriteFile(img, []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := map[string]any{"mask_path": "/srv/node/jobs/accel-x/person.segmask.png", "mask_b64": b64, "mask_name": "person.segmask.png"}

	want := filepath.Join(dir, "seg.png")
	out := call(good, map[string]any{"image_path": img, "out_path": want})
	if out["mask_path"] != want {
		t.Fatalf("mask_path = %v, want %s (result %v)", out["mask_path"], want, out)
	}
	if b, err := os.ReadFile(want); err != nil || string(b) != string(png) {
		t.Fatalf("mask not written at out_path: %v %q", err, b)
	}
	if _, leaked := out["mask_b64"]; leaked {
		t.Fatal("the mask's bytes still ride in the answer")
	}

	// No out_path: beside the caller's image under the local lane's default name.
	if out = call(good, map[string]any{"image_path": img}); out["mask_path"] != filepath.Join(dir, "person.segmask.png") {
		t.Fatalf("default mask_path = %v", out["mask_path"])
	}

	// An existing non-PNG file, and the input image itself, are never replaced.
	keep := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(keep, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dst := range []string{keep, img} {
		out = call(good, map[string]any{"image_path": img, "out_path": dst})
		if _, ok := out["mask_error"].(string); !ok || out["mask_path"] != nil {
			t.Fatalf("out_path %s: refusal not reported: %v", dst, out)
		}
	}
	if b, _ := os.ReadFile(keep); string(b) != "keep me" {
		t.Fatalf("a non-PNG file was replaced: %q", b)
	}
	if b, _ := os.ReadFile(img); string(b) != "jpeg" {
		t.Fatalf("the input image was replaced: %q", b)
	}

	// A hostile node: a stem that climbs out, and a file name that is the caller's own image.
	evil := map[string]any{"../up_b64": b64, "../up_name": ".", "mask_b64": b64, "mask_name": "person.jpg"}
	out = call(evil, map[string]any{"image_path": img})
	if b, _ := os.ReadFile(img); string(b) != "jpeg" {
		t.Fatalf("a node's file name replaced the input image: %q", b)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatalf("a node's stem wrote outside the image's directory: %v", entries)
	}
	if out["mask_path"] != filepath.Join(dir, "person.mask.png") {
		t.Fatalf("the hostile node's mask did not land under a caller-built name: %v", out)
	}
}
