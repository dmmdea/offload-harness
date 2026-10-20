package hostneed

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// THE CARD-ONLY IMAGE AND EDIT PLACEMENT (comfy-generate.mjs / comfy-render.mjs / comfy-edit.mjs --placement multicard, render/multicard-recipes.json).
// Two things must both hold before a call declares 0 host RAM: a recipe whose host growth was MEASURED with the read-only loader (today only the
// 2511 fp8mixed edit; every image arm of the 2026-10-11 benchmark ran the stock loader, so the rest are predicted) and its per-card peaks fitting
// the TWO cards the lease holds, each less the runner's reserve. A recipe that is only predicted, or does not fit, is the ordinary estimate, the honest
// streaming figure, with the reason in the detail.

const (
	editFp8Cmd  = "node render/comfy-edit.mjs o.png in.png p --unet qwen_image_edit_2511_fp8mixed.safetensors --placement multicard"
	editGgufCmd = "node render/comfy-edit.mjs o.png in.png p --unet qwen-image-edit-2511-Q5_1.gguf --placement multicard"
	img21Cmd    = "node render/comfy-generate.mjs o.png p --family qwen-image-2.1 --ckpt qwen_image_2.1_bf16.safetensors --clip qwen3vl_8b_bf16.safetensors --placement multicard"
	img2512Cmd  = "node render/comfy-generate.mjs o.png p --family qwen-image --ckpt qwen-image-2512-Q5_1.gguf --placement multicard"
	krea2McCmd  = "node render/comfy-generate.mjs o.png p --family krea2 --ckpt krea2_turbo_bf16.safetensors --placement multicard"
)

func imgFacts(t *testing.T, cards ...float64) Facts {
	t.Helper()
	dir, stat := comfyTree(t, map[string]float64{
		"qwen_image_edit_2511_fp8mixed.safetensors": 19.12,
		"qwen-image-edit-2511-Q5_1.gguf":            14.34,
		"qwen_2.5_vl_7b_fp8_scaled.safetensors":     8.74,
		"qwen_image_2.1_bf16.safetensors":           13.25,
		"qwen_image_2.1_int8_convrot.safetensors":   6.76,
		"qwen3vl_8b_bf16.safetensors":               16.33,
		"qwen-image-2512-Q5_1.gguf":                 14.34,
		"krea2_turbo_bf16.safetensors":              krea2UnetGiB,
		"qwen3vl_4b_bf16.safetensors":               krea2TEGiB,
	})
	f := Facts{Cfg: config.Config{ComfyDir: dir}, Stat: stat, CardsGiB: cards}
	for _, c := range cards {
		if c > f.VRAMGiB {
			f.VRAMGiB = c
		}
	}
	return f
}

func need(t *testing.T, cmd string, f Facts) Need {
	t.Helper()
	return Resolve(Request{Class: gpulease.ClassMedia, Args: strings.Fields(cmd)}, f)
}

func without(cmd string) string { return strings.Replace(cmd, " --placement multicard", "", 1) }

func TestACardOnlyImageCallWithAMeasuredRowDeclaresTheRowOnTwoLeasedCards(t *testing.T) {
	n := need(t, editFp8Cmd, imgFacts(t, 15.9, 15.9))
	if n.GiB != 0 || n.Source != SourceEstimate {
		t.Fatalf("a multicard 2511 fp8mixed edit whose measured peaks fit the two leased cards streams nothing from RAM: %v", n)
	}
	for _, want := range []string{"--placement multicard", "qwen-image-edit-2511/fp8mixed", "2 leased cards", "measured", "read-only", "13.0", "11.0"} {
		if !strings.Contains(n.Detail, want) {
			t.Errorf("the detail must say why (%q missing): %q", want, n.Detail)
		}
	}
	// the 2512 Q5_1 image row was measured card-only too (2026-10-20, 1328x1328 and 928x1664)
	if m := need(t, img2512Cmd, imgFacts(t, 15.9, 15.9)); m.GiB != 0 || !m.CardOnly {
		t.Fatalf("a multicard 2512 Q5_1 call on two fitting cards: %v, want 0 and card-only", m)
	}
	// the flag in its --name=value form is the same call (the Go side splits it; the runner refuses it, so the call never runs single undeclared)
	if m := need(t, strings.Replace(editFp8Cmd, "--placement multicard", "--placement=multicard", 1), imgFacts(t, 15.9, 15.9)); m.GiB != 0 {
		t.Fatalf("--placement=multicard: %v", m)
	}
}

