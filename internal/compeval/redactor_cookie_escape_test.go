package compeval

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactorTakesTheSessionBehindAQuotedCookieWithEscapes: Django's messages cookie is a quoted value with
// an escaped quote and octal escapes in it (the way Python's http.cookies writes them). The match ended at
// the first escape, so the session cookie behind it went to the corpus verbatim, and the residual gate did
// not see it, because the value was never inside a match. The line is replaced whole, on the plain line and
// as file content in Args, and nothing of either value is left.
func TestRedactorTakesTheSessionBehindAQuotedCookieWithEscapes(t *testing.T) {
	h, v1, v2 := tailFake(731, 40, "abcdef0123456789"), tailFake(732, 32, tailAlnum), tailFake(733, 26, tailAlnum)
	const (
		ck    = "Coo" + "kie: "
		sck   = "Set-Coo" + "kie: "
		flash = `$[[\"__json_message\"\0540\05425\054\"Saved\"]]`
	)
	for _, c := range []struct{ name, text string }{
		{"messages before the session", ck + `messages="` + h + flash + `"; sessionid=` + v1},
		{"messages before the session, set header", sck + `messages="` + h + flash + `"; sessionid=` + v1 + `; HttpOnly`},
		{"messages alone", sck + `messages="` + h + flash + `"; HttpOnly`},
		{"octal escape inside a long value", ck + `messages="` + h + `\054` + v2 + `"; sid=` + v1},
		{"messages between two cookies", ck + `csrftoken=` + h + `; messages="[[\"__json_message\"\0540\05425\054\"Saved\"]]:` + v2 + `"; sessionid=` + v1},
		{"escaped quote in a short pair", ck + `flash="a\"b"; sid=` + v1 + `; extra=` + v2},
	} {
		for _, asArgs := range []bool{false, true} {
			in := c.text
			if asArgs {
				in = writeFileArgs(t, "line one\n"+c.text+"\nline three\n")
			}
			r := NewRedactor()
			out := r.Redact(in)
			for _, secret := range []string{v1, v2, h} {
				if !strings.Contains(c.text, secret) {
					continue
				}
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

// TestValueTailGateRemembersTheSessionBehindAQuotedCookie: the session value that follows a quoted cookie
// with escapes is among the values the value-tail gate remembers.
func TestValueTailGateRemembersTheSessionBehindAQuotedCookie(t *testing.T) {
	h, v := tailFake(734, 40, "abcdef0123456789"), tailFake(735, 32, tailAlnum)
	r := newRedactor()
	r.remember("cookie-header", "Coo"+"kie: messages=\""+h+`$[[\"__json_message\"\054x\"]]"; sessionid=`+v)
	if !r.survivingTail("xx " + v + " yy") {
		t.Errorf("the session value is not remembered")
	}
}
