package servingtmpl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Gate-token substitution guards.
//
// 0.72.0 review finding I-3: __Q354B_AND__ was substituted by Render but consumed
// by NO template, while its siblings ARE live. It was removed in 0.73.0.
//
// 0.73.0's guard for that removal was VACUOUS, and a fresh-context reviewer proved
// it by mutation. Its loop looked for a surviving literal __TOKEN__ in the rendered
// output, but Render's own uniqueTokens gate already errors on any surviving token,
// so the render-error t.Fatalf always fired first and the loop never ran. Worse, it
// was blind to the mutation a future reader would actually make — emptying a
// substitution's VALUE rather than removing its map key — which leaves no token
// behind and kept the whole suite GREEN.
//
// That mutation ships a llama-swap config where a gated model sits in the models
// map and its matrix var but belongs to NO group: the silent-capability-loss class
// Render's refusal-by-name exists to end.
//
// TWO further vacuous rewrites were caught here before this one held, both for the
// same reason: flipping a gate also adds or removes the model block and its matrix
// var, so ANY whole-document comparison (or whole-document var count) is satisfied
// by that churn regardless of what the substitution expands to. The assertions
// below therefore compare ONLY the group-expression line the token lives on.

// tokenTemplates is the live token -> templates map (verified against
// setup/templates/ 2026-08-19). A token moving templates must update it; the
// mapping IS the contract, and a stale entry fails loudly rather than skipping.
var tokenTemplates = map[string][]string{
	"__M26_ALT__":   {"llama-swap.linux-cuda.yaml", "llama-swap.win-cpu.yaml", "llama-swap.win-cuda.yaml", "llama-swap.win-dual-blackwell.yaml", "llama-swap.win-vulkan.yaml"},
	"__M26_AND__":   {"llama-swap.win-cuda-resident.yaml", "llama-swap.win-dual-cuda.yaml"},
	"__Q38_ALT__":   {"llama-swap.win-dual-blackwell.yaml"},
	"__Q38_AND__":   {"llama-swap.win-cuda-resident.yaml"},
	"__Q354B_ALT__": {"llama-swap.linux-cuda.yaml", "llama-swap.win-cuda.yaml"},
}

// tokenVar is the matrix VARIABLE each token's expansion must reference when its
// gate is on. Deliberately not a fixed fragment: a template that also declares the
// 26B *-agent entry makes __M26_AND__ expand to " & (m26 | a26)" rather than
// " & m26", so pinning the exact string fails on win-dual-cuda for a legitimate
// shape. The invariant that matters is weaker and truer — the gate must ADD a
// reference to the model's matrix var inside its group.
var tokenVar = map[string]string{
	"__M26_ALT__":   "m26",
	"__M26_AND__":   "m26",
	"__Q38_ALT__":   "q38",
	"__Q38_AND__":   "q38",
	"__Q354B_ALT__": "q354",
}

