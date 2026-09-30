package servingtmpl

import "testing"

// Audit reads what llama-swap will run, and llama-swap substitutes `${name}` macros into an entry's
// cmd and env first. A CPU-only flag or an emptied CUDA_VISIBLE_DEVICES placed in a macro is run by
// every entry that references it, so it is a violation of the entry that does.
func TestAuditReadsMacros(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  string
		rule string // "" = compliant
	}{
		{"-ngl 0 in a macro the cmd references", `
macros:
  common: >-
    --n-gpu-layers 0 --jinja
models:
  m:
    ttl: 300
    cmd: /bin/llama-server ${common}
`, "ngl0"},
		{"an emptied CUDA_VISIBLE_DEVICES in a macro the env references", `
macros:
  gpus: CUDA_VISIBLE_DEVICES=
models:
  m:
    ttl: 300
    env: ["${gpus}"]
    cmd: /bin/llama-server --n-gpu-layers 99
`, "cvd-empty"},
		{"-ngl 0 in a macro that references another", `
macros:
  cpu: -ngl 0
  common: ${cpu} --jinja
models:
  m:
    ttl: 300
    cmd: /bin/llama-server ${common}
`, "ngl0"},
		{"a macro no entry references is never run", `
macros:
  unused: -ngl 0
models:
  m:
    ttl: 300
    cmd: /bin/llama-server --n-gpu-layers 99
`, ""},
		{"an ordinary macro", `
macros:
  common: --n-gpu-layers 99 --jinja
models:
  m:
    ttl: 300
    cmd: /bin/llama-server ${common}
`, ""},
		{"a macro cycle terminates", `
macros:
  a: ${b}
  b: ${a}
models:
  m:
    ttl: 300
    cmd: /bin/llama-server --n-gpu-layers 99 ${a}
`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vs := Audit(tc.cfg)
			if tc.rule == "" {
				if len(vs) != 0 {
					t.Fatalf("compliant config flagged:\n%s", Violations(vs))
				}
				return
			}
			if len(vs) != 1 || vs[0].Rule != tc.rule || vs[0].Where != "m" {
				t.Fatalf("want one %s violation on m, got:\n%s", tc.rule, Violations(vs))
			}
		})
	}
}

// expandMacros is the reading both audits share. A reference that names no macro is left as
// written (llama-swap's own `${PORT}` is one), a macro may use another, and a cycle stops.
func TestExpandMacros(t *testing.T) {
	macros := map[string]string{"x": "--n-cpu-moe 3", "y": "${x} more", "self": "${self}"}
	for in, want := range map[string]string{
		"a ${PORT} b":  "a ${PORT} b",
		"${x}":         "--n-cpu-moe 3",
		"${y}":         "--n-cpu-moe 3 more",
		"no reference": "no reference",
		"${self} end":  "${self} end",
		"${x} ${x}":    "--n-cpu-moe 3 --n-cpu-moe 3",
	} {
		if got := expandMacros(in, macros); got != want {
			t.Errorf("expandMacros(%q) = %q, want %q", in, got, want)
		}
	}
	if got := expandMacros("${x}", nil); got != "${x}" {
		t.Errorf("with no macros the text must be untouched, got %q", got)
	}
}

// macroTable takes scalars only: anything else is a config llama-swap refuses on its own, and a
// rule about flags does not choke on it.
func TestMacroTableReadsScalarsAndIgnoresTheRest(t *testing.T) {
	got := macroTable(map[string]any{"s": "text", "i": 30, "b": true, "list": []any{"a"}, "map": map[string]any{"k": "v"}})
	if len(got) != 3 || got["s"] != "text" || got["i"] != "30" || got["b"] != "true" {
		t.Errorf("macroTable = %v, want the three scalars as text", got)
	}
	for _, v := range []any{nil, "not a mapping", []any{"a"}} {
		if got := macroTable(v); len(got) != 0 {
			t.Errorf("macroTable(%v) = %v, want nothing", v, got)
		}
	}
}
