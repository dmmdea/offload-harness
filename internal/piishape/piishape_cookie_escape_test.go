package piishape

import (
	"encoding/json"
	"strings"
	"testing"
)

// jsonEncoded is s as it stands inside a JSON string: the text of a tool call's Args.
func jsonEncoded(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b[1 : len(b)-1])
}

// cookieEscapeSamples are a cookie line in which a quoted cookie holds escapes the way Python's http.cookies
// writes them, which is how Django writes its messages cookie: a backslash and a quote for a quote, a
// backslash and three octal digits for a comma, a semicolon or a space. want is the end of the match, in
// plain text. Built from pieces at run time so no literal in the tree is credential-shaped.
func cookieEscapeSamples(h, v, v2 string) []struct{ name, text, want string } {
	const (
		ck    = "Coo" + "kie: "
		sck   = "Set-Coo" + "kie: "
		flash = `$[[\"__json_message\"\0540\05425\054\"Saved\"]]`
	)
	return []struct{ name, text, want string }{
		// The quoted cookie is the first long value: it has to be taken whole, or the cookie behind it is lost.
		{"messages before the session", ck + `messages="` + h + flash + `"; sessionid=` + v, v},
		{"messages before the session, set header", sck + `messages="` + h + flash + `"; sessionid=` + v + `; HttpOnly`, v + `; HttpOnly`},
		{"octal escape inside a long value", ck + `messages="` + h + `\054` + v2 + `"; sid=` + v, v},
		{"messages alone", sck + `messages="` + h + flash + `"; HttpOnly`, flash + `"; HttpOnly`},
		// The quoted cookie comes after a bare long value: the tail steps over it.
		{"messages between two cookies", ck + `csrftoken=` + h + `; messages="[[\"__json_message\"\0540\05425\054\"Saved\"]]:` + v2 + `"; sessionid=` + v, v},
		{"escaped quote in a short pair after", ck + `sid=` + v + `; flash="a\"b"; extra=` + v2, v2},
		{"escaped quote in a short pair before", ck + `flash="a\"b"; sid=` + v, v},
		{"escaped backslash before the closing quote", ck + `flash="a\\"; sid=` + v, v},
	}
}

// TestCookieHeaderStepsOverEscapesInQuotedPairs: a quoted pair was taken to end at the first quote after a
// backslash, so a Django messages cookie (a long value with an escaped quote and octal escapes) ended the
// match, and the session cookie behind it was written to the corpus and the seat verbatim, with the
// residual gate blind to it because the value was never inside a match. In plain text a backslash and a
// quote is inside the value; in JSON text the same value carries more backslashes and the bare backslash and
// quote is the one that closes it.
func TestCookieHeaderStepsOverEscapesInQuotedPairs(t *testing.T) {
	h, v, v2 := valueFixture(721, 40, "abcdef0123456789"), valueFixture(722, 32, alnum), valueFixture(723, 26, alnum)
	for _, s := range cookieEscapeSamples(h, v, v2) {
		for _, args := range []bool{false, true} {
			text, want := s.text, s.want
			if args {
				text, want = asArgs(t, s.text), jsonEncoded(t, s.want)
			}
			got := cookieMatch(text)
			if got == "" {
				t.Errorf("%s (args %v): cookie-header does not recognise %q", s.name, args, text)
				continue
			}
			if !strings.HasSuffix(got, want) {
				t.Errorf("%s (args %v): matched %q, want it to end with %q", s.name, args, got, want)
			}
		}
	}
}

// TestCookieHeaderInACommandStepsOverEscapesInQuotedPairs: the same cookie in a curl command, where the
// header is a double-quoted shell string: a quote is a backslash and a quote, and an octal escape has its
// backslash doubled. The header ends before the quote that closes the string.
func TestCookieHeaderInACommandStepsOverEscapesInQuotedPairs(t *testing.T) {
	h, v := valueFixture(724, 40, "abcdef0123456789"), valueFixture(725, 32, alnum)
	for _, c := range []struct{ name, text, want string }{
		{"long value with an escape", `curl -H "Coo` + `kie: messages=\"` + h + `\\054x\"; s=` + v + `" https://host.invalid/x`, v},
		{"long value with an escaped quote", `curl -H "Coo` + `kie: messages=\"` + h + `\\\"x\\\"\"; s=` + v + `" https://host.invalid/x`, v},
		{"short pair with an escaped quote", `curl -H "Coo` + `kie: m=\"a\\\"b\"; s=` + v + `" https://host.invalid/x`, v},
	} {
		got := cookieMatch(c.text)
		if got == "" || !strings.HasSuffix(got, c.want) {
			t.Errorf("%s: matched %q, want it to end with %q: %q", c.name, got, c.want, c.text)
		}
	}
}

// TestQuotedCookiePairsWithEscapesStayQuiet: a quoted pair with an escape in it is not a credential when no
// value of 12 or more characters stands on the line, in plain text or in JSON text.
func TestQuotedCookiePairsWithEscapesStayQuiet(t *testing.T) {
	const ck = "Coo" + "kie: "
	for _, text := range []string{
		ck + `flash="a\"b"; theme=dark`,
		ck + `flash="a\054b"; lang=en`,
		ck + `flash="a\\"; lang=en`,
		ck + `flash="\"\"\""; theme=dark`,
		ck + `a="x"; b="y\"z"`,
	} {
		for _, args := range []bool{false, true} {
			if args {
				text = asArgs(t, text)
			}
			if got := cookieMatch(text); got != "" {
				t.Errorf("args %v: matched %q in %q", args, got, text)
			}
		}
	}
}
