// composite.go — D5 of the composite-tier design (ADR 0039): a composite
// tier's rendered serving config must be the CHECKED UNION of the tiers it
// composes, not a copy that drifted from them.
//
// The defect this exists to stop has already happened twice on the reference
// box: blackwell-3x16's media block was a byte-for-byte copy of the 2-card
// tier's, so the vision seat was pinned to the display card and the third card
// was named nowhere (0.113.33), and the tier shipped a template whose only
// long-context seats referenced macros the template never defined. Both were
// readable in the rendered text; nothing read it. CheckComposite reads it, and
// `install render` refuses a composite render that fails.
package servingtmpl

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dmmdea/offload-harness/internal/config"
)

// CompositeDecl is what the tier DECLARES: its id, the tiers it composes, the
// device layers it places work on, and the media-seat kinds it binds.
type CompositeDecl struct {
	Tier       string
	Composes   []string
	Layers     []config.LayerSpec
	MediaKinds []string
}

// ComposedTier is one tier the composite claims to be a complete instance of,
// reduced to the capabilities that claim has to survive: the media-seat kinds
// it declares and its vLLM seat id (empty when it declares none).
type ComposedTier struct {
	ID        string
	SeatKinds []string
	// VLLMSeatID and FallbackID are the engine seat that tier declares and the
	// llama.cpp seat it falls back to when the box has no vLLM runtime (a
	// supported outcome the renderer prints a NOTE for). The composition claim
	// survives EITHER: what must never happen is neither being served, which
	// would mean the composite cannot run that tier's agent lane at all.
	VLLMSeatID string
	FallbackID string
}

