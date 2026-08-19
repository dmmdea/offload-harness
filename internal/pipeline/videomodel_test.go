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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
)

// TestResolveVideoFamilyPrecedence pins the precedence itself: request beats
// config, config beats the runner default.
//
// argModel is what reaches the ComfyUI graph builder, so every arm's wantArg is a
// render-behavior assertion. It matches the pre-0.73.1 router for every
// non-whitespace input, but NOT byte-identically in general — the resolver now
// trims where the old block did not (see the "is trimmed" arms). That difference is
// corrective and deliberate; an earlier version of this docstring claimed full
// byte-identity, which a fresh-context review measured as false across 40 inputs.
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

		// A family the RUNNER knows: arg passes through, recorded as itself.
		{"runner-known family", "ltx25", "hunyuan", "hunyuan", "hunyuan"},

		// UNRECOGNIZED input. The arg still passes through verbatim (the runner
		// receives exactly what the caller asked for), but the runner matches its
		// families EXACTLY and case-sensitively and silently falls through to Wan
		// — so the RECORDED family must be Wan. Echoing the caller's string here
		// is what re-created the false-provenance class one layer out: the ledger
		// claimed comfyui-video:LTX25 and the footprint store gained an LTX25 key
		// while WAN actually rendered.
		{"wrong case renders Wan", "ltx25", "LTX25", "LTX25", "wan22"},
		{"typo renders Wan", "ltx25", "ltx2.5", "ltx2.5", "wan22"},
		{"junk renders Wan", "", "not-a-family", "not-a-family", "wan22"},

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

// TestVideoProvenanceNamespaceMapping pins the TWO namespaces explicitly, as a
// table, because they are deliberately different for exactly one family.
//
// The ledger label uses the CONFIG namespace ("wan22"); the footprint store uses
// its OWN historical spelling ("wan2.2"), which is also what
// fleetnode.familyFor advertises on /fleet/health. Writer and advertiser must
// intersect, so the store keeps its spelling. An earlier revision of this fix
// unified them on "wan22" and thereby split the store from the advertiser
// fleet-wide — the review caught it.
//
// An earlier version of this test asserted string equality between the two derived
// values. That was both wrong (it fails on the one legitimate mapping) and weak:
// comparing two derivations of the same input passes when BOTH are wrong together,
// which is the derived-vs-derived vacuity class. The real wiring guard is
// TestRunGenerateVideo_OverrideProvenanceEndToEnd, which reads both surfaces out of
// a real Run(); this test's job is only to pin the mapping.
func TestVideoProvenanceNamespaceMapping(t *testing.T) {
	for _, c := range []struct {
		cfgFamily, reqModel string
		wantLabel           string
		wantFootprint       string
	}{
		// unbound: historical label, but the store still keys the family that runs
		{"", "", "comfyui-video", "wan2.2"},
		{"ltx25", "", "comfyui-video:ltx25", "ltx25"},
		{"wan22", "", "comfyui-video:wan22", "wan2.2"},
		// the override arm: Wan renders, so BOTH surfaces say Wan
		{"ltx25", "wan", "comfyui-video:wan22", "wan2.2"},
		{"", "ltx25", "comfyui-video:ltx25", "ltx25"},
		{"ltx25", "hunyuan", "comfyui-video:hunyuan", "hunyuan"},
		// unrecognized override: the runner silently renders Wan, so provenance
		// must say Wan rather than echo the caller's typo
		{"ltx25", "LTX25", "comfyui-video:wan22", "wan2.2"},
		{"ltx25", "ltx2.5", "comfyui-video:wan22", "wan2.2"},
	} {
		_, fam := resolveVideoFamily(config.Config{VideoGenFamily: c.cfgFamily}, c.reqModel)
		if got := videoModelLabel(fam); got != c.wantLabel {
			t.Errorf("cfg=%q req=%q: ledger label = %q, want %q", c.cfgFamily, c.reqModel, got, c.wantLabel)
		}
		if got := videoFootprintFamily(fam); got != c.wantFootprint {
			t.Errorf("cfg=%q req=%q: footprint family = %q, want %q", c.cfgFamily, c.reqModel, got, c.wantFootprint)
		}
	}
}

