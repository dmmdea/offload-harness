package tierdocs

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/mediaseat"
)

// A registered extra (mediaseat.Seat.Extra) is served but bound to no config key. The tier
// page must say so, and the index must not list it as a second "vision" capability the tier
// routes to.
func extraTierProfile() Profile {
	primary := mediaseat.Seat{Kind: mediaseat.KindVision, Name: "main-vl", Model: "m.gguf", MMProj: "p.gguf", CtxSize: 8192, Residency: mediaseat.Swappable}
	extra := mediaseat.Seat{Kind: mediaseat.KindVision, Name: "side-vl", Aliases: []string{"side"}, Model: "s.gguf", MMProj: "sp.gguf",
		CtxSize: 8192, Residency: mediaseat.Swappable, Extra: true}
	return Profile{CtxSize: 8192, MediaSeats: []mediaseat.Seat{primary, extra}}
}

func TestAnExtraSeatIsDocumentedAsBindingNothing(t *testing.T) {
	page := renderTier("t", extraTierProfile(), nil)
	for _, want := range []string{
		"| `main-vl` | vision | `vision_model` | `m.gguf` | swappable |", // the primary row is unchanged
		"| `side-vl` | vision | — (extra) | `s.gguf` | swappable |",
		"`extra`",                   // the explanatory note
		"measure the rendered form", // a rendered extra is not the hand-wired entry
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q:\n%s", want, page)
		}
	}
	if strings.Count(page, "`vision_model`") != 1 {
		t.Errorf("exactly one seat binds vision_model, the page says otherwise:\n%s", page)
	}
}

func TestATierWithoutAnExtraSaysNothingAboutExtras(t *testing.T) {
	p := extraTierProfile()
	p.MediaSeats = p.MediaSeats[:1]
	if page := renderTier("t", p, nil); strings.Contains(page, "extra") {
		t.Errorf("a tier with no extra seat mentions extras:\n%s", page)
	}
}

func TestTheIndexCountsExtrasApartFromTheBoundKinds(t *testing.T) {
	idx := renderIndex([]string{"t"}, map[string]Profile{"t": extraTierProfile()}, nil)
	if !strings.Contains(idx, "vision seat (+1 extra)") {
		t.Errorf("index row should read `vision seat (+1 extra)`:\n%s", idx)
	}
	if strings.Contains(idx, "vision/vision") {
		t.Errorf("the extra was listed as a second bound vision capability:\n%s", idx)
	}
}
