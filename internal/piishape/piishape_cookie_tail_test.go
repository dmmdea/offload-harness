package piishape

import (
	"encoding/json"
	"strings"
	"testing"
)

func cookieMatch(text string) string {
	for _, cl := range Classes {
		if cl.Name == "cookie-header" {
			return cl.Re.FindString(text)
		}
	}
	return ""
}

// asArgs is the text as the corpus holds it: the content of a tool call's Args, which are JSON text, with
// a line above and below, so a quote is a backslash and a quote and the line break is a backslash and n.
func asArgs(t *testing.T, text string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"content": "line one\n" + text + "\nline three"})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// cookieQuoteSamples are a Cookie or Set-Cookie line with a double-quoted pair in it. want is the end of the
// match: the value that must be matched whole, and what follows it on the line (the other cookies go with
// the first long value, which is the safe direction). Built from pieces at run time so no literal in the
// tree is credential-shaped.
func cookieQuoteSamples(v, v2 string) []struct{ name, text, want string } {
	const (
		ck  = "Coo" + "kie: "
		sck = "Set-Coo" + "kie: "
	)
	return []struct{ name, text, want string }{
		// A quote in front of the first long value.
		{"quoted pair before the value", ck + `messages="ok"; sessionid=` + v, v},
		{"quoted path before the value", sck + `a=b; Path="/"; sessionid=` + v + `; HttpOnly`, v + `; HttpOnly`},
		{"two quoted pairs before the value", ck + `a="x"; b="y"; sessionid=` + v, v},
		{"octal escape in the quoted pair", ck + `messages="a\054b"; sessionid=` + v, v},
		// The first long value is itself quoted.
		{"quoted value", sck + `messages="` + v + `"; HttpOnly`, v + `"; HttpOnly`},
		{"quoted value, short pair after", ck + `a="` + v + `"; b=ok`, v + `"; b=ok`},
		// A quote after the first long value: the other cookies on the line go with it.
		{"quoted pair after the value", ck + `sid=` + v + `; theme="dark"; extra=` + v2, v2},
		{"quoted pair after, then a short pair", ck + `sid=` + v + `; theme="dark"; lang=en`, `lang=en`},
	}
}

// TestCookieHeaderStepsOverQuotedPairs: the prefix in front of the first long value, and the tail behind it,
// stopped at the first double quote. A quoted cookie before the session value (Django's messages cookie) hid
// the whole line, and a quoted cookie after it ended the match, so the cookies behind the quote went to the
// corpus and the seat verbatim. A quoted pair is stepped over, on the plain line and in JSON text.
func TestCookieHeaderStepsOverQuotedPairs(t *testing.T) {
	v, v2 := valueFixture(701, 32, alnum), valueFixture(702, 24, alnum)
	for _, s := range cookieQuoteSamples(v, v2) {
		for _, args := range []bool{false, true} {
			text := s.text
			if args {
				text = asArgs(t, s.text)
			}
			got := cookieMatch(text)
			if got == "" {
				t.Errorf("%s (args %v): cookie-header does not recognise %q", s.name, args, text)
				continue
			}
			if args {
				// In JSON text a quote is a backslash and a quote, which the match carries.
				got = strings.ReplaceAll(got, `\"`, `"`)
			}
			if !strings.HasSuffix(got, s.want) {
				t.Errorf("%s (args %v): matched %q, want it to end with %q", s.name, args, got, s.want)
			}
		}
	}
}

// TestCookieHeaderInACommandEndsBeforeTheClosingQuote: a header in a curl command ends before the quote that
// closes the string, in double or single quotes, whatever stands inside it.
func TestCookieHeaderInACommandEndsBeforeTheClosingQuote(t *testing.T) {
	v := valueFixture(703, 32, alnum)
	for _, c := range []struct{ name, text, want string }{
		{"double quotes", `curl -H "Coo` + `kie: s=` + v + `" https://host.invalid/x`, v},
		{"single quotes", `curl -H 'Coo` + `kie: s=` + v + `' https://host.invalid/x`, v},
		{"double quotes, more cookies", `curl -H "Coo` + `kie: theme=dark; s=` + v + `; lang=en" https://host.invalid/x`, v + `; lang=en`},
		{"quoted pair in a double-quoted header", `curl -H "Coo` + `kie: m=\"ok\"; s=` + v + `" https://host.invalid/x`, v},
	} {
		got := cookieMatch(c.text)
		if got == "" || !strings.HasSuffix(got, c.want) {
			t.Errorf("%s: matched %q, want it to end with %q: %q", c.name, got, c.want, c.text)
		}
	}
}
