package piishape

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The axes along which the third review found the whole-span class to break:
//
//   - a header printed with the Go-syntax verb, or a JSON or Python dict, that holds a Cookie with short values in front of
//     a Set-Cookie with a long one (the verb sorts the keys, so every real print has this order): the match started at
//     the first key and ran to the end of the text;
//   - a Python list or a JSON list with a quoted cookie value, inside the Args of a tool call;
//   - a bare value with a shell escape, a slash escape or a backslash path in its first 12 characters, and a capital N or R
//     after a backslash, which is no escape of a line break;
//   - the greater-than sign right behind the header name (an XML element, an arrow).

// round3Shapes are bare values whose escape stands inside the 12 characters that start a match, with the value of 12
// characters (the class takes the line) and the one of 11 (it does not), and a path with a capital N and R.
func round3Shapes() []cookieShape {
	var out []cookieShape
	for _, k := range []struct{ name, esc string }{
		{"escaped dollar sign", `\$`},
		{"escaped backtick", "\\`"},
		{"escaped slash", `\/`},
	} {
		for _, at := range []struct {
			name string
			pos  int
		}{{"at the start", 0}, {"in the middle", 6}} {
			body := "bcdefghijkl"
			long := body[:at.pos] + k.esc + body[at.pos:]
			short := body[:at.pos] + k.esc + body[at.pos:len(body)-1]
			out = append(out,
				cookieShape{"bare, 12 characters, " + k.name + " " + at.name, "e=" + long, []string{long}, true},
				cookieShape{"bare, 11 characters, " + k.name + " " + at.name, "e=" + short, []string{short}, false})
		}
	}
	al := func(seed, n int) string { return valueFixture(seed, n, alnum) }
	A, B := al(841, 16), al(842, 16)
	return append(out, cookieShape{"bare with a capital N and R in a path", "d=" + A + `\Name\Reports` + B, []string{A, B}, true})
}

// round3Wrappers are the wrappers of those axes.
func round3Wrappers() []cookieWrapper {
	short := []string{"theme=dark"}
	pyShort := `'` + ckName + `': ['theme=dark'], `
	return []cookieWrapper{
		{name: "Go header, a short Cookie ahead of Set-Cookie", build: func(_ string, elems []string) (string, string) {
			t := fmt.Sprintf("%#v", http.Header{ckName: short, sckName: elems})
			return t, goHeaderSpan(t)
		}},
		{name: "Go map, a short Cookie ahead of Set-Cookie", build: func(_ string, elems []string) (string, string) {
			t := fmt.Sprintf("%#v", map[string][]string{ckName: short, sckName: elems})
			return t, goHeaderSpan(t)
		}},
		{name: "Go header, a short Cookie ahead of Set-Cookie, in args", envelope: true, build: func(_ string, elems []string) (string, string) {
			t := fmt.Sprintf("%#v", http.Header{ckName: short, sckName: elems})
			return argsOf(t), enc(goHeaderSpan(t))
		}},
		{name: "Go header, a short Cookie ahead of Set-Cookie, in a test message", build: func(_ string, elems []string) (string, string) {
			t := fmt.Sprintf("%#v", http.Header{ckName: short, sckName: elems})
			return "    x_test.go:42: headers = " + t + ", want nothing", goHeaderSpan(t)
		}},
		{name: "json object of two lists", build: func(_ string, elems []string) (string, string) {
			w := sckName + `":[` + quotedList(elems, ",")
			return `{"` + ckName + `":["theme=dark"],"` + w + `]}`, w
		}},
		{name: "json object of two lists, Set-Cookie first", build: func(_ string, elems []string) (string, string) {
			w := sckName + `":[` + quotedList(elems, ",")
			return `{"` + w + `],"` + ckName + `":["theme=dark"]}`, w
		}},
		{name: "json object of two lists, in args", envelope: true, build: func(_ string, elems []string) (string, string) {
			w := sckName + `":[` + quotedList(elems, ",")
			return argsOf(`{"` + ckName + `":["theme=dark"],"` + w + `]}`), enc(w)
		}},
		{name: "python list, in args", envelope: true, plainOnly: true, build: func(_ string, elems []string) (string, string) {
			w := sckName + `': ['` + strings.Join(elems, `', '`) + `'`
			return argsOf(`{'` + w + `]}`), enc(w)
		}},
		{name: "python dict of two lists", plainOnly: true, build: func(_ string, elems []string) (string, string) {
			w := sckName + `': ['` + strings.Join(elems, `', '`) + `'`
			return `{` + pyShort + `'` + w + `]}`, w
		}},
		{name: "python dict of two lists, in args", envelope: true, plainOnly: true, build: func(_ string, elems []string) (string, string) {
			w := sckName + `': ['` + strings.Join(elems, `', '`) + `'`
			return argsOf(`{` + pyShort + `'` + w + `]}`), enc(w)
		}},
	}
}

// TestCookieLineOpenerTakesAGreaterThanSign: a greater-than sign right behind the header name is the end of an XML tag or
// the head of an arrow, and the cookies follow it.
func TestCookieLineOpenerTakesAGreaterThanSign(t *testing.T) {
	v := valueFixture(851, 36, alnum)
	re := cookieClass().Re
	for name, c := range map[string]struct{ text, want string }{
		"xml element":     {"<" + ckName + ">sid=" + v + "</" + ckName + ">", ckName + ">sid=" + v + "</" + ckName + ">"},
		"xml set element": {"<" + sckName + ">sid=" + v + "; Path=/</" + sckName + ">", sckName + ">sid=" + v + "; Path=/</" + sckName + ">"},
		"bare":            {ckName + ">sid=" + v, ckName + ">sid=" + v},
		"arrow":           {ckName + " -> sid=" + v, ckName + " -> sid=" + v},
		"hash rocket":     {ckName + " => sid=" + v, ckName + " => sid=" + v},
	} {
		if got := re.FindString(c.text); got != c.want {
			t.Errorf("%s: matched %q in %q, want %q", name, got, c.text, c.want)
		}
	}
}

// TestCookieListOpensBehindASpaceOrAName: a slice printed after the header name with the plain verb, fmt.Println("Cookie",
// []string{...}), puts a space and a bracket behind the name, and a subscript a bracket right behind it. A bracket there is the
// head of a list whether a colon stands in front of it or not; the match ends in front of the closing bracket.
func TestCookieListOpensBehindASpaceOrAName(t *testing.T) {
	v, w := valueFixture(871, 36, alnum), valueFixture(872, 36, alnum)
	re := cookieClass().Re
	for name, c := range map[string]struct{ text, want string }{
		"space":                 {ckName + " [sid=" + v + "]", ckName + " [sid=" + v},
		"space, two cookies":    {"x " + ckName + " [sid=" + v + " b=" + w + "]", ckName + " [sid=" + v + " b=" + w},
		"println of a slice":    {fmt.Sprint("x ", ckName, " ", []string{"sid=" + v, "b=" + w}), ckName + " [sid=" + v + " b=" + w},
		"subscript":             {ckName + "[sid=" + v + "]", ckName + "[sid=" + v},
		"subscript, a receiver": {"h." + ckName + "[sid=" + v + "; b=x]", ckName + "[sid=" + v + "; b=x"},
		"line break":            {ckName + "\n[sid=" + v + "]", ckName + "\n[sid=" + v},
	} {
		if got := re.FindString(c.text); got != c.want {
			t.Errorf("%s: matched %q in %q, want %q", name, got, c.text, c.want)
		}
	}
}
