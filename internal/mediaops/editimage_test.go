package mediaops

import (
	"strings"
	"testing"
)

func ops(o ...EditOp) []EditOp { return o }

func TestValidateOps_HappyPipeline(t *testing.T) {
	err := ValidateOps(ops(
		EditOp{Op: "crop", X: 0, Y: 0, Width: 100, Height: 100},
		EditOp{Op: "resize", Width: 50},
		EditOp{Op: "text", Text: "hi", X: 5, Y: 5},
		EditOp{Op: "convert", Format: "jpg"},
	))
	if err != nil {
		t.Fatalf("valid pipeline rejected: %v", err)
	}
}

func TestValidateOps_EmptyAndUnknown(t *testing.T) {
	if err := ValidateOps(nil); err == nil {
		t.Fatal("empty ops must error")
	}
	err := ValidateOps(ops(EditOp{Op: "sharpen"}))
	if err == nil || !strings.Contains(err.Error(), "sharpen") {
		t.Fatalf("unknown op must error naming the op, got %v", err)
	}
}

func TestValidateOps_ErrorsNameTheOpIndex(t *testing.T) {
	err := ValidateOps(ops(
		EditOp{Op: "resize", Width: 10},
		EditOp{Op: "crop", Width: -5, Height: 10},
	))
	if err == nil || !strings.Contains(err.Error(), "ops[1]") {
		t.Fatalf("error must name ops[1], got %v", err)
	}
}

func TestValidateOps_FlattenDesignFirstOnly(t *testing.T) {
	if err := ValidateOps(ops(EditOp{Op: "flatten_design"}, EditOp{Op: "resize", Width: 10})); err != nil {
		t.Fatalf("flatten_design as first op is valid: %v", err)
	}
	err := ValidateOps(ops(EditOp{Op: "resize", Width: 10}, EditOp{Op: "flatten_design"}))
	if err == nil || !strings.Contains(err.Error(), "first") {
		t.Fatalf("flatten_design not-first must error, got %v", err)
	}
}

func TestValidateOps_OpArgRules(t *testing.T) {
	bad := [][]EditOp{
		ops(EditOp{Op: "crop", Width: 0, Height: 10}),          // zero crop dim
		ops(EditOp{Op: "resize"}),                              // resize needs a dim
		ops(EditOp{Op: "composite", X: 1, Y: 1}),               // composite needs overlay
		ops(EditOp{Op: "text", X: 1, Y: 1}),                    // text needs text
		ops(EditOp{Op: "convert"}),                             // convert needs format
		ops(EditOp{Op: "convert", Format: "tiff"}),             // unsupported format
		ops(EditOp{Op: "composite", Overlay: "o.png", Opacity: 1.5}), // opacity out of range
	}
	for i, b := range bad {
		if err := ValidateOps(b); err == nil {
			t.Fatalf("bad case %d must error: %+v", i, b)
		}
	}
}

func TestUsesGimp(t *testing.T) {
	if !UsesGimp(ops(EditOp{Op: "flatten_design"}, EditOp{Op: "resize", Width: 9})) {
		t.Fatal("flatten_design pipeline must report gimp usage")
	}
	if UsesGimp(ops(EditOp{Op: "resize", Width: 9})) {
		t.Fatal("pure PIL pipeline must not report gimp usage")
	}
}
