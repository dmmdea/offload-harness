package confidence

import (
	"math"
	"testing"

	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// tok builds a chosen-only token (no alternatives) for string reconstruction.
func tok(s string) llamaclient.TokenLogprob { return llamaclient.TokenLogprob{Token: s} }

// decTok builds the decision-value token carrying the raw top alternatives.
func decTok(s string, alts ...llamaclient.AltToken) llamaclient.TokenLogprob {
	return llamaclient.TokenLogprob{Token: s, Top: alts}
}

func alt(t string, lp float64) llamaclient.AltToken { return llamaclient.AltToken{Token: t, Logprob: lp} }

// triage builds logprobs for {"decision":"<v>",...} with the given alts on the value token.
func triage(v string, alts ...llamaclient.AltToken) []llamaclient.TokenLogprob {
	return []llamaclient.TokenLogprob{
		tok(`{"decision":"`), decTok(v, alts...), tok(`","reason":"`), tok("x"), tok(`"}`),
	}
}

func TestMargin(t *testing.T) {
	yesno := []string{"yes", "no", "unsure"}
	cases := []struct {
		name    string
		toks    []llamaclient.TokenLogprob
		key     string
		classes []string
		wantOK  bool
		// expected margin bound: hi => want >= 0.5, lo => want < 0.2
		hi bool
	}{
		{"confident yes", triage("yes", alt("yes", -0.05), alt("no", -3.0)), "decision", yesno, true, true},
		{"borderline yes/no", triage("yes", alt("yes", -0.70), alt("no", -0.80)), "decision", yesno, true, false},
		{"spelling variants aggregate", triage("yes", alt("Yes", -0.10), alt("yes", -2.0), alt("no", -3.0)), "decision", yesno, true, true},
		{"illegal token ignored", triage("yes", alt("Yes", -0.07), alt("Blue", -2.9), alt("yes", -6.7)), "decision", yesno, true, true},
		{"key absent", []llamaclient.TokenLogprob{tok(`{"x":"y"}`)}, "decision", yesno, false, false},
		{"no alts on value token", triage("yes"), "decision", yesno, false, false},
		{"too few classes", triage("yes", alt("yes", -0.05)), "decision", []string{"yes"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := Margin(tc.toks, tc.key, tc.classes)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v (margin=%.3f)", ok, tc.wantOK, m)
			}
			if !ok {
				return
			}
			if m < 0 || m > 1 || math.IsNaN(m) {
				t.Fatalf("margin out of range: %v", m)
			}
			if tc.hi && m < 0.5 {
				t.Fatalf("expected high margin, got %.3f", m)
			}
			if !tc.hi && m >= 0.2 {
				t.Fatalf("expected low margin, got %.3f", m)
			}
		})
	}
}

func TestMarginClassifyLabel(t *testing.T) {
	toks := []llamaclient.TokenLogprob{
		tok(`{"label":"`), decTok("hardware", alt("hardware", -0.0004), alt("software", -8.27)), tok(`","confidence":0.9}`),
	}
	m, ok := Margin(toks, "label", []string{"hardware", "software", "finance"})
	if !ok || m < 0.9 {
		t.Fatalf("classify label margin: ok=%v m=%.4f want ok && >=0.9", ok, m)
	}
}

func TestClassOf(t *testing.T) {
	classes := []string{"sales", "support", "saas"}
	folded := make([]string, len(classes))
	for i, c := range classes {
		folded[i] = fold(c)
	}
	cases := []struct {
		tok  string
		want int
	}{
		{"sales", 0},       // exact match wins
		{"support", 1},     // exact
		{"sal", 0},         // unique prefix of "sales"
		{"sa", -1},         // ambiguous: prefix of both "sales" and "saas"
		{"salesforce", -1}, // extends beyond "sales" -> not a match
		{"xyz", -1},        // unrelated
	}
	for _, c := range cases {
		if got := classOf(fold(c.tok), folded); got != c.want {
			t.Errorf("classOf(%q) = %d, want %d", c.tok, got, c.want)
		}
	}
}
