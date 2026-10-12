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
)

// Violation is one broken rule in one place of a serving config.
type Violation struct {
	Rule  string // the rule id: ttl, ngl0, cvd-empty, persistent, preload, n-cpu-moe, cpu-moe
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

// Violations formats an audit result as one line per violation.
func Violations(vs []Violation) string {
	lines := make([]string, 0, len(vs))
	for _, v := range vs {
		lines = append(lines, v.String())
	}
	return strings.Join(lines, "\n")
}
