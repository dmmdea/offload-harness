package piishape

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The probes of the review of the whole-span class: each one is a line the first version of the class took the
// wrong span of, or took none of, with the credential left in the text and the residual gate clean.

// TestCookieSpanEndsBeforeTheQuoteAfterAnEqualsSign: a last cookie that ends in an equals sign (base64 padding, or no
// value at all) stands right in front of the quote that closes the string the header sits in. That quote is not the
// opening quote of a value, so the span ends in front of it: the next field of Args, the next key of a document and
// the next argument of a command are not taken, Args stay valid JSON, and a second header behind the first is found
// by its own name and loses its value.
func TestCookieSpanEndsBeforeTheQuoteAfterAnEqualsSign(t *testing.T) {
	v, v2, _, _ := spanValues()
	for _, last := range []string{"p=QUJD==", "flag=", "tok=QUJD=", "p=="} {
		body := "sid=" + v + "; " + last
		// Args with the header last in its string and another field behind it.
		b, _ := json.Marshal(map[string]string{"content": "GET / HTTP/1.1\n" + ckName + ": " + body, "path": "/x"})
		assertSpan(t, "args, last cookie "+last, string(b), enc(ckName+": "+body), true, v)
		if !strings.Contains(string(b), `"path":"/x"`) {
			t.Fatal("the fixture lost its second field")
		}
		// A document, with another key behind the header.
		doc := `{"headers":{"` + ckName + `":"` + body + `","Accept":"x"},"url":"u"}`
		assertSpan(t, "document, last cookie "+last, doc, ckName+`":"`+body, false, v)
		// A command, with other arguments behind the header.
		curl := `curl -H "` + ckName + ": " + body + `" -d "a=b" -H "X-Next: y" https://host.invalid/p`
		assertSpan(t, "curl, last cookie "+last, curl, ckName+": "+body, false, v)
		// The same command in Args: the quotes are a backslash and a quote.
		assertSpan(t, "curl in args, last cookie "+last, argsOf(curl), enc(ckName+": "+body), true, v)
		// A second header behind the first: its value must not be left outside every match.
		two := `{"` + ckName + `":"` + body + `","` + sckName + `":"sid=` + v2 + `; Path=/"}`
		assertSpan(t, "two headers, last cookie "+last, two, ckName+`":"`+body, false, v, v2)
		assertSpan(t, "two headers in args, last cookie "+last, argsOf(two), enc(ckName+`":"`+body), true, v, v2)
	}
}

// TestCookieSpanTakesEveryElementOfAPrintedGoHeader: a header printed with the Go-syntax verb (http.Header, a map of
// string slices, a test failure message) is a slice literal. The span runs through every element of it, to the
// brace that closes the slice, and an element that comes after a short first one is found.
func TestCookieSpanTakesEveryElementOfAPrintedGoHeader(t *testing.T) {
	v, w, a, b := spanValues()
	for _, c := range []struct {
		name  string
		elems []string
	}{
		{"two long elements", []string{"csrf=" + a + "; Path=/", "sess=" + v + "; HttpOnly"}},
		{"three long elements", []string{"a=" + a, "b=" + b, "c=" + w}},
		{"short first element", []string{"theme=dark", "sess=" + v + "; HttpOnly"}},
		{"short first, long second, short third", []string{"theme=dark", "sess=" + v, "lang=en"}},
		{"quoted element", []string{"theme=dark", `m="` + a + " " + b + `"`, "sess=" + v}},
	} {
		for _, kind := range []string{"http.Header", "map"} {
			var t0 string
			if kind == "map" {
				t0 = fmt.Sprintf("%#v", map[string][]string{sckName: c.elems})
			} else {
				t0 = fmt.Sprintf("%#v", http.Header{sckName: c.elems})
			}
			want := goHeaderSpan(t0)
			label := c.name + ", " + kind
			assertSpan(t, label, t0, want, false, v, w, a, b)
			assertSpan(t, label+", in args", argsOf(t0), enc(want), true, v, w, a, b)
			assertSpan(t, label+", in a test message", "    x_test.go:42: headers = "+t0+", want nothing", want, false, v, w, a, b)
		}
	}
	for _, elems := range [][]string{{"theme=dark", "lang=en; Path=/"}, {"a=b"}} {
		if m := cookieMatch(fmt.Sprintf("%#v", http.Header{sckName: elems})); m != "" {
			t.Errorf("matched %q in a printed header of short elements", m)
		}
	}
}