func deadTokenTemplate(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// gatesFor turns on exactly the gates a template can honour. Setting a gate
// against a template with no such model entry is a Render REFUSAL by design, not a
// token bug, so the gates are derived the way Render derives them.
func gatesFor(tmpl string) Params {
	p := params()
	p.Include26B = definesModel(tmpl, "gemma4-26b-a4b")
	p.IncludeQ38 = definesModel(tmpl, modelQ38)
	p.IncludeQ354B = definesModel(tmpl, modelQ354B)
	return p
}

// setGate flips the ONE gate a token belongs to, leaving every other gate exactly
// as it was. Toggling all gates at once is not good enough: the outputs then differ
// because of some OTHER gate, which satisfies the comparison for every token.
func setGate(p Params, tok string, on bool) Params {
	switch tokenVar[tok] {
	case "m26":
		p.Include26B = on
	case "q38":
		p.IncludeQ38 = on
	case "q354":
		p.IncludeQ354B = on
	}
	return p
}

// groupLinesFor returns the rendered lines whose YAML key matches the key of each
// TEMPLATE line carrying tok. The token always lives inside a group EXPRESSION, so
// comparing just those lines isolates the substitution from the model-block churn
// that defeated the earlier versions of this test.
func groupLinesFor(t *testing.T, tmpl, rendered, tok string) []string {
	t.Helper()
	var keys []string
	for _, ln := range strings.Split(tmpl, "\n") {
		if strings.Contains(ln, tok) {
			if k, _, ok := strings.Cut(ln, ":"); ok {
				keys = append(keys, strings.TrimSpace(k))
			}
		}
	}
	if len(keys) == 0 {
		t.Fatalf("no template line carries %s — the contract map is stale", tok)
	}
	var out []string
	for _, k := range keys {
		found := false
		for _, ln := range strings.Split(rendered, "\n") {
			if strings.TrimSpace(strings.SplitN(ln, ":", 2)[0]) == k {
				out = append(out, ln)
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("rendered output has no line for group key %q (the template carried %s there)", k, tok)
		}
	}
	return out
}

// TestGateTokenSubstitutionsActuallySubstitute is the assertion 0.73.0 should have
// had: three checks per token per template, on the token's own group line, none of
// which an empty substitution can satisfy.
func TestGateTokenSubstitutionsActuallySubstitute(t *testing.T) {
	for tok, files := range tokenTemplates {
		v := tokenVar[tok]
		if v == "" {
			t.Fatalf("%s has no expected matrix var — the test's own map is incomplete", tok)
		}
		for _, f := range files {
			t.Run(tok+"/"+f, func(t *testing.T) {
				tmpl := deadTokenTemplate(t, f)
				if !strings.Contains(tmpl, tok) {
					t.Fatalf("tokenTemplates says %s carries %s but it does not — the contract map is stale", f, tok)
				}

				// Flip ONLY this token's gate; every other gate is held at the
				// value the template supports.
				base := gatesFor(tmpl)
				on, err := Render(tmpl, setGate(base, tok, true))
				if err != nil {
					t.Fatalf("render (%s gate on): %v", tok, err)
				}
				off, err := Render(tmpl, setGate(base, tok, false))
				if err != nil {
					t.Fatalf("render (%s gate off): %v", tok, err)
				}

				onLines := groupLinesFor(t, tmpl, on, tok)
				offLines := groupLinesFor(t, tmpl, off, tok)

				for i := range onLines {
					// 1. The group LINE must change when the gate flips. This is
					//    what dies when a substitution's value is emptied, and it
					//    is invisible to Render's leftover-token gate.
					if onLines[i] == offLines[i] {
						t.Errorf("%s expands to empty: its group line is identical gate-on and gate-off: %s | the gated model then sits in the models map with a matrix var but in NO group, which llama-swap cannot solve",
							tok, strings.TrimSpace(onLines[i]))
					}
					// 2. The gate-ON group line must reference the matrix var.
					if !strings.Contains(onLines[i], v) {
						t.Errorf("%s expanded without putting matrix var %q into its group: %s",
							tok, v, strings.TrimSpace(onLines[i]))
					}
					// 3. No literal token may survive either render.
					for _, ln := range []string{onLines[i], offLines[i]} {
						if strings.Contains(ln, tok) {
							t.Errorf("literal %s survived rendering — llama-swap rejects an unexpanded token at startup", tok)
						}
					}
				}
			})
		}
	}
}

// TestNoTemplateCarriesTheRemovedQ354BAndToken pins finding I-3 from the TEMPLATE
// side: the dead token must stay unused. Its live siblings are covered positively
// above, so this cannot be over-applied to them.
func TestNoTemplateCarriesTheRemovedQ354BAndToken(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "setup", "templates", "llama-swap.*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 5 {
		t.Fatalf("expected the full shipped template set, globbed %d: %v", len(files), files)
	}
	for _, f := range files {
		if strings.Contains(deadTokenTemplate(t, filepath.Base(f)), "__Q354B_AND__") {
			t.Errorf("%s carries __Q354B_AND__, which Render no longer substitutes — it would survive into the rendered config and llama-swap would reject it at startup. Use __Q354B_ALT__, or re-add the substitution deliberately", filepath.Base(f))
		}
	}
}

// TestRenderCarriesNoQ354BAndSubstitution is the source-side companion: a re-added
// dead substitution leaves NO trace in rendered output (that is precisely why it
// went unnoticed for a release), so only a source assertion can catch it.
func TestRenderCarriesNoQ354BAndSubstitution(t *testing.T) {
	b, err := os.ReadFile("servingtmpl.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if strings.Contains(src, `"__Q354B_AND__":`) {
		t.Error(`servingtmpl.go re-introduced a "__Q354B_AND__" substitution — it is consumed by no shipped template, unlike __M26_AND__ and __Q38_AND__ which are live`)
	}
	// Pin that the live siblings are still substituted, so this file's
	// "remove the dead one" lesson is never over-applied to them.
	for _, live := range []string{`"__M26_ALT__":`, `"__M26_AND__":`, `"__Q38_ALT__":`, `"__Q38_AND__":`, `"__Q354B_ALT__":`} {
		if !strings.Contains(src, live) {
			t.Errorf("substitution %s is gone — it IS consumed by a shipped template, and an unexpanded token bricks llama-swap at startup", live)
		}
	}
}
