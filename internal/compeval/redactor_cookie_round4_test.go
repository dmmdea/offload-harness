package compeval

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The fourth review of the whole-span cookie class (the round after the third) found leaks and breakages in the
// redactor itself: a bracket inside a cookie value or name in a printed list ended the span, an apostrophe in such a
// value opened a tail that ate the closers of the Args around it, Go's print of decoded JSON and Python's bytes
// literals were no opener, an underscore in front of the header name hid it, a value that starts with an ampersand
// or a closing bracket was not counted, credential headers printed as lists and a Basic header across a line break
// were missed, and a JSON document keyed by the header name came out invalid. Every test here plants values of 12
// or more characters, redacts the text on its own and as the Args of a tool call, and asserts the values are gone,
// the residual gate is clean, the Args are still JSON, the trace is kept by the harvest and, where it is named, the
// text after the span is untouched.

// r4Case is one text with the values planted in it.
type r4Case struct {
	label  string
	text   string
	values []string
	suffix string // the text must still end with this after redaction (the closers behind the span)
	keep   string // the text must still hold this after redaction
	valid  bool   // the text is a JSON document and must stay one
}

func r4Run(t *testing.T, cases []r4Case) {
	t.Helper()
	for _, c := range cases {
		out := reviewRedact(t, c.label, c.text, c.values...)
		if c.suffix != "" && !strings.HasSuffix(out, c.suffix) {
			t.Errorf("%s: the redacted text should end with %q, got %q", c.label, c.suffix, out)
		}
		if c.keep != "" && !strings.Contains(out, c.keep) {
			t.Errorf("%s: the redacted text should still hold %q, got %q", c.label, c.keep, out)
		}
		if c.valid && !json.Valid([]byte(out)) {
			t.Errorf("%s: the redacted JSON is not valid: %q", c.label, out)
		}
		args := reviewJSON(t, map[string]string{"cmd": c.text})
		aout := reviewRedact(t, c.label+", in Args", args, c.values...)
		if !json.Valid([]byte(aout)) {
			t.Errorf("%s, in Args: the redacted Args are not valid JSON: %q", c.label, aout)
		}
		reviewHarvest(t, c.label, c.text, c.values...)
	}
}

// r4Basic is the encoded value of a Basic header, built at run time.
func r4Basic(seed int) string {
	return base64.StdEncoding.EncodeToString([]byte("user:" + tailFake(seed, 20, tailAlnum)))
}

// TestRedactorTakesBracketsInAnUnquotedPrintedList: fmt prints a header with the plain verb as map[Cookie:[a=b c=d]], and
// a closing or opening bracket is legal in a cookie value and in a PHP-style name (user[id]=v). The span ended at the first
// closing bracket, so the tail of the value, or the whole cookie, stayed in the text with the residual gate clean.
func TestRedactorTakesBracketsInAnUnquotedPrintedList(t *testing.T) {
	a, b := tailFake(7101, 22, tailAlnum), tailFake(7102, 32, tailAlnum)
	r4Run(t, []r4Case{
		{label: "tail behind a bracket", text: "map[" + spanCk + ":[sid=" + a + "]" + b + "; theme=dark]]", values: []string{a, b}, suffix: "]]"},
		{label: "bracket after two characters", text: "map[" + spanCk + ":[sid=ab]" + b + "]]", values: []string{b}, suffix: "]]"},
		{label: "subscript name", text: "map[" + spanCk + ":[user[id]=" + a + "]]", values: []string{a}, suffix: "]]"},
		{label: "subscript name, second cookie", text: "map[" + spanSetCk + ":[a=" + a + " user[id]=" + b + "]]", values: []string{a, b}, suffix: "]]"},
		{label: "value that starts with a bracket", text: "map[" + spanCk + ":[lang=en sid=]" + a + " theme=dark]]", values: []string{a}, suffix: "]]"},
		{label: "fmt of a header", text: fmt.Sprintf("%v", http.Header{spanCk: {"sid=" + a + "]" + b + "; theme=dark"}}), values: []string{a, b}, suffix: "]]"},
		{label: "fmt of a header, name with a subscript", text: fmt.Sprintf("%v", http.Header{spanCk: {"user[id]=" + a}}), values: []string{a}, suffix: "]]"},
		{label: "struct print", text: "{GET https://host.invalid/x map[" + spanCk + ":[lang=en; sid=" + a + "]" + b + "; theme=dark]] 200 OK}", values: []string{a, b}, keep: " 200 OK}"},
		{label: "slog line", text: `msg=req headers="map[` + spanCk + ":[sid=" + a + "]" + b + `; theme=dark]]" status=200`, values: []string{a, b}, keep: `" status=200`},
	})
}

