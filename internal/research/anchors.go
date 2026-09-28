package research

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// The research acceptance check is a WRONG-DOCUMENT tripwire built from the
// page's PROSE only. An earlier version tokenized the whole stripped page and
// scored identifier-shaped tokens highest, so CSS classes, script names, user
// handles and URL slugs won, and a faithful digest (which restates content,
// never markup) failed roughly three runs in four (register C-65). The rules
// now are:
//
//   - candidates come only from prose lines (sentences) and from headings — a
//     short capitalised line that introduces a prose line;
//   - a token that looks like an identifier (underscore, digit, camelCase, two
//     or more hyphens, or a run that is not purely letters) is never a
//     candidate, and neither is UI / markup vocabulary (chromeWords);
//   - a token is never cut: an over-long one is skipped;
//   - the words come from the page and never from the goal, so an echoed
//     question cannot pass; and they form ONE any-of check.
const (
	minWordLen     = 6
	maxWordLen     = 24
	minProseWords  = 7
	maxHeadWords   = 10
	minAnchorWords = 4
	anchorWords    = 24
)

var (
	reRun     = regexp.MustCompile(`[A-Za-z0-9_-]+`)
	reURL     = regexp.MustCompile(`(?i)\b(?:https?://|www\.)\S+|\S+@\S+\.\S+`)
	reMention = regexp.MustCompile(`[@#][A-Za-z0-9_]+`)
	reCodey   = regexp.MustCompile(`[{};=<>\$|]|\(\)|=>|::`)
)

// chromeWords is the UI, markup and script vocabulary that survives HTML
// stripping on forum, Q&A, media and blog pages. None of it is something a
// digest of the page's content would restate.
var chromeWords = map[string]struct{}{}

func init() {
	for _, w := range strings.Fields("helpful follow following followers question questions answer answers anonymous " +
		"reply replies comment comments share shared sharing subscribe subscribed subscription login logout signup " +
		"profile username password account notification notifications report reported upvote upvotes downvote " +
		"vote votes posted posts thread threads member members guest visitor visitors sidebar menu button buttons " +
		"toggle dropdown modal popup overlay lightbox carousel slider widget widgets banner footer header wrapper " +
		"container containers layout grid column columns section row icon icons image images thumbnail avatar " +
		"animation animated transition hover click clicked scroll scrolling listener handler callback function " +
		"element elements attribute attributes selector class classes style styles script scripts javascript " +
		"stylesheet viewport responsive desktop mobile tablet display hidden visible loading loaded detected " +
		"cookie cookies consent advert advertisement sponsored newsletter facebook twitter instagram youtube " +
		"pinterest linkedin whatsapp reddit email emails print printed previous next related popular trending " +
		"recent latest browser browsers window windows unread mark required optional") {
		chromeWords[w] = struct{}{}
	}
}

// looksLikeIdentifier reports whether a whole [A-Za-z0-9_-] run is
// identifier-shaped: not a plain word, not a single hyphenated prose word.
func looksLikeIdentifier(run string) bool {
	if strings.ContainsAny(run, "_0123456789") || camel(run) {
		return true
	}
	if strings.Count(run, "-") >= 2 {
		return true
	}
	return strings.HasPrefix(run, "-") || strings.HasSuffix(run, "-")
}

func camel(t string) bool {
	for i := 1; i < len(t); i++ {
		if t[i-1] >= 'a' && t[i-1] <= 'z' && t[i] >= 'A' && t[i] <= 'Z' {
			return true
		}
	}
	return false
}

func isProseLine(l string) bool {
	if len(strings.Fields(l)) < minProseWords || isCodey(l) {
		return false
	}
	letters := 0
	for _, r := range l {
		if unicode.IsLetter(r) || r == ' ' {
			letters++
		}
	}
	return float64(letters) >= 0.8*float64(len([]rune(l)))
}

func isHeadingShape(l string) bool {
	if strings.HasPrefix(l, "#") {
		return true
	}
	n := len(strings.Fields(l))
	if n < 2 || n > maxHeadWords || isCodey(l) {
		return false
	}
	r := []rune(l)
	return unicode.IsUpper(r[0]) && !strings.HasSuffix(l, ".") && !strings.ContainsAny(l, "0123456789@")
}

// proseWords returns the page's candidate words with their weights: an
// occurrence in a prose line counts 1, in a heading 2.
func proseWords(text string) map[string]float64 {
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	weights := map[string]float64{}
	add := func(l string, w float64) {
		l = reURL.ReplaceAllString(l, " ")
		l = reMention.ReplaceAllString(l, " ")
		l = strings.TrimLeft(l, "# ")
		for _, run := range reRun.FindAllString(l, -1) {
			if looksLikeIdentifier(run) {
				continue
			}
			for _, part := range strings.Split(run, "-") {
				p := strings.ToLower(part)
				if len(p) < minWordLen || len(p) > maxWordLen {
					continue
				}
				if _, b := boilerplate[p]; b {
					continue
				}
				if _, c := chromeWords[p]; c {
					continue
				}
				weights[p] += w
			}
		}
	}
	for i, l := range lines {
		switch {
		case isProseLine(l):
			add(l, 1)
		case isHeadingShape(l) && i+1 < len(lines) && (isProseLine(lines[i+1]) || strings.HasPrefix(l, "#")):
			add(l, 2)
		}
	}
	return weights
}

// Anchors returns up to n distinctive prose words of the page that do NOT
// occur in the goal, most central first: weight (prose and heading
// occurrences, capped so one repeated word cannot crowd the rest out), then
// length, then alphabetical for determinism.
func Anchors(text, goal string, n int) []string {
	goalLower := strings.ToLower(goal)
	type cand struct {
		w     string
		score float64
	}
	var cands []cand
	for w, wt := range proseWords(text) {
		if strings.Contains(goalLower, w) {
			continue
		}
		cands = append(cands, cand{w, min(wt, 6) + float64(min(len(w), 12))/24})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].w < cands[j].w
	})
	out := make([]string, 0, n)
	for _, c := range cands {
		if len(out) == n {
			break
		}
		out = append(out, c.w)
	}
	return out
}

// Anchor is the single most distinctive prose word (see Anchors).
func Anchor(text, goal string) string {
	if a := Anchors(text, goal, 1); len(a) == 1 {
		return a[0]
	}
	return ""
}

// AnchorCheck is the ONE acceptance line the research lane emits for a page: a
// case-insensitive alternation of the page's top prose words, tagged with the
// named group `docanchor` (delegate.strikeOnFingerprint keys on it, so a
// contract-caused failure never quarantines a node). A faithful digest passes
// by restating ANY of them; an answer about another document, or an echo of
// the goal (no goal word is ever included), does not. It is a wrong-document
// tripwire, not a quality gate: lexical overlap is a poor faithfulness metric
// (Maynez et al. 2020; Cao et al. 2022), so the bar is one word of
// twenty-four. A page with fewer than minAnchorWords usable words yields "" —
// nothing trustworthy to check.
func AnchorCheck(text, goal string) string {
	toks := Anchors(text, goal, anchorWords)
	if len(toks) < minAnchorWords {
		return ""
	}
	q := make([]string, len(toks))
	for i, t := range toks {
		q[i] = regexp.QuoteMeta(t)
	}
	return "regex:(?i)(?P<docanchor>" + strings.Join(q, "|") + ")"
}

func isCodey(l string) bool { return reCodey.MatchString(l) || strings.Contains(l, "`") }
