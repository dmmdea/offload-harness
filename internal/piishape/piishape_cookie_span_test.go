package piishape

import (
	"encoding/json"
	"strings"
	"testing"
)

// wrapperNamed finds a wrapper of the generator by its name.
func wrapperNamed(t *testing.T, name string) cookieWrapper {
	t.Helper()
	for _, w := range cookieWrappers {
		if w.name == name {
			return w
		}
	}
	t.Fatalf("no wrapper named %q", name)
	return cookieWrapper{}
}

// assertSpan checks one line: the cookie-header class names it, its match is exactly the span the contract
// describes (header name to the end of the line, the closing quote of the enclosing string, or the closing
// bracket of the list), and a redaction of the match leaves no piece of 8 or more characters of any planted
// value, in any of the forms the wrapper writes it.
func assertSpan(t *testing.T, label, text, want string, envelope bool, secrets ...string) {
	t.Helper()
	found := false
	for _, c := range Matching(text) {
		found = found || c == "cookie-header"
	}
	if !found {
		t.Errorf("%s: cookie-header does not name %q", label, text)
		return
	}
	m := cookieMatch(text)
	if got := m[len(Lead(m)):]; got != want {
		t.Errorf("%s: matched %q, want the whole span %q", label, got, want)
	}
	redacted := cookieClass().Re.ReplaceAllStringFunc(text, func(m string) string { return Lead(m) + "[redacted]" })
	for _, v := range secrets {
		for _, form := range []string{v, enc(v), shellEsc(v), enc(shellEsc(v))} {
			if run, ok := survivingRun(form, redacted, 8); ok {
				t.Errorf("%s: %q of the value %q survives in %q", label, run, v, redacted)
				break
			}
		}
	}
	if envelope && !json.Valid([]byte(redacted)) {
		t.Errorf("%s: the redacted Args are not valid JSON: %q", label, redacted)
	}
}

// spanValues are the fixtures of the probes: two long cookie values and the halves of a quoted message.
func spanValues() (v, w, a, b string) {
	return valueFixture(901, 32, alnum), valueFixture(902, 32, alnum), valueFixture(903, 16, alnum), valueFixture(904, 16, alnum)
}

// TestCookieSpanTakesAQuotedValueThatHoldsAnyCharacter: a quoted cookie value that holds a space, an apostrophe, an
// ampersand, a comma, a semicolon, a tab, a shell dollar sign or backtick, or a character that Go's encoder writes as
// an HTML escape ended the match (the class read the quoted body from a short alphabet), so the session cookie
// behind it, and the rest of the value, went to the corpus and to the seat verbatim. The span runs through the
// whole line whatever the quoted body holds, on a header line, in Args, in a curl command and in Args that hold one.
func TestCookieSpanTakesAQuotedValueThatHoldsAnyCharacter(t *testing.T) {
	v, _, a, b := spanValues()
	for _, ch := range []string{" ", "'", "&", ",", ";", "\t", "$", "`", "<", ">", "&<>", " and then "} {
		body := `m="` + a + ch + b + `"; sessionid=` + v
		for _, wn := range []string{"header line", "set header line", "args", "curl, double quotes", "args of a curl command", "args of args", "Header.Set call"} {
			w := wrapperNamed(t, wn)
			text, want := w.build(body, nil)
			assertSpan(t, wn+" with "+strings.ReplaceAll(strings.ReplaceAll(ch, "\t", "tab"), " ", "space"), text, want, w.envelope, v, a, b)
		}
	}
}

// TestCookieSpanTakesDjangosShortMessages: the messages a Django site flashes are short quoted values with a space,
// an apostrophe or an ampersand in them. None is a credential alone, but each one ended the walk to the session
// cookie behind it, so the session value was written out. With a session value on the line the whole line goes.
func TestCookieSpanTakesDjangosShortMessages(t *testing.T) {
	v, _, _, _ := spanValues()
	for _, msg := range []string{"Saved ok", "Can't save", "Tom&Jerry"} {
		for _, body := range []string{
			`messages="` + msg + `"; sessionid=` + v,
			`sessionid=` + v + `; messages="` + msg + `"`,
			`a=b; messages="` + msg + `"; csrftoken=` + v + `; lang=en`,
		} {
			for _, wn := range []string{"header line", "args", "curl, double quotes", "args of a curl command"} {
				w := wrapperNamed(t, wn)
				text, want := w.build(body, nil)
				assertSpan(t, wn+" "+msg, text, want, w.envelope, v, msg)
			}
		}
	}
}

