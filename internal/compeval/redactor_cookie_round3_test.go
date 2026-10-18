package compeval

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The third review of the whole-span cookie class found that running the credential classes before the cookie class
// had traded one leak for others: a placeholder left by an earlier class is text the cookie span has to cross, and the
// quote an assignment match leaves behind is not the quote that closes the string the header sits in. Every test here
// puts a value of 12 or more characters behind such a placeholder or quote and asserts it is gone, the residual gate is
// clean, the Args are still JSON, and the harvest keeps the trace.

// round3Redact redacts text and, when it is the Args of a tool call, asserts the output is still JSON.
func round3Redact(t *testing.T, label, text string, envelope bool, values ...string) string {
	t.Helper()
	out := reviewRedact(t, label, text, values...)
	if envelope && !json.Valid([]byte(out)) {
		t.Errorf("%s: the redacted Args are not valid JSON: %q", label, out)
	}
	return out
}

// TestRedactorTakesACookieBehindAQuotedKeyWordCookie: a cookie named like a credential with a double-quoted value is
// taken by the assignment class, which leaves the closing quote behind its placeholder, and the cookie span then
// stopped at that stray quote and left every cookie after it in the clear.
func TestRedactorTakesACookieBehindAQuotedKeyWordCookie(t *testing.T) {
	v1, v2, v3 := tailFake(6001, 28, tailAlnum), tailFake(6002, 28, tailAlnum), tailFake(6003, 20, tailAlnum)
	for _, kw := range []string{spanKwToken, "refresh_" + spanKwToken, "auth_" + spanKwToken, spanKwPass, spanKwSecr, "api_key", "csrf_" + spanKwToken} {
		for _, body := range []string{
			kw + `="` + v1 + `"; sid=` + v2,
			"sid=" + v3 + "; " + kw + `="` + v1 + `"; csrf=` + v2,
			kw + `="` + v1 + `"; sid=` + v2 + "; Path=/; HttpOnly",
		} {
			for _, name := range []string{spanCk, spanSetCk} {
				for _, how := range []string{"plain", "args", "curl", "args of curl", "fold"} {
					text, envelope := spanWrap(t, how, name, body, nil)
					round3Redact(t, fmt.Sprintf("%s / %s / %s", name, how, body[:12]), text, envelope, v1, v2, v3)
				}
			}
			reviewHarvest(t, "harvest / "+body[:12], spanCk+": "+body, v1, v2, v3)
		}
	}
}

// TestRedactorTakesAPrintedListThatHoldsAnotherClassValue: a JWT, a Bearer value or a key-word cookie inside an unquoted
// printed list (a header printed with %v, a Java map) leaves a placeholder that holds a closing bracket, and the span
// of the list ended there.
func TestRedactorTakesAPrintedListThatHoldsAnotherClassValue(t *testing.T) {
	a, b, c := tailFake(6011, 26, tailAlnum), tailFake(6012, 26, tailAlnum), tailFake(6013, 26, tailAlnum)
	jwt := tailJWT()
	bearer := "Bearer " + tailFake(6014, 24, tailAlnum)
	kwElems := []string{"sessionid=" + a + "; Path=/", "csrf_" + spanKwToken + "=" + b + "; Path=/", "other=" + c + "; HttpOnly"}
	jwtElems := []string{"a=" + a + "; Path=/", "sid=" + jwt + "; Path=/", "other=" + c + "; HttpOnly"}
	for _, h := range []struct {
		name string
		text string
		vals []string
	}{
		{"%v, key word in the middle", fmt.Sprintf("%v", http.Header{spanSetCk: kwElems}), []string{a, b, c}},
		{"%+v, key word in the middle", fmt.Sprintf("%+v", http.Header{spanSetCk: kwElems}), []string{a, b, c}},
		{"%v, jwt in the middle", fmt.Sprintf("%v", http.Header{spanSetCk: jwtElems}), []string{a, jwt, c}},
		{"%v, Cookie with a jwt then a value", "map[" + spanCk + ":[a=" + jwt + "; b=" + c + "]]", []string{jwt, c}},
		{"%v, Cookie with a bearer then a value", "map[" + spanCk + ":[auth=" + bearer + "; b=" + c + "]]", []string{bearer[7:], c}},
		{"%v, Cookie with a key word then values", "map[" + spanCk + ":[sid=" + a + " auth_" + spanKwToken + "=" + b + " csrf=" + c + "]]", []string{a, b, c}},
		{"Sprint of a slice", spanSetCk + ": " + fmt.Sprint(kwElems), []string{a, b, c}},
		{"Java map", "{" + spanSetCk + "=[" + strings.Join(kwElems, ", ") + "]}", []string{a, b, c}},
		{"Java map, jwt", "{" + spanSetCk + "=[" + strings.Join(jwtElems, ", ") + "]}", []string{a, jwt, c}},
	} {
		round3Redact(t, h.name, h.text, false, h.vals...)
		round3Redact(t, h.name+", in Args", reviewJSON(t, map[string]string{"output": h.text}), true, h.vals...)
		reviewHarvest(t, h.name, h.text, h.vals...)
	}
}