// TestRedactedArgsStayJSONBehindAnApostropheInAPrintedCookie: an apostrophe in a cookie value inside a printed list
// opened a single-quoted tail that ran to the end of the line, through the closers of the Args around it.
func TestRedactedArgsStayJSONBehindAnApostropheInAPrintedCookie(t *testing.T) {
	a := tailFake(7111, 22, tailAlnum)
	r4Run(t, []r4Case{
		{label: "apostrophe in a Go print", text: "map[" + spanCk + ":[sid=" + a + "'x]] and more", values: []string{a}, keep: " and more"},
		{label: "apostrophe, then a second cookie", text: "map[" + spanCk + ":[sid=" + a + "'x; theme=dark]] and more", values: []string{a}, keep: " and more"},
		{label: "fmt of a header", text: fmt.Sprintf("%v", http.Header{spanCk: {"sid=" + a + "'x"}}) + " done", values: []string{a}, keep: " done"},
	})
	for _, c := range []struct{ text, suffix string }{
		{`{"cmd":"map[` + spanCk + `:[sid=` + a + `'x]]"}`, `]]"}`},
		{`{"cmd":"map[` + spanCk + `:[sid=` + a + `'x]]","timeout":30}`, `]]","timeout":30}`},
	} {
		out := reviewRedact(t, "apostrophe in Args "+c.text[:12], c.text, a)
		if !json.Valid([]byte(out)) || !strings.HasSuffix(out, c.suffix) {
			t.Errorf("Args with an apostrophe in a printed cookie: the closers are gone or the JSON is invalid: %q", out)
		}
	}
}

// TestRedactorTakesAGoPrintOfDecodedJSON: json.Unmarshal into an interface gives map[string]interface {} and
// []interface {}, and the Go-syntax print of those writes the list as []interface {}{"..."}, not []string{"..."}.
func TestRedactorTakesAGoPrintOfDecodedJSON(t *testing.T) {
	a, b := tailFake(7121, 22, tailAlnum), tailFake(7122, 24, tailAlnum)
	decoded := func(doc string) string {
		var v any
		if err := json.Unmarshal([]byte(doc), &v); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%#v", v)
	}
	r4Run(t, []r4Case{
		{label: "any map", text: fmt.Sprintf("%#v", map[string]any{spanCk: []any{"sid=" + a}}), values: []string{a}},
		{label: "decoded Cookie", text: decoded(`{"` + spanCk + `":["sid=` + a + `"]}`), values: []string{a}},
		{label: "decoded Set-Cookie with attributes", text: decoded(`{"` + spanSetCk + `":["sid=` + a + `; Path=/"]}`), values: []string{a}},
		{label: "decoded, two elements", text: decoded(`{"` + spanSetCk + `":["sid=` + a + `; Path=/","other=` + b + `"]}`), values: []string{a, b}},
		{label: "decoded, nested", text: decoded(`{"headers":{"` + spanSetCk + `":["sid=` + a + `; Path=/"]},"status":200}`), values: []string{a}},
		{label: "source, no space", text: `map[string]interface{}{"` + spanCk + `":[]interface{}{"sid=` + a + `"}}`, values: []string{a}},
		{label: "source, any", text: `map[string]any{"` + spanCk + `": []any{"sid=` + a + `"}}`, values: []string{a}},
		{label: "source, any, short ahead", text: `map[string]any{"` + spanCk + `": []any{"theme=dark"}, "` + spanSetCk + `": []any{"sid=` + a + `"}}`, values: []string{a}},
	})
}

