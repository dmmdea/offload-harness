package piishape

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// bsu is a backslash and a u, written as a byte value: a source literal of the escape that Go's encoder writes for
// an ampersand is rewritten into the character by some editors and tools, and the test would then pin nothing.
const bsu = "\x5c" + "u"

// Names that would make a source line credential-shaped are written in two pieces.
const (
	ckName   = "Coo" + "kie"
	sckName  = "Set-Coo" + "kie"
	kwToken  = "tok" + "en"
	kwPass   = "pass" + "word"
	kwSecret = "sec" + "ret"
)

// cookieShape is one cookie of a header line: its text as a header carries it, every value on it of 8 or more
// characters (none may survive a redaction of the line it stands on) and whether it holds a value of 12 or more
// characters, which is what makes the class take the line.
type cookieShape struct {
	name   string
	text   string
	values []string
	long   bool
}

// cookieShapes are cookies as a browser, Django and a plain server write them, built from pieces at run time so no
// literal in the tree is credential-shaped: bare and quoted values, quoted values that hold a space, an
// apostrophe, an ampersand, a comma, a semicolon, a tab, a dollar sign, a backtick or an HTML-escaped character,
// an escaped quote, a doubled backslash and an octal escape, Django's short messages, and names that hold a key word.
func cookieShapes() []cookieShape {
	al := func(seed, n int) string { return valueFixture(seed, n, alnum) }
	V, W, X := al(811, 32), al(812, 32), al(813, 24)
	H := valueFixture(814, 40, "abcdef0123456789")
	A, B := al(815, 16), al(816, 16)
	flash := `$[[\"__json_message\"\0540\05425\054\"Saved\"]]`
	quoted := func(mid string) string { return `m="` + A + mid + B + `"` }
	shapes := []cookieShape{
		{"bare long", "sid=" + V, []string{V}, true},
		{"bare long, other name", "csrf=" + X, []string{X}, true},
		{"short", "theme=dark", nil, false},
		{"attribute", "Path=/", nil, false},
		{"quoted short", `ok="x"`, nil, false},
		{"escaped quote, short", `a="b\"c"`, nil, false},
		{"octal escape, short", `a="b\054c"`, nil, false},
		{"escaped backslash, short", `a="b\\"`, nil, false},
		{"quoted long", `q="` + W + `"`, []string{W}, true},
		{"flash with escapes", `messages="` + H + flash + `"`, []string{H}, true},
		{"octal escape inside a long value", `messages="` + H + `\054` + X + `"`, []string{H, X}, true},
		{"quoted body with a space", quoted(" "), []string{A, B}, true},
		{"quoted body with an apostrophe", quoted("'"), []string{A, B}, true},
		{"quoted body with an ampersand", quoted("&"), []string{A, B}, true},
		{"quoted body with a comma", quoted(","), []string{A, B}, true},
		{"quoted body with a semicolon", quoted(";"), []string{A, B}, true},
		{"quoted body with a tab", quoted("\t"), []string{A, B}, true},
		{"quoted body with a dollar sign", quoted("$HOME"), []string{A, B}, true},
		{"quoted body with a backtick", quoted("`id`"), []string{A, B}, true},
		{"quoted body with HTML characters", quoted("<b>&"), []string{A, B}, true},
		{"literal HTML escapes in a quoted body", quoted(bsu + "0026" + bsu + "003c" + bsu + "003e"), []string{A, B}, true},
		{"django saved", `m="Saved ok"`, []string{"Saved ok"}, false},
		{"django apostrophe", `m="Can't save"`, []string{"Can't save"}, false},
		{"django ampersand", `m="Rock&Roll"`, []string{"Rock&Roll"}, false},
		{"literal HTML escape, bare", "a=x" + bsu + "0026y", nil, false},
		{"ampersand, bare", `a=x&y`, nil, false},
		{"refresh key word", "refresh_" + kwToken + "=" + V, []string{V}, true},
		{"auth key word", "auth_" + kwToken + "=" + W, []string{W}, true},
		{"password key word", kwPass + "=" + V, []string{V}, true},
		{"secret key word", kwSecret + "=" + W, []string{W}, true},
		{"tab in a bare value", "a=x\ty", nil, false},
		{"white space in the first characters of a quoted value", `e="ab cdefghijklmnopqrstuvwxyz"`, []string{"cdefghijklmnopqrstuvwxyz"}, false},
	}
	return append(append(append(shapes, reviewShapes()...), edgeShapes()...), round3Shapes()...)
}

