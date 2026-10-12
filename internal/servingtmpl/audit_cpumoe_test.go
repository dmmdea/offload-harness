package servingtmpl

import (
	"strings"
	"testing"
)

// W4: the live-yaml audit refuses the every-expert placement. INV-1 (the cards do the
// inference, RAM is overflow only) sanctions one host-RAM use for experts: a PARTIAL spill,
// `--n-cpu-moe N`, up to a measured N. The bare form (`--cpu-moe`, `-cmoe`, and the
// LLAMA_ARG_CPU_MOE environment twin) parks EVERY expert layer in host RAM, and until now
// nothing read a live file for it: the table-level seed gate sees profiles.json only, so a
// hand-edited yaml could carry it undetected. AuditCPUMoE is that reader. It is pure and
// exemption-free; the cpu backend (no card to overflow from) and the off-matrix render are
// exempted by the two callers, each from the evidence it holds.

// cpuMoeEntry is a one-entry config; env items are written as a YAML list.
func cpuMoeEntry(name, cmd string, env ...string) string {
	var b strings.Builder
	b.WriteString("models:\n  " + name + ":\n    ttl: 300\n")
	if len(env) > 0 {
		b.WriteString("    env:\n")
		for _, e := range env {
			b.WriteString("      - " + e + "\n")
		}
	}
	b.WriteString("    cmd: >-\n      " + cmd + "\n")
	return b.String()
}

func TestAuditCPUMoERefusesTheBareFlagInEverySpelling(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  string
		want string // the spelling the finding must name
	}{
		{"--cpu-moe with -ngl 999", cpuMoeEntry("m26", "/bin/llama-server -m m.gguf --cpu-moe -ngl 999 --port ${PORT}"), "--cpu-moe"},
		{"-cmoe short form", cpuMoeEntry("m26", "/bin/llama-server -m m.gguf -cmoe -ngl 999"), "-cmoe"},
		{"the flag last on the line", cpuMoeEntry("m26", "/bin/llama-server -m m.gguf -ngl 999 --cpu-moe"), "--cpu-moe"},
		{"LLAMA_ARG_CPU_MOE=1 in env", cpuMoeEntry("m26", "/bin/llama-server -m m.gguf -ngl 999", "LLAMA_ARG_CPU_MOE=1"), "LLAMA_ARG_CPU_MOE"},
		{"LLAMA_ARG_CPU_MOE=true in env", cpuMoeEntry("m26", "/bin/llama-server -m m.gguf", "LLAMA_ARG_CPU_MOE=true"), "LLAMA_ARG_CPU_MOE"},
		{"LLAMA_ARG_CPU_MOE=on in env", cpuMoeEntry("m26", "/bin/llama-server -m m.gguf", "LLAMA_ARG_CPU_MOE=on"), "LLAMA_ARG_CPU_MOE"},
		{"LLAMA_ARG_CPU_MOE=enabled in env", cpuMoeEntry("m26", "/bin/llama-server -m m.gguf", "LLAMA_ARG_CPU_MOE=enabled"), "LLAMA_ARG_CPU_MOE"},
		{"the draft model's flag", cpuMoeEntry("m26", "/bin/llama-server -m m.gguf --cpu-moe-draft -ngl 999"), "--cpu-moe-draft"},
		{"the draft model's short form", cpuMoeEntry("m26", "/bin/llama-server -m m.gguf -cmoed"), "-cmoed"},
		{"the draft model's environment twin", cpuMoeEntry("m26", "/bin/llama-server -m m.gguf", "LLAMA_ARG_SPEC_DRAFT_CPU_MOE=1"), "LLAMA_ARG_SPEC_DRAFT_CPU_MOE"},
		{"the flag inside a macro an entry uses",
			"macros:\n  moe: \"--cpu-moe -ngl 999\"\nmodels:\n  m26:\n    ttl: 300\n    cmd: >-\n      /bin/llama-server -m m.gguf ${moe}\n", "--cpu-moe"},
		{"the env twin inside a macro an entry uses",
			"macros:\n  moeenv: \"LLAMA_ARG_CPU_MOE=1\"\nmodels:\n  m26:\n    ttl: 300\n    env:\n      - ${moeenv}\n    cmd: /bin/llama-server -m m.gguf\n", "LLAMA_ARG_CPU_MOE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vs := AuditCPUMoE(tc.cfg)
			if len(vs) != 1 {
				t.Fatalf("want exactly one finding, got %d:\n%s", len(vs), Violations(vs))
			}
			if vs[0].Rule != "cpu-moe" || vs[0].Where != "m26" {
				t.Errorf("finding = rule %q where %q, want cpu-moe (m26)", vs[0].Rule, vs[0].Where)
			}
			for _, w := range []string{tc.want, "INV-1"} {
				if !strings.Contains(vs[0].Text, w) {
					t.Errorf("the finding must carry %q, got: %s", w, vs[0].Text)
				}
			}
			if line := vs[0].String(); !strings.HasPrefix(line, "cpu-moe (m26): ") {
				t.Errorf("the one-line form must start with the rule and the entry, got %q", line)
			}
		})
	}
}

