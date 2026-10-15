package compeval

import (
	"encoding/json"
	"strings"
	"testing"
)

// A tool call's Args are JSON text. A value class that runs to the next non-space byte swallows
// the backslash of a JSON escape, so the closing quote or the \n after the value goes with it:
// the Args stop being valid JSON and the command after the secret is gone. Replay code parses
// Args, so a broken one is no longer classified as the call it was.
func TestRedactorKeepsJSONArgsValid(t *testing.T) {
	v := tailFake(31, 24, tailAlnum)
	cases := []struct{ name, in, keep string }{
		{"bearer in a quoted header", `{"command":"curl -H \"Authorization: Bearer ` + v + `\" https://x.invalid"}`, `\" https://x.invalid"}`},
		{"assignment before a quote", `{"command":"echo pass` + `word=` + v + `\"; done"}`, `\"; done"}`},
		{"assignment before a newline", `{"command":"export TO` + `KEN=` + v + `\nls -la /tmp"}`, `\nls -la /tmp"}`},
		{"assignment before a tab", `{"command":"export TO` + `KEN=` + v + `\tls"}`, `\tls"}`},
		{"quoted value", `{"command":"export API_` + `KEY=\"` + v + `\" && ls"}`, `\" && ls"}`},
	}
	for _, c := range cases {
		if !json.Valid([]byte(c.in)) {
			t.Fatalf("%s: fixture is not valid JSON: %s", c.name, c.in)
		}
		x := NewRedactor()
		out := x.Redact(c.in)
		if !json.Valid([]byte(out)) {
			t.Errorf("%s: the redacted Args are not valid JSON: %s", c.name, out)
		}
		if !strings.Contains(out, c.keep) {
			t.Errorf("%s: the text after the value was eaten (want %q): %s", c.name, c.keep, out)
		}
		if strings.Contains(out, v[:12]) {
			t.Errorf("%s: the value survived: %s", c.name, out)
		}
		if got := x.Residual(out); len(got) != 0 {
			t.Errorf("%s: the gate reports %v on %s", c.name, got, out)
		}
	}
}

// A password that starts with a symbol is still a password. The value class started at a letter,
// digit, underscore or hyphen, so these were neither redacted nor seen by the vet.
func TestRedactorTakesValuesThatStartWithASymbol(t *testing.T) {
	body := tailFake(41, 20, tailAlnum)
	for _, c := range []struct{ name, prefix, first string }{
		{"bang", "DB_PASS" + "WORD=", "!"},
		{"dollar", "pass" + "word: ", "$"},
		{"hash", "sec" + "ret=", "#"},
		{"percent", "export API_" + "KEY=", "%"},
		{"paren", "tok" + "en=", "("},
		{"star", "tok" + "en=", "*"},
	} {
		in := "before " + c.prefix + c.first + body + " after"
		x := NewRedactor()
		out := x.Redact(in)
		if strings.Contains(out, body[:12]) {
			t.Errorf("%s: the value was not redacted: %q", c.name, out)
		}
		if !strings.HasSuffix(out, " after") {
			t.Errorf("%s: the text after the value was eaten: %q", c.name, out)
		}
		if got := x.Residual(out); len(got) != 0 {
			t.Errorf("%s: the gate reports %v on %q", c.name, got, out)
		}
	}
}