// TestCookieSpanTakesEveryElementOfAList: a Set-Cookie list was matched to the end of its first element that held a
// long value, or not at all when the first element was short, so the later elements went out verbatim. A list is one
// span to its closing bracket, however many elements it has, on a line, across lines, in Args and in Args that
// hold Args.
func TestCookieSpanTakesEveryElementOfAList(t *testing.T) {
	v, w2, a, b := spanValues()
	lists := map[string][]string{
		"short first element":       {"theme=dark", "session=" + v + "; Path=/"},
		"two long elements":         {"session=" + v, "other=" + w2},
		"long, short, long":         {"session=" + v + "; HttpOnly", "theme=dark", "other=" + w2 + "; Path=/"},
		"three short, then a long":  {"theme=dark", "lang=en", "a=b", "session=" + v},
		"quoted element with space": {`m="` + a + ` ` + b + `"`, "session=" + v},
	}
	for name, elems := range lists {
		for _, wn := range []string{"json list", "json list with spaces", "json list in args", "json list in args of args", "pretty-printed list", "pretty-printed list in args", "python list"} {
			w := wrapperNamed(t, wn)
			if w.plainOnly && strings.ContainsAny(strings.Join(elems, ""), `\'`) {
				continue
			}
			text, want := w.build("", elems)
			assertSpan(t, wn+", "+name, text, want, w.envelope, v, w2, a, b)
		}
	}
}

// TestCookieSpanTakesHTMLEscapesAheadOfTheCredential: Go's encoder writes an ampersand, a less-than and a greater-than
// as backslash-u escapes, and a backslash stopped the walk, so a cookie before the credential that held one hid the
// credential. The escapes are part of the line.
func TestCookieSpanTakesHTMLEscapesAheadOfTheCredential(t *testing.T) {
	v, _, _, _ := spanValues()
	for _, body := range []string{
		"a=x" + bsu + "0026y; sessionid=" + v,
		`a=x&y; sessionid=` + v,
		"a=x" + bsu + "003cy" + bsu + "003e; sessionid=" + v,
		`a=x<y>z&; sessionid=` + v,
	} {
		for _, wn := range []string{"header line", "args", "args of args", "folded header in args"} {
			w := wrapperNamed(t, wn)
			text, want := w.build(body, nil)
			assertSpan(t, wn+" "+body, text, want, w.envelope, v)
		}
	}
}

// TestCookieSpanTakesAFoldedHeader: an HTTP header can be folded, a line break and a space or tab, between the name
// and the cookies; the line break ended the walk before it began. The fold may stand before or after the colon, as
// raw characters or as the JSON escapes of them.
func TestCookieSpanTakesAFoldedHeader(t *testing.T) {
	v, _, a, b := spanValues()
	for _, c := range []struct{ name, line string }{
		{"after the colon", ckName + ":\r\n sid=" + v},
		{"after the colon, line feed and tab", ckName + ":\n\tsid=" + v},
		{"after the colon, two spaces", ckName + ":\r\n  m=\"" + a + " " + b + "\"; sid=" + v},
		{"before the colon", ckName + "\r\n : sid=" + v},
		{"set header", sckName + ":\r\n sid=" + v + "; Path=/"},
	} {
		text := "line one\r\n" + c.line + "\r\nline three"
		assertSpan(t, "raw, "+c.name, text, c.line, false, v, a, b)
		args := argsOf("one\n" + c.line + "\nthree")
		assertSpan(t, "args, "+c.name, args, enc(c.line), true, v, a, b)
	}
}

// TestCookieSpanTakesAFoldedList: a list can be folded too: a line break before the opening bracket, after it, or
// between the name and the colon. The span still ends at the closing bracket.
func TestCookieSpanTakesAFoldedList(t *testing.T) {
	v, w, _, _ := spanValues()
	tail := `], "Other": "x=` + w + `"`
	for _, c := range []struct{ name, span string }{
		{"line break before the bracket", sckName + ":\r\n [\"sid=" + v + "\""},
		{"line break after the bracket", sckName + ": [\r\n  \"sid=" + v + "\"\r\n"},
		{"fold before the colon", sckName + "\r\n : [\"sid=" + v + "\""},
	} {
		assertSpan(t, "raw, "+c.name, "x "+c.span+tail, c.span, false, v)
		assertSpan(t, "args, "+c.name, argsOf("x "+c.span+tail), enc(c.span), true, v)
	}
}

// TestCookieSpanTakesShellEscapesAtLevelThree: in Args that hold a curl command, a quoted cookie value is three
// backslashes and a quote, and a dollar sign or a backtick the shell had to escape carries two backslashes in front
// of it. The escapes ended the walk.
func TestCookieSpanTakesShellEscapesAtLevelThree(t *testing.T) {
	v, _, a, b := spanValues()
	for _, mid := range []string{"$HOME", "`id`", "$(id)", `\$`, "$$ and `x`", "a\\b"} {
		body := `m="` + a + mid + b + `"; sessionid=` + v
		w := wrapperNamed(t, "args of a curl command")
		text, want := w.build(body, nil)
		assertSpan(t, "args of a curl command with "+mid, text, want, true, v, a, b)
	}
}