func TestACardOnlyImageCallWithoutAMeasuredRowKeepsTheOrdinaryEstimate(t *testing.T) {
	f := imgFacts(t, 15.9, 15.9)
	for name, cmd := range map[string]string{"2511 GGUF": editGgufCmd} {
		got, plain := need(t, cmd, f), need(t, without(cmd), f)
		if got.GiB != plain.GiB || got.Source != plain.Source || got.GiB <= 0 {
			t.Errorf("%s: %v, want the ordinary %v (a predicted row declares nothing less than the set until a run with the read-only loader has measured its host growth)", name, got, plain)
		}
		if !strings.HasPrefix(got.Detail, plain.Detail) || !strings.Contains(got.Detail, "predicted") || !strings.Contains(got.Detail, "fits the two leased cards") {
			t.Errorf("%s: the detail keeps the ordinary text and says why the row declares the set: %q", name, got.Detail)
		}
	}
}

func TestThePlacementFlagMeansNothingToAFamilyWithoutARecipe(t *testing.T) {
	f := imgFacts(t, 15.9, 15.9)
	for name, cmd := range map[string]string{
		"sdxl":                  "node render/comfy-generate.mjs o.png p --placement multicard",
		"hidream":               "node render/comfy-generate.mjs o.png p --family hidream-o1 --ckpt krea2_turbo_bf16.safetensors --placement multicard",
		"2512 bf16":             "node render/comfy-generate.mjs o.png p --family qwen-image --ckpt qwen_image_2.1_bf16.safetensors --placement multicard",
		"2.1 int8 text encoder": strings.Replace(img21Cmd, "qwen3vl_8b_bf16", "qwen3vl_8b_int8", 1),
		// the int8_convrot DiT has a row that is held back (its DiT on cuda:1 was never run), so it is no recipe: the runner refuses it by name
		"2.1 int8 DiT, held back": strings.Replace(img21Cmd, "qwen_image_2.1_bf16", "qwen_image_2.1_int8_convrot", 1),
	} {
		got, plain := need(t, cmd, f), need(t, without(cmd), f)
		if got != plain {
			t.Errorf("%s: with the flag %v, without %v: no recipe, so the flag is the runner's refusal and not this package's business", name, got, plain)
		}
		if strings.Contains(name, "held back") && (got.GiB <= 0 || got.CardOnly || strings.Contains(got.Detail, "card-only") || strings.Contains(got.Detail, "predicted")) {
			t.Errorf("%s: %v: a held-back row declares the ordinary streaming figure and names no recipe", name, got)
		}
	}
}

func TestNoPlacementFlagIsSizedAsAlways(t *testing.T) {
	for name, cmd := range map[string]string{"edit fp8": editFp8Cmd, "edit gguf": editGgufCmd, "2.1": img21Cmd, "2512": img2512Cmd, "krea2": krea2McCmd} {
		for _, flagless := range []string{without(cmd), strings.Replace(cmd, "--placement multicard", "--placement single", 1)} {
			pair, one := need(t, flagless, imgFacts(t, 15.9, 15.9)), need(t, flagless, imgFacts(t, 15.9))
			if pair.GiB <= 0 || strings.Contains(pair.Detail, "card-only") {
				t.Errorf("%s: %v: the ordinary estimate, sized against the one largest card", name, pair)
			}
			if pair != one {
				t.Errorf("%s: the leased pair changes a call that did not ask for the placement: %v vs %v", name, pair, one)
			}
		}
		f := imgFacts(t, 15.9, 15.9)
		f.Placement = "single"
		if a, b := need(t, without(cmd), f), need(t, without(cmd), imgFacts(t, 15.9, 15.9)); a != b {
			t.Errorf("%s: Facts.Placement single changes the estimate: %v vs %v", name, a, b)
		}
	}
}

