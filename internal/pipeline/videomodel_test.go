package pipeline

// Video-family provenance: the ledger's model_tier and the footprint store's
// family key must both name the family that ACTUALLY rendered.
//
// 0.73.0 shipped this wrong and three independent reviewers caught it. The label
// was computed from config while the runner argument was resolved 30 lines later
// with the OPPOSITE precedence (an explicit per-request `model` wins), so every
// caller using the documented override got a row naming this box's configured
// seat for a render that used a different family. That is a FALSE provenance
// value, strictly worse than the vague label it replaced, and the original tests
// could not see it because they only ever passed a config.
//
// The lesson is encoded structurally: resolveVideoFamily is the single source of
// truth, and videoModelLabel takes the RESOLVED family rather than the config, so
// a future edit cannot reintroduce a second precedence rule without deleting a
// parameter these tests pin.

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// TestResolveVideoFamilyPrecedence pins the precedence itself: request beats
// config, config beats the runner default, and argModel stays byte-identical to
// what the pre-0.73.1 router emitted (that is the no-render-behavior-change
// guarantee — argModel is what reaches the ComfyUI graph builder).
func TestResolveVideoFamilyPrecedence(t *testing.T) {
	for _, c := range []struct {
		name             string
		cfgFamily        string
		reqModel         string
		wantArg          string
		wantRenderFamily string
	}{
		// Nothing bound anywhere: no --model, runner applies its own default.
		{"unbound", "", "", "", ""},

		// Config-driven (the shipped 2x16 seat).
		{"config ltx25", "ltx25", "", "ltx25", "ltx25"},

		// Config bound to the runner default: no arg is passed (byte-identical to
		// the old router) but the render IS Wan, so provenance says so instead of
		// pretending nothing is bound.
		{"config wan22 sentinel", "wan22", "", "", "wan22"},

		// THE REGRESSION ARM. The request wins in the router, so it must win in
		// the label too. Before 0.73.1 this produced arg=wan + label=ltx25.
		{"request overrides config", "ltx25", "wan", "wan", "wan22"},

		// Inverse direction, equally wrong before the fix: an unbound box asked
		// for ltx25 rendered LTX and was recorded as "family unbound".
		{"request on unbound box", "", "ltx25", "ltx25", "ltx25"},

		// A family the resolver has never heard of is passed through verbatim —
		// the runner owns the arg namespace — and recorded as itself.
		{"unknown family passes through", "ltx25", "hunyuan", "hunyuan", "hunyuan"},

		// Whitespace from a sloppy caller must not become a distinct family.
		{"request is trimmed", "ltx25", "  wan  ", "wan", "wan22"},
		{"config is trimmed", " ltx25 ", "", "ltx25", "ltx25"},
	} {
		t.Run(c.name, func(t *testing.T) {
			arg, fam := resolveVideoFamily(config.Config{VideoGenFamily: c.cfgFamily}, c.reqModel)
			if arg != c.wantArg {
				t.Errorf("argModel = %q, want %q (this is what reaches the render graph — a change here IS a render behavior change)", arg, c.wantArg)
			}
			if fam != c.wantRenderFamily {
				t.Errorf("renderFamily = %q, want %q", fam, c.wantRenderFamily)
			}
		})
	}
}

// TestVideoModelLabelFromResolvedFamily pins the label mapping. It takes the
// resolved family, so there is no config to disagree with.
func TestVideoModelLabelFromResolvedFamily(t *testing.T) {
	for _, c := range []struct{ fam, want string }{
		{"", "comfyui-video"}, // historical label; changing it fragments existing health tiers
		{"ltx25", "comfyui-video:ltx25"},
		{"wan22", "comfyui-video:wan22"},
		{"hunyuan", "comfyui-video:hunyuan"},
	} {
		if got := videoModelLabel(c.fam); got != c.want {
			t.Errorf("videoModelLabel(%q) = %q, want %q", c.fam, got, c.want)
		}
	}
}

// TestVideoProvenanceIsConsistentEndToEnd is the guard that the two surfaces can
// never disagree again: for every precedence case, the ledger label and the
// footprint family must describe the SAME family. This is the assertion whose
// absence let 0.73.0 ship — it compares the two derived values against each
// other rather than each against its own input.
func TestVideoProvenanceIsConsistentEndToEnd(t *testing.T) {
	for _, c := range []struct{ cfgFamily, reqModel string }{
		{"", ""}, {"ltx25", ""}, {"wan22", ""},
		{"ltx25", "wan"}, {"", "ltx25"}, {"ltx25", "hunyuan"},
	} {
		_, fam := resolveVideoFamily(config.Config{VideoGenFamily: c.cfgFamily}, c.reqModel)
		label := videoModelLabel(fam)
		footFam := videoFootprintFamily(fam)

		// The footprint key never has an empty family: an unbound box still
		// renders Wan, so "" resolves to the runner's real default there.
		if footFam == "" {
			t.Fatalf("cfg=%q req=%q: footprint family is empty — the store would key on nothing", c.cfgFamily, c.reqModel)
		}
		// The label either names the same family, or is the historical
		// unbound label. It must never name a DIFFERENT family.
		if label != "comfyui-video" && label != "comfyui-video:"+footFam {
			t.Errorf("cfg=%q req=%q: ledger says %q but footprint says %q — the two provenance surfaces disagree",
				c.cfgFamily, c.reqModel, label, footFam)
		}
	}
}

// TestVideoFootprintQuantIsScopedToWan: the Wan GGUF keys stay bound on a box
// whose seat is another family (they are the recorded fallback), so deriving the
// quant from them unconditionally stamped "q8_0" onto LTX-2.5 renders whose
// transformer is int8-convrot — a quant that render never used, written into a
// store the fleet reads for placement.
func TestVideoFootprintQuantIsScopedToWan(t *testing.T) {
	cfg := config.Default()
	cfg.VideoGenUnetHigh = "Wan2.2-I2V-A14B-HighNoise-Q8_0.gguf"
	cfg.VideoGenUnetLow = "Wan2.2-I2V-A14B-LowNoise-Q8_0.gguf"

	// The Wan family still reports its quant — the original behavior, preserved.
	if q := videoFootprintQuant(cfg, "wan22"); q != "q8_0" {
		t.Errorf("wan22 with Q8_0 unets: quant = %q, want \"q8_0\"", q)
	}
	// An unbound box renders Wan, so it keeps the Wan quant too.
	if q := videoFootprintQuant(cfg, ""); q != "q8_0" {
		t.Errorf("unbound (renders Wan) with Q8_0 unets: quant = %q, want \"q8_0\"", q)
	}
	// Every other family must NOT inherit Wan's quant. This is the live shape on
	// the reference box: family ltx25 with the Wan GGUFs still configured.
	for _, fam := range []string{"ltx25", "hunyuan", "ace"} {
		if q := videoFootprintQuant(cfg, fam); q != "" {
			t.Errorf("family %q must not inherit the Wan quant, got %q", fam, q)
		}
	}
}
