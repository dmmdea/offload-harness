package compeval

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The first review of the whole-span cookie class found one ordering defect and several edges, and every one of them
// let a value of 12 or more characters through the redactor with the residual gate clean. These tests drive the
// edges through the redactor and the harvest, which is where the corpus is written, and assert the values are gone
// and not only that the text is valid.

// reviewForms are the ways a planted value stands in the text: as it is, JSON-escaped, shell-escaped and both.
func reviewForms(v string) []string {
	return []string{v, spanEnc(v), spanShell(v), spanEnc(spanShell(v))}
}

// reviewLeak reports the first run of 8 characters of any planted value that is still in out.
func reviewLeak(out string, values ...string) (string, bool) {
	for _, v := range values {
		for _, form := range reviewForms(v) {
			if run, ok := anyRun(form, out, 8); ok {
				return run, true
			}
		}
	}
	return "", false
}

// reviewRedact redacts the text, and asserts that no run of 8 characters of any planted value is left and that the
// residual gate has nothing to say. It returns the redacted text.
func reviewRedact(t *testing.T, label, text string, values ...string) string {
	t.Helper()
	r := NewRedactor()
	out := r.Redact(text)
	if run, ok := reviewLeak(out, values...); ok {
		t.Errorf("%s: %q of a value is still in %q (residual %v)", label, run, out, r.Residual(out))
	}
	if res := r.Residual(out); len(res) != 0 {
		t.Errorf("%s: residual gate reports %v on %q", label, res, out)
	}
	return out
}

// reviewHarvest writes a trace whose tool output holds the text and whose tool call's Args hold it as a JSON
// string, runs the harvest, and asserts that the trace is kept and that no run of 8 characters of any planted
// value is in what would be written.
func reviewHarvest(t *testing.T, label, text string, values ...string) {
	t.Helper()
	args, err := json.Marshal(map[string]string{"cmd": text})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rec := sampleTrace("rv", text)
	rec.Transcript[2].ToolCalls[0].Args = string(args)
	writeTraceFile(t, dir, "rv.json", rec)
	entries, stats, err := HarvestTraces(dir, HarvestOpts{})
	if err != nil {
		t.Errorf("%s: the harvest refused: %v", label, err)
		return
	}
	if len(entries) != 1 {
		t.Errorf("%s: want the trace kept, got %d entries and skips %+v", label, len(entries), stats.Skipped)
		return
	}
	var all []string
	for _, turn := range entries[0].Turns {
		all = append(all, turn.Content)
		for _, c := range turn.ToolCalls {
			all = append(all, c.Args)
		}
	}
	joined := strings.Join(all, "\n")
	if run, ok := reviewLeak(joined, values...); ok {
		t.Errorf("%s: %q of a value is in the harvested entry: %q", label, run, joined)
	}
}

// reviewCookies are cookie lines with the session value w on them, as a header, a Set-Cookie header, with a
// pair or a quoted message before or after it.
func reviewCookies(w string) []string {
	return []string{
		spanCk + ": sid=" + w,
		spanCk + ":sid=" + w,
		spanSetCk + ": sid=" + w + "; Path=/; HttpOnly",
		spanCk + ": a=b; sid=" + w,
		spanCk + ": sid=" + w + "; theme=dark",
		spanCk + ": sid=" + w + "; lang=en; a=b",
		spanCk + `: m="Saved ok"; sid=` + w,
		spanCk + ": sid=" + w + `; messages="Rock&Roll"`,
	}
}

// reviewCredential is a credential with a quoted value: its text and the values to look for.
type reviewCredential struct {
	text   string
	values []string
}

// reviewCredentials are credentials whose value is quoted and whose key word stands in front of the quote, with
// a space, a colon or an equals sign between them, built from pieces so no literal is credential-shaped.
func reviewCredentials(v, a, b string) []reviewCredential {
	return []reviewCredential{
		{spanKwToken + " = '" + v + "'", []string{v}},
		{spanKwToken + ": '" + v + "'", []string{v}},
		{spanKwToken + "='" + v + "'", []string{v}},
		{spanKwToken + `: "` + v + `"`, []string{v}},
		{spanKwToken + ` = "` + v + `"`, []string{v}},
		{spanKwPass + ": '" + a + " " + b + "'", []string{a, b}},
		{spanKwPass + "='" + a + " " + b + "'", []string{a, b}},
		{"client_" + spanKwSecr + `: "` + v + `"`, []string{v}},
		{`Authorization: "Basic ` + v + `"`, []string{v}},
		{`Authorization:'Basic ` + v + `'`, []string{v}},
		{`Authorization:"Bearer ` + v + `"`, []string{v}},
		{"X-Api-" + "Key:'" + v + "'", []string{v}},
	}
}

