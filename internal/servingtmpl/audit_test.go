package servingtmpl

import (
	"strings"
	"testing"
)

// The serving-config gate (H-01). One red case per rule, then every shipped
// template must pass the same checker the installer refuses on.

func auditRules(t *testing.T, text string) []string {
	t.Helper()
	var rules []string
	for _, v := range Audit(text) {
		rules = append(rules, v.Rule)
	}
	return rules
}

func TestAuditRefusesEveryOperatorRuleBreak(t *testing.T) {
	cases := map[string]string{
		"ttl": `models:
  a:
    cmd: x --port 1
    ttl: 600
  b:
    cmd: x --port 2
`,
		"ngl0": `models:
  a:
    cmd: llama-server -m m.gguf -ngl 0 --port 1
    ttl: 300
  b:
    cmd: |
      llama-server -m m.gguf
      --n-gpu-layers 0
    ttl: 300
`,
		"cvd-empty": `models:
  a:
    cmd: llama-server -m m.gguf -ngl 99
    env:
      - CUDA_VISIBLE_DEVICES=
    ttl: 300
`,
		"persistent": `models:
  a:
    cmd: x
    ttl: 300
groups:
  hot:
    persistent: true
    members: [a]
`,
		"preload": `models:
  a:
    cmd: x
    ttl: 300
hooks:
  on_startup:
    preload: [a]
`,
	}
	for rule, text := range cases {
		got := auditRules(t, text)
		found := false
		for _, r := range got {
			if r == rule {
				found = true
			}
		}
		if !found {
			t.Errorf("rule %q not raised on its fixture; got %v", rule, got)
		}
	}
	// ttl 0 and a string ttl are ttl violations too.
	if got := auditRules(t, "models:\n  a:\n    cmd: x\n    ttl: 0\n"); len(got) != 1 || got[0] != "ttl" {
		t.Errorf("ttl 0: %v", got)
	}
	// -ngl 90 is not -ngl 0; CUDA_VISIBLE_DEVICES=0,2 is not empty; a non-persistent group is fine.
	clean := `models:
  a:
    cmd: llama-server -m m.gguf -ngl 99 --port 9000
    env:
      - CUDA_VISIBLE_DEVICES=0,2
    ttl: 300
groups:
  swap:
    swap: true
    members: [a]
`
	if got := Audit(clean); len(got) != 0 {
		t.Fatalf("clean config raised %v", got)
	}
	if got := Audit("models: ["); len(got) != 1 || got[0].Rule != "yaml" {
		t.Fatalf("unparseable config: %v", got)
	}
}

// TestEveryTemplatePassesTheAudit: the shipped templates are the installer's
// own output before rendering; the same checker the installer refuses on must
// pass every one of them, or a fresh install renders a violation.
func TestEveryTemplatePassesTheAudit(t *testing.T) {
	for _, name := range templateNames(t) {
		if vs := Audit(string(readTemplate(t, name))); len(vs) != 0 {
			t.Errorf("%s:\n%s", name, Violations(vs))
		}
	}
}

func TestViolationsFormatIsOneLinePerRule(t *testing.T) {
	vs := Audit("models:\n  a:\n    cmd: x -ngl 0\n")
	s := Violations(vs)
	if strings.Count(s, "\n") != 1 || !strings.Contains(s, "ngl0 (a)") || !strings.Contains(s, "ttl (a)") {
		t.Fatalf("format: %q", s)
	}
}
