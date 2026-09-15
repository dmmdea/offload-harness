package servingtmpl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediaseat"
	"github.com/dmmdea/offload-harness/internal/tierseed"
	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// tierTable is the COMMITTED tier table — never a fixture. A fixture would pass
// while the shipped declaration was broken, which is the exact class of failure
// this file exists to catch.
type tierTable struct {
	Profiles map[string]struct {
		Composes   []string           `json:"composes"`
		Layers     []config.LayerSpec `json:"layers"`
		MediaSeats []mediaseat.Seat   `json:"media_seats"`
		VLLMSeat   *vllmseat.Spec     `json:"vllm_seat"`
		GPUEnv     []string           `json:"gpu_env"`
		CtxSize    int                `json:"ctx_size"`
		KVType     string             `json:"kv_type"`
		FlashAttn  string             `json:"flash_attn"`
		MoE26B     string             `json:"moe_26b"`
		Include26B bool               `json:"include_26b"`
		IncludeQ38 bool               `json:"include_qwen38"`
	} `json:"profiles"`
}

func readTierTable(t *testing.T) tierTable {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc tierTable
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// renderTriple renders the committed blackwell-3x16 declaration through its own
// template, exactly as `install render` does.
func renderTriple(t *testing.T) (string, CompositeDecl) {
	t.Helper()
	doc := readTierTable(t)
	p3, ok := doc.Profiles["blackwell-3x16"]
	if !ok {
		t.Fatal("no blackwell-3x16 in the tier table")
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "llama-swap.win-triple-blackwell.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	params := params()
	params.GOOS = "windows"
	params.Home = "C:/llama-swap"
	params.Ctx, params.KVType, params.FlashAttn = p3.CtxSize, p3.KVType, p3.FlashAttn
	params.MoE26B, params.Include26B, params.IncludeQ38 = p3.MoE26B, p3.Include26B, p3.IncludeQ38
	params.Seats, params.GPUEnv = p3.MediaSeats, p3.GPUEnv
	params.VLLMSeat = p3.VLLMSeat
	params.VLLMRuntime = vllmseat.Runtime{
		User: "BOX\\operator", ProxyHost: "127.0.0.1",
		StackDir: "C:/llama-swap", SeatDir: "C:/llama-swap/seat",
		VenvDir: "/venv", HFHome: "/hf", Distro: "freetoken", WSLSeatDir: "/seat",
	}
	var kinds []string
	for _, s := range p3.MediaSeats {
		kinds = append(kinds, s.Kind)
	}
	// The layers are RESOLVED the way tierseed seeds them: the tier leaves the
	// pair's agent seat bare so the seat's numbers live in one place, and a
	// check run against the bare declaration would test a shape no box runs.
	decl := CompositeDecl{Tier: "blackwell-3x16", Composes: p3.Composes,
		Layers: tierseed.FillPairAgent(p3.Layers, p3.VLLMSeat, true), MediaKinds: kinds}
	for i := range decl.Layers {
		if decl.Layers[i].DisplayDevice != "" && decl.Layers[i].Dormant {
			params.DisplayLayer = &decl.Layers[i]
		}
	}
	out, err := Render(string(raw), params)
	if err != nil {
		t.Fatalf("the composite tier's own template refused its own declaration: %v", err)
	}
	return out, decl
}

// composedFromTable builds the []ComposedTier for the tiers blackwell-3x16
// claims to be a complete instance of, from the table itself.
func composedFromTable(t *testing.T, decl CompositeDecl) []ComposedTier {
	t.Helper()
	doc := readTierTable(t)
	var out []ComposedTier
	for _, id := range decl.Composes {
		p, ok := doc.Profiles[id]
		if !ok {
			t.Fatalf("composes %q, which the tier table does not define", id)
		}
		c := ComposedTier{ID: id}
		for _, s := range p.MediaSeats {
			c.SeatKinds = append(c.SeatKinds, s.Kind)
		}
		if p.VLLMSeat != nil {
			c.VLLMSeatID, c.FallbackID = p.VLLMSeat.ID, p.VLLMSeat.Fallback
		}
		out = append(out, c)
	}
	return out
}

// TestCompositeTierIsTheCheckedUnionOfItsLayers renders the COMMITTED tier
// through its own template and checks the union: every layer seat exists, every
// seat sits on the cards its layer declares, and every capability the composed
// tiers bind is present. This is the gate on the defect that shipped twice —
// a media block copied from the 2-card tier onto a 3-card box, pinning the
// vision seat to the display card and naming the third card nowhere.
func TestCompositeTierIsTheCheckedUnionOfItsLayers(t *testing.T) {
	out, decl := renderTriple(t)
	if len(decl.Composes) == 0 || len(decl.Layers) == 0 {
		t.Fatalf("blackwell-3x16 must declare composes and layers: %+v", decl)
	}
	if err := CheckComposite(out, decl, composedFromTable(t, decl)); err != nil {
		t.Fatalf("the shipped composite tier does not render as the union it claims:\n%v", err)
	}
	// The rendered config must also pass the operator-rule audit: this is the
	// render a composite install writes, twins and all.
	if vs := Audit(out); len(vs) != 0 {
		t.Fatalf("the composite render breaks operator rules:\n%s", Violations(vs))
	}
}

func TestCheckCompositeRefusesASeatWhoseRenderedPinDiffersFromItsDeclaredDevice(t *testing.T) {
	out, decl := renderTriple(t)
	for i := range decl.Layers {
		for j := range decl.Layers[i].Seats {
			if decl.Layers[i].Seats[j].Role == "ocr" {
				decl.Layers[i].Seats[j].Device = "0" // the shipped render pins it to card 2
			}
		}
	}
	err := CheckComposite(out, decl, composedFromTable(t, decl))
	if err == nil || !strings.Contains(err.Error(), "declared device pin") {
		t.Fatalf("a seat rendered onto other cards than it declares must be refused, got %v", err)
	}
}

func TestCheckCompositeRefusesAMissingComposedCapability(t *testing.T) {
	out, decl := renderTriple(t)
	composed := composedFromTable(t, decl)
	composed = append(composed, ComposedTier{ID: decl.Composes[0], SeatKinds: []string{"stt-hq"}})
	err := CheckComposite(out, decl, composed)
	if err == nil || !strings.Contains(err.Error(), "stt-hq") {
		t.Fatalf("a capability the composed tier binds and this one does not must be refused, got %v", err)
	}

	// …and a composed tier whose vLLM seat is absent from the render: the
	// "complete instance" claim is exactly what that seat backs.
	composed = append(composedFromTable(t, decl), ComposedTier{ID: decl.Composes[1], VLLMSeatID: "a-seat-that-is-not-there"})
	err = CheckComposite(out, decl, composed)
	if err == nil || !strings.Contains(err.Error(), "a-seat-that-is-not-there") {
		t.Fatalf("a composed tier's missing vLLM seat must be refused, got %v", err)
	}
}

func TestCheckCompositeRefusesAnUndefinedLayerSeatOrModelMapTarget(t *testing.T) {
	out, decl := renderTriple(t)
	mutated := func(mut func(l *config.LayerSpec)) CompositeDecl {
		d := decl
		d.Layers = append([]config.LayerSpec(nil), decl.Layers...)
		for i := range d.Layers {
			d.Layers[i].Seats = append([]config.LayerSeat(nil), d.Layers[i].Seats...)
			mut(&d.Layers[i])
		}
		return d
	}
	seatGone := mutated(func(l *config.LayerSpec) {
		for j := range l.Seats {
			if l.Seats[j].Role == "long" {
				l.Seats[j].Model = "qwen3.8-27b-999k"
			}
		}
	})
	if err := CheckComposite(out, seatGone, composedFromTable(t, decl)); err == nil || !strings.Contains(err.Error(), "qwen3.8-27b-999k") {
		t.Fatalf("an undefined layer seat must be refused by name, got %v", err)
	}
	twinGone := mutated(func(l *config.LayerSpec) {
		for j := range l.Seats {
			if len(l.Seats[j].ModelMap) > 0 {
				m := map[string]string{}
				for k, v := range l.Seats[j].ModelMap {
					m[k] = v
				}
				m["workhorse"] = "gemma-4-e4b-that-never-rendered"
				l.Seats[j].ModelMap = m
			}
		}
	})
	if err := CheckComposite(out, twinGone, composedFromTable(t, decl)); err == nil || !strings.Contains(err.Error(), "never-rendered") {
		t.Fatalf("an undefined model_map target must be refused by name, got %v", err)
	}
}

// TestRenderedTemplatesDefineEveryMacroTheyUse: a ${macro} the template never
// defines renders a config llama-swap refuses at startup, and the tier that
// shipped one (the triple template's removed Flash-Next seats referenced
// ${server} and ${mdir}) looked perfectly fine in review. Every shipped
// template, every macro, mechanically.
func TestRenderedTemplatesDefineEveryMacroTheyUse(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "setup", "templates", "llama-swap.*.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no templates found: %v", err)
	}
	use := regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	def := regexp.MustCompile(`(?m)^ {2}([A-Za-z_][A-Za-z0-9_]*): `)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		macros := ""
		if i := strings.Index(body, "\nmacros:"); i >= 0 {
			macros = body[i:]
			if j := strings.Index(macros[1:], "\nmodels:"); j >= 0 {
				macros = macros[:j+1]
			}
		}
		defined := map[string]bool{"PORT": true} // llama-swap's own
		for _, m := range def.FindAllStringSubmatch(macros, -1) {
			defined[m[1]] = true
		}
		for _, m := range use.FindAllStringSubmatch(body, -1) {
			if !defined[m[1]] {
				t.Errorf("%s uses ${%s}, which no macro defines — llama-swap refuses the rendered config at startup",
					filepath.Base(path), m[1])
			}
		}
	}
}

