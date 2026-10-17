package piishape

import (
	"strings"
	"testing"
)

// goAmp is the six characters Go's JSON encoder writes for an ampersand (a backslash, u, 0, 0, 2, 6), so
// every corpus Args string that held one carries it. Written as a hex escape for the backslash so that
// this file holds no JSON escape of its own.
const goAmp = "\x5cu0026"

// quotedBodySamples are a quoted credential literal whose value holds a character that ends an UNQUOTED
// value (a space, a comma, a semicolon, an ampersand), which is legal inside quotes. Each half of the
// value is shorter than 12 characters, so a pattern that stops at the character sees no value at all.
// Built from pieces at run time so no literal in the tree is credential-shaped. The literal is the last
// thing in each text before its closers.
func quotedBodySamples(a, b string) []struct{ name, text, value string } {
	return []struct{ name, text, value string }{
		{"ampersand", `pass` + `word = "` + a + `&` + b + `"`, a + `&` + b},
		{"semicolon", `pass` + `word = "` + a + `;` + b + `"`, a + `;` + b},
		{"comma in a json value", `{"pass` + `word": "` + a + `,` + b + `"}`, a + `,` + b},
		{"space, single quotes", `DB_PASS` + `WORD='` + a + ` ` + b + `'`, a + ` ` + b},
		{"space, export", `export API_` + `KEY="` + a + ` ` + b + `"`, a + ` ` + b},
		{"python raw string", `pass` + `word = r'` + a + ` ` + b + `'`, a + ` ` + b},
		{"typed declaration", `const to` + `ken: string = "` + a + ` ` + b + `"`, a + ` ` + b},
		{"go short declaration", `pass` + `word := "` + a + `&` + b + `"`, a + `&` + b},
		{"setenv call", `t.Setenv("API_TO` + `KEN", "` + a + `&` + b + `")`, a + `&` + b},
		{"the other quote inside", `pass` + `word = "` + a + `'` + b + `"`, a + `'` + b},
		{"json escaped", `{\"pass` + `word\": \"` + a + `,` + b + `\"}`, a + `,` + b},
		{"json escaped, ampersand as written by Go's encoder", `{\"pass` + `word\": \"` + a + goAmp + b + `\"}`, a + goAmp + b},
	}
}

func assignmentMatch(text string) string {
	for _, cl := range Classes {
		if cl.Name == "api-key-assignment" {
			return cl.Re.FindString(text)
		}
	}
	return ""
}

// TestQuotedLiteralRunsToItsClosingQuote: a quoted literal ended at the first space, comma, semicolon or
// ampersand inside it, so a value with one of them was a total miss when each side was shorter than 12
// characters. A quoted literal runs to the matching closing quote (or to a backslash, which is an escaped
// quote in JSON text), never past a line break, and the closing quote is not part of the match.
func TestQuotedLiteralRunsToItsClosingQuote(t *testing.T) {
	a, b := valueFixture(601, 8, alnum), valueFixture(602, 8, alnum)
	for _, s := range quotedBodySamples(a, b) {
		got := assignmentMatch(s.text)
		if got == "" {
			t.Errorf("%s: api-key-assignment does not recognise %q", s.name, s.text)
			continue
		}
		if !strings.HasSuffix(got, s.value) {
			t.Errorf("%s: matched %q, want it to end with the value %q", s.name, got, s.value)
		}
	}
}

// TestQuotedLiteralStopsAtALineBreak: the body of a quoted literal never runs onto the next line, so an
// unterminated quote does not swallow the text below it.
func TestQuotedLiteralStopsAtALineBreak(t *testing.T) {
	a, b := valueFixture(603, 8, alnum), valueFixture(604, 14, alnum)
	text := `pass` + `word = "` + a + "\n" + b + `"`
	if got := assignmentMatch(text); got != "" {
		t.Errorf("matched %q across a line break in %q", got, text)
	}
}

// TestQuotedLiteralsStayQuietWhenShortOrNotAnAssignment: the wider body needs the same 12 characters, the
// same operator and the same first-character rule as before.
func TestQuotedLiteralsStayQuietWhenShortOrNotAnAssignment(t *testing.T) {
	for _, text := range []string{
		`pass` + `word = "short value"`,
		`pass` + `word = "a b c d"`,
		`token_count = "abcdef ghijkl mnop"`,
		`max_tokens: "abcdef ghijkl mnop"`,
		`the token is "abcdef ghijkl mnop" today`,
		`pass` + `word = "/etc/ssl/certs/ca bundle"`,
		`pass` + `word = "   "`,
		`pass` + `word = hello world and a few more words`,
		`pass` + `word = ""`,
	} {
		if got := Matching(text); got != nil {
			t.Errorf("Matching(%q) = %v, want nil", text, got)
		}
	}
}
