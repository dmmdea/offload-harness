package mediaops

import (
	"encoding/json"
	"strings"
	"testing"
)

func ops(o ...EditOp) []EditOp { return o }

// pointer literals for FinishSharpen's pointer fields (explicit-0 round-trip fix)
func fptr(f float64) *float64 { return &f }
func iptr(i int) *int         { return &i }

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

func TestValidateRenditions(t *testing.T) {
	good := [][]Rendition{
		nil, // renditions are optional
		{{Width: 1080, Format: "webp", Suffix: "-ig"}, {Width: 1920, Format: "jpg", Suffix: "-web"}},
		{{Height: 720, Format: "png", Suffix: "-720"}},
	}
	for i, g := range good {
		if err := ValidateRenditions(g); err != nil {
			t.Errorf("good renditions case %d rejected: %v", i, err)
		}
	}
	bad := [][]Rendition{
		{{Format: "png", Suffix: "-a"}},                                            // width or height required
		{{Width: 100, Suffix: "-a"}},                                               // format required
		{{Width: 100, Format: "tiff", Suffix: "-a"}},                               // png|jpg|webp only
		{{Width: 100, Format: "png"}},                                              // suffix required
		{{Width: 100, Format: "png", Suffix: "-a"}, {Width: 50, Format: "jpg", Suffix: "-a"}}, // unique suffixes
	}
	for i, b := range bad {
		if err := ValidateRenditions(b); err == nil {
			t.Errorf("bad renditions case %d accepted", i)
		}
	}
}

func TestRenditionOut(t *testing.T) {
	got := renditionOut(`D:\out\hero.png`, Rendition{Width: 1080, Format: "webp", Suffix: "-ig"})
	if want := `D:\out\hero-ig.webp`; got != want {
		t.Fatalf("renditionOut = %q, want %q", got, want)
	}
	// jpeg normalizes to .jpg
	got = renditionOut("hero.png", Rendition{Width: 10, Format: "jpeg", Suffix: "-x"})
	if want := "hero-x.jpg"; got != want {
		t.Fatalf("renditionOut jpeg = %q, want %q", got, want)
	}
}

func TestUsesGimp(t *testing.T) {
	if !UsesGimp(ops(EditOp{Op: "flatten_design"}, EditOp{Op: "resize", Width: 9})) {
		t.Fatal("flatten_design pipeline must report gimp usage")
	}
	if !UsesGimp(ops(EditOp{Op: "instantiate_design", SetText: map[string]string{"H": "x"}})) {
		t.Fatal("instantiate_design pipeline must report gimp usage")
	}
	if UsesGimp(ops(EditOp{Op: "resize", Width: 9})) {
		t.Fatal("pure PIL pipeline must not report gimp usage")
	}
}

func TestValidateOps_InstantiateDesign(t *testing.T) {
	if err := ValidateOps(ops(
		EditOp{Op: "instantiate_design", SetText: map[string]string{"Headline": "New copy"},
			ReplaceImage: map[string]string{"ProductShot": "D:/renders/watch.png"}},
		EditOp{Op: "resize", Width: 1080})); err != nil {
		t.Fatalf("valid instantiate_design rejected: %v", err)
	}
	bad := [][]EditOp{
		ops(EditOp{Op: "resize", Width: 10}, EditOp{Op: "instantiate_design", SetText: map[string]string{"H": "x"}}), // first only
		ops(EditOp{Op: "instantiate_design"}),                                                    // >=1 replacement
		ops(EditOp{Op: "instantiate_design", SetText: map[string]string{"": "x"}}),               // layer name required
		ops(EditOp{Op: "instantiate_design", ReplaceImage: map[string]string{"P": ""}}),          // path required
	}
	for i, b := range bad {
		if err := ValidateOps(b); err == nil {
			t.Errorf("bad instantiate_design case %d accepted", i)
		}
	}
}