// TestDisplayTwinsRenderOnlyForATierThatDeclaresADisplayLayer is the
// byte-identity gate on the display fences: a tier without a display layer
// renders exactly what it rendered before the fences existed, and a tier with
// one gets the twins — pinned to the display card and to NOTHING else, with
// every other seat still off that card.
func TestDisplayTwinsRenderOnlyForATierThatDeclaresADisplayLayer(t *testing.T) {
	withTwins, decl := renderTriple(t)
	var display *config.LayerSpec
	for i := range decl.Layers {
		if decl.Layers[i].DisplayDevice != "" && decl.Layers[i].Dormant {
			display = &decl.Layers[i]
		}
	}
	if display == nil {
		t.Fatal("blackwell-3x16 must declare a dormant display layer — the twins have no other reason to exist")
	}
	for _, twin := range display.Seats[0].ModelMap {
		if !strings.Contains(withTwins, "\n  "+twin+":") {
			t.Fatalf("declared twin %q did not render", twin)
		}
	}
	if !strings.Contains(withTwins, `display: "+residents & vagt & (e4bd | e2bd)"`) {
		t.Fatalf("the display set must name the vLLM seat's matrix var — the rung runs BESIDE the pair seat, never instead of it:\n%s", setsOf(withTwins))
	}

	// Every device-1 pin in the render belongs to a twin, and nothing else.
	for _, seat := range renderedSeats(withTwins) {
		if !strings.Contains(seat.body, "CUDA_VISIBLE_DEVICES=1") {
			continue
		}
		if !strings.HasSuffix(seat.name, "-display") {
			t.Errorf("seat %q is pinned to device 1 — that is the RTX 5070 Ti driving the desktop; only the display layer's twins may name it", seat.name)
		}
	}

	// The same tier WITHOUT a display layer: the fences leave nothing behind.
	doc := readTierTable(t)
	p3 := doc.Profiles["blackwell-3x16"]
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "llama-swap.win-triple-blackwell.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p := params()
	p.GOOS = "windows"
	p.Home = "C:/llama-swap"
	p.Ctx, p.KVType, p.FlashAttn = p3.CtxSize, p3.KVType, p3.FlashAttn
	p.MoE26B, p.Include26B, p.IncludeQ38 = p3.MoE26B, p3.Include26B, p3.IncludeQ38
	p.Seats, p.GPUEnv = p3.MediaSeats, p3.GPUEnv
	plain, err := Render(string(raw), p)
	if err != nil {
		t.Fatalf("render without a display layer: %v", err)
	}
	for _, marker := range []string{"__DISPLAY", "#|", "-display:", "e4bd", "e2bd"} {
		if strings.Contains(plain, marker) {
			t.Fatalf("a tier with no display layer left %q in the render", marker)
		}
	}
	if strings.Contains(plain, "CUDA_VISIBLE_DEVICES=1") {
		t.Fatal("a tier with no display layer must pin nothing to the display card")
	}
}