// TestCookieSpanEndsWhereTheContractSays: the span stops at the end of the line, at the closing quote of the string
// the header sits in (a bare quote, or a quote with 1, 3, 7 or 15 backslashes) and at the closing bracket of a
// list, whichever comes first; the closing quote and the bracket are not part of it.
func TestCookieSpanEndsWhereTheContractSays(t *testing.T) {
	v, _, a, b := spanValues()
	span := ckName + ": theme=dark; m=\"" + a + " " + b + "\"; sid=" + v
	for _, c := range []struct{ name, text, want string }{
		{"line feed", "x\n" + span + "\nmore sid=" + v, span},
		{"carriage return", "x\r" + span + "\rmore sid=" + v, span},
		{"carriage return and line feed", "x\r\n" + span + "\r\nmore sid=" + v, span},
		{"end of text", span, span},
		{"escaped line feed in Args", argsOf("x\n" + span + "\nmore sid=" + v), enc(span)},
		{"bare closing quote", `{"h":"` + span + `","k":"w sid=` + v + `"}`, span},
		{"escaped closing quote", argsOf(`{"h":"` + span + `","k":"w sid=` + v + `"}`), enc(span)},
		{"closing quote of a shell string", `curl -H "` + ckName + `: sid=` + v + `" -H "X: y z" https://host.invalid/x`, ckName + ": sid=" + v},
		{"closing quote of a single-quoted shell string", `curl -H '` + ckName + `: sid=` + v + `' -H 'X: y z' https://host.invalid/x`, ckName + ": sid=" + v},
		{"closing bracket of a list", `{"` + sckName + `":["sid=` + v + `"],"Other":["x=` + v + `"]}`, sckName + `":["sid=` + v + `"`},
		{"closing bracket of a printed header", "Header:map[" + ckName + ":[sid=" + v + "] Other:[x=" + v + "]]", ckName + ":[sid=" + v},
	} {
		m := cookieMatch(c.text)
		if got := m[len(Lead(m)):]; got != c.want {
			t.Errorf("%s: matched %q, want %q in %q", c.name, got, c.want, c.text)
		}
	}
}

// TestCookieSpanOnTruncatedText: a harvest and a seat call can see text that was cut anywhere, inside a value. Once
// 12 characters of a value are there, the line matches, whatever the cut left of the quoting, and the match runs
// to the end of what is there.
func TestCookieSpanOnTruncatedText(t *testing.T) {
	v, _, a, b := spanValues()
	for _, c := range []struct{ name, line, value string }{
		{"bare value", ckName + ": a=b; sid=" + v + "; theme=dark", v},
		{"quoted value", ckName + `: a=b; m="` + a + ` ` + b + `"; theme=dark`, a},
		{"after a quoted pair", ckName + `: m="x y"; sid=` + v, v},
	} {
		for _, wrap := range []string{"raw", "args"} {
			text := c.line
			if wrap == "args" {
				text = enc(c.line)
			}
			start := strings.Index(text, c.value)
			for cut := start + 12; cut <= len(text); cut++ {
				part := text[:cut]
				m := cookieMatch(part)
				if m == "" {
					t.Errorf("%s (%s): no match on the text cut at %d: %q", c.name, wrap, cut, part)
					break
				}
				if !strings.Contains(m, c.value[:12]) {
					t.Errorf("%s (%s): match %q leaves out the start of the value, cut at %d", c.name, wrap, m, cut)
					break
				}
			}
		}
	}
}

// TestCookieSpanStaysQuietWithoutALongValue: the contract widens how far a match runs, not what starts one. A header
// whose cookies are all short, code that handles cookies, a list of short elements and prose about the header
// match nothing.
func TestCookieSpanStaysQuietWithoutALongValue(t *testing.T) {
	for _, text := range []string{
		ckName + `: messages="Saved ok"; theme=dark`,
		ckName + `: m="Can't save"; lang=en`,
		ckName + `: m="Tom&Jerry"`,
		ckName + ": session=\" + fixtureJWT() + \"; path=/",
		ckName + `: m="Your changes were saved successfully"`,
		ckName + ": a=x" + bsu + "0026y; b=z",
		ckName + `: a=x&y; b=z`,
		ckName + ":\r\n theme=dark",
		sckName + `: id=a3fWa; Expires=Wed, 21 Oct 2015 07:28:00 GMT; Secure; HttpOnly`,
		`{"` + sckName + `":["theme=dark","lang=en; Path=/"]}`,
		"{\n  \"" + sckName + "\": [\n    \"theme=dark\",\n    \"lang=en\"\n  ]\n}",
		`req.Header.Set("` + ckName + `", c.String())`,
		`req.Header.Get("` + ckName + `")`,
		`http.SetCookie(w, &http.Cookie{Name: "session", Value: "abcdefghijklmnopqrstuvwxyz", Path: "/"})`,
		`cookie := &http.Cookie{Name: "a", Value: "abcdefghijklmnopqrstuvwxyz"}`,
		"the cookie, path=/some/long/path/value",
		"A " + ckName + " header carries name=value pairs; see RFC 6265 for the full list of attributes",
	} {
		if m := cookieMatch(text); m != "" {
			t.Errorf("matched %q in %q", m, text)
		}
	}
}