// edgeShapes are values at the edge of the count that starts a match: 12 characters with no white space from the
// start of a value, an escape counting as one. Each kind comes as a value of 12 (the class takes the line) and as
// one of 11 (it does not), bare and quoted, so a rule that counts an escape as more or less than one character
// is caught in every wrapping.
func edgeShapes() []cookieShape {
	var out []cookieShape
	for _, k := range []struct{ name, esc string }{
		{"unicode escape", bsu + "0026"},
		{"octal escape", `\054`},
		{"escaped quote", `\"`},
		{"escaped backslash", `\\`},
		{"escaped character", `\;`},
		{"dollar sign", "$"},
		{"backtick", "`"},
	} {
		long, short := "abcdef"+k.esc+"ghijk", "abcdef"+k.esc+"ghij"
		out = append(out,
			cookieShape{"quoted, 12 characters with an " + k.name, `e="` + long + `"`, []string{long}, true},
			cookieShape{"quoted, 11 characters with an " + k.name, `e="` + short + `"`, []string{short}, false})
	}
	long, short := "abcdef"+bsu+"0026ghijk", "abcdef"+bsu+"0026ghij"
	return append(out,
		cookieShape{"bare, 12 characters with a unicode escape", "e=" + long, []string{long}, true},
		cookieShape{"bare, 11 characters with a unicode escape", "e=" + short, []string{short}, false})
}

// enc is s as it stands inside a JSON string, the way Go's encoder writes it (an ampersand, a less-than and a
// greater-than become backslash-u escapes).
func enc(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// shellEsc is s inside a double-quoted shell string.
func shellEsc(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(s)
}

// goEsc is s inside a double-quoted Go string.
func goEsc(s string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) }

// cookieWrapper puts cookies where the corpus meets them. build gets the cookies joined as a header carries them
// and one per list element, and returns the text and the exact span the class has to take: from the header name
// to the end of the line, to the closing quote of the string the header sits in, or to the closing bracket of
// the list, whichever comes first. envelope marks the wrappers whose text is a JSON document around the header
// (a tool call's Args): the redacted text has to stay valid JSON. plainOnly wrappers cannot hold a backslash, an
// apostrophe or a line feed in the cookies. openOK marks the wrappers in which a quoted value that is never closed
// (a truncated line) ends the span where the line, or the string that holds the header, ends.
type cookieWrapper struct {
	name      string
	envelope  bool
	plainOnly bool
	openOK    bool
	// fourLayers marks the wrappers that escape the line four times: a backslash in front of a dollar sign or a backtick
	// is then 31 backslashes, more than the 16 the class takes in a bare value (a named gap).
	fourLayers bool
	build      func(body string, elems []string) (text, want string)
}

func argsOf(content string) string {
	b, _ := json.Marshal(map[string]string{"content": content})
	return string(b)
}

func quotedList(elems []string, sep string) string {
	q := make([]string, len(elems))
	for i, e := range elems {
		q[i] = `"` + enc(e) + `"`
	}
	return strings.Join(q, sep)
}