// reviewLayouts put a cookie line c and a credential s on one line, or on two.
var reviewLayouts = []struct {
	name string
	f    func(c, s string) string
}{
	{"cookie then credential", func(c, s string) string { return c + " " + s }},
	{"cookie, semicolon", func(c, s string) string { return c + "; " + s }},
	{"cookie, and-and", func(c, s string) string { return c + " && " + s }},
	{"braces", func(c, s string) string { return "{" + c + ", " + s + "}" }},
	{"sentence", func(c, s string) string { return "req " + c + " " + s + " done" }},
	{"credential then cookie", func(c, s string) string { return s + " " + c }},
	{"two lines", func(c, s string) string { return c + "\n" + s }},
	{"http command", func(c, s string) string { return "http POST https://host.invalid/x " + c + " " + s }},
	{"comma", func(c, s string) string { return c + ", " + s }},
	{"credential, semicolon, cookie", func(c, s string) string { return s + "; " + c }},
	{"hash rocket", func(c, s string) string { return c + " => " + s }},
	{"log line", func(c, s string) string { return "INFO headers " + c + " " + s }},
}

// TestRedactorTakesAQuotedCredentialAfterACookie: the cookie span ends in front of a quote, and with the cookie class
// run first it swallows the key word of a quoted credential on the same line, so the quoted value is left
// with no key word in front of it, for the other classes and for the value-tail gate alike, and goes out with the
// residual gate clean unless the credential values the classes find in the text as it was written are swept
// (sweepCredentials). Every credential with a quoted value, after an unquoted cookie on the same line, in every
// layout, as text and as the string of a tool call's Args, loses its value.
func TestRedactorTakesAQuotedCredentialAfterACookie(t *testing.T) {
	w, v := tailFake(5001, 32, tailAlnum), tailFake(5002, 28, tailAlnum)
	a, b := tailFake(5003, 14, tailAlnum), tailFake(5004, 14, tailAlnum)
	cases := 0
	for _, c := range reviewCookies(w) {
		for _, s := range reviewCredentials(v, a, b) {
			for _, l := range reviewLayouts {
				line := l.f(c, s.text)
				values := append([]string{w}, s.values...)
				for _, asArgs := range []bool{false, true} {
					text := line
					if asArgs {
						arg, _ := json.Marshal(map[string]string{"cmd": line})
						text = string(arg)
					}
					cases++
					out := reviewRedact(t, fmt.Sprintf("%s / %s (args %v)", l.name, s.text, asArgs), text, values...)
					if asArgs && !json.Valid([]byte(out)) {
						t.Errorf("%s (args): the redacted Args are not valid JSON: %q", l.name, out)
					}
				}
			}
		}
	}
	t.Logf("%d cases", cases)
}

// TestHarvestKeepsNoCredentialAfterACookie: through the harvest, a trace whose tool output and tool-call Args hold a
// cookie line followed on the same line by a quoted credential is kept, and the credential's value is not in what
// is written.
func TestHarvestKeepsNoCredentialAfterACookie(t *testing.T) {
	w, v := tailFake(5011, 32, tailAlnum), tailFake(5012, 28, tailAlnum)
	for _, line := range []string{
		"req " + spanCk + ": sid=" + w + " " + spanKwToken + " = '" + v + "' done",
		"http POST https://host.invalid/x " + spanCk + ":sid=" + w + ` Authorization:"Basic ` + v + `"`,
		"{" + spanCk + ": sid=" + w + ", " + spanKwPass + `: "` + v + `"}`,
		spanSetCk + ": sid=" + w + "; Path=/; " + spanKwPass + "='" + v + "'",
	} {
		reviewHarvest(t, line[:12], line, w, v)
	}
}

