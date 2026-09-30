package servingtmpl

import (
	"strings"
	"testing"
)

// H-01's remaining rule: `--n-cpu-moe` above the tier's measured spill. INV-1 (the
// cards do the inference, RAM is overflow only) sanctions exactly one host-RAM use — a
// PARTIAL spill of a model that does not fit its card — and only up to the number of
// expert layers that was measured to be needed. Audit is a text-only checker with no
// tier in hand, so it could not know that ceiling; AuditSpill takes it.

func spillConfig(cmd string, env ...string) string {
	var b strings.Builder
	b.WriteString("models:\n  m26:\n    ttl: 300\n")
	if len(env) > 0 {
		b.WriteString("    env: [" + strings.Join(env, ", ") + "]\n")
	}
	b.WriteString("    cmd: >-\n      " + cmd + "\n")
	return b.String()
}

func TestAuditSpillRefusesNCPUMoEAboveTheTiersMeasuredSpill(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  string
		max  int
		want []string // substrings of the single violation; nil = compliant
	}{
		{"above the measured spill", spillConfig("/bin/llama-server --model m.gguf --n-cpu-moe 20 -ngl 999 --port ${PORT}"), 14,
			[]string{"n-cpu-moe", "m26", "20", "14"}},
		{"exactly the measured spill", spillConfig("/bin/llama-server --model m.gguf --n-cpu-moe 14 -ngl 999"), 14, nil},
		{"below the measured spill", spillConfig("/bin/llama-server --model m.gguf --n-cpu-moe 13 -ngl 999"), 14, nil},
		{"a tier with no measured spill refuses any", spillConfig("/bin/llama-server --model m.gguf --n-cpu-moe 1 -ngl 999"), 0,
			[]string{"n-cpu-moe", "m26", "no measured spill"}},
		{"zero layers spilled is no spill", spillConfig("/bin/llama-server --model m.gguf --n-cpu-moe 0 -ngl 999"), 0, nil},
		{"the = spelling", spillConfig("/bin/llama-server --model m.gguf --n-cpu-moe=20"), 14, []string{"20", "14"}},
		{"the -ncmoe short form", spillConfig("/bin/llama-server --model m.gguf -ncmoe 20"), 14, []string{"20", "14"}},
		{"the environment twin", spillConfig("/bin/llama-server --model m.gguf", "LLAMA_ARG_N_CPU_MOE=20"), 14, []string{"20", "14"}},
		{"no spill flag at all", spillConfig("/bin/llama-server --model m.gguf -ngl 99"), 0, nil},
		// --cpu-moe is the every-expert form, a different rule: the table-level
		// TestNoGPUTierParksTheMoEExpertsInRAM owns it (and the cpu tier renders it).
		{"--cpu-moe is not this rule", spillConfig("/bin/llama-server --model m.gguf --cpu-moe -ngl 999"), 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vs := AuditSpill(tc.cfg, tc.max)
			if tc.want == nil {
				if len(vs) != 0 {
					t.Fatalf("compliant config flagged:\n%s", Violations(vs))
				}
				return
			}
			if len(vs) != 1 {
				t.Fatalf("want exactly one violation, got %d:\n%s", len(vs), Violations(vs))
			}
			if vs[0].Rule != "n-cpu-moe" {
				t.Errorf("rule = %q, want n-cpu-moe", vs[0].Rule)
			}
			for _, w := range tc.want {
				if !strings.Contains(vs[0].String(), w) {
					t.Errorf("violation %q does not carry %q", vs[0], w)
				}
			}
		})
	}
}

// Every violating entry is named, in a stable order, so an operator fixing a config
// gets the whole list once.
func TestAuditSpillNamesEveryOffendingEntry(t *testing.T) {
	cfg := "models:\n" +
		"  zz-second:\n    ttl: 300\n    cmd: llama-server --n-cpu-moe 30\n" +
		"  aa-first:\n    ttl: 300\n    cmd: llama-server --n-cpu-moe 25\n" +
		"  fine:\n    ttl: 300\n    cmd: llama-server --n-cpu-moe 10\n"
	vs := AuditSpill(cfg, 14)
	if len(vs) != 2 || vs[0].Where != "aa-first" || vs[1].Where != "zz-second" {
		t.Fatalf("want aa-first then zz-second, got:\n%s", Violations(vs))
	}
}

// Audit owns parse failures; a second, differently worded report of the same fault
// from the spill rule would only bury it.
func TestAuditSpillLeavesParseFailuresToAudit(t *testing.T) {
	if vs := AuditSpill("models: [unterminated", 14); len(vs) != 0 {
		t.Fatalf("AuditSpill reported on a document Audit already refuses:\n%s", Violations(vs))
	}
}

// llama-swap substitutes a `${name}` macro into an entry's cmd and env before it runs them, and the
// shipped templates keep their shared flags in `macros:` (`common`). A rule that reads only each
// entry's own text never sees a spill placed there, and every entry that references the macro would
// run it.
func TestAuditSpillReadsMacros(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  string
		max  int
		want []string // substrings of the single violation; nil = compliant
	}{
		{"a flag in a macro the cmd references", `
macros:
  common: >-
    --n-cpu-moe 30 --jinja
models:
  m26:
    ttl: 300
    cmd: /bin/llama-server --model m.gguf ${common}
`, 14, []string{"n-cpu-moe", "m26", "30", "14"}},
		{"a macro that references another macro", `
macros:
  spill: --n-cpu-moe 30
  common: >-
    --jinja ${spill}
models:
  m26:
    ttl: 300
    cmd: /bin/llama-server ${common}
`, 14, []string{"m26", "30"}},
		{"the environment twin in a macro", `
macros:
  moe: LLAMA_ARG_N_CPU_MOE=30
models:
  m26:
    ttl: 300
    env: ["${moe}"]
    cmd: /bin/llama-server --model m.gguf
`, 14, []string{"m26", "30", "LLAMA_ARG_N_CPU_MOE"}},
		{"a numeric macro value", `
macros:
  n: 30
models:
  m26:
    ttl: 300
    cmd: /bin/llama-server --model m.gguf --n-cpu-moe ${n}
`, 14, []string{"m26", "30", "14"}},
		{"a macro no entry references is never run", `
macros:
  unused: --n-cpu-moe 30
models:
  m26:
    ttl: 300
    cmd: /bin/llama-server --model m.gguf
`, 14, nil},
		{"a macro within the measured spill", `
macros:
  common: --n-cpu-moe 10
models:
  m26:
    ttl: 300
    cmd: /bin/llama-server ${common}
`, 14, nil},
		{"llama-swap's own substitutions are not macros", `
models:
  m26:
    ttl: 300
    cmd: /bin/llama-server --model m.gguf --port ${PORT}
`, 0, nil},
		{"a macro cycle terminates and flags nothing", `
macros:
  a: ${b}
  b: ${a}
models:
  m26:
    ttl: 300
    cmd: /bin/llama-server ${a}
`, 14, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vs := AuditSpill(tc.cfg, tc.max)
			if tc.want == nil {
				if len(vs) != 0 {
					t.Fatalf("compliant config flagged:\n%s", Violations(vs))
				}
				return
			}
			if len(vs) != 1 {
				t.Fatalf("want exactly one violation, got %d:\n%s", len(vs), Violations(vs))
			}
			for _, w := range tc.want {
				if !strings.Contains(vs[0].String(), w) {
					t.Errorf("violation %q does not carry %q", vs[0], w)
				}
			}
		})
	}
}