// TestRedactedArgsStayJSONForAHeaderWithTwoKeys: a header printed with the Go-syntax verb holds its keys in sorted
// order, so a Cookie with a short value comes before a Set-Cookie with a long one. The match ran from the first key to the
// end of the text, past the quote that closes the Args string.
func TestRedactedArgsStayJSONForAHeaderWithTwoKeys(t *testing.T) {
	v, w := tailFake(6021, 33, tailAlnum), tailFake(6022, 33, tailAlnum)
	short := []string{"theme=dark"}
	long := []string{"sid=" + v + "; Path=/"}
	for _, h := range []struct {
		name string
		text string
	}{
		{"http.Header", fmt.Sprintf("%#v", http.Header{spanCk: short, spanSetCk: long})},
		{"map", fmt.Sprintf("%#v", map[string][]string{spanCk: short, spanSetCk: long})},
		{"Go code", "h := http.Header{\"" + spanCk + "\": []string{\"theme=dark\"}, \"" + spanSetCk + "\": []string{\"sid=" + v + "; Path=/\"}}\n_ = h\n"},
		{"JSON object of two lists", `{"` + spanCk + `":["theme=dark"],"` + spanSetCk + `":["sid=` + v + `; Path=/"]}`},
		{"JSON object, Set-Cookie first", `{"` + spanSetCk + `":["sid=` + v + `; Path=/"],"` + spanCk + `":["theme=dark"]}`},
		{"Python dict of two lists", `{'` + spanCk + `': ['theme=dark'], '` + spanSetCk + `': ['sid=` + v + `; Path=/']}`},
		{"Python dict, quoted value", `{'` + spanCk + `': ['theme=dark'], '` + spanSetCk + `': ['sid="` + w + `"; Path=/']}`},
		{"Python list, quoted value", `{'` + spanSetCk + `': ['a="` + w + `"']}`},
	} {
		round3Redact(t, h.name, h.text, false, v, w)
		round3Redact(t, h.name+", in content", reviewJSON(t, map[string]string{"content": h.text, "path": "x_test.go"}), true, v, w)
		round3Redact(t, h.name+", in cmd", reviewJSON(t, map[string]string{"cmd": h.text}), true, v, w)
		reviewHarvest(t, h.name, h.text, v, w)
	}
}

// TestRedactedArgsStayJSONBehindAnEmptyQuotedCookie: an empty quoted cookie ahead of a key-word cookie in a printed list,
// inside Args, and the span swallowed the quote that closes the string.
func TestRedactedArgsStayJSONBehindAnEmptyQuotedCookie(t *testing.T) {
	v := tailFake(6031, 20, tailAlnum)
	dotted := tailFake(6032, 12, tailAlnum) + "." + tailFake(6033, 8, tailAlnum) + "." + tailFake(6034, 8, tailAlnum)
	for _, first := range []string{`p=""`, `p="ab"`, "p=ok"} {
		text := "map[" + spanCk + ":[" + first + "; auth_" + spanKwToken + "=" + v + "; lang=" + dotted + "]]"
		round3Redact(t, "list: "+first, reviewJSON(t, map[string]string{"cmd": text}), true, v, dotted)
	}
}

// TestRedactorTakesABareValueWithAnEscapeInItsFirstCharacters: a shell escape, a JSON slash escape or a backslash path
// inside the first 12 characters of the only long value stopped the count that starts a match, and the value was left
// whole with the residual gate clean. A capital N or R after a backslash (a Windows path) is no escape of a line break.
func TestRedactorTakesABareValueWithAnEscapeInItsFirstCharacters(t *testing.T) {
	v := tailFake(6041, 33, tailAlnum)
	bs := "\x5c"
	for _, esc := range []string{bs + "$", bs + "/", bs + "`", bs + ";", bs + " ", bs + bs} {
		for _, at := range []int{0, 2, 5, 11} {
			val := v[:at] + esc + v[at:]
			for name, text := range map[string]string{
				"line":  spanCk + ": sid=" + val,
				"curl":  `curl -H "` + spanCk + ": sid=" + spanShell(val) + `" https://host.invalid/p`,
				"args":  reviewJSON(t, map[string]string{"cmd": spanCk + ": sid=" + val, "path": "/x"}),
				"args2": reviewJSON(t, map[string]string{"cmd": `curl -H "` + spanCk + ": sid=" + spanShell(val) + `" https://host.invalid/p`}),
			} {
				round3Redact(t, fmt.Sprintf("%s / escape %q at %d", name, esc, at), text, strings.HasPrefix(name, "args"), v)
			}
		}
	}
	for _, path := range []string{`C:\Name`, `C:\Data\Rick`, `C:\Rick`} {
		text := spanCk + ": dir=" + path + "; sid=" + v
		round3Redact(t, "capital path "+path, text, false, v)
		round3Redact(t, "capital path "+path+", in Args", reviewJSON(t, map[string]string{"cmd": text}), true, v)
	}
}

// TestRedactorTakesACookieNamedByAnAngleBracketOrAnArrow: the greater-than sign right behind the header name was live (an
// XML element, an arrow), and the refix deleted it as dead code.
func TestRedactorTakesACookieNamedByAnAngleBracketOrAnArrow(t *testing.T) {
	v := tailFake(6051, 36, tailAlnum)
	for name, text := range map[string]string{
		"xml element":     "<" + spanCk + ">sid=" + v + "</" + spanCk + ">",
		"xml set element": "<" + spanSetCk + ">sid=" + v + "; Path=/</" + spanSetCk + ">",
		"bare":            spanCk + ">sid=" + v,
		"arrow":           spanCk + " -> sid=" + v,
		"hash rocket":     spanCk + " => sid=" + v,
	} {
		round3Redact(t, name, text, false, v)
	}
}
