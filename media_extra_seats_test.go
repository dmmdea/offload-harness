package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/mediaseat"
	"github.com/dmmdea/offload-harness/internal/servingtmpl"
	"github.com/dmmdea/offload-harness/internal/tierseed"
)

// A-131. blackwell-8's reference box serves two more vision models by alias beside the
// tier's bound vision seat: a small screenshot reader and the E4B vision build. They were
// hand-wired because the seat schema bound every vision seat to vision_model and refused a
// second writer. They are now registered EXTRAS (mediaseat.Seat.Extra): rendered into
// llama-swap with their aliases, bound to nothing.
//
// The rendered form is not the hand-wired one (a rendered vision seat adds --reasoning off,
// -ngl 99 and the tier's q8_0 KV), so what these tests pin is the declaration and that it
// renders, never that the rendered seat is quality-equivalent: the seat note says so.
var wantBlackwell8Extras = []struct {
	name, model, mmproj string
	aliases             []string
}{
	{"lfm2.5-vl", "LFM2.5-VL-3B-Q8_0.gguf", "mmproj-LFM2.5-VL-3B-F16.gguf", []string{"lfm-vl", "screen-vl"}},
	{"gemma4-e4b-vision", "gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf", "mmproj-gemma-4-E4B-F16.gguf", []string{"gemma-vision"}},
}

func blackwell8Seats(t *testing.T) (tierseed.Profile, map[string]mediaseat.Seat) {
	t.Helper()
	profiles, err := tierseed.Parse(embeddedProfiles)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := profiles["blackwell-8"]
	if !ok {
		t.Fatal("blackwell-8 not in the shipped table — this gate went blind")
	}
	byName := map[string]mediaseat.Seat{}
	for _, s := range p.MediaSeats {
		byName[s.Name] = s
	}
	return p, byName
}

func TestBlackwell8SeedsTheTwoVisionExtras(t *testing.T) {
	p, seats := blackwell8Seats(t)
	for _, w := range wantBlackwell8Extras {
		s, ok := seats[w.name]
		if !ok {
			t.Errorf("blackwell-8 declares no %q seat — the reference box serves it", w.name)
			continue
		}
		if s.Kind != mediaseat.KindVision || !s.Extra {
			t.Errorf("%s: kind %q extra %v, want a vision seat flagged extra", w.name, s.Kind, s.Extra)
		}
		if s.Model != w.model || s.MMProj != w.mmproj {
			t.Errorf("%s: model/mmproj %q / %q, want %q / %q", w.name, s.Model, s.MMProj, w.model, w.mmproj)
		}
		if strings.Join(s.Aliases, ",") != strings.Join(w.aliases, ",") {
			t.Errorf("%s: aliases %v, want %v", w.name, s.Aliases, w.aliases)
		}
		if s.CtxSize != 8192 || s.Residency != mediaseat.Swappable || s.TTL != 300 {
			t.Errorf("%s: ctx %d residency %q ttl %d, want 8192 / swappable / 300", w.name, s.CtxSize, s.Residency, s.TTL)
		}
		// A rendered vision seat adds --reasoning off, -ngl 99 and q8_0 KV, which the
		// hand-wired seat does not run; the record must not let a reader assume otherwise.
		if !strings.Contains(s.Measured, "rendered form not yet re-measured") {
			t.Errorf("%s: the seat note must say 'rendered form not yet re-measured', got %q", w.name, s.Measured)
		}
	}
	if err := mediaseat.Validate(p.MediaSeats, "blackwell-8"); err != nil {
		t.Fatal(err)
	}
	// The extras bind nothing: the tier's own vision seat keeps the route.
	b := mediaseat.Bindings(p.MediaSeats)
	if b["vision_model"] != "qwen3.5-9b-vl" || b["ocr_model"] != "paddleocr-vl" {
		t.Errorf("bindings = %v, want vision_model qwen3.5-9b-vl and ocr_model paddleocr-vl untouched by the extras", b)
	}
	for k, v := range b {
		for _, w := range wantBlackwell8Extras {
			if v == w.name {
				t.Errorf("config key %q is bound to the extra %q", k, w.name)
			}
		}
	}
}