// The placement runs on exactly two cards. One card, three cards or no table are not that placement.
func TestACardOnlyImageCallThatDoesNotHoldTwoCardsKeepsTheOrdinaryEstimate(t *testing.T) {
	for name, cards := range map[string][]float64{"one card": {15.9}, "three cards": {15.9, 15.9, 15.9}, "no table": nil} {
		f := imgFacts(t, cards...)
		got, plain := need(t, editFp8Cmd, f), need(t, without(editFp8Cmd), f)
		if got.GiB != plain.GiB || got.Source != plain.Source || got.GiB <= 0 || !strings.HasPrefix(got.Detail, plain.Detail) || !strings.Contains(got.Detail, "exactly two leased cards") {
			t.Errorf("%s: %v, want the ordinary %v with the reason", name, got, plain)
		}
	}
}

// What must fit is each card's own peak, less the runner's reserve: the compute card (the larger one in this sizing; the runner may give the role to the faster of two
// cards within 1 percent of one size) holds 13.0 GiB for the fp8mixed row, the other stores (11.0 GiB).
func TestACardOnlyImageCallThatDoesNotFitTheLeasedPairIsSizedAsBefore(t *testing.T) {
	ordinary := func(name string, n Need) {
		t.Helper()
		if n.GiB <= 0 || !strings.Contains(n.Detail, "does not fit the two leased cards") {
			t.Fatalf("%s: %v, want the ordinary estimate with the reason", name, n)
		}
	}
	ordinary("two 12 GiB cards", need(t, editFp8Cmd, imgFacts(t, 12, 12)))
	ordinary("a 13.9 GiB compute card (13.0 + 1.0 reserve = 14.0)", need(t, editFp8Cmd, imgFacts(t, 13.9, 13.9)))
	if n := need(t, editFp8Cmd, imgFacts(t, 14.0, 14.0)); n.GiB != 0 {
		t.Fatalf("two 14.0 GiB cards (13.0 and 11.0 against 13.0 usable): %v, want 0", n)
	}
	// unequal cards: the larger computes, the smaller stores, in either order
	ordinary("24 + 11.9 GiB (the storage card needs 11.0 + 1.0)", need(t, editFp8Cmd, imgFacts(t, 24, 11.9)))
	ordinary("11.9 + 24 GiB, the other order", need(t, editFp8Cmd, imgFacts(t, 11.9, 24)))
	if n := need(t, editFp8Cmd, imgFacts(t, 12, 24)); n.GiB != 0 {
		t.Fatalf("12 + 24 GiB: the 24 GiB card computes and the 12 GiB card stores 11.0 of 11.0 usable: %v, want 0", n)
	}
	// --reserve-vram is the runner's
	ordinary("a 3 GiB reserve on 2 x 15.9 GiB", need(t, editFp8Cmd+" --reserve-vram 3", imgFacts(t, 15.9, 15.9)))
	if n := need(t, editFp8Cmd+" --reserve-vram 0.2", imgFacts(t, 13.5, 13.5)); n.GiB != 0 {
		t.Fatalf("a 0.2 GiB reserve on 2 x 13.5 GiB: %v, want 0", n)
	}
	// a card whose size is unknown (0) is no pair
	ordinary("a zero-size card", need(t, editFp8Cmd, imgFacts(t, 0, 15.9)))
}