// TestRedactorTakesPythonBytesLiterals: ASGI scope headers, aiohttp raw headers and dicts of bytes write the name and the value
// as b'...' literals, and a b in front of the quote is neither the quote that follows a comma nor a gap.
func TestRedactorTakesPythonBytesLiterals(t *testing.T) {
	a, b := tailFake(7131, 22, tailAlnum), tailFake(7132, 24, tailAlnum)
	r4Run(t, []r4Case{
		{label: "tuple list", text: "[(b'cookie', b'sid=" + a + "')]", values: []string{a}},
		{label: "dict", text: "{b'" + spanCk + "': b'sid=" + a + "'}", values: []string{a}},
		{label: "asgi scope", text: "{'type': 'http', 'headers': [(b'host', b'x.example'), (b'cookie', b'sessionid=" + a + "; csrf=" + b + "')]}", values: []string{a, b}},
		{label: "set-cookie tuple", text: "[(b'" + spanSetCk + "', b'sid=" + a + "; Path=/')]", values: []string{a}},
		{label: "double quotes, tuple", text: `[(b"cookie", b"sid=` + a + `")]`, values: []string{a}},
		{label: "double quotes, dict", text: `{b"` + spanCk + `": b"sid=` + a + `"}`, values: []string{a}},
		{label: "mixed dict", text: "{b'" + spanCk + "': [b'lang=en; credentials=" + b + "'], b'" + spanSetCk + "': b'session=" + a + "'}", values: []string{a, b}},
		{label: "aiohttp raw headers", text: "((b'" + spanSetCk + "', b'sid=" + a + "; Path=/'),)", values: []string{a}},
	})
}

// TestRedactorTakesAnUnderscoreBeforeTheCookieName: HTTP_COOKIE is the name CGI, WSGI environ dumps and web servers print,
// and an underscore in front of the word is no word boundary.
func TestRedactorTakesAnUnderscoreBeforeTheCookieName(t *testing.T) {
	a, b := tailFake(7141, 22, tailAlnum), tailFake(7142, 20, tailAlnum)
	up, low := strings.ToUpper(spanCk), strings.ToLower(spanCk)
	r4Run(t, []r4Case{
		{label: "environ line", text: "HTTP_" + up + "=sessionid=" + a + "; csrf=" + b, values: []string{a, b}},
		{label: "wsgi dict", text: "{'HTTP_HOST': 'x.example', 'HTTP_" + up + "': 'sessionid=" + a + "; csrf=" + b + "'}", values: []string{a, b}},
		{label: "nginx variable, quoted", text: "http_" + low + `="sessionid=` + a + `"`, values: []string{a}},
		{label: "export", text: "export HTTP_" + up + "=sessionid=" + a, values: []string{a}},
		{label: "set-cookie variable", text: "HTTP_SET_" + up + "=sid=" + a + "; Path=/", values: []string{a}},
	})
}

// TestRedactorCountsAValueThatStartsWithAnAmpersandOrABracket: random generators emit a leading ampersand, and a closing
// bracket is legal in a cookie value; neither was counted, so a header whose only long value started with one stayed whole.
func TestRedactorCountsAValueThatStartsWithAnAmpersandOrABracket(t *testing.T) {
	a := tailFake(7151, 22, tailAlnum)
	kwPass, kwKey := "DB_"+strings.ToUpper(spanKwPass), "API_"+"KEY"
	r4Run(t, []r4Case{
		{label: "cookie, ampersand in the middle of a header", text: spanCk + ": lang=en; sid=&" + a + "; theme=dark", values: []string{a}},
		{label: "cookie, key word name", text: spanCk + ": refresh_" + spanKwToken + "=&" + a, values: []string{a}},
		{label: "set-cookie, ampersand", text: spanSetCk + ": sid=&" + a + "; Path=/; HttpOnly", values: []string{a}},
		{label: "assignment, ampersand", text: kwPass + "=&" + a, values: []string{a}},
		{label: "assignment in Args, ampersand", text: `{"cmd":"export ` + kwKey + `=&` + a + `"}`, values: []string{a}, valid: true},
		{label: "cookie, bracket", text: spanCk + ": lang=en; sid=]" + a + "; theme=dark", values: []string{a}},
		{label: "set-cookie, bracket", text: spanSetCk + ": sid=]" + a + "; Path=/; HttpOnly", values: []string{a}},
		{label: "printed list, bracket", text: "map[" + spanCk + ":[sid=]" + a + "]]", values: []string{a}, suffix: "]]"},
	})
}