func TestValidateOps_Grade(t *testing.T) {
	// good: each sub-object alone, and all combined
	good := [][]EditOp{
		ops(EditOp{Op: "grade", Levels: &GradeLevels{Black: 8, White: 248, Gamma: 1.05}}),
		ops(EditOp{Op: "grade", Curve: &GradeCurve{Points: [][]float64{{0, 16}, {255, 240}}}}),
		ops(EditOp{Op: "grade", WB: &GradeWB{Mode: "gray_world"}}),
		ops(EditOp{Op: "grade", WB: &GradeWB{Mode: "scale", R: 1.1, G: 1.0, B: 0.9}}),
		ops(EditOp{Op: "grade", Levels: &GradeLevels{White: 250}, WB: &GradeWB{Mode: "gray_world"}, LuminanceOnly: true}),
	}
	for i, g := range good {
		if err := ValidateOps(g); err != nil {
			t.Errorf("good grade case %d rejected: %v", i, err)
		}
	}
	bad := [][]EditOp{
		ops(EditOp{Op: "grade"}),                                                     // needs >=1 of levels/curve/wb
		ops(EditOp{Op: "grade", Levels: &GradeLevels{Black: -1}}),                    // black 0-254
		ops(EditOp{Op: "grade", Levels: &GradeLevels{White: 256}}),                   // white 1-255
		ops(EditOp{Op: "grade", Levels: &GradeLevels{Black: 200, White: 100}}),       // black < white
		ops(EditOp{Op: "grade", Levels: &GradeLevels{Gamma: 0.05}}),                  // gamma 0.1-10
		ops(EditOp{Op: "grade", Levels: &GradeLevels{Gamma: 11}}),                    // gamma 0.1-10
		ops(EditOp{Op: "grade", Curve: &GradeCurve{}}),                               // curve needs points
		ops(EditOp{Op: "grade", Curve: &GradeCurve{Points: [][]float64{{0}}}}),       // pairs only
		ops(EditOp{Op: "grade", Curve: &GradeCurve{Points: [][]float64{{0, 300}}}}),  // 0-255
		ops(EditOp{Op: "grade", WB: &GradeWB{Mode: "auto"}}),                         // unknown wb mode
		ops(EditOp{Op: "grade", WB: &GradeWB{Mode: "scale", R: -0.5}}),               // scales >= 0
	}
	for i, b := range bad {
		if err := ValidateOps(b); err == nil {
			t.Errorf("bad grade case %d accepted", i)
		}
	}
}

func TestValidateOps_LutCube(t *testing.T) {
	if err := ValidateOps(ops(EditOp{Op: "lut_cube", Path: "look.cube"})); err != nil {
		t.Fatalf("valid lut_cube rejected: %v", err)
	}
	s := 0.6
	if err := ValidateOps(ops(EditOp{Op: "lut_cube", Path: "look.cube", Strength: &s})); err != nil {
		t.Fatalf("valid lut_cube with strength rejected: %v", err)
	}
	zero := 0.0
	if err := ValidateOps(ops(EditOp{Op: "lut_cube", Path: "look.cube", Strength: &zero})); err != nil {
		t.Fatalf("strength 0 is in range: %v", err)
	}
	neg, over := -0.1, 1.1
	bad := [][]EditOp{
		ops(EditOp{Op: "lut_cube"}),                                    // path required
		ops(EditOp{Op: "lut_cube", Path: "look.cube", Strength: &neg}), // 0..1
		ops(EditOp{Op: "lut_cube", Path: "look.cube", Strength: &over}),
	}
	for i, b := range bad {
		if err := ValidateOps(b); err == nil {
			t.Errorf("bad lut_cube case %d accepted", i)
		}
	}
}

func TestValidateOps_PerspectiveComposite(t *testing.T) {
	quad := [][]float64{{20, 20}, {60, 25}, {58, 60}, {18, 55}}
	if err := ValidateOps(ops(EditOp{Op: "perspective_composite", Overlay: "content.png", Quad: quad})); err != nil {
		t.Fatalf("valid perspective_composite rejected: %v", err)
	}
	bad := [][]EditOp{
		ops(EditOp{Op: "perspective_composite", Quad: quad}),                                        // overlay required
		ops(EditOp{Op: "perspective_composite", Overlay: "c.png"}),                                  // quad required
		ops(EditOp{Op: "perspective_composite", Overlay: "c.png", Quad: quad[:3]}),                  // 4 corners
		ops(EditOp{Op: "perspective_composite", Overlay: "c.png", Quad: [][]float64{{1}, {2, 2}, {3, 3}, {4, 4}}}), // pairs
	}
	for i, b := range bad {
		if err := ValidateOps(b); err == nil {
			t.Errorf("bad perspective_composite case %d accepted", i)
		}
	}
}