// The resident share (--resident-gb) moves bytes between the two cards one for one: a larger share raises the compute card's peak, and a smaller one
// puts the same bytes on the storage card (the DiT's total is the invariant, the allocation string sends the remainder to the storage card).
func TestTheResidentShareIsReadFromTheImageCall(t *testing.T) {
	two := imgFacts(t, 15.9, 15.9)
	if n := need(t, editFp8Cmd+" --resident-gb 8", two); n.GiB != 0 || !strings.Contains(n.Detail, "storage card at 12.0") {
		t.Fatalf("an 8 GiB share (9 GiB resident in the row, so the storage card takes 1 GiB more: 11.0 + 1 = 12.0 of 14.9 usable): %v, want 0", n)
	}
	if n := need(t, editFp8Cmd+" --resident-gb 5", two); n.GiB <= 0 || !strings.Contains(n.Detail, "does not fit the two leased cards") || !strings.Contains(n.Detail, "storage card at 15.0") {
		t.Fatalf("a 5 GiB share puts 4 GiB more of the 19.1 GiB DiT on the storage card (11.0 + 4 = 15.0 of 14.9 usable): %v, want the ordinary estimate and the storage card's figure", n)
	}
	if n := need(t, editFp8Cmd+" --resident-gb 0", two); n.GiB <= 0 {
		t.Fatalf("a 0 GiB share leaves the whole 19.1 GiB DiT on the storage card, which ran out of memory at load in the benchmark (edit-gpuonly): %v, want the ordinary estimate", n)
	}
	// a share above the row's does not lower the storage figure: that is a guess no run has measured
	if n := need(t, editFp8Cmd+" --resident-gb 10", two); n.GiB != 0 || !strings.Contains(n.Detail, "storage card at 11.0") {
		t.Fatalf("a 10 GiB share keeps the measured 11.0 GiB on the storage card: %v, want 0", n)
	}
	// the GGUF row's storage peak is the DiT stage too: 15.6 GiB with every weight on the storage card is what the benchmark measured there
	if n := need(t, editGgufCmd+" --resident-gb 0", two); n.GiB <= 0 || !strings.Contains(n.Detail, "storage card at 15.6") {
		t.Fatalf("the GGUF row with a 0 GiB share (12.6 + 3 = 15.6 GiB, the benchmark's measured peak with every weight on the storage card): %v", n)
	}
	if n := need(t, editFp8Cmd+" --resident-gb=12", two); n.GiB <= 0 {
		t.Fatalf("a 12 GiB share (13.0 + 3 = 16.0 GiB on the compute card, 14.9 usable): %v, want the ordinary estimate", n)
	}
	for _, bad := range []string{"x", "-1", "65", "NaN"} {
		if n := need(t, editFp8Cmd+" --resident-gb "+bad, two); n.GiB != 0 {
			t.Fatalf("--resident-gb %s: %v, want the recipe's own share (the runner refuses such a call before it starts)", bad, n)
		}
	}
}

// The 2511 canvas is the source's size capped at 2.0 MP unless the binding or the call says otherwise; the row's compute peak is scaled to that cap.
func TestAnEditCanvasAboveTheRowsCapIsNotTheRow(t *testing.T) {
	two := imgFacts(t, 15.9, 15.9)
	if n := need(t, editFp8Cmd+" --megapixels 2", two); n.GiB != 0 {
		t.Fatalf("2.0 MP is the cap the row is scaled to: %v, want 0", n)
	}
	if n := need(t, editFp8Cmd+" --megapixels 3.5", two); n.GiB <= 0 || !strings.Contains(n.Detail, "megapixels") {
		t.Fatalf("a 3.5 MP canvas is above the row's 2.0 MP: %v, want the ordinary estimate and why", n)
	}
	f := two
	f.Megapixels = 4
	if n := need(t, editFp8Cmd, f); n.GiB <= 0 {
		t.Fatalf("a binding that pins a 4 MP canvas (Facts.Megapixels): %v, want the ordinary estimate", n)
	}
}