// CheckComposite refuses a composite render in which a layer seat is undefined,
// a layer seat's rendered CUDA_VISIBLE_DEVICES differs from its declared device
// pin, a router model_map target is undefined or pinned elsewhere, or a
// composed tier's declared capability (a media-seat kind, its vLLM seat) is
// missing. It reads the RENDERED text, because that is what llama-swap loads:
// a declaration that is right in profiles.json and wrong in the yaml is the
// shape every one of these defects took.
//
// It returns one error naming every failure, not the first: an operator fixing
// a tier table wants the whole list, and a test that reports one at a time
// turns a five-minute fix into five render cycles.
func CheckComposite(rendered string, decl CompositeDecl, composed []ComposedTier) error {
	var doc struct {
		Models map[string]struct {
			Aliases []string `yaml:"aliases"`
			Env     []string `yaml:"env"`
			Cmd     string   `yaml:"cmd"`
			Proxy   string   `yaml:"proxy"`
		} `yaml:"models"`
	}
	if err := yaml.Unmarshal([]byte(rendered), &doc); err != nil {
		return fmt.Errorf("composite tier %s: the rendered config is not parseable YAML: %w", decl.Tier, err)
	}
	// One name → entry index over ids AND aliases: a seat is declared by
	// whichever name the tier table uses, and llama-swap resolves both.
	type entry struct {
		id      string
		devices string
		// proxied marks a seat llama-swap does not launch itself (a vLLM engine
		// behind `proxy:`): its device pin lives in the engine's own unit/env,
		// which internal/vllmseat renders and gates, so demanding one here would
		// report every proxied seat as unpinned.
		proxied bool
	}
	byName := map[string]entry{}
	for id, m := range doc.Models {
		e := entry{id: id, devices: cudaDevicesOf(m.Env, m.Cmd), proxied: strings.TrimSpace(m.Proxy) != ""}
		byName[strings.ToLower(id)] = e
		for _, a := range m.Aliases {
			byName[strings.ToLower(strings.TrimSpace(a))] = e
		}
	}
	var problems []string
	// A seat's pin must be the pin it declared. The display-card law is
	// enforced elsewhere by name; this check is the general one: a seat that
	// renders onto different cards than the layer says is a placement decision
	// made on a fiction.
	checkSeat := func(layer config.LayerSpec, role, model, device string) {
		e, ok := byName[strings.ToLower(strings.TrimSpace(model))]
		if !ok {
			problems = append(problems, fmt.Sprintf("layer %s seat %s: model %q is not defined in the rendered config", layer.Name, role, model))
			return
		}
		want := strings.TrimSpace(device)
		if want == "" {
			return
		}
		if e.proxied {
			return // the engine owns its pin; vllmseat gates it
		}
		if e.devices == "" {
			problems = append(problems, fmt.Sprintf("layer %s seat %s (%s): declared device pin %q, but the rendered entry %q pins no CUDA_VISIBLE_DEVICES",
				layer.Name, role, model, want, e.id))
			return
		}
		if !sameDeviceSet(e.devices, want) {
			problems = append(problems, fmt.Sprintf("layer %s seat %s (%s): declared device pin %q, rendered %q",
				layer.Name, role, model, want, e.devices))
		}
	}
	for _, l := range decl.Layers {
		for _, s := range l.Seats {
			if s.Model != "" {
				checkSeat(l, s.Role, s.Model, s.Device)
			}
			// A router seat carries no model of its own; its model_map names
			// the twins it may substitute, and every one of them must exist
			// AND sit on the layer's device — a map target rendered onto
			// another card would move the rung off the layer it was chosen for.
			for route, twin := range s.ModelMap {
				checkSeat(l, s.Role+" model_map["+route+"]", twin, s.Device)
			}
			if s.Model == "" && len(s.ModelMap) == 0 && s.Role != "router" {
				problems = append(problems, fmt.Sprintf("layer %s seat %s: no model and no model_map — it can never be placed on", l.Name, s.Role))
			}
		}
	}
	// The composition claim: being "a complete blackwell-2x16" means the
	// capabilities that tier declares are all here. A kind the composed tier
	// binds and the composite does not is the 0.113.33 defect exactly.
	have := map[string]bool{}
	for _, k := range decl.MediaKinds {
		have[strings.ToLower(strings.TrimSpace(k))] = true
	}
	declared := map[string]bool{}
	for _, id := range decl.Composes {
		declared[id] = true
	}
	for _, c := range composed {
		if !declared[c.ID] {
			continue
		}
		for _, k := range c.SeatKinds {
			if !have[strings.ToLower(strings.TrimSpace(k))] {
				problems = append(problems, fmt.Sprintf("composes %s, which binds a %s seat this tier does not declare", c.ID, k))
			}
		}
		if c.VLLMSeatID != "" {
			_, engine := byName[strings.ToLower(c.VLLMSeatID)]
			_, fallback := byName[strings.ToLower(c.FallbackID)]
			if !engine && !fallback {
				problems = append(problems, fmt.Sprintf("composes %s, whose agent seat is in the rendered config under neither its engine id %q nor its fallback %q",
					c.ID, c.VLLMSeatID, c.FallbackID))
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("composite tier %s is not the checked union of %s:\n  - %s",
		decl.Tier, strings.Join(decl.Composes, ", "), strings.Join(problems, "\n  - "))
}

var cudaDevicesRe = regexp.MustCompile(`CUDA_VISIBLE_DEVICES=([0-9,]*)`)

// cudaDevicesOf reads a rendered entry's device pin from its env list first and
// its cmd second (a seat wrapper may export the variable inline). "" = the
// entry pins nothing, which for a layer seat is itself a finding: an unpinned
// seat runs wherever CUDA orders the cards that boot, and this board reorders
// them on power loss.
func cudaDevicesOf(env []string, cmd string) string {
	for _, e := range env {
		if m := cudaDevicesRe.FindStringSubmatch(e); m != nil {
			return m[1]
		}
	}
	if m := cudaDevicesRe.FindStringSubmatch(cmd); m != nil {
		return m[1]
	}
	return ""
}

// sameDeviceSet compares two CUDA_VISIBLE_DEVICES lists as SETS: "0,2" and
// "2,0" are the same two cards, and the order only decides which is cuda:0
// inside the process — a distinction the tensor-split flags own, not this
// check. Whitespace and empty entries are ignored.
func sameDeviceSet(a, b string) bool {
	norm := func(s string) []string {
		var out []string
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		sort.Strings(out)
		return out
	}
	x, y := norm(a), norm(b)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// Display-layer rendering. The twins live in the template as a COMMENTED,
// fenced block (`#|` lines between BEGIN/END markers) for three reasons: the
// raw template stays parseable YAML, an operator reads exactly what would be
// rendered instead of a Go string literal, and a tier that declares no display
// layer renders byte-identically to a build that had no display support at all.
const (
	displayTwinsBegin = "__DISPLAY_TWINS_BEGIN__"
	displayTwinsEnd   = "__DISPLAY_TWINS_END__"
	displayVarsBegin  = "__DISPLAY_VARS_BEGIN__"
	displayVarsEnd    = "__DISPLAY_VARS_END__"
	displaySetBegin   = "__DISPLAY_SET_BEGIN__"
	displaySetEnd     = "__DISPLAY_SET_END__"
	// fencePrefix marks a line that is rendered only when the display layer is
	// declared. It is two characters so no YAML comment can be mistaken for it.
	fencePrefix = "#|"
	// vllmVarToken is replaced with the matrix var of the tier's vLLM seat plus
	// its conjunction, so the display set reads "+residents & vagt & (…)": the
	// rung runs BESIDE the agent seat, never instead of it. A tier with no vLLM
	// seat renders it empty and the set is simply "+residents & (…)".
	vllmVarToken = "__VLLM_VAR__"
)

// renderDisplayLayer resolves the template's display fences: it drops them
// entirely when the tier declares no display layer, and otherwise uncomments
// the fenced lines and binds the vLLM matrix var. It refuses, by name, a tier
// that declares a display layer against a template that has no fences, and one
// whose model_map names a twin the rendered config does not define — the
// silent-capability-loss rule every other gated seat here follows.
func renderDisplayLayer(out string, layer *config.LayerSpec, vllmSeatID string) (string, error) {
	lines := strings.Split(out, "\n")
	hasFence := false
	for _, l := range lines {
		if strings.Contains(l, displayTwinsBegin) {
			hasFence = true
			break
		}
	}
	if layer == nil {
		return dropFences(lines), nil
	}
	if !hasFence {
		return "", fmt.Errorf("this tier declares a display layer (%s) but the target serving template carries no "+
			"%s block, so the layer's rungs would exist in the tier table and nowhere in the served config — add the "+
			"fenced twins (and their matrix var + set) to the template, or drop the layer from the tier",
			layer.Name, displayTwinsBegin)
	}
	out = uncommentFences(lines)
	varID := ""
	if vllmSeatID != "" {
		if id, ok := matrixVarFor(out, vllmSeatID); ok {
			varID = id + " & "
		}
	}
	out = strings.ReplaceAll(out, vllmVarToken, varID)
	for _, s := range layer.Seats {
		for route, twin := range s.ModelMap {
			if !definesModel(out, twin) {
				return "", fmt.Errorf("display layer %s: route %q names twin %q, which the rendered config does not define",
					layer.Name, route, twin)
			}
		}
	}
	return out, nil
}

// dropFences removes every display fence line AND every line it fences, which
// is what a non-composite (or display-less) tier renders: nothing.
func dropFences(lines []string) string {
	markers := []string{displayTwinsBegin, displayTwinsEnd, displayVarsBegin, displayVarsEnd, displaySetBegin, displaySetEnd}
	out := make([]string, 0, len(lines))
	inFence := false
	for _, l := range lines {
		isMarker := false
		for _, m := range markers {
			if strings.Contains(l, m) {
				isMarker = true
				inFence = strings.Contains(l, "BEGIN")
				break
			}
		}
		if isMarker {
			continue
		}
		// A comment line inside a fence is that block's own prose; a `#|` line
		// is its body. Both go.
		if inFence {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// uncommentFences keeps the fenced body (minus the `#|` prefix) and drops the
// markers and the prose comments between them.
func uncommentFences(lines []string) string {
	markers := []string{displayTwinsBegin, displayTwinsEnd, displayVarsBegin, displayVarsEnd, displaySetBegin, displaySetEnd}
	out := make([]string, 0, len(lines))
	inFence := false
	for _, l := range lines {
		isMarker := false
		for _, m := range markers {
			if strings.Contains(l, m) {
				isMarker = true
				inFence = strings.Contains(l, "BEGIN")
				break
			}
		}
		if isMarker {
			continue
		}
		if strings.HasPrefix(l, fencePrefix) {
			out = append(out, strings.TrimPrefix(l, fencePrefix))
			continue
		}
		if inFence {
			continue // the block's prose, not its body
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

var matrixVarLine = regexp.MustCompile(`(?m)^ {4}([A-Za-z0-9]{1,8}): +(\S+) *$`)

// matrixVarFor finds the matrix var bound to a model id. The display set names
// the vLLM seat by its VAR (llama-swap sets are written in vars), and that var
// is allocated at render time (vagt, vagt2, …) rather than fixed, so the set
// cannot hard-code it.
func matrixVarFor(out, model string) (string, bool) {
	for _, m := range matrixVarLine.FindAllStringSubmatch(out, -1) {
		if m[2] == model {
			return m[1], true
		}
	}
	return "", false
}

// flowItems renders env entries for a YAML FLOW list (`env: [a, b]`), quoting
// any entry a flow list would otherwise tear apart.
//
// This is not a style choice. `env: [CUDA_VISIBLE_DEVICES=0,2]` is valid YAML
// that parses as TWO entries — "CUDA_VISIBLE_DEVICES=0" and 2 — so every
// two-card seat the templates and the tier tables pinned to "0,2" was rendered
// as a ONE-card seat, silently, in the parser llama-swap itself uses. The 27B
// and its 262k twin (both `-sm layer --tensor-split 24,26`, both needing the
// pair) are exactly the seats that were pinned that way. Entries with no comma
// stay barewords so every existing render is byte-identical.
func flowItems(entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.ContainsAny(e, ",[]{}#") || strings.Contains(e, ": ") {
			out = append(out, strconv.Quote(e))
			continue
		}
		out = append(out, e)
	}
	return out
}