// TestRedactorTakesCredentialHeadersPrintedAsLists: a header other than Authorization that carries a credential prints as
// name:[value] in Go's print of http.Header and as a list in JSON, Python and YAML flow style, and a value cannot start at
// the opening bracket.
func TestRedactorTakesCredentialHeadersPrintedAsLists(t *testing.T) {
	a, b := tailFake(7161, 22, tailAlnum), tailFake(7162, 22, tailAlnum)
	x := "X-Api-" + "Key"
	r4Run(t, []r4Case{
		{label: "api key, Go print", text: "map[" + x + ":[" + a + "]]", values: []string{a}},
		{label: "api key, other header ahead", text: "map[Content-Type:[a] " + x + ":[" + a + "]]", values: []string{a}},
		{label: "auth token, Go print", text: "map[Content-Type:[a] X-Auth-" + "Token:[" + a + "]]", values: []string{a}},
		{label: "security token, Go print", text: "map[X-Amz-Security-" + "Token:[" + a + "] Host:[x.example]]", values: []string{a}},
		{label: "api key without the prefix", text: "map[Api-" + "Key:[" + a + "]]", values: []string{a}},
		{label: "fmt of a header", text: fmt.Sprintf("%v", http.Header{x: {a}, "X-Auth-" + "Token": {b}}), values: []string{a, b}},
		{label: "json", text: `{"` + x + `":["` + a + `"]}`, values: []string{a}},
		{label: "python", text: `{'` + x + `': ['` + a + `']}`, values: []string{a}},
		{label: "flow list with a space", text: x + ": [" + a + "]", values: []string{a}},
	})
}

// TestRedactorTakesBasicAcrossABreakOrAListPosition: json.MarshalIndent of a header, jq output and a YAML list put a line
// break and indentation between the name and the scheme, and a header with two values puts the Basic one second.
func TestRedactorTakesBasicAcrossABreakOrAListPosition(t *testing.T) {
	b64, bearer := r4Basic(7171), tailFake(7172, 26, tailAlnum)
	indent := func(prefix string) string {
		out, err := json.MarshalIndent(map[string][]string{"Authorization": {"Basic " + b64}}, "", prefix)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	r4Run(t, []r4Case{
		{label: "MarshalIndent, spaces", text: indent("  "), values: []string{b64}},
		{label: "MarshalIndent, tabs (jq)", text: indent("\t"), values: []string{b64}},
		{label: "yaml list", text: "Authorization:\n  - Basic " + b64 + "\n", values: []string{b64}},
		{label: "yaml list, no indent", text: "Authorization:\n- Basic " + b64 + "\n", values: []string{b64}},
		{label: "break after the colon", text: "{\"Authorization\":\n \"Basic " + b64 + "\"}", values: []string{b64}},
		{label: "json, second value", text: `{"Authorization":["Bearer ` + bearer + `","Basic ` + b64 + `"]}`, values: []string{b64, bearer}},
		{label: "Go print, second value", text: fmt.Sprintf("%v", http.Header{"Authorization": {"Bearer " + bearer, "Basic " + b64}}), values: []string{b64, bearer}},
	})
}

// TestRedactedJSONKeyedByCookieStaysValid: headers as a JSON object are the usual JSON shape of a header map. The match
// started at the header name inside the key's quotes and ran to the end of the value, so the key lost its closing quote
// and the document its value.
func TestRedactedJSONKeyedByCookieStaysValid(t *testing.T) {
	a, b := tailFake(7181, 22, tailAlnum), tailFake(7182, 24, tailAlnum)
	r4Run(t, []r4Case{
		{label: "object, string value", text: `{"` + spanCk + `":"sid=` + a + `"}`, values: []string{a}, valid: true},
		{label: "object, list value", text: `{"` + spanCk + `":["sid=` + a + `"]}`, values: []string{a}, valid: true},
		{label: "nested, two elements", text: `{"headers":{"` + spanSetCk + `":["sid=` + a + `; Path=/","b=` + b + `"]}}`, values: []string{a, b}, valid: true},
		{label: "object, other keys around it", text: `{"status":200,"` + spanCk + `":"sid=` + a + `","host":"x.example"}`, values: []string{a}, valid: true},
		{label: "marshalled header", text: reviewJSON(t, http.Header{spanSetCk: {"sid=" + a + "; Path=/", "b=" + b}}), values: []string{a, b}, valid: true},
		{label: "pretty-printed", text: func() string {
			out, _ := json.MarshalIndent(map[string][]string{spanSetCk: {"sid=" + a + "; Path=/", "b=" + b}}, "", "  ")
			return string(out)
		}(), values: []string{a, b}, valid: true},
	})
}