func TestValidateOps_Finish(t *testing.T) {
	good := [][]EditOp{
		ops(EditOp{Op: "finish"}), // bare = default delivery sharpen
		ops(EditOp{Op: "finish", Sharpen: &FinishSharpen{Radius: fptr(1.2), Percent: iptr(80), Threshold: iptr(3)}}),
		ops(EditOp{Op: "finish", Median: 3}),
		ops(EditOp{Op: "finish", Median: 5, Sharpen: &FinishSharpen{Radius: fptr(2)}}),
	}
	for i, g := range good {
		if err := ValidateOps(g); err != nil {
			t.Errorf("good finish case %d rejected: %v", i, err)
		}
	}
	bad := [][]EditOp{
		ops(EditOp{Op: "finish", Median: 4}),                                  // median 3|5 only
		ops(EditOp{Op: "finish", Sharpen: &FinishSharpen{Radius: fptr(-1)}}),     // radius >= 0
		ops(EditOp{Op: "finish", Sharpen: &FinishSharpen{Radius: fptr(11)}}),    // radius <= 10 (halos)
		ops(EditOp{Op: "finish", Sharpen: &FinishSharpen{Percent: iptr(-5)}}),   // percent >= 0
		ops(EditOp{Op: "finish", Sharpen: &FinishSharpen{Percent: iptr(501)}}),  // percent <= 500
		ops(EditOp{Op: "finish", Sharpen: &FinishSharpen{Threshold: iptr(256)}}), // threshold 0-255
	}
	for i, b := range bad {
		if err := ValidateOps(b); err == nil {
			t.Errorf("bad finish case %d accepted", i)
		}
	}
}

func TestValidateOps_MaskBoxes(t *testing.T) {
	// good: boxes with positive dims, optional feather/pad/invert
	err := ValidateOps(ops(EditOp{Op: "mask_boxes",
		Boxes: []MaskBox{{X: 10, Y: 10, Width: 30, Height: 20}}, Feather: 8, Pad: 2, Invert: true}))
	if err != nil {
		t.Fatalf("valid mask_boxes rejected: %v", err)
	}
	bad := [][]EditOp{
		ops(EditOp{Op: "mask_boxes"}),                                            // boxes required
		ops(EditOp{Op: "mask_boxes", Boxes: []MaskBox{}}),                        // non-empty required
		ops(EditOp{Op: "mask_boxes", Boxes: []MaskBox{{Width: 0, Height: 5}}}),   // positive dims
		ops(EditOp{Op: "mask_boxes", Boxes: []MaskBox{{X: -1, Width: 5, Height: 5}}}), // non-negative origin
		ops(EditOp{Op: "mask_boxes", Boxes: []MaskBox{{Width: 5, Height: 5}}, Feather: -1}), // feather >= 0
		ops(EditOp{Op: "mask_boxes", Boxes: []MaskBox{{Width: 5, Height: 5}}, Pad: -1}),     // pad >= 0
	}
	for i, b := range bad {
		if err := ValidateOps(b); err == nil {
			t.Errorf("bad mask_boxes case %d accepted", i)
		}
	}
}

// Review fixes: renditions must reject negative dims and unsafe suffixes.
func TestValidateRenditions_NegativeAndSuffix(t *testing.T) {
	if err := ValidateRenditions([]Rendition{{Width: -5, Height: 100, Format: "png", Suffix: "-a"}}); err == nil {
		t.Fatal("negative width must be rejected")
	}
	if err := ValidateRenditions([]Rendition{{Width: 100, Format: "png", Suffix: `..\evil`}}); err == nil {
		t.Fatal("path-separator suffix must be rejected")
	}
	if err := ValidateRenditions([]Rendition{{Width: 100, Format: "png", Suffix: "-web_1x"}}); err != nil {
		t.Fatalf("safe suffix wrongly rejected: %v", err)
	}
}

// Review fix: an explicit sharpen zero survives the struct round-trip (pointer
// fields) — percent 0 must re-marshal as 0, not vanish into the worker default.
func TestFinishSharpenZeroRoundTrip(t *testing.T) {
	var ops []EditOp
	if err := json.Unmarshal([]byte(`[{"op":"finish","sharpen":{"percent":0,"threshold":0}}]`), &ops); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOps(ops); err != nil {
		t.Fatalf("zero sharpen values must validate: %v", err)
	}
	b, _ := json.Marshal(ops[0])
	for _, want := range []string{`"percent":0`, `"threshold":0`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("round-trip lost %s: %s", want, b)
		}
	}
}