// TestRedactorTakesACookieWhoseNameHoldsAKeyWord: a cookie named like a credential (refresh_token, auth_token,
// password, secret) is taken, both values are gone, the residual gate is clean and a cookie-header placeholder
// stands where the line was. Which class counts the substitution is not the point: the cookie class runs first and
// takes the line whole, and the credential values found in the text as it was written are swept after it.
func TestRedactorTakesACookieWhoseNameHoldsAKeyWordWhicheverClassCountsIt(t *testing.T) {
	v, w := tailFake(911, 32, tailAlnum), tailFake(912, 32, tailAlnum)
	for _, names := range [][2]string{
		{"refresh_" + spanKwToken, "sessionid"},
		{"auth_" + spanKwToken, "sessionid"},
		{spanKwPass, "sessionid"},
		{spanKwSecr, "csrftoken"},
		{"sessionid", "refresh_" + spanKwToken},
		{"api_key", "sessionid"},
	} {
		line := spanCk + ": " + names[0] + "=" + v + "; " + names[1] + "=" + w
		for _, asArgs := range []bool{false, true} {
			in := line
			if asArgs {
				in = writeFileArgs(t, "one\n"+line+"\nthree\n")
			}
			out := reviewRedact(t, fmt.Sprint(names, asArgs), in, v, w)
			if !strings.Contains(out, "[redacted-") {
				t.Errorf("%v (args %v): no substitution in %q", names, asArgs, out)
			}
		}
	}
}

