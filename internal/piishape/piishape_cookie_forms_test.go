package piishape

import (
	"strings"
	"testing"
)

// cookieFormSamples are a cookie credential in the places Go, JavaScript, Python, PHP and Ruby code, a JSON
// document and a printed http.Header put a Cookie or Set-Cookie header: a list, a call that takes the name
// and the value as two arguments, a subscript, a hash. want is the end of the match: the value, and what
// follows it on the line. Built from pieces at run time so no literal in the tree is credential-shaped.
func cookieFormSamples(v string) []struct{ name, text, want string } {
	const (
		ck  = "Coo" + "kie"
		sck = "Set-Coo" + "kie"
	)
	return []struct{ name, text, want string }{
		{"header line", ck + ": sid=" + v, v},
		{"set header line", sck + ": sid=" + v + "; Path=/", v + "; Path=/"},
		{"json set-cookie list", `{"` + sck + `":["session=` + v + `; Path=/"]}`, v + `; Path=/"`},
		{"json cookie list with spaces", `"` + ck + `": [ "sid=` + v + `" ]`, v + `" `},
		{"go Header.Set", `req.Header.Set("` + ck + `", "sid=` + v + `")`, v},
		{"go Header.Add", `w.Header().Add("` + sck + `", "sid=` + v + `; HttpOnly")`, v + "; HttpOnly"},
		{"js setRequestHeader", `xhr.setRequestHeader('` + ck + `','sid=` + v + `')`, v},
		{"js setHeader", `req.setHeader('` + ck + `', 'sid=` + v + `')`, v},
		{"java property", `con.setRequestProperty("` + ck + `", "sid=` + v + `")`, v},
		{"go header dump", "Header:map[" + ck + ":[sid=" + v + "]]", v},
		{"python headers dict", `headers={'` + ck + `': 'sid=` + v + `'}`, v},
		{"subscript", `headers["` + ck + `"] = "sid=` + v + `"`, v},
		{"hash rocket", `'` + ck + `' => 'sid=` + v + `'`, v},
		{"php array", "[" + ck + "] => sid=" + v, v},
		{"document.cookie", `document.` + strings.ToLower(ck) + ` = "sid=` + v + `; path=/"`, v + "; path=/"},
	}
}

// TestCookieHeaderInCodeAndDumpsIsRecognised: the class took only a colon or an equals sign straight after the
// header name (and a quote), so a list, a comma between two arguments or a subscript hid the credential. It
// takes the same punctuation between the name and the value that the Basic header does, and a comma only
// when a quote follows it. Each form is also checked as the corpus holds it, as JSON text.
func TestCookieHeaderInCodeAndDumpsIsRecognised(t *testing.T) {
	v := valueFixture(711, 32, alnum)
	for _, s := range cookieFormSamples(v) {
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
				got = strings.ReplaceAll(got, `\"`, `"`)
			}
			if !strings.HasSuffix(got, s.want) {
				t.Errorf("%s (args %v): matched %q, want it to end with %q", s.name, args, got, s.want)
			}
		}
	}
}

// TestCookieWordsInProseAndCodeStayQuiet: the header name near a value is not a header. A comma counts only
// when a quote follows it, a long value needs an equals sign of its own, and a function that handles cookies
// holds no credential.
func TestCookieWordsInProseAndCodeStayQuiet(t *testing.T) {
	for _, text := range []string{
		"Set-Cookie headers are sent by the server",
		"Cookie: theme=dark",
		"cookie=abc",
		"the cookie, path=/some/long/path/value",
		"the cookie, then set name=abcdefghijklmnop",
		`req.Header.Set("Cookie", c.String())`,
		`req.Header.Get("Cookie")`,
		`got := resp.Header.Get("Set-Cookie")`,
		`if cookie == nil { return }`,
		`c, err := r.Cookie("session")`,
		`http.SetCookie(w, &http.Cookie{Name: "session", Value: "abcdefghijklmnopqrstuvwxyz", Path: "/"})`,
		`cookie := &http.Cookie{Name: "a", Value: "abcdefghijklmnopqrstuvwxyz"}`,
		`cookie.Value = "abcdefghijklmnopqrstuvwxyz"`,
		`jar.SetCookies(u, cookies)`,
		`func (j *Jar) Cookies(u *url.URL) []*http.Cookie {`,
		"Set-Cookie: id=a3fWa; Expires=Wed, 21 Oct 2015 07:28:00 GMT; Secure; HttpOnly",
		"The Cookie header carries name=value pairs separated by semicolons",
		`cookie="expired-cookie-value-here", want nothing`,
		`assert.Equal(t, "sid=abc", w.Header().Get("Set-Cookie"))`,
	} {
		for _, c := range Matching(text) {
			if c == "cookie-header" {
				t.Errorf("cookie-header matches %q", text)
			}
		}
	}
}