// TestBlackwell8ExtrasRenderOnEveryOS: the seed renders both extras, with their aliases and
// their own weights, on the Linux and Windows serving templates.
func TestBlackwell8ExtrasRenderOnEveryOS(t *testing.T) {
	var doc struct {
		Profiles map[string]servingProfile `json:"profiles"`
	}
	if err := json.Unmarshal(embeddedProfiles, &doc); err != nil {
		t.Fatal(err)
	}
	sp, ok := doc.Profiles["blackwell-8"]
	if !ok {
		t.Fatal("blackwell-8 not in the shipped table — this gate went blind")
	}
	for _, goos := range []string{"linux", "windows"} {
		tmpl, err := templateFor(goos, sp.Backend)
		if err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
		out, err := servingtmpl.Render(tmpl, renderParams(sp, goos))
		if err != nil {
			t.Fatalf("%s: rendering: %v", goos, err)
		}
		served := servedAliases(out)
		for _, w := range wantBlackwell8Extras {
			for _, id := range append([]string{w.name}, w.aliases...) {
				if !served[id] {
					t.Errorf("%s: the rendered serving config does not answer to %q", goos, id)
				}
			}
			block := seatBlockOf(out, w.name)
			if block == "" {
				t.Errorf("%s: no model block for %s", goos, w.name)
				continue
			}
			// The weights and the alias list must sit INSIDE the seat's own block: the E4B QAT
			// file is also the resident chat seat's, so a whole-config substring check would
			// pass with the seat absent.
			for _, f := range []string{"/" + w.model, "/" + w.mmproj, "aliases: [" + strings.Join(w.aliases, ", ") + "]"} {
				if !strings.Contains(block, f) {
					t.Errorf("%s: the %s block does not carry %q:\n%s", goos, w.name, f, block)
				}
			}
		}
	}
}

// TestNoShippedExtraSeatIsBoundByTheEffectiveConfig: across the whole table, whatever a
// tier flags extra is never what a seeded binding names — the closure gate would otherwise
// pass on the alias being served while an extra quietly became a route.
func TestNoShippedExtraSeatIsBoundByTheEffectiveConfig(t *testing.T) {
	profiles, err := tierseed.Parse(embeddedProfiles)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for id, p := range profiles {
		for _, s := range p.MediaSeats {
			if !s.Extra {
				continue
			}
			n++
			for _, goos := range []string{"linux", "windows"} {
				cfg := effectiveConfig(t, p, id, goos)
				for _, a := range modelAliases(cfg) {
					if a.Alias == s.Name {
						t.Errorf("tier %s (%s): config key %s is bound to the extra seat %q", id, goos, a.Key, s.Name)
					}
				}
			}
		}
	}
	if n < len(wantBlackwell8Extras) {
		t.Errorf("only %d extra seats ship; want at least the %d blackwell-8 extras — this gate went blind", n, len(wantBlackwell8Extras))
	}
}

// seatBlockOf returns one llama-swap model entry: its `  name:` line through the line before
// the next entry (or the end of the models block). "" when the config has no such entry.
func seatBlockOf(rendered, name string) string {
	lines := strings.Split(strings.ReplaceAll(rendered, "\r\n", "\n"), "\n")
	var out []string
	in := false
	for _, l := range lines {
		if !in {
			if l == "  "+name+":" {
				in = true
				out = append(out, l)
			}
			continue
		}
		// The next 2-space key, or any column-0 line other than a comment, ends the entry.
		if (strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   ") && strings.TrimSpace(l) != "") ||
			(l != "" && !strings.HasPrefix(l, " ")) {
			break
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}
