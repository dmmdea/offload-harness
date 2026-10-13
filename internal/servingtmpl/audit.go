// audit.go — the serving-config gate (plan v2 register H-01, INV-1 / INV-2).
//
// The operator's hard rules of 2026-09-10 are that the cards do the inference
// (no `-ngl 0`, no empty CUDA_VISIBLE_DEVICES, RAM is overflow only) and that
// every idle model unloads at five minutes (`ttl: 300` on every entry of every
// engine, no persistent group, no preload hook). The live boxes complied by
// hand and the templates were fixed by hand twice (0.115.4, 0.115.7); nothing
// stopped the next regression. This audit is that stop: `install render`
// refuses a rendered config that breaks a rule, the template test runs the
// same checker over every shipped template, and `local-offload audit-yaml`
// runs it over a live file at session start (H-02).
package servingtmpl

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// Violation is one broken rule in one place of a serving config.
type Violation struct {
	Rule  string // the rule id: ttl, ngl0, cvd-empty, persistent, preload, n-cpu-moe, cpu-moe, pin-by-index, pin-unresolved
	Where string // model / group name, or "top-level"
	Text  string // the operator-facing sentence
}

func (v Violation) String() string { return fmt.Sprintf("%s (%s): %s", v.Rule, v.Where, v.Text) }

// TTLRequired is the one ttl every model entry must carry (seconds).
const TTLRequired = 300

var (
	nglZeroRe = regexp.MustCompile(`(?:^|\s)(?:-ngl|--n-gpu-layers|--gpu-layers)\s+0(?:\s|$)`)
	cvdEmpty  = regexp.MustCompile(`(?m)CUDA_VISIBLE_DEVICES=(?:\s*$|["']\s*["'])`)
)

