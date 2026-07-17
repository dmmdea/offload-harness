package mediaops

import (
	"strings"
	"testing"
)

func TestBuildGimpScript_ContainsVerifiedContract(t *testing.T) {
	// The template embeds the GIMP 3.2 batch contract LIVE-VERIFIED on
	// gimp-console-3.2.4 (2026-07-16): file-load -> get-layers -> sidecar write
	// via open-output-file (display does NOT reach batch stdout in GIMP 3) ->
	// flatten -> file-save.
	script, err := BuildGimpScript("C:/in/design.psd", "C:/out/flat.png", "C:/out/layers.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, need := range []string{
		`gimp-file-load RUN-NONINTERACTIVE "C:/in/design.psd"`,
		"gimp-image-get-layers",
		`open-output-file "C:/out/layers.txt"`,
		"gimp-image-flatten",
		`gimp-file-save RUN-NONINTERACTIVE image "C:/out/flat.png"`,
	} {
		if !strings.Contains(script, need) {
			t.Fatalf("missing %q in script:\n%s", need, script)
		}
	}
}

func TestBuildGimpScript_SourceTypeAndEscaping(t *testing.T) {
	if _, err := BuildGimpScript("C:/in/photo.png", "C:/out/x.png", "C:/out/l.txt"); err == nil {
		t.Fatal("non-design source (.png) must be rejected — flatten_design is xcf/psd only")
	}
	// Backslashes normalize to forward slashes; embedded quotes are rejected outright
	// (no escaping game with TinyScheme string literals — a quoted path is hostile input).
	script, err := BuildGimpScript(`C:\in\my design.xcf`, `C:\out\flat.png`, `C:\out\l.txt`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, `"C:/in/my design.xcf"`) {
		t.Fatalf("backslash paths must normalize: %s", script)
	}
	if _, err := BuildGimpScript(`C:/in/a"b.xcf`, "C:/o.png", "C:/l.txt"); err == nil {
		t.Fatal("a path containing a double quote must be rejected")
	}
}

func TestBuildInstantiateScript_ContainsVerifiedContract(t *testing.T) {
	// PDB calls verified against the installed gimp-console-3.2 (2026-07-17):
	// every name below greps in GIMP 3.2's own shipped scripts or libgimp-3.0-0.dll
	// (gimp-drawable-get-offsets is the 3.x rename of 2.x gimp-drawable-offsets).
	script, err := BuildInstantiateScript("C:/tpl/promo.xcf", "C:/out/flat.png",
		map[string]string{"Headline": "Hola Bogotá"},
		map[string]string{"ProductShot": `D:\renders\watch.png`})
	if err != nil {
		t.Fatal(err)
	}
	for _, need := range []string{
		`gimp-file-load RUN-NONINTERACTIVE "C:/tpl/promo.xcf"`,
		`gimp-image-get-layer-by-name image "Headline"`,
		`gimp-text-layer-set-text`,
		`"Hola Bogotá"`,
		`gimp-image-get-layer-by-name image "ProductShot"`,
		`gimp-file-load-layer RUN-NONINTERACTIVE image "D:/renders/watch.png"`,
		// live E2E 2026-07-17: insert at the OLD layer's stack position (a -1/top
		// insert covered the text layers above it) + scale to the old bounds.
		"gimp-image-get-item-position image old",
		"gimp-image-insert-layer image new 0 pos",
		"gimp-layer-scale new",
		"gimp-drawable-get-width old",
		"gimp-drawable-get-offsets",
		"gimp-layer-set-offsets",
		"gimp-image-remove-layer image old",
		"gimp-image-flatten",
		`gimp-file-save RUN-NONINTERACTIVE image "C:/out/flat.png"`,
	} {
		if !strings.Contains(script, need) {
			t.Fatalf("missing %q in script:\n%s", need, script)
		}
	}
}

func TestBuildInstantiateScript_Rules(t *testing.T) {
	// non-design source rejected
	if _, err := BuildInstantiateScript("C:/x.png", "C:/o.png",
		map[string]string{"A": "b"}, nil); err == nil {
		t.Fatal(".png template must be rejected — instantiate_design is xcf/psd only")
	}
	// at least one replacement required
	if _, err := BuildInstantiateScript("C:/x.xcf", "C:/o.png", nil, nil); err == nil {
		t.Fatal("empty set_text+replace_image must be rejected")
	}
	// text values are ESCAPED (quotes/backslashes), not rejected — copy is arbitrary
	script, err := BuildInstantiateScript("C:/x.xcf", "C:/o.png",
		map[string]string{"H": `say "hi" \now`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, `say \"hi\" \\now`) {
		t.Fatalf("text must be scheme-escaped:\n%s", script)
	}
	// replacement image paths with quotes are hostile input
	if _, err := BuildInstantiateScript("C:/x.xcf", "C:/o.png", nil,
		map[string]string{"P": `C:/a"b.png`}); err == nil {
		t.Fatal("quoted replacement path must be rejected")
	}
	// deterministic output: sorted by layer name
	a, _ := BuildInstantiateScript("C:/x.xcf", "C:/o.png",
		map[string]string{"B": "2", "A": "1"}, nil)
	if strings.Index(a, `"A"`) > strings.Index(a, `"B"`) {
		t.Fatalf("set_text must render in sorted layer order:\n%s", a)
	}
}

func TestParseLayerList(t *testing.T) {
	layers := ParseLayerList("LAYER:logo|visible\nLAYER:background|hidden\nnoise line\n")
	if len(layers) != 2 {
		t.Fatalf("want 2 layers, got %+v", layers)
	}
	if layers[0].Name != "logo" || !layers[0].Visible {
		t.Fatalf("layer 0 = %+v", layers[0])
	}
	if layers[1].Name != "background" || layers[1].Visible {
		t.Fatalf("layer 1 = %+v", layers[1])
	}
	// pipe in a layer name: split on the LAST pipe
	l := ParseLayerList("LAYER:a|b|visible\n")
	if len(l) != 1 || l[0].Name != "a|b" {
		t.Fatalf("pipe-in-name parse = %+v", l)
	}
}

func TestGimpArgs(t *testing.T) {
	args := GimpArgs("(script)")
	s := strings.Join(args, " ")
	for _, need := range []string{"-i", "--batch-interpreter=plug-in-script-fu-eval", "-b (script)", "-b (gimp-quit 0)"} {
		if !strings.Contains(s, need) {
			t.Fatalf("missing %q in %s", need, s)
		}
	}
}