// TestCookieSpanTakesAQuotedCookieBehindThreeLayersOfEscaping: JSON text in a double-quoted shell string in a tool
// call's Args puts seven backslashes in front of the quotes of a quoted cookie value, and JSON in that in a
// fourth layer puts fifteen. The class knew the levels none, one and three, and took nothing of such a line, or
// stopped at the first quote.
func TestCookieSpanTakesAQuotedCookieBehindThreeLayersOfEscaping(t *testing.T) {
	v, w, a, b := spanValues()
	for _, body := range []string{
		`m="` + a + " " + b + `"; sid=` + v,
		"sid=" + v + `; m="` + a + " " + b + `"; z=` + w,
		`m="` + a + `$HOME` + b + `"; sid=` + v,
	} {
		inner, _ := json.Marshal(map[string]map[string]string{"headers": {ckName: body}})
		text, _ := json.Marshal(map[string]string{"command": `curl -d "` + shellEsc(string(inner)) + `" https://host.invalid/p`})
		want := enc(shellEsc(ckName + `":"` + enc(body)))
		assertSpan(t, "three layers: "+body[:6], string(text), want, true, v, w, a, b)
		// A fourth layer: that command in the Args of a tool call that is itself JSON text.
		text4, _ := json.Marshal(map[string]string{"args": string(text)})
		assertSpan(t, "four layers: "+body[:6], string(text4), enc(want), true, v, w, a, b)
	}
}

// TestCookieSpanContinuesAcrossALineBreakInsideAnOpenQuote: a quoted cookie value that is open at a line break (a
// header wrapped by a terminal or a log formatter, a folded header) is not finished: the line break is part of the
// value and the span runs to the closing quote and past it. A line break with no quote open still ends the span, and
// so does a quote that is never closed within three line breaks (the span then ends at the first one).
func TestCookieSpanContinuesAcrossALineBreakInsideAnOpenQuote(t *testing.T) {
	v, w, a, b := spanValues()
	for _, c := range []struct{ name, line string }{
		{"line feed", ckName + `: m="` + a + "\n" + b + `"; sid=` + v},
		{"line feed in the second cookie", ckName + ": sid=" + w + `; m="` + a + "\n" + b + `"; z=` + v},
		{"carriage return and line feed", ckName + `: m="` + a + "\r\n" + b + `"; sid=` + v},
		{"obs-fold", ckName + `: m="` + a + "\r\n " + b + `"; sid=` + v},
		{"two line breaks", ckName + `: m="` + a + "\n" + b + "\n" + a + `"; sid=` + v},
	} {
		assertSpan(t, "raw, "+c.name, "x\n"+c.line+"\nmore sid="+v, c.line, false, v, w, a, b)
		assertSpan(t, "args, "+c.name, argsOf("x\n"+c.line+"\nmore sid="+v), enc(c.line), true, v, w, a, b)
	}
	// No quote is open at the line break: the span ends there, and the next line stays.
	span := ckName + ": sid=" + v + `; m="short"`
	if m := cookieMatch("one\n" + span + "\nnext sid=" + w); m[len(Lead(m)):] != span {
		t.Errorf("matched %q, want %q", m, span)
	}
	// A quote that is never closed ends at the line: the closing quote is more than three line breaks away.
	open := ckName + ": sid=" + v + `; m="` + a + " " + b
	far := "one\n" + open + "\nl1\nl2\nl3\nl4 \"quoted\" and sid=" + w
	if m := cookieMatch(far); m[len(Lead(m)):] != open {
		t.Errorf("a quote that closes four line breaks away: matched %q, want %q", m, open)
	}
	near := "one\n" + open + "\nl1\nl2\nl3 \"quoted\" tail\nl4"
	if m := cookieMatch(near); !strings.Contains(m, `l3 "`) {
		t.Errorf("a quote that closes three line breaks away: matched %q, want it to run to the quote", m)
	}
}