type renderedSeat struct{ name, body string }

var seatHeadRe = regexp.MustCompile(`(?m)^  ([A-Za-z0-9._-]+):$`)

// renderedSeats splits a rendered models mapping into one entry per seat.
func renderedSeats(rendered string) []renderedSeat {
	i := strings.Index(rendered, "\nmodels:")
	if i < 0 {
		return nil
	}
	body := rendered[i:]
	locs := seatHeadRe.FindAllStringSubmatchIndex(body, -1)
	var out []renderedSeat
	for n, loc := range locs {
		name := body[loc[2]:loc[3]]
		if name == "vars" || name == "evict_costs" || name == "sets" {
			continue
		}
		end := len(body)
		if n+1 < len(locs) {
			end = locs[n+1][0]
		}
		out = append(out, renderedSeat{name: name, body: body[loc[1]:end]})
	}
	return out
}

// setsOf returns the rendered matrix sets block, for failure messages.
func setsOf(rendered string) string {
	i := strings.Index(rendered, "  sets:")
	if i < 0 {
		return "(no sets block)"
	}
	return rendered[i:]
}

// TestEveryRenderedEnvEntrySurvivesYAMLAsOneString is the gate on a defect the
// checked union found in the shipped template: `env: [CUDA_VISIBLE_DEVICES=0,2]`
// is valid YAML that parses as TWO entries — "CUDA_VISIBLE_DEVICES=0" and 2 —
// so every two-card seat pinned that way was served as a ONE-card seat, in the
// same parser llama-swap uses. It cost nothing to read and nobody read it.
//
// The check runs over every shipped template AND over a rendered composite,
// because the torn entries came from both: two template literals and the
// media-seat renderer's bareword join.
func TestEveryRenderedEnvEntrySurvivesYAMLAsOneString(t *testing.T) {
	check := func(name, text string) {
		t.Helper()
		var doc struct {
			Models map[string]struct {
				Env []any `yaml:"env"`
			} `yaml:"models"`
		}
		if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
			t.Fatalf("%s: not parseable YAML: %v", name, err)
		}
		for seat, m := range doc.Models {
			for _, item := range m.Env {
				if _, ok := item.(string); !ok {
					t.Errorf("%s seat %q: env entry %v is not a string — a flow-list entry with a comma was torn in half, "+
						"so a two-card pin serves ONE card (quote the entry)", name, seat, item)
				}
			}
		}
	}
	paths, err := filepath.Glob(filepath.Join("..", "..", "setup", "templates", "llama-swap.*.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no templates: %v", err)
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		check(filepath.Base(p), string(raw))
	}
	rendered, _ := renderTriple(t)
	check("the rendered composite tier", rendered)
}

// TestFlowItemsQuotesOnlyWhatYAMLWouldTear keeps the fix minimal by name: an
// entry with no comma stays a bareword, so every existing render is byte-identical.
func TestFlowItemsQuotesOnlyWhatYAMLWouldTear(t *testing.T) {
	got := flowItems([]string{"CUDA_VISIBLE_DEVICES=0", "CUDA_VISIBLE_DEVICES=0,2", "CUDA_MODULE_LOADING=LAZY"})
	want := []string{"CUDA_VISIBLE_DEVICES=0", `"CUDA_VISIBLE_DEVICES=0,2"`, "CUDA_MODULE_LOADING=LAZY"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("flowItems[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
