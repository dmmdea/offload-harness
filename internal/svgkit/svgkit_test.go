package svgkit

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

// wellFormed reports whether s parses as well-formed XML (full token stream).
func wellFormed(s string) error {
	dec := xml.NewDecoder(strings.NewReader(s))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// TestRenderEscapesMaliciousLabel is the end-to-end injection guard: caller text
// crafted to break out of an SVG text node must be escaped, never echoed raw, and
// the output must stay well-formed XML — across every text-bearing component.
func TestRenderEscapesMaliciousLabel(t *testing.T) {
	const evil = `"/><script>alert(1)</script>`
	cases := map[string]map[string]any{
		"gauge":          {"value": 50, "label": evil, "unit": evil},
		"comparison-bar": {"items": []map[string]any{{"label": evil, "value": 10}}, "unit": evil},
		"chromatogram":   {"peaks": []map[string]any{{"rt": 1.0, "height": 50, "label": evil}}, "x_label": evil, "y_label": evil},
	}
	for kind, spec := range cases {
		b, _ := json.Marshal(spec)
		svg, _, _, err := Render(kind, b)
		if err != nil {
			t.Fatalf("%s: render error: %v", kind, err)
		}
		if strings.Contains(svg, "<script>") {
			t.Fatalf("%s: raw <script> leaked into output", kind)
		}
		if err := wellFormed(svg); err != nil {
			t.Fatalf("%s: output not well-formed XML after escaping: %v", kind, err)
		}
	}
}

func TestThemeWithDefaults(t *testing.T) {
	got := (Theme{Accent: "#ff0000"}).withDefaults()
	if got.Accent != "#ff0000" {
		t.Fatalf("explicit accent kept: got %s", got.Accent)
	}
	if got.FG == "" || got.BG == "" || got.Muted == "" || got.Font == "" {
		t.Fatalf("empty fields must be filled from defaults: %+v", got)
	}
}

func TestEscEscapesXML(t *testing.T) {
	if got := esc(`a<b>&"c'`); strings.ContainsAny(got, "<>") || strings.Contains(got, "\"") {
		t.Fatalf("unescaped XML chars: %q", got)
	}
}

func TestRenderUnknownKindErrors(t *testing.T) {
	if _, _, _, err := Render("nope", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unknown kind: want error")
	}
}

func TestRenderBadSpecErrors(t *testing.T) {
	if _, _, _, err := Render("gauge", json.RawMessage(`{bad json`)); err == nil {
		t.Fatal("bad spec JSON: want error")
	}
}
