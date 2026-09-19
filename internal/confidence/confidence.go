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

// Detail is the full view of one decision position. Margin and MatchedMargin
// are the SAME numerator over two different denominators, and they are NOT
// comparable to each other: a stored history of one scale must never be read
// as the other (register D-130).
//
//	MatchedMargin = (first - second) / mass on MATCHED class tokens  (today's gate)
//	Margin        = (first - second) / mass on EVERY alternative      (full scale)
//	DeclaredMass  = matched / all, in 0..1 — measured ~0.097 on llama.cpp, i.e.
//	                the matched scale inflates the margin by about 10x.
//	Ambiguous     = alternatives credited to NO class because they prefix more
//	                than one (classOf returns -1). On taxonomy-shaped label sets
//	                like {billing, billing_dispute} that silently zeroes real
//	                mass; counting it is what makes the zeroing rate measurable.
type Detail struct {
	Margin        float64
	MatchedMargin float64
	DeclaredMass  float64
	Ambiguous     int
}

// MarginDetail returns the full decision-position view at the value position of
// jsonKey. ok=false when the position or a usable raw distribution can't be
// resolved (no alternatives, or every alternative clamped to the -inf
// sentinel) — the caller then falls back to other signals rather than
// escalating blindly, exactly as with Margin.
//
// ok here means "the RAW distribution was usable", which is weaker than
// Margin's ok ("matched class mass was usable"): a position where the model put
// all of its mass outside the legal classes yields ok=true with
// DeclaredMass == 0 and both margins 0 — that is a fact worth recording, not an
// absence of data.
func MarginDetail(toks []llamaclient.TokenLogprob, jsonKey string, classes []string) (Detail, bool) {
	if len(toks) == 0 || len(classes) < 2 {
		return Detail{}, false
	}
	pos, ok := decisionPos(toks, jsonKey)
	if !ok {
		return Detail{}, false
	}
	d, _, allOK := classMassDetail(toks[pos].Top, classes)
	return d, allOK
}

// MarginFull is the full-denominator margin and the declared (matched) mass
// share, for callers that need only those two numbers.
func MarginFull(toks []llamaclient.TokenLogprob, jsonKey string, classes []string) (margin float64, declaredMass float64, ok bool) {
	d, ok := MarginDetail(toks, jsonKey, classes)
	return d.Margin, d.DeclaredMass, ok
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
// probability mass per class, and returns the normalized top-1 vs top-2 margin
// over the MATCHED mass only — the scale the gate has always used, kept
// byte-identical because three consumers compare it against stored history.
func classMassMargin(top []llamaclient.AltToken, classes []string) (float64, bool) {
	d, matchedOK, _ := classMassDetail(top, classes)
	if !matchedOK {
		return 0, false
	}
	return d.MatchedMargin, true
}

// classMassDetail is the single pass both scales are derived from. It folds
// each candidate token to a legal class and sums the raw probability mass per
// class (matchedTotal) while also summing EVERY alternative's mass (totalAll),
// matched or not — the -inf sentinel is the only exclusion, via expLogprob.
//
// matchedOK reports whether matched mass was usable (the pre-D-130 contract);
// allOK whether any raw mass was usable at all.
func classMassDetail(top []llamaclient.AltToken, classes []string) (d Detail, matchedOK bool, allOK bool) {
	if len(top) == 0 {
		return Detail{}, false, false
	}
	folded := make([]string, len(classes))
	for i, c := range classes {
		folded[i] = fold(c)
	}
	mass := make([]float64, len(classes))
	var matchedTotal, totalAll float64
	var ambiguous int
	for _, a := range top {
		p := expLogprob(a.Logprob)
		totalAll += p
		ft := fold(a.Token)
		if ft == "" {
			continue
		}
		ci, amb := classOfDetail(ft, folded)
		if amb {
			ambiguous++
		}
		if ci < 0 {
			continue // grammar-illegal / unrelated / ambiguous token
		}
		mass[ci] += p
		matchedTotal += p
	}
	if totalAll <= 0 {
		return Detail{}, false, false
	}
	first, second := 0.0, 0.0
	for _, m := range mass {
		if m > first {
			second, first = first, m
		} else if m > second {
			second = m
		}
	}
	d = Detail{
		Margin:       (first - second) / totalAll,
		DeclaredMass: matchedTotal / totalAll,
		Ambiguous:    ambiguous,
	}
	if matchedTotal > 0 {
		d.MatchedMargin = (first - second) / matchedTotal
		matchedOK = true
	}
	return d, matchedOK, true
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
	i, _ := classOfDetail(ft, folded)
	return i
}

// classOfDetail is classOf plus the reason a -1 happened: ambiguous=true means
// the token prefixed MORE THAN ONE class (real mass silently dropped),
// ambiguous=false with -1 means it matched no class at all (grammar-illegal or
// unrelated vocabulary, which is a correct drop).
func classOfDetail(ft string, folded []string) (idx int, ambiguous bool) {
	match := -1
	for i, c := range folded {
		if c == ft {
			return i, false // exact match wins outright
		}
		if strings.HasPrefix(c, ft) { // token abbreviates / begins this class
			if match >= 0 {
				return -1, true // prefix of more than one class — ambiguous, credit none
			}
			match = i
		}
	}
	return match, false
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
