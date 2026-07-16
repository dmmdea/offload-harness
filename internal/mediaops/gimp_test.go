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
