package servingtmpl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 0.72.0 review finding I-3: __Q354B_AND__ was substituted by Render but consumed
// by NO template, while its two siblings ARE live (__M26_AND__ in 2 templates,
// __Q38_AND__ in 1). The substitution was removed 2026-08-19.
//
// This test guards both halves of that asymmetry, because the obvious "cleanup"
// in either direction is wrong:
//
//   - Re-adding a __Q354B_AND__ substitution by symmetry with its siblings would
//     restore dead code. The small-tier agent seat is never a member of an
//     interactive "&" group, so there is nothing for it to render into.
//   - Deleting the __M26_AND__ / __Q38_AND__ substitutions by symmetry with the
//     removal would BRICK the rendered config: an unexpanded __M26_AND__ left in
//     a group expression is a llama-swap parse error at startup.
//
// It asserts on the RENDERED OUTPUT of EVERY shipped template, and derives each
// template's gates the same way Render itself does (definesModel), because the
// gates are not uniform: linux-cuda/win-cuda carry the 4B agent seat, while
// win-cuda-resident carries the 27B. Hardcoding one template's gate set is what
// made the earlier PowerShell-only coverage miss the Linux template entirely.
func TestNoUnexpandedGateTokenSurvivesInAnyTemplate(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "setup", "templates", "llama-swap.*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 5 {
		t.Fatalf("expected the full shipped template set, globbed only %d: %v", len(files), files)
	}

	var sawQ354B, sawQ38, saw26B int
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			tmpl := string(b)

			// Derive the gates this template can actually honour. Setting a gate
			// against a template with no such entry is a Render REFUSAL by design
			// (silent-capability-loss guard), not a token bug.
			p := params()
			p.Include26B = definesModel(tmpl, "gemma4-26b-a4b")
			p.IncludeQ38 = definesModel(tmpl, modelQ38)
			p.IncludeQ354B = definesModel(tmpl, modelQ354B)

			if p.Include26B {
				saw26B++
			}
			if p.IncludeQ38 {
				sawQ38++
			}
			if p.IncludeQ354B {
				sawQ354B++
			}

			out, err := Render(tmpl, p)
			if err != nil {
				t.Fatalf("render: %v", err)
			}

			// No unexpanded token of ANY kind may survive into a rendered config.
			// This is the check that catches the dead token being re-added AND a
			// live one being dropped: llama-swap rejects the literal at startup
			// either way.
			for _, tok := range []string{
				"__Q354B_AND__", "__Q354B_ALT__",
				"__M26_AND__", "__M26_ALT__",
				"__Q38_AND__", "__Q38_ALT__",
			} {
				if strings.Contains(out, tok) {
					t.Errorf("rendered config still contains the literal %s — llama-swap rejects an unexpanded token at startup", tok)
				}
			}
		})
	}

	// Guard the guard: if no template exercised a gate, the assertions above
	// passed vacuously for it. Counts come from the live template set, so this
	// also fails loudly if a gated seat is dropped from every template.
	if sawQ354B == 0 || sawQ38 == 0 || saw26B == 0 {
		t.Fatalf("a gate was never exercised, so its token assertions are vacuous: 26b=%d q38=%d q354b=%d", saw26B, sawQ38, sawQ354B)
	}
}

// The source-level companion: pin that Render no longer carries a __Q354B_AND__
// substitution at all. Without this, someone could re-add the dead substitution
// and the rendered-output test above would still pass — a substitution nothing
// consumes leaves no trace in the output, which is precisely why it went
// unnoticed for a whole release.
func TestRenderCarriesNoQ354BAndSubstitution(t *testing.T) {
	b, err := os.ReadFile("servingtmpl.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	// The doc comment legitimately mentions the token, so look for the
	// substitution SHAPE (a map entry) rather than the bare name.
	if strings.Contains(src, `"__Q354B_AND__":`) {
		t.Error(`servingtmpl.go re-introduced a "__Q354B_AND__" substitution — it is consumed by no shipped template, unlike __M26_AND__ and __Q38_AND__ which are live`)
	}
	// And pin that the live siblings are still substituted, so this file's
	// "remove the dead one" lesson is never over-applied to them.
	for _, live := range []string{`"__M26_AND__":`, `"__Q38_AND__":`, `"__Q354B_ALT__":`} {
		if !strings.Contains(src, live) {
			t.Errorf("substitution %s is gone — it IS consumed by a shipped template, and an unexpanded token bricks llama-swap at startup", live)
		}
	}
}