// Audit checks one llama-swap config (a template or a live file) against the
// rules and returns every violation, sorted for stable output. A config that
// does not parse is a single violation of rule "yaml".
func Audit(text string) []Violation {
	var doc struct {
		Models map[string]map[string]any `yaml:"models"`
		Groups map[string]map[string]any `yaml:"groups"`
		Hooks  map[string]any            `yaml:"hooks"`
		Macros any                       `yaml:"macros"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return []Violation{{Rule: "yaml", Where: "top-level", Text: "not parseable YAML: " + err.Error()}}
	}
	macros := macroTable(doc.Macros)
	var out []Violation
	add := func(rule, where, text string) { out = append(out, Violation{Rule: rule, Where: where, Text: text}) }
	// A template whose every model is a tier seat (rk3588's) has an empty models map until it
	// is rendered; Render refuses a result that still serves nothing, so the exception cannot
	// ship an empty config.
	if len(doc.Models) == 0 && !anchorRe.MatchString(text) {
		add("models", "top-level", "no models mapping")
	}
	for name, m := range doc.Models {
		ttl, ok := m["ttl"]
		switch {
		case !ok:
			add("ttl", name, "no ttl — the model would stay loaded forever (INV-2: ttl 300 on every entry)")
		default:
			if v, isInt := ttl.(int); !isInt || v != TTLRequired {
				add("ttl", name, fmt.Sprintf("ttl %v, want %d (INV-2: every idle model unloads at five minutes)", ttl, TTLRequired))
			}
		}
		// What llama-swap runs is the entry with its `${name}` macros substituted, so that is what
		// the rules read: a flag placed in `macros:` is run by every entry that references it.
		cmd, _ := m["cmd"].(string)
		cmd = expandMacros(cmd, macros)
		if cmd != "" && nglZeroRe.MatchString(cmd) {
			add("ngl0", name, "cmd runs the model on the CPU (-ngl 0 / --n-gpu-layers 0): INV-1, the cards do the inference")
		}
		if cvdEmpty.MatchString(expandMacros(flatEnv(m["env"]), macros)) {
			add("cvd-empty", name, "env sets CUDA_VISIBLE_DEVICES to nothing — a CPU-only process (INV-1)")
		}
		if cmd != "" && cvdEmpty.MatchString(cmd) {
			add("cvd-empty", name, "cmd sets CUDA_VISIBLE_DEVICES to nothing — a CPU-only process (INV-1)")
		}
	}
	for name, g := range doc.Groups {
		if p, ok := g["persistent"].(bool); ok && p {
			add("persistent", name, "group is persistent — its members never unload (INV-2)")
		}
	}
	if len(doc.Hooks) > 0 {
		if s := fmt.Sprint(doc.Hooks); strings.Contains(s, "preload") {
			add("preload", "hooks", "a preload hook loads a model with no request in flight (INV-2: a model loaded without active work is a violation)")
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rule != out[j].Rule {
			return out[i].Rule < out[j].Rule
		}
		return out[i].Where < out[j].Where
	})
	return out
}

var (
	// nCPUMoeFlagRe matches llama.cpp's partial expert placement in any spelling of the
	// flag: `--n-cpu-moe N`, `--n-cpu-moe=N` and the short `-ncmoe N`.
	nCPUMoeFlagRe = regexp.MustCompile(`(?:^|\s)(?:--n-cpu-moe|-ncmoe)(?:\s+|=)(\d+)(?:\s|$)`)
	// nCPUMoeEnvRe is the flag's environment twin, which llama-server reads the same way.
	nCPUMoeEnvRe = regexp.MustCompile(`(?:^|[\s,\[])LLAMA_ARG_N_CPU_MOE=(\d+)`)
)

// AuditSpill is the H-01 rule Audit cannot carry, because Audit reads one config with no
// tier in hand: `--n-cpu-moe` above the tier's MEASURED spill (INV-1). The cards do the
// inference and RAM is overflow only; the single sanctioned host-RAM use is a PARTIAL
// spill of a model that does not fit its card, and only up to the number of expert layers
// a measurement showed was needed. A tier that recorded no measurement (maxNCPUMoE 0) has
// no sanctioned spill at all, so any `--n-cpu-moe N` with N above zero is refused; N 0
// spills nothing and is compliant.
//
// It checks the flag in every spelling and its LLAMA_ARG_N_CPU_MOE environment twin, on
// every entry, reading each entry the way llama-swap runs it (a `${name}` macro is replaced
// by its text first: the templates keep their shared flags in `macros:`), and reports every
// offender (sorted by entry) so a fix is one pass. The
// every-expert `--cpu-moe` is a different rule (AuditCPUMoE, whose exemptions belong to its
// callers; the table level also refuses a GPU tier that seeds `cpu_moe`), and a document that
// does not parse is Audit's finding, not this rule's: reporting it twice would only bury it.
func AuditSpill(text string, maxNCPUMoE int) []Violation {
	var doc struct {
		Models map[string]map[string]any `yaml:"models"`
		Macros any                       `yaml:"macros"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil
	}
	macros := macroTable(doc.Macros)
	var out []Violation
	for name, m := range doc.Models {
		cmd, _ := m["cmd"].(string)
		cmd = expandMacros(cmd, macros)
		env := expandMacros(flatEnv(m["env"]), macros)
		worst, spelling := 0, ""
		see := func(re *regexp.Regexp, from, label string) {
			for _, mm := range re.FindAllStringSubmatch(from, -1) {
				n, err := strconv.Atoi(mm[1])
				if err != nil {
					n = math.MaxInt // digits too long for an int are certainly above any measured spill
				}
				if n > worst {
					worst, spelling = n, label
				}
			}
		}
		see(nCPUMoeFlagRe, cmd, "--n-cpu-moe")
		see(nCPUMoeEnvRe, env, "LLAMA_ARG_N_CPU_MOE")
		if worst <= maxNCPUMoE {
			continue
		}
		limit := fmt.Sprintf("the tier's measured spill is %d", maxNCPUMoE)
		if maxNCPUMoE == 0 {
			limit = "the tier has no measured spill (n_cpu_moe_max 0), so none is sanctioned"
		}
		out = append(out, Violation{Rule: "n-cpu-moe", Where: name, Text: fmt.Sprintf(
			"%s %d spills %d expert layers to the CPU, but %s (INV-1: RAM is overflow only, and only up to a measured spill)",
			spelling, worst, worst, limit)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Where < out[j].Where })
	return out
}

// macroRefRe matches a llama-swap macro reference, `${name}`. llama-swap's own substitutions
// (`${PORT}`, `${MODEL_ID}`) match it too and are left alone, because no macro carries their name.
var macroRefRe = regexp.MustCompile(`\$\{([A-Za-z0-9_-]+)\}`)

// macroDepth bounds the expansion of a macro that references another macro, so a cycle (a config
// llama-swap would refuse on its own) terminates here instead of looping.
const macroDepth = 8

// macroTable reads a config's `macros:` block as name -> text. Anything that is not a mapping of
// scalars is ignored: llama-swap refuses that config itself, and a rule about flags is not the
// place to say so.
func macroTable(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		switch val.(type) {
		case string, int, int64, float64, bool:
			out[k] = fmt.Sprint(val)
		}
	}
	return out
}

// expandMacros substitutes every `${name}` that names a macro with the macro's text, so a rule
// reads the command line llama-swap will run and not the one that was written. A reference that
// names no macro stays as it is.
func expandMacros(s string, macros map[string]string) string {
	if len(macros) == 0 || !strings.Contains(s, "${") {
		return s
	}
	for i := 0; i < macroDepth; i++ {
		next := macroRefRe.ReplaceAllStringFunc(s, func(ref string) string {
			if v, ok := macros[ref[2:len(ref)-1]]; ok {
				return v
			}
			return ref
		})
		if next == s {
			break
		}
		s = next
	}
	return s
}

// flatEnv renders a model's env (llama-swap accepts a list of "K=V" strings)
// as one text for the regex checks.
func flatEnv(v any) string {
	switch e := v.(type) {
	case []any:
		parts := make([]string, 0, len(e))
		for _, x := range e {
			parts = append(parts, fmt.Sprint(x))
		}
		return strings.Join(parts, "\n")
	case string:
		return e
	}
	return ""
}

var (
	// cpuMoeFlags are llama.cpp's every-expert placement in every spelling: `--cpu-moe` and its
	// short form, and the same two for the draft model of a speculative pair (`--cpu-moe-draft`,
	// `--spec-draft-cpu-moe`, `-cmoed`), whose experts are just as much in host RAM.
	cpuMoeFlags = map[string]bool{
		"--cpu-moe": true, "-cmoe": true,
		"--spec-draft-cpu-moe": true, "--cpu-moe-draft": true, "-cmoed": true,
	}
	// cpuMoeEnvRe matches the environment twins of those flags, with the value (bare or quoted)
	// so a switched-off twin (`LLAMA_ARG_CPU_MOE=0`) is told from a set one.
	cpuMoeEnvRe = regexp.MustCompile(`(?:^|[\s,\["'])(LLAMA_ARG_CPU_MOE|LLAMA_ARG_SPEC_DRAFT_CPU_MOE)=(?:"([^"]*)"|'([^']*)'|([^\s"',\]]*))`)
)

// cpuMoeEnvSet says whether an environment twin's value turns the flag on. llama.cpp documents
// true/1/on/enabled as on and false/0/off/disabled as off for its boolean options (the llama-server
// README); what it does with any other string is not documented, so this reads it as ON: a gate that
// must not miss a placement refuses an unknown spelling rather than assuming llama.cpp ignores it.
// An empty value is a variable that is present but not set to anything, which is off.
func cpuMoeEnvSet(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "off", "disabled":
		return false
	}
	return true
}