var baseCookieWrappers = []cookieWrapper{
	{name: "header line", openOK: true, build: func(body string, _ []string) (string, string) {
		w := ckName + ": " + body
		return "line one\r\n" + w + "\r\nline three", w
	}},
	{name: "set header line", openOK: true, build: func(body string, _ []string) (string, string) {
		w := sckName + ": " + body + "; HttpOnly"
		return "line one\n" + w + "\nline three", w
	}},
	{name: "args", envelope: true, openOK: true, build: func(body string, _ []string) (string, string) {
		w := ckName + ": " + body
		return argsOf("one\n" + w + "\nthree"), enc(w)
	}},
	{name: "curl, double quotes", openOK: true, build: func(body string, _ []string) (string, string) {
		w := ckName + ": " + shellEsc(body)
		return `curl -H "` + w + `" https://host.invalid/x`, w
	}},
	{name: "curl, single quotes", plainOnly: true, build: func(body string, _ []string) (string, string) {
		w := ckName + ": " + body
		return `curl -H '` + w + `' https://host.invalid/x`, w
	}},
	{name: "args of a curl command", envelope: true, openOK: true, build: func(body string, _ []string) (string, string) {
		w := ckName + ": " + shellEsc(body)
		return argsOf(`curl -H "` + w + `" https://host.invalid/x`), enc(w)
	}},
	{name: "args of args", envelope: true, openOK: true, build: func(body string, _ []string) (string, string) {
		w := ckName + ": " + body
		inner, _ := json.Marshal(map[string]string{"h": "one\n" + w + "\nthree"})
		return argsOf(string(inner)), enc(enc(w))
	}},
	{name: "Header.Set call", openOK: true, build: func(body string, _ []string) (string, string) {
		w := ckName + `", "` + goEsc(body)
		return `req.Header.Set("` + w + `")`, w
	}},
	{name: "folded header", openOK: true, build: func(body string, _ []string) (string, string) {
		w := ckName + ":\r\n " + body
		return "line one\r\n" + w + "\r\nline three", w
	}},
	{name: "folded header in args", envelope: true, openOK: true, build: func(body string, _ []string) (string, string) {
		w := ckName + ":\r\n\t" + body
		return argsOf("one\n" + w + "\nthree"), enc(w)
	}},
	{name: "json list", build: func(_ string, elems []string) (string, string) {
		w := sckName + `":[` + quotedList(elems, ",")
		return `{"` + w + `]}`, w
	}},
	{name: "json list with spaces", build: func(_ string, elems []string) (string, string) {
		w := sckName + `": [ ` + quotedList(elems, ", ") + " "
		return `{"` + w + `]}`, w
	}},
	{name: "json list in args", envelope: true, build: func(_ string, elems []string) (string, string) {
		w := sckName + `":[` + quotedList(elems, ",")
		return argsOf(`{"` + w + `]}`), enc(w)
	}},
	{name: "json list in args of args", envelope: true, build: func(_ string, elems []string) (string, string) {
		w := sckName + `":[` + quotedList(elems, ",")
		inner, _ := json.Marshal(map[string]string{"h": `{"` + w + `]}`})
		return argsOf(string(inner)), enc(enc(w))
	}},
	{name: "pretty-printed list", build: func(_ string, elems []string) (string, string) {
		b, _ := json.MarshalIndent(map[string][]string{sckName: elems}, "", "  ")
		t := string(b)
		return t, t[strings.Index(t, sckName):strings.LastIndex(t, "]")]
	}},
	{name: "pretty-printed list in args", envelope: true, build: func(_ string, elems []string) (string, string) {
		b, _ := json.MarshalIndent(map[string][]string{sckName: elems}, "", "  ")
		t := string(b)
		return argsOf(t), enc(t[strings.Index(t, sckName):strings.LastIndex(t, "]")])
	}},
	{name: "printed Go header", build: func(body string, _ []string) (string, string) {
		w := ckName + ":[" + body
		return "Header:map[" + w + "]]", w
	}},
	{name: "python list", plainOnly: true, build: func(_ string, elems []string) (string, string) {
		w := sckName + `': ['` + strings.Join(elems, `', '`) + `'`
		return `{'` + w + `]}`, w
	}},
}

// cookieWrappers are the base wrappers and the ones the reviews of the whole-span class added (reviewWrappers, round3Wrappers).
var cookieWrappers = append(append(append([]cookieWrapper{}, baseCookieWrappers...), reviewWrappers()...), round3Wrappers()...)

type cookieCase struct {
	wrapper string
	text    string
	want    string
	long    bool
	values  []string
	env     bool
}

// cookieCases are the lines the generator builds: every shape alone, a sample of ordered pairs and a sample of
// ordered triples, in every wrapper. A cookie list element is one cookie, every second one with an attribute.
func cookieCases() []cookieCase {
	shapes := cookieShapes()
	var out []cookieCase
	add := func(w cookieWrapper, idx ...int) {
		var texts, elems, values []string
		long := false
		for k, i := range idx {
			s := shapes[i]
			if w.plainOnly && strings.ContainsAny(s.text, "\\'\n") {
				return
			}
			if openEndShapes[s.name] && (k != len(idx)-1 || !w.openOK) {
				return
			}
			if w.fourLayers && strings.ContainsAny(s.text, "$`") && strings.Contains(s.text, "\\") && strings.Contains(s.name, "escaped") {
				return
			}
			texts = append(texts, s.text)
			e := s.text
			if k%2 == 1 {
				e += "; Path=/"
			}
			elems = append(elems, e)
			values = append(values, s.values...)
			long = long || s.long
		}
		text, want := w.build(strings.Join(texts, "; "), elems)
		out = append(out, cookieCase{w.name, text, want, long, values, w.envelope})
	}
	n := len(shapes)
	for wi, w := range cookieWrappers {
		for a := 0; a < n; a++ {
			add(w, a)
			for b := 0; b < n; b++ {
				if (a*31+b*17+wi)%5 == 0 {
					add(w, a, b)
				}
				for c := 0; c < n; c++ {
					if (a*7+b*13+c*29+wi*3)%211 == 0 {
						add(w, a, b, c)
					}
				}
			}
		}
	}
	return out
}