// The pipeline's media admission sizes the call from the binding, not from a command line.
func TestTheEditRouteReadsThePlacementFromTheFacts(t *testing.T) {
	f := imgFacts(t, 15.9, 15.9)
	cfg := config.Config{ComfyDir: f.Cfg.ComfyDir, GenEditFamily: config.EditFamily2511, GenEditUnet: "qwen_image_edit_2511_fp8mixed.safetensors"}
	if n, ok := ForRoute(RouteEdit, cfg, f); !ok || n.GiB <= 0 {
		t.Fatalf("without a placement: %v %v", n, ok)
	}
	f.Placement = PlacementMulticard
	if n, ok := ForRoute(RouteEdit, cfg, f); !ok || n.GiB != 0 || !strings.Contains(n.Detail, "qwen-image-edit-2511/fp8mixed") {
		t.Fatalf("with the binding's placement: %v %v, want 0", n, ok)
	}
	img := config.Config{ComfyDir: f.Cfg.ComfyDir, ImageGenFamily: "qwen-image", ImageGenCkpt: "qwen-image-2512-Q5_1.gguf"}
	if n, ok := ForRoute(RouteImage, img, f); !ok || n.GiB != 0 || !strings.Contains(n.Detail, "qwen-image/gguf-q5_1") {
		t.Fatalf("the image binding's placement, on the measured 2512 Q5_1 row: %v %v, want 0", n, ok)
	}
	// the 2.1 bf16 row is predicted too (its text encoder is built off the cards by the harness's own loader, a transient staging the estimate declares)
	bf16 := config.Config{ComfyDir: f.Cfg.ComfyDir, ImageGenFamily: "qwen-image-2.1", ImageGenCkpt: "qwen_image_2.1_bf16.safetensors", ImageGenCLIP: "qwen3vl_8b_bf16.safetensors"}
	if n, ok := ForRoute(RouteImage, bf16, f); !ok || n.GiB <= 0 || !strings.Contains(n.Detail, "qwen-image-2.1/bf16") || !strings.Contains(n.Detail, "predicted") {
		t.Fatalf("a predicted row declares the set: %v %v", n, ok)
	}
	// the 2.1 int8_convrot row is held back, so the same placement is no recipe there: the ordinary estimate, with no card-only note
	held := config.Config{ComfyDir: f.Cfg.ComfyDir, ImageGenFamily: "qwen-image-2.1", ImageGenCkpt: "qwen_image_2.1_int8_convrot.safetensors", ImageGenCLIP: "qwen3vl_8b_bf16.safetensors"}
	if n, ok := ForRoute(RouteImage, held, f); !ok || n.GiB <= 0 || strings.Contains(n.Detail, "card-only") || strings.Contains(n.Detail, "predicted") {
		t.Fatalf("a held-back row is no recipe, so the ordinary estimate: %v %v", n, ok)
	}
	// the other routes do not read it
	f.Placement = PlacementMulticard
	if a, ok := ForRoute(RouteInpaint, config.Config{ComfyDir: f.Cfg.ComfyDir, InpaintCkpt: "krea2_turbo_bf16.safetensors"}, f); !ok || strings.Contains(a.Detail, "card-only") {
		t.Fatalf("inpaint: %v", a)
	}
}

// The media class default is this box's largest bound family, whatever placement a call asks for: a placement is a call's word, not the box's.
func TestThePlacementDoesNotChangeTheClassDefault(t *testing.T) {
	f := imgFacts(t, 15.9, 15.9)
	f.Cfg.ImageGenScript = "render/comfy-generate.mjs"
	f.Cfg.ImageGenFamily, f.Cfg.ImageGenCkpt, f.Cfg.ImageGenCLIP = "qwen-image-2.1", "qwen_image_2.1_bf16.safetensors", "qwen3vl_8b_bf16.safetensors"
	f.Cfg.GenEditScript, f.Cfg.GenEditUnet = "render/comfy-edit.mjs", "qwen_image_edit_2511_fp8mixed.safetensors"
	a := ClassDefault(f)
	f.Placement = PlacementMulticard
	if b := ClassDefault(f); a != b {
		t.Fatalf("class default %v with a placement, %v without", b, a)
	}
}

// A 0 is the silent default of a declaration, so the one a card-only recipe earns must be marked: the lease verb and the pipeline say it aloud, with the
// detail that explains it, because a false 0 (the reader not engaged, a pack upgrade that changed the loaders) streams silently otherwise.
func TestOnlyAMeasuredCardOnlyDeclarationIsMarkedCardOnly(t *testing.T) {
	two := imgFacts(t, 15.9, 15.9)
	if n := need(t, editFp8Cmd, two); n.GiB != 0 || !n.CardOnly {
		t.Fatalf("the measured fp8mixed row on two fitting cards: %+v, want 0 and marked card-only", n)
	}
	for name, c := range map[string]struct {
		cmd string
		f   Facts
	}{
		"a predicted row":                      {editGgufCmd, two},
		"a held-back row":                      {img21Cmd, two},
		"a row that does not fit":              {editFp8Cmd, imgFacts(t, 12, 12)},
		"one card":                             {editFp8Cmd, imgFacts(t, 15.9)},
		"no placement":                         {without(editFp8Cmd), two},
		"a canvas above the row's cap":         {editFp8Cmd + " --megapixels 3.5", two},
		"a share the storage card cannot hold": {editFp8Cmd + " --resident-gb 5", two},
	} {
		if n := need(t, c.cmd, c.f); n.CardOnly {
			t.Errorf("%s: %+v is marked card-only, but it is not the measured zero", name, n)
		}
	}
}