// TestVideoFootprintFamilyMatchesTheAdvertisedFamily is the cross-package guard for
// the namespace split the review found: the pipeline WRITES the footprint family
// and fleetnode.familyFor ADVERTISES it on /fleet/health. If they diverge, a node
// advertises a family whose measurements land under a key nothing advertised, and
// the dispatcher has nothing to admit against.
//
// It asserts against the REAL advertiser — fleetnode.Families — never against
// hardcoded strings. Two prior versions of this guard were vacuous and each was
// proven so by mutation: the first compared videoFootprintFamily to a 3-row table
// of literals (reverting the familyFor fix left every package green, because
// nothing read the advertiser), justified by a comment claiming importing
// fleetnode "would be an import cycle" — which was false; internal/pipeline
// already imports it. The input list deliberately includes the unrecognized
// values (wrong case, typos, the request-level "wan" spelling pasted into
// config), because that is exactly the subspace where the passthrough advertiser
// and the folding writer used to split.
func TestVideoFootprintFamilyMatchesTheAdvertisedFamily(t *testing.T) {
	for _, cfgFamily := range []string{
		"", "wan22", "ltx25", "hunyuan", "ace", // the recognized space
		"wan", "LTX25", "ltx2.5", " ltx25 ", "Wan22", // the split-prone space
	} {
		cfg := config.Config{VideoGenScript: "render/comfy-video.mjs", VideoGenFamily: cfgFamily}
		_, fam := resolveVideoFamily(cfg, "")
		writes := videoFootprintFamily(fam)
		advertised := fleetnode.Families(cfg)
		if len(advertised) != 1 || advertised[0] != writes {
			t.Errorf("videogen_family=%q: pipeline WRITES footprint family %q but fleetnode ADVERTISES %v — writer and advertiser must be one namespace",
				cfgFamily, writes, advertised)
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

// TestVideoRunnerFamiliesMatchTheRunner pins the Go-side family allowlist against
// the RUNNER's own dispatch literals in render/comfy-video.mjs.
//
// This is the guard that keeps the allowlist honest. canonicalVideoFamily records
// anything outside videoRunnerFamilies as Wan, on the premise that the runner
// silently falls through to Wan for unrecognized values. If someone adds a family
// to the runner and forgets this map, that new family renders correctly but is
// RECORDED as Wan — a false provenance row, the exact class this all exists to
// close, arriving silently. Same contract-assertion shape as
// install_gatedmodels_contract_test.go's filename pins: assert against the real
// artifact, never against another Go literal.
func TestVideoRunnerFamiliesMatchTheRunner(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "render", "comfy-video.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)

	// Every family this resolver claims the runner dispatches on must appear as an
	// exact-equality test in the runner.
	for fam := range videoRunnerFamilies {
		if !strings.Contains(src, `=== "`+fam+`"`) {
			t.Errorf("videoRunnerFamilies lists %q but render/comfy-video.mjs has no `=== %q` dispatch — the resolver would record a family the runner never selects", fam, fam)
		}
	}

	// And every family the runner DOES dispatch on must be listed here, or its
	// renders get recorded as Wan. Scan the runner for `model === "x"` /
	// `flags.model === "x"` literals.
	re := regexp.MustCompile(`model === "([a-z0-9._-]+)"`)
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		fam := m[1]
		if _, ok := videoRunnerFamilies[fam]; !ok {
			t.Errorf("render/comfy-video.mjs dispatches on family %q but videoRunnerFamilies does not list it — renders of that family would be RECORDED AS WAN in both the ledger and the footprint store", fam)
		}
	}

	// Guard the guard: a regex that matches nothing would make the second loop a
	// no-op and this test half-vacuous.
	if len(re.FindAllStringSubmatch(src, -1)) == 0 {
		t.Fatal("found no `model === \"...\"` dispatch literals in render/comfy-video.mjs — the runner was restructured and this contract test now proves nothing")
	}
}
