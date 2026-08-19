package pipeline

// TestVideoModelLabel pins the video route's ledger label. The label is the ONLY
// place the render family reaches telemetry: before this, every video row carried
// a flat "comfyui-video", so "which graph family actually rendered" was
// unrecoverable from the ledger — which is why the 2026-08-12 ltx25 seat binding
// could not be confirmed from the record even though config, code and binary all
// carried it. Sibling of imagemodel_test.go's mapping guard.

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

func TestVideoModelLabel(t *testing.T) {
	// An UNBOUND family keeps the historical label so health tiers do not
	// fragment (same rule runGenerateImage applies to "comfyui-sdxl").
	if got := videoModelLabel(config.Config{}); got != "comfyui-video" {
		t.Fatalf("unbound family: want %q, got %q", "comfyui-video", got)
	}

	// A bound family is recorded, so a seat-binding verdict is provable from
	// telemetry alone.
	if got := videoModelLabel(config.Config{VideoGenFamily: "ltx25"}); got != "comfyui-video:ltx25" {
		t.Fatalf("ltx25: want %q, got %q", "comfyui-video:ltx25", got)
	}

	// "wan22" is BOTH the runner's own default family and the sentinel the
	// family router treats as "leave --model unset" (pipeline.go, runGenerateVideo).
	// It must still be RECORDED: "rendered with wan22" and "family unbound" are
	// different facts, and collapsing them would recreate exactly the ambiguity
	// this label exists to remove.
	if got := videoModelLabel(config.Config{VideoGenFamily: "wan22"}); got != "comfyui-video:wan22" {
		t.Fatalf("wan22: want %q, got %q", "comfyui-video:wan22", got)
	}
}