// AuditCPUMoE is the W4 rule Audit cannot carry, because the exemption belongs to the caller: the
// EVERY-expert placement, `--cpu-moe` (INV-1). The cards do the inference and RAM is overflow only;
// the one sanctioned host-RAM use for experts is a PARTIAL spill, `--n-cpu-moe N`, up to a measured N
// (AuditSpill). The bare form parks every expert layer of the model in host RAM. It checks the flag in
// every spelling and its LLAMA_ARG_CPU_MOE environment twin, and the draft-model forms, on every entry,
// reading each entry the way llama-swap runs it (a `${name}` macro is replaced by its text first).
//
// It is pure and exemption-free. The cpu backend has no card to overflow from, and an off-matrix render
// writes the literal on purpose; whether either applies is the caller's evidence to read (`install
// render` holds the resolved profile and runs before the stamp exists; `audit-yaml` holds the file and
// its stamp). It reads parsed entries, never the raw text: a stamped file carries
// `"moe_26b": "--cpu-moe -ngl 999"` in its stamp comment. One finding per offending entry, sorted by
// entry; a document that does not parse is Audit's finding, not this rule's.
func AuditCPUMoE(text string) []Violation {
	var doc struct {
		Models map[string]map[string]any `yaml:"models"`
		Macros any                       `yaml:"macros"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil
	}
	macros := macroTable(doc.Macros)
	var out []Violation
	for name, m := range doc.Models {
		cmd, _ := m["cmd"].(string)
		cmd = expandMacros(cmd, macros)
		env := expandMacros(flatEnv(m["env"]), macros)
		var found []string
		see := func(spelling string) {
			for _, f := range found {
				if f == spelling {
					return
				}
			}
			found = append(found, spelling)
		}
		for _, tok := range strings.FieldsFunc(cmd, func(r rune) bool { return unicode.IsSpace(r) || r == '"' || r == '\'' }) {
			if cpuMoeFlags[tok] {
				see(tok)
			}
		}
		for _, from := range []string{env, cmd} {
			for _, mm := range cpuMoeEnvRe.FindAllStringSubmatch(from, -1) {
				if cpuMoeEnvSet(mm[2] + mm[3] + mm[4]) {
					see(mm[1])
				}
			}
		}
		if len(found) == 0 {
			continue
		}
		out = append(out, Violation{Rule: "cpu-moe", Where: name, Text: fmt.Sprintf(
			"%s keeps every expert layer of the model in host RAM (INV-1: the cards do the inference and RAM is overflow only; "+
				"the sanctioned spill is a measured partial --n-cpu-moe N, never the every-expert form)",
			strings.Join(found, ", "))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Where < out[j].Where })
	return out
}

var (
	// cvdValueRe matches a CUDA_VISIBLE_DEVICES assignment and captures its value, bare or quoted.
	cvdValueRe = regexp.MustCompile(`(?:^|[\s,\["'])CUDA_VISIBLE_DEVICES=(?:"([^"]*)"|'([^']*)'|([^\s"']*))`)
	// cudaOrderPCIRe matches CUDA_DEVICE_ORDER=PCI_BUS_ID, the setting that makes a CUDA index the
	// nvidia-smi index (CUDA's own default order is fastest-first).
	cudaOrderPCIRe = regexp.MustCompile(`(?:^|[\s,\["'])CUDA_DEVICE_ORDER=["']?PCI_BUS_ID\b`)
)

// entryPin is the device pin in force for one entry: its CUDA_VISIBLE_DEVICES value, and whether the
// entry orders CUDA's devices by PCI bus id.
type entryPin struct {
	value string
	pci   bool
}

// pinOf reads an entry the way llama-swap runs it: macros substituted, and when the variable is set
// more than once the last setting wins (a setting inline in the command is applied over the env list).
func pinOf(m map[string]any, macros map[string]string) (entryPin, bool) {
	cmd, _ := m["cmd"].(string)
	cmd = expandMacros(cmd, macros)
	env := expandMacros(flatEnv(m["env"]), macros)
	last := func(s string) (string, bool) {
		v, found := "", false
		for _, mm := range cvdValueRe.FindAllStringSubmatch(s, -1) {
			v, found = mm[1]+mm[2]+mm[3], true
		}
		return v, found
	}
	v, ok := last(cmd)
	if !ok {
		v, ok = last(env)
	}
	if !ok {
		return entryPin{}, false
	}
	return entryPin{value: v, pci: cudaOrderPCIRe.MatchString(env) || cudaOrderPCIRe.MatchString(cmd)}, true
}

// pinTokens splits a CUDA_VISIBLE_DEVICES value into its plain indices, its GPU UUIDs, and the tokens
// that are neither (a typo, a bare UUID without its GPU- prefix, a token with a non-ASCII hyphen): CUDA
// cannot resolve those either. Four things are not judged here: an empty value or an empty token between
// commas (Audit's cvd-empty rule owns the empty value), -1 (it hides every card, so it is no pin on a
// card), and a MIG instance id (a slice of a data-centre card, which a table of whole cards cannot
// resolve).
func pinTokens(value string) (indices []int, uuids []string, others []string) {
	for _, tok := range strings.Split(value, ",") {
		tok = strings.Trim(strings.TrimSpace(tok), `"'`)
		if tok == "" || tok == "-1" || (len(tok) >= 4 && strings.EqualFold(tok[:4], "MIG-")) {
			continue
		}
		if n, err := strconv.Atoi(tok); err == nil && n >= 0 && tok[0] != '+' {
			indices = append(indices, n)
			continue
		}
		if len(tok) >= 4 && strings.EqualFold(tok[:4], "GPU-") {
			uuids = append(uuids, tok)
			continue
		}
		others = append(others, tok)
	}
	return indices, uuids, others
}

// PinsCUDADevices reports whether any entry pins CUDA devices, so a caller reads the box's card table
// (an nvidia-smi run) only for a file that needs it. A document that does not parse pins nothing.
func PinsCUDADevices(text string) bool {
	var doc struct {
		Models map[string]map[string]any `yaml:"models"`
		Macros any                       `yaml:"macros"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return false
	}
	macros := macroTable(doc.Macros)
	for _, m := range doc.Models {
		if p, ok := pinOf(m, macros); ok {
			if idx, uu, other := pinTokens(p.value); len(idx)+len(uu)+len(other) > 0 {
				return true
			}
		}
	}
	return false
}

// AuditPins is the A-87 rule: on a box with two cards of the same model, a device pin is by UUID, and
// every UUID names a card. It reads the entries of a live file against the box's own card table
// (gpuprobe.ReadCards) and returns two kinds of finding.
//
//   - pin-by-index: an entry whose CUDA_VISIBLE_DEVICES is a list of plain indices at least one of which
//     names a card whose model name another card also carries. An index is the card's place on the PCI
//     bus; nothing in the config records which of two identical cards was meant, and a power loss or a
//     BIOS reset that reorders enumeration moves the seat to the other one without a word. The index of a
//     card no other card resembles names exactly one card and is left alone. An index maps to the
//     nvidia-smi index only under CUDA_DEVICE_ORDER=PCI_BUS_ID (CUDA's default order is fastest-first), so
//     an entry that does not set it is judged by the weaker form: any plain index while two cards share a
//     name. A card table that gives no model names cannot show two cards to be identical, so it never
//     makes an index ambiguous.
//   - pin-unresolved: a pin that names no card: a GPU UUID (or UUID prefix) that matches no card, or
//     several; a plain index that no card has (under PCI_BUS_ID the nvidia-smi index of a card on the
//     table, otherwise a position past the last card); or a token that is neither a plain index nor a
//     GPU- UUID. CUDA hides every device from the first pin it cannot resolve, so the seat runs on fewer
//     cards than it was sized for or on none (INV-1). -1 and a MIG instance id are not judged.
//
// The severity is the caller's, because only it holds the stamp: a seed cannot carry a UUID, so the
// index pins of a verbatim render are the renderer's own output and a note, while the same pin in a
// hand-maintained file is its maintainer's choice and a violation. With no card table (cards empty)
// there is nothing to compare with and the result is empty; the caller says so.
func AuditPins(text string, cards []gpuprobe.Card) []Violation {
	if len(cards) == 0 {
		return nil
	}
	var doc struct {
		Models map[string]map[string]any `yaml:"models"`
		Macros any                       `yaml:"macros"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil
	}
	macros := macroTable(doc.Macros)

	// Cards that share a model name, as groups of nvidia-smi indices, keyed by the lower-cased name.
	byName := map[string][]int{}
	nameOf := map[string]string{}
	cardAt := map[int]gpuprobe.Card{}
	for _, c := range cards {
		cardAt[c.NvidiaIndex] = c
		if n := strings.ToLower(strings.TrimSpace(c.Name)); n != "" {
			byName[n] = append(byName[n], c.NvidiaIndex)
			nameOf[n] = strings.TrimSpace(c.Name)
		}
	}
	var shared []string // names carried by more than one card, in order of their first card
	for n, idx := range byName {
		sort.Ints(idx)
		if len(idx) > 1 {
			shared = append(shared, n)
		}
	}
	sort.Slice(shared, func(i, j int) bool { return byName[shared[i]][0] < byName[shared[j]][0] })
	describe := func(n string) string {
		idx := byName[n]
		parts := make([]string, len(idx))
		for i, x := range idx {
			parts[i] = strconv.Itoa(x)
		}
		both := "both"
		if len(idx) > 2 {
			both = "all"
		}
		return fmt.Sprintf("cards %s and %s are %s %s", strings.Join(parts[:len(parts)-1], ", "), parts[len(parts)-1], both, nameOf[n])
	}

	var out []Violation
	for name, m := range doc.Models {
		pin, ok := pinOf(m, macros)
		if !ok {
			continue
		}
		tokenIndices, uuids, others := pinTokens(pin.value)

		var unresolved []string
		for _, u := range uuids {
			if _, err := gpuprobe.ResolveCards(cards, []string{u}); err != nil {
				unresolved = append(unresolved, fmt.Sprintf("%s (%v)", u, err))
			}
		}
		// An index names a card when a card has that nvidia-smi index (CUDA_DEVICE_ORDER=PCI_BUS_ID), and
		// otherwise when it is a position among the box's cards in CUDA's own order, which only the count
		// can bound.
		var indices []int // the indices that name a card
		for _, ix := range tokenIndices {
			named := ix < len(cards)
			if pin.pci {
				_, named = cardAt[ix]
			}
			if named {
				indices = append(indices, ix)
				continue
			}
			unresolved = append(unresolved, fmt.Sprintf("index %d (this box has cards %s)", ix, cardIndexList(pin.pci, cardAt, len(cards))))
		}
		for _, o := range others {
			unresolved = append(unresolved, fmt.Sprintf("%q (neither a plain index nor a GPU- UUID)", o))
		}
		if len(unresolved) > 0 {
			out = append(out, Violation{Rule: "pin-unresolved", Where: name, Text: fmt.Sprintf(
				"CUDA_VISIBLE_DEVICES=%s names no card on this box: %s. CUDA hides every device from the first pin it cannot resolve, "+
					"so the seat runs on fewer cards than it was sized for, or on none (INV-1); nvidia-smi -L lists the UUIDs this box has",
				pin.value, strings.Join(unresolved, "; "))})
		}

		var ambiguous []string // the groups of identical cards the pinned indices could mean
		seen := map[string]bool{}
		add := func(n string) {
			if !seen[n] {
				seen[n] = true
				ambiguous = append(ambiguous, describe(n))
			}
		}
		if len(indices) > 0 {
			if pin.pci {
				for _, ix := range indices {
					c, found := cardAt[ix]
					if !found {
						continue
					}
					if n := strings.ToLower(strings.TrimSpace(c.Name)); len(byName[n]) > 1 {
						add(n)
					}
				}
			} else {
				for _, n := range shared {
					add(n)
				}
			}
		}
		if len(ambiguous) > 0 {
			why := ""
			if !pin.pci {
				why = " (this entry does not set CUDA_DEVICE_ORDER=PCI_BUS_ID, so its index follows CUDA's own fastest-first order and cannot be mapped to a card at all)"
			}
			out = append(out, Violation{Rule: "pin-by-index", Where: name, Text: fmt.Sprintf(
				"CUDA_VISIBLE_DEVICES=%s pins by index, but %s: an index is the card's place on the PCI bus, and a power loss or a BIOS reset "+
					"that reorders enumeration moves the seat to the other card without a word%s. Pin by UUID instead (GPU-..., from nvidia-smi -L)",
				pin.value, strings.Join(ambiguous, "; "), why)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Where != out[j].Where {
			return out[i].Where < out[j].Where
		}
		return out[i].Rule < out[j].Rule
	})
	return out
}

// Violations formats an audit result as one line per violation.
func Violations(vs []Violation) string {
	lines := make([]string, 0, len(vs))
	for _, v := range vs {
		lines = append(lines, v.String())
	}
	return strings.Join(lines, "\n")
}

// cardIndexList names the indices a CUDA_VISIBLE_DEVICES index can take on this box, for a finding: the
// nvidia-smi indices of the cards under PCI_BUS_ID, and 0..n-1 in CUDA's own order.
func cardIndexList(pci bool, cardAt map[int]gpuprobe.Card, n int) string {
	var ix []int
	if pci {
		for i := range cardAt {
			ix = append(ix, i)
		}
		sort.Ints(ix)
	} else {
		for i := 0; i < n; i++ {
			ix = append(ix, i)
		}
	}
	parts := make([]string, len(ix))
	for i, x := range ix {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ", ")
}
