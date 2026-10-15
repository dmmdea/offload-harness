package compeval

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactorTakesEveryCookieOnTheLine: a quoted pair before or after a session value ended the match, so a
// cookie behind a quote was written to the corpus verbatim. The line is replaced whole (the other cookies
// go with it), on the plain line and as file content in Args, and the value-tail gate agrees.
func TestRedactorTakesEveryCookieOnTheLine(t *testing.T) {
	v1, v2 := tailFake(701, 32, tailAlnum), tailFake(702, 24, tailAlnum)
	const (
		ck  = "Coo" + "kie: "
		sck = "Set-Coo" + "kie: "
	)
	for _, c := range []struct{ name, text string }{
		{"quoted pair before the value", ck + `messages="ok"; sessionid=` + v1},
		{"quoted path before the value", sck + `a=b; Path="/"; sessionid=` + v1 + `; HttpOnly`},
		{"quoted value", sck + `messages="` + v1 + `"; HttpOnly`},
		{"quoted pair between two values", ck + `sid=` + v1 + `; theme="dark"; extra=` + v2},
	} {
		for _, asArgs := range []bool{false, true} {
			in := c.text
			if asArgs {
				in = writeFileArgs(t, "line one\n"+c.text+"\nline three\n")
			}
			r := NewRedactor()
			out := r.Redact(in)
			for _, secret := range []string{v1, v2} {
				if run, ok := anyRun(secret, out, 8); ok {
					t.Errorf("%s (args %v): %q of a value is still in %q", c.name, asArgs, run, out)
				}
			}
			if res := r.Residual(out); len(res) != 0 {
				t.Errorf("%s (args %v): residual gate reports %v on %q", c.name, asArgs, res, out)
			}
			if asArgs {
				var back map[string]string
				if err := json.Unmarshal([]byte(out), &back); err != nil {
					t.Errorf("%s: redacted Args are not valid JSON: %v: %q", c.name, err, out)
				} else if lines := strings.Split(back["content"], "\n"); len(lines) != 4 || lines[0] != "line one" || lines[2] != "line three" {
					t.Errorf("%s: the lines around the credential changed: %q", c.name, back["content"])
				}
			}
		}
	}
}

// TestValueTailGateRemembersEveryCookieValue: only the first long cookie value on a line was remembered, so
// a second one that survived redaction was invisible to the residual gate.
func TestValueTailGateRemembersEveryCookieValue(t *testing.T) {
	v1, v2 := tailFake(703, 32, tailAlnum), tailFake(704, 24, tailAlnum)
	r := newRedactor()
	r.remember("cookie-header", "Coo"+"kie: sid="+v1+`; theme="dark"; extra=`+v2+"; lang=en")
	for name, v := range map[string]string{"first": v1, "second": v2} {
		if !r.survivingTail("xx " + v + " yy") {
			t.Errorf("the %s cookie value is not remembered", name)
		}
	}
	if r.survivingTail("xx lang=en; theme=dark yy") {
		t.Errorf("a short cookie pair is remembered")
	}
}

// TestValueTailGateSkipsCookieAttributes: a domain or an expiry attribute is ordinary text, so the gate must
// not refuse a harvest that names the same domain elsewhere.
func TestValueTailGateSkipsCookieAttributes(t *testing.T) {
	v := tailFake(705, 32, tailAlnum)
	r := newRedactor()
	r.remember("cookie-header", "Set-Coo"+"kie: sid="+v+"; Domain=subdomain.example.invalid; Path=/some/long/path/here; Max-Age=31536000000000000")
	if !r.survivingTail("xx " + v + " yy") {
		t.Errorf("the session value is not remembered")
	}
	for _, text := range []string{"host subdomain.example.invalid", "/some/long/path/here", "31536000000000000"} {
		if r.survivingTail(text) {
			t.Errorf("an attribute value is remembered: %q", text)
		}
	}
}