// reviewJSON is v marshalled as the text of a JSON document.
func reviewJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRedactorTakesACookieThatEndsInAnEqualsSign: a last cookie that ends in an equals sign (base64 padding, or no value
// at all) stands in front of the quote that closes the string the header sits in, and that quote was read as the
// opening quote of a value. The span then ran through the next field or argument, which broke the JSON of Args, and
// when the next field was a second header's name its value was left outside every match.
func TestRedactorTakesACookieThatEndsInAnEqualsSign(t *testing.T) {
	v1, v2 := tailFake(5021, 28, tailAlnum), tailFake(5022, 28, tailAlnum)
	for _, last := range []string{"p=QUJD==", "flag=", "tok=QUJD="} {
		args := reviewJSON(t, map[string]string{"content": "GET / HTTP/1.1\n" + spanCk + ": sid=" + v1 + "; " + last, "path": "/x"})
		out := reviewRedact(t, "args, last cookie "+last, args, v1)
		if !json.Valid([]byte(out)) || !strings.Contains(out, `"path":"/x"`) {
			t.Errorf("args, last cookie %s: the field after the header was taken or the JSON broke: %q", last, out)
		}
		curl := `curl -H "` + spanCk + ": sid=" + v1 + "; " + last + `" -d "a=b" -H "X-Next: y" https://host.invalid/p`
		out = reviewRedact(t, "curl, last cookie "+last, curl, v1)
		if !strings.Contains(out, `-d "a=b" -H "X-Next: y"`) {
			t.Errorf("curl, last cookie %s: the arguments after the header were taken: %q", last, out)
		}
		doc := `{"` + spanCk + `":"sid=` + v1 + `; ` + last + `","` + spanSetCk + `":"sid=` + v2 + `; Path=/"}`
		reviewRedact(t, "two headers in a document, last cookie "+last, doc, v1, v2)
		reviewRedact(t, "two headers in Args, last cookie "+last, reviewJSON(t, map[string]string{"content": doc}), v1, v2)
		reviewHarvest(t, "two headers, last cookie "+last, doc, v1, v2)
	}
}

// TestRedactorTakesAPrintedGoHeader: a header printed with the Go-syntax verb is a slice literal, and the span has to
// run through every element of it, and not stop at the quote that closes the first one.
func TestRedactorTakesAPrintedGoHeader(t *testing.T) {
	a, b, c := tailFake(5031, 20, tailAlnum), tailFake(5032, 24, tailAlnum), tailFake(5033, 18, tailAlnum)
	for _, h := range []struct {
		name  string
		elems []string
	}{
		{"two long elements", []string{"csrf=" + a + "; Path=/", "sess=" + b + "; HttpOnly"}},
		{"three long elements", []string{"a=" + a, "b=" + b, "c=" + c}},
		{"short first element", []string{"theme=dark", "sess=" + b + "; HttpOnly"}},
	} {
		for _, typed := range []string{"http.Header", "map"} {
			var t0 string
			if typed == "map" {
				t0 = fmt.Sprintf("%#v", map[string][]string{spanSetCk: h.elems})
			} else {
				t0 = fmt.Sprintf("%#v", http.Header{spanSetCk: h.elems})
			}
			label := h.name + ", " + typed
			reviewRedact(t, label, t0, a, b, c)
			reviewRedact(t, label+", in Args", reviewJSON(t, map[string]string{"output": t0}), a, b, c)
			reviewRedact(t, label+", in a test message", "    x_test.go:42: headers = "+t0+", want nothing", a, b, c)
			reviewHarvest(t, label, t0, a, b, c)
		}
	}
}

// reviewLayered is the text of a JSON document of headers, put through a double-quoted shell string and then into the
// Args of a tool call: the quotes of a cookie value then carry seven backslashes.
func reviewLayered(t *testing.T, body string) string {
	t.Helper()
	inner := reviewJSON(t, map[string]map[string]string{"headers": {spanCk: body}})
	return reviewJSON(t, map[string]string{"command": `curl -d "` + spanShell(inner) + `" https://host.invalid/p`})
}

// TestRedactorTakesACookieBehindThreeLayersOfEscaping: JSON in a shell string in JSON puts seven backslashes in
// front of the quotes of a quoted cookie value, and the class stopped at three.
func TestRedactorTakesACookieBehindThreeLayersOfEscaping(t *testing.T) {
	v, w := tailFake(5041, 32, tailAlnum), tailFake(5042, 32, tailAlnum)
	a, b := tailFake(5043, 14, tailAlnum), tailFake(5044, 14, tailAlnum)
	for _, body := range []string{
		`m="` + a + ` ` + b + `"; sid=` + v,
		"sid=" + v + `; m="` + a + ` ` + b + `"; z=` + w,
		`m="` + a + `$HOME` + b + `"; sid=` + v,
	} {
		text := reviewLayered(t, body)
		out := reviewRedact(t, "three layers: "+body[:6], text, a, b, v, w)
		if !json.Valid([]byte(out)) {
			t.Errorf("three layers: the redacted Args are not valid JSON: %q", out)
		}
		reviewHarvest(t, "three layers: "+body[:6], text, a, b, v, w)
	}
}

// TestRedactorTakesAQuotedValueWithALineBreak: a line break inside a quoted cookie value that is still open (a wrapped or
// folded header) is not the end of the header; the value goes to its closing quote, and the cookies behind it go with it.
func TestRedactorTakesAQuotedValueWithALineBreak(t *testing.T) {
	q1, q2 := tailFake(5051, 16, tailAlnum), tailFake(5052, 16, tailAlnum)
	v, w := tailFake(5053, 28, tailAlnum), tailFake(5054, 28, tailAlnum)
	for _, c := range []struct{ name, line string }{
		{"line feed", spanCk + `: m="` + q1 + "\n" + q2 + `"; sid=` + v},
		{"line feed, second cookie", spanCk + ": sid=" + w + `; m="` + q1 + "\n" + q2 + `"; z=` + v},
		{"carriage return and line feed", spanCk + `: m="` + q1 + "\r\n" + q2 + `"; sid=` + v},
		{"fold", spanCk + `: m="` + q1 + "\r\n " + q2 + `"; sid=` + v},
	} {
		reviewRedact(t, c.name, "one\n"+c.line+"\nthree", q1, q2, v, w)
		out := reviewRedact(t, c.name+", in Args", writeFileArgs(t, "one\n"+c.line+"\nthree\n"), q1, q2, v, w)
		if !json.Valid([]byte(out)) {
			t.Errorf("%s: the redacted Args are not valid JSON: %q", c.name, out)
		}
		reviewHarvest(t, c.name, c.line, q1, q2, v, w)
	}
}

// TestRedactorTakesShellEscapesAndATruncatedQuote: a shell escape (an escaped dollar sign or backtick, a backslash path)
// in an unquoted cookie value did not end the header, and neither does a quoted value that the text is cut in.
func TestRedactorTakesShellEscapesAndATruncatedQuote(t *testing.T) {
	v, w := tailFake(5061, 28, tailAlnum), tailFake(5062, 28, tailAlnum)
	q1, q2 := tailFake(5063, 16, tailAlnum), tailFake(5064, 16, tailAlnum)
	for _, c := range []struct {
		name, text string
		values     []string
	}{
		{"escaped dollar after a long value", `curl -H "` + spanCk + ": sid=" + v + `; v=\$Version; z=` + w + `" https://host.invalid/p`, []string{v, w}},
		{"escaped dollar before the first long value", `curl -H "` + spanCk + `: a=x\$y; sid=` + v + `" https://host.invalid/p`, []string{v}},
		{"escaped backtick before the first long value", `curl -H "` + spanCk + ": a=x\\`id\\`; sid=" + v + `" https://host.invalid/p`, []string{v}},
		{"backslash path", spanCk + ": sid=" + v + `; dir=C:\users\x; z=` + w, []string{v, w}},
		{"truncated quoted value behind a long one", spanCk + ": sid=" + v + `; m="` + q1 + " " + q2, []string{v, q1, q2}},
	} {
		reviewRedact(t, c.name, c.text, c.values...)
		reviewRedact(t, c.name+", in Args", reviewJSON(t, map[string]string{"cmd": c.text}), c.values...)
		reviewHarvest(t, c.name, c.text, c.values...)
	}
}
