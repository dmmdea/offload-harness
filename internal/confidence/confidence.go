// Package confidence derives a decision-confidence signal from a grammar-
// constrained model's per-token logprobs. With a grammar active, llama.cpp
// reports the RAW (pre-mask) distribution, so at the decision-value position the
// top alternatives reveal the model's genuine preference among the legal
// options. We aggregate that raw mass by legal class (folding spelling variants
// like "Yes"/"yes", dropping grammar-illegal tokens) and return the normalized
// margin between the top two classes — a low margin means the model was
// genuinely torn (e.g. an "eager-YES" borderline call) and the pipeline should
// escalate to a larger tier rather than accept it.
package confidence

import (
	"math"
	"strings"

	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// Margin returns a 0..1 separation between the model's top-1 and top-2 legal
// decision classes at the value position of jsonKey (e.g. "decision" / "label"),
// computed on the raw logprob distribution. ok=false when the position or a
// usable class distribution can't be resolved — the caller then falls back to
// other signals (self-reported confidence) rather than escalating blindly.
func Margin(toks []llamaclient.TokenLogprob, jsonKey string, classes []string) (float64, bool) {
	if len(toks) == 0 || len(classes) < 2 {
		return 0, false
	}
	pos, ok := decisionPos(toks, jsonKey)
	if !ok {
		return 0, false
	}
	return classMassMargin(toks[pos].Top, classes)
}

// decisionPos reconstructs the output string from the chosen tokens and returns
// the index of the token that begins the value of `"jsonKey": "..."`.
func decisionPos(toks []llamaclient.TokenLogprob, jsonKey string) (int, bool) {
	starts := make([]int, len(toks))
	var sb strings.Builder
	for i, t := range toks {
		starts[i] = sb.Len()
		sb.WriteString(t.Token)
	}
	off, ok := valueOffset(sb.String(), jsonKey)
	if !ok {
		return 0, false
	}
	for i := range toks {
		end := starts[i] + len(toks[i].Token)
		if off >= starts[i] && off < end {
			return i, true
		}
		if starts[i] > off { // value began exactly at a token boundary
			return i, true
		}
	}
	return 0, false
}

// valueOffset finds the char offset where the string value of `"key"` begins,
// tolerant of whitespace around the colon. Returns false if the pattern (a JSON
// string-valued key) isn't present.
func valueOffset(s, key string) (int, bool) {
	k := `"` + key + `"`
	i := strings.Index(s, k)
	if i < 0 {
		return 0, false
	}
	j := i + len(k)
	j = skipSpace(s, j)
	if j >= len(s) || s[j] != ':' {
		return 0, false
	}
	j = skipSpace(s, j+1)
	if j >= len(s) || s[j] != '"' {
		return 0, false
	}
	j++ // first char of the value
	if j >= len(s) {
		return 0, false // value starts at/after end of string — nothing to score
	}
	return j, true
}

func skipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return i
}

// classMassMargin folds each candidate token to a legal class, sums its raw
// probability mass per class, and returns the normalized top-1 vs top-2 margin.
func classMassMargin(top []llamaclient.AltToken, classes []string) (float64, bool) {
	if len(top) == 0 {
		return 0, false
	}
	folded := make([]string, len(classes))
	for i, c := range classes {
		folded[i] = fold(c)
	}
	mass := make([]float64, len(classes))
	var total float64
	for _, a := range top {
		ft := fold(a.Token)
		if ft == "" {
			continue
		}
		ci := classOf(ft, folded)
		if ci < 0 {
			continue // grammar-illegal / unrelated token
		}
		p := expLogprob(a.Logprob)
		mass[ci] += p
		total += p
	}
	if total <= 0 {
		return 0, false
	}
	first, second := 0.0, 0.0
	for _, m := range mass {
		if m > first {
			second, first = first, m
		} else if m > second {
			second = m
		}
	}
	return (first - second) / total, true
}

func fold(s string) string {
	return strings.ToLower(strings.Trim(s, " \t\n\r\""))
}

// classOf maps a folded token to the single legal class it identifies, or -1 if
// it matches none or is ambiguous. A token matches a class if it equals the
// class (exact, wins outright) or is a PREFIX of it (an abbreviation / first
// token of a multi-token label, e.g. "uns" -> "unsure"). A token that extends
// BEYOND a class ("salesforce" vs "sales") is NOT a match — that would credit
// unrelated vocabulary. A token that prefixes more than one class is ambiguous
// and credited to none, so it can't bias the margin by class order.
func classOf(ft string, folded []string) int {
	match := -1
	for i, c := range folded {
		if c == ft {
			return i // exact match wins outright
		}
		if strings.HasPrefix(c, ft) { // token abbreviates / begins this class
			if match >= 0 {
				return -1 // prefix of more than one class — ambiguous, credit none
			}
			match = i
		}
	}
	return match
}

// expLogprob converts a natural-log prob to a probability, guarding llama.cpp's
// clamped -inf sentinel (~-3.4e38) which would otherwise be a valid 0 but should
// not be mistaken for real mass.
func expLogprob(lp float64) float64 {
	if math.IsInf(lp, -1) || lp < -700 {
		return 0
	}
	return math.Exp(lp)
}