func TestAuditCPUMoENamesEveryOffendingEntryOnceAndSorted(t *testing.T) {
	cfg := "models:\n" +
		"  zeta:\n    ttl: 300\n    env:\n      - LLAMA_ARG_CPU_MOE=1\n    cmd: /bin/llama-server -m z.gguf --cpu-moe\n" +
		"  alpha:\n    ttl: 300\n    cmd: /bin/llama-server -m a.gguf -cmoe\n" +
		"  clean:\n    ttl: 300\n    cmd: /bin/llama-server -m c.gguf --n-cpu-moe 14 -ngl 999\n"
	vs := AuditCPUMoE(cfg)
	if len(vs) != 2 || vs[0].Where != "alpha" || vs[1].Where != "zeta" {
		t.Fatalf("want one finding each for alpha and zeta, in that order, got:\n%s", Violations(vs))
	}
	if !strings.Contains(vs[1].Text, "--cpu-moe") || !strings.Contains(vs[1].Text, "LLAMA_ARG_CPU_MOE") {
		t.Errorf("an entry with two spellings names both in its one finding, got: %s", vs[1].Text)
	}
}

// The sanctioned partial spill and the off switches are not this rule's business.
func TestAuditCPUMoELeavesThePartialSpillAlone(t *testing.T) {
	for name, cfg := range map[string]string{
		"--n-cpu-moe":         cpuMoeEntry("m26", "/bin/llama-server -m m.gguf --n-cpu-moe 14 -ngl 999"),
		"--n-cpu-moe=":        cpuMoeEntry("m26", "/bin/llama-server -m m.gguf --n-cpu-moe=14 -ngl 999"),
		"-ncmoe":              cpuMoeEntry("m26", "/bin/llama-server -m m.gguf -ncmoe 14 -ngl 999"),
		"LLAMA_ARG_N_CPU_MOE": cpuMoeEntry("m26", "/bin/llama-server -m m.gguf", "LLAMA_ARG_N_CPU_MOE=14"),
		"CPU_MOE=0":           cpuMoeEntry("m26", "/bin/llama-server -m m.gguf", "LLAMA_ARG_CPU_MOE=0"),
		"CPU_MOE=false":       cpuMoeEntry("m26", "/bin/llama-server -m m.gguf", "LLAMA_ARG_CPU_MOE=false"),
		"CPU_MOE=off":         cpuMoeEntry("m26", "/bin/llama-server -m m.gguf", "LLAMA_ARG_CPU_MOE=off"),
		"CPU_MOE=disabled":    cpuMoeEntry("m26", "/bin/llama-server -m m.gguf", "LLAMA_ARG_CPU_MOE=disabled"),
		"CPU_MOE empty":       cpuMoeEntry("m26", "/bin/llama-server -m m.gguf", "LLAMA_ARG_CPU_MOE="),
		"no moe flag at all":  cpuMoeEntry("m26", "/bin/llama-server -m m.gguf -ngl 99"),
	} {
		t.Run(name, func(t *testing.T) {
			if vs := AuditCPUMoE(cfg); len(vs) != 0 {
				t.Fatalf("not the every-expert form, flagged:\n%s", Violations(vs))
			}
		})
	}
}

// The provenance stamp records the render's inputs, and `moe_26b: "--cpu-moe -ngl 999"` is
// one of them (an off-matrix render writes exactly that literal). A scan of the raw text would
// read the stamp's comment and flag a file whose entries carry no such flag.
func TestAuditCPUMoEIgnoresTheStampComment(t *testing.T) {
	cfg := "# local-offload serving-config provenance -- generated\n" +
		"# basis: {\"params\":{\"moe_26b\":\"--cpu-moe -ngl 999\",\"backend\":\"cuda\"}}\n\n" +
		cpuMoeEntry("m26", "/bin/llama-server -m m.gguf --n-cpu-moe 14 -ngl 999")
	if vs := AuditCPUMoE(cfg); len(vs) != 0 {
		t.Fatalf("a flag in a yaml comment is not a placement, flagged:\n%s", Violations(vs))
	}
}

// A document that does not parse is Audit's finding; reporting it twice would only bury it.
func TestAuditCPUMoEIsSilentOnADocumentThatDoesNotParse(t *testing.T) {
	if vs := AuditCPUMoE("models: [unclosed\n  --cpu-moe"); len(vs) != 0 {
		t.Fatalf("an unparseable document is Audit's finding, not this rule's:\n%s", Violations(vs))
	}
}

// The shipped templates carry the placement as a token the tier fills (__MOE_26B__), never as a
// literal flag: a template that hard-coded the every-expert form would put it on every tier that
// uses it, whatever that tier's seeds say.
func TestEveryTemplateCarriesNoLiteralCPUMoE(t *testing.T) {
	for _, name := range templateNames(t) {
		if vs := AuditCPUMoE(string(readTemplate(t, name))); len(vs) != 0 {
			t.Errorf("%s:\n%s", name, Violations(vs))
		}
	}
}