// TestCookieSpanTakesAShellEscapeInABareValue: a shell escape (an escaped dollar sign or backtick, a backslash path) in
// an unquoted cookie value is part of the value, whether it comes before the first long value or after it, and a
// quoted value that is cut by the end of the text is taken to the end of the text.
func TestCookieSpanTakesAShellEscapeInABareValue(t *testing.T) {
	v, w, a, b := spanValues()
	for _, c := range []struct{ name, span string }{
		{"escaped dollar after the long value", ckName + ": sid=" + v + `; v=\$Version; z=` + w},
		{"escaped dollar before the long value", ckName + `: a=x\$y; sid=` + v},
		{"escaped backtick before the long value", ckName + ": a=x\\`id\\`; sid=" + v},
		{"escaped backtick after the long value", ckName + ": sid=" + v + "; a=x\\`id\\`; z=" + w},
		{"escaped space", ckName + ": sid=" + v + `; a=x\ y; z=` + w},
		{"escaped semicolon", ckName + ": sid=" + v + `; a=x\;y; z=` + w},
	} {
		text := `curl -H "` + c.span + `" https://host.invalid/p`
		assertSpan(t, c.name, text, c.span, false, v, w)
		assertSpan(t, c.name+", in args", argsOf(text), enc(c.span), true, v, w)
	}
	// A backslash path in JSON text, where the backslashes are doubled.
	path := ckName + `: sid=` + v + `; dir=C:\users\x; z=` + w
	assertSpan(t, "backslash path", "one\n"+path+"\nthree", path, false, v, w)
	assertSpan(t, "backslash path in args", argsOf("one\n"+path+"\nthree"), enc(path), true, v, w)
	// A quoted value cut by the end of the text, behind a long bare one and in front of it.
	for _, span := range []string{
		ckName + ": sid=" + v + `; m="` + a + " " + b,
		ckName + ": sid=" + v + `; m="` + a + b,
		ckName + `: m="` + a + " " + b,
	} {
		assertSpan(t, "truncated quote: "+span[:20], "one\n"+span, span, false, v, a, b)
		assertSpan(t, "truncated quote in args: "+span[:20], argsOf("one\n"+span), enc(span), true, v, a, b)
	}
}

// TestCookieListOpenersAndTruncation: the opener of a list is a colon, an equals sign or a hash rocket, and a list
// that is cut in a later element, in double or single quotes, is still taken whole to the end of the text. These
// pin guards of the class that the generator reaches only in part.
func TestCookieListOpenersAndTruncation(t *testing.T) {
	v, w, _, _ := spanValues()
	for _, c := range []struct{ name, text, want string }{
		{"equals", sckName + `=["sid=` + v + `","b=` + w + `"]`, sckName + `=["sid=` + v + `","b=` + w + `"`},
		{"equals, spaced", sckName + ` = ["sid=` + v + `", "b=` + w + `"]`, sckName + ` = ["sid=` + v + `", "b=` + w + `"`},
		{"hash rocket", `{"` + sckName + `" => ["sid=` + v + `", "b=` + w + `"]}`, sckName + `" => ["sid=` + v + `", "b=` + w + `"`},
		{"cut in the second element", `{"` + sckName + `":["sid=` + v + `","b=` + w, sckName + `":["sid=` + v + `","b=` + w},
		{"cut in the second element, single quotes", `{'` + sckName + `': ['sid=` + v + `', 'b=` + w, sckName + `': ['sid=` + v + `', 'b=` + w},
		{"cut in the first element", `{"` + sckName + `":["sid=` + v, sckName + `":["sid=` + v},
	} {
		m := cookieMatch(c.text)
		if got := m[len(Lead(m)):]; got != c.want {
			t.Errorf("%s: matched %q, want %q", c.name, got, c.want)
		}
	}
}

// TestCookieOpenerLimits: what starts a match and what does not. A subscript opens a header, and a name inside
// another word, an opener more than eight characters from the name and a bare run of commas or ampersands do not.
func TestCookieOpenerLimits(t *testing.T) {
	v, _, _, _ := spanValues()
	for _, text := range []string{
		ckName + "[sid=" + v + "]",
		"h." + ckName + "[sid=" + v + "; b=x]",
		ckName + "=>sid=" + v,
		ckName + " => sid=" + v,
	} {
		if cookieMatch(text) == "" {
			t.Errorf("no match in %q", text)
		}
	}
	for _, text := range []string{
		ckName + strings.Repeat(" ", 12) + ": sid=" + v,
		"my_" + ckName + ": sid=" + v,
		ckName + ": prefs=en,fr,de,es,it,pt,ja",
		ckName + ": q=aa&bb&cc&dd&ee&ff",
	} {
		if m := cookieMatch(text); m != "" {
			t.Errorf("matched %q in %q", m, text)
		}
	}
}