// cookieClass is the cookie-header class of the table.
func cookieClass() Class {
	for _, cl := range Classes {
		if cl.Name == "cookie-header" {
			return cl
		}
	}
	panic("no cookie-header class in the table")
}

// survivingRun reports the first run of n characters of v that appears in out. A run of mostly escape characters
// (the backslashes of an escaped quote in text that was escaped four times) says nothing, because the text around the
// span is full of them; a run counts when four or more of its characters are letters or digits.
func survivingRun(v, out string, n int) (string, bool) {
	for i := 0; i+n <= len(v); i++ {
		run := v[i : i+n]
		alnum := 0
		for _, c := range run {
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
				alnum++
			}
		}
		if alnum >= 4 && strings.Contains(out, run) {
			return run, true
		}
	}
	return "", false
}

// TestCookieHeaderSpanIsWholeInEveryWrapping: whatever the cookies on the line hold (a quoted value with a space, an
// apostrophe, an ampersand, a comma, a semicolon, a tab, a shell dollar sign or backtick, Go's HTML escapes, an
// escaped quote, a key word in the name), however many cookies or list elements there are, and however many times
// the line has been escaped, the class takes the line from the header name to the end of the line, to the
// closing quote of the string it sits in, or to the closing bracket of the list, whichever comes first, and it
// takes it whole: nothing of any value on the line survives a redaction of the match, the text around the
// match is untouched, and a tool call's Args stay valid JSON. A line that holds no value of 12 or more characters
// matches nothing.
func TestCookieHeaderSpanIsWholeInEveryWrapping(t *testing.T) {
	re := cookieClass().Re
	cases := cookieCases()
	if len(cases) < 2000 {
		t.Fatalf("the generator built %d cases, want at least 2000", len(cases))
	}
	type tally struct{ total, wrongSpan, leaks, falsePositives, badJSON int }
	byWrapper := map[string]*tally{}
	var order []string
	for _, c := range cases {
		tl := byWrapper[c.wrapper]
		if tl == nil {
			tl = &tally{}
			byWrapper[c.wrapper] = tl
			order = append(order, c.wrapper)
		}
		tl.total++
		m := re.FindString(c.text)
		if !c.long {
			if m != "" {
				tl.falsePositives++
				if tl.falsePositives <= 2 {
					t.Errorf("%s: matched %q in %q, which holds no value of 12 or more characters", c.wrapper, m, c.text)
				}
			}
			continue
		}
		got := m[len(Lead(m)):]
		if got != c.want {
			tl.wrongSpan++
			if tl.wrongSpan <= 2 {
				t.Errorf("%s: matched %q, want the whole span %q in %q", c.wrapper, got, c.want, c.text)
			}
		}
		redacted := re.ReplaceAllStringFunc(c.text, func(m string) string { return Lead(m) + "[redacted]" })
		leaked := false
		for _, v := range c.values {
			if len(v) < 8 || leaked {
				continue
			}
			for _, form := range []string{v, enc(v), shellEsc(v), enc(shellEsc(v))} {
				if run, ok := survivingRun(form, redacted, 8); ok {
					leaked = true
					tl.leaks++
					if tl.leaks <= 2 {
						t.Errorf("%s: %q of the value %q survives the redaction %q", c.wrapper, run, v, redacted)
					}
					break
				}
			}
		}
		if c.env && !json.Valid([]byte(redacted)) {
			tl.badJSON++
			if tl.badJSON <= 2 {
				t.Errorf("%s: the redacted Args are not valid JSON: %q", c.wrapper, redacted)
			}
		}
	}
	var failing []string
	for _, w := range order {
		tl := byWrapper[w]
		if tl.wrongSpan+tl.leaks+tl.falsePositives+tl.badJSON > 0 {
			failing = append(failing, fmt.Sprintf("%s: of %d cases %d have the wrong span, %d leak a value, %d match with no long value, %d break the JSON", w, tl.total, tl.wrongSpan, tl.leaks, tl.falsePositives, tl.badJSON))
		}
	}
	if len(failing) > 0 {
		t.Errorf("%d cases in %d wrappings, %d wrappings break the contract:\n%s", len(cases), len(order), len(failing), strings.Join(failing, "\n"))
	}
	t.Logf("%d cases from %d cookie shapes in %d wrappings", len(cases), len(cookieShapes()), len(order))
}
