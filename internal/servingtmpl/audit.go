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
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Violation is one broken rule in one place of a serving config.
type Violation struct {
	Rule  string // the rule id: ttl, ngl0, cvd-empty, persistent, preload
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
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return []Violation{{Rule: "yaml", Where: "top-level", Text: "not parseable YAML: " + err.Error()}}
	}
	var out []Violation
	add := func(rule, where, text string) { out = append(out, Violation{Rule: rule, Where: where, Text: text}) }
	if len(doc.Models) == 0 {
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
		if cmd, _ := m["cmd"].(string); cmd != "" && nglZeroRe.MatchString(cmd) {
			add("ngl0", name, "cmd runs the model on the CPU (-ngl 0 / --n-gpu-layers 0): INV-1, the cards do the inference")
		}
		if cvdEmpty.MatchString(flatEnv(m["env"])) {
			add("cvd-empty", name, "env sets CUDA_VISIBLE_DEVICES to nothing — a CPU-only process (INV-1)")
		}
		if cmd, _ := m["cmd"].(string); cmd != "" && cvdEmpty.MatchString(cmd) {
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

// Violations formats an audit result as one line per violation.
func Violations(vs []Violation) string {
	lines := make([]string, 0, len(vs))
	for _, v := range vs {
		lines = append(lines, v.String())
	}
	return strings.Join(lines, "\n")
}
