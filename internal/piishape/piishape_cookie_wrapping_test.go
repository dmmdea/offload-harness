package piishape

import (
	"encoding/json"
	"strings"
	"testing"
)

// cookieShape is one cookie of a header line and the values on it that are credentials.
type cookieShape struct {
	text    string
	secrets []string
}

// cookieShapes are cookies as a browser, Django and a plain server write them: bare, quoted, with an escaped
// quote, a doubled backslash or an octal escape in the quotes, and a long quoted value with and without escapes.
// Built from pieces at run time so no literal in the tree is credential-shaped.
func cookieShapes() []cookieShape {
	h, v, w := valueFixture(801, 40, "abcdef0123456789"), valueFixture(802, 32, alnum), valueFixture(803, 26, alnum)
	return []cookieShape{
		{"sid=" + v, []string{v}},
		{"theme=dark", nil},
		{`ok="x"`, nil},
		{`a="b\"c"`, nil},
		{`a="b\054c"`, nil},
		{`a="b\\"`, nil},
		{`q="` + w + `"`, []string{w}},
		{`messages="` + h + `$[[\"__json_message\"\0540\05425\054\"Saved\"]]"`, []string{h}},
		{`messages="` + h + `\054` + w + `"`, []string{h, w}},
		{`messages="[[\"__json_message\"\0540\054` + w + `\"]]:` + h + `"`, []string{w, h}},
	}
}

// wrapCookieLine puts a cookie line where the corpus meets it: as a header line, as a Set-Cookie line, as
// the content of a tool call's Args, in a curl command (a double-quoted shell string, so a quote and a
// backslash are escaped), and in the Args of a tool call that holds such a command.
func wrapCookieLine(how, line string) string {
	shell := func() string {
		e := strings.ReplaceAll(strings.ReplaceAll(line, `\`, `\\`), `"`, `\"`)
		return `curl -H "Coo` + `kie: ` + e + `" https://host.invalid/x`
	}
	args := func(s string) string {
		b, _ := json.Marshal(map[string]string{"c": s})
		return string(b)
	}
	switch how {
	case "header":
		return "Coo" + "kie: " + line
	case "set-header":
		return "Set-Coo" + "kie: " + line + "; HttpOnly"
	case "args":
		return args("one\nCoo" + "kie: " + line + "\nthree")
	case "shell":
		return shell()
	default:
		return args(shell())
	}
}

// TestCookieHeaderTakesEveryLongValueInEveryWrapping: whatever the cookies before and after it and however
// many times the line has been escaped, every credential on a cookie line is inside the match, and a line
// that holds none matches nothing. Every line of two or three cookies is tried in each wrapping; with the
// pattern before the escape fix, 578 of the 1100 lines in each wrapping left a value outside the match.
func TestCookieHeaderTakesEveryLongValueInEveryWrapping(t *testing.T) {
	shapes := cookieShapes()
	for _, how := range []string{"header", "set-header", "args", "shell", "args of a shell command"} {
		var leaks, falsePositives, total int
		for a := range shapes {
			for b := range shapes {
				for c := -1; c < len(shapes); c++ {
					parts := []string{shapes[a].text, shapes[b].text}
					secrets := append(append([]string{}, shapes[a].secrets...), shapes[b].secrets...)
					if c >= 0 {
						parts = append(parts, shapes[c].text)
						secrets = append(secrets, shapes[c].secrets...)
					}
					text := wrapCookieLine(how, strings.Join(parts, "; "))
					got := cookieMatch(text)
					total++
					for _, s := range secrets {
						if !strings.Contains(got, s) {
							leaks++
							if leaks <= 3 {
								t.Errorf("%s: %q of %q is outside the match %q", how, s, text, got)
							}
							break
						}
					}
					if len(secrets) == 0 && got != "" {
						falsePositives++
						t.Errorf("%s: matched %q in %q, which holds no long value", how, got, text)
					}
				}
			}
		}
		if leaks > 0 || falsePositives > 0 {
			t.Errorf("%s: %d of %d lines leak a value, %d match without one", how, leaks, total, falsePositives)
		}
	}
}
