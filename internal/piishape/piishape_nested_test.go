package piishape

import (
	"encoding/json"
	"strings"
	"testing"
)

// nestedQuoteArgs returns the Args text of a run_shell call whose command quotes a shell string that
// itself holds an escaped quote around the value. The command is a double-quoted shell string, and the
// inner quotes are escaped, so after json.Marshal the quote in front of the value is three backslashes
// and a quote.
func nestedQuoteArgs(t *testing.T, command string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestValueAfterANestedEscapedQuoteIsRecognised: a shell command that wraps a double-quoted string in
// another one (bash -c "export KEY=\"value\"", ssh host "...") is, as JSON text, a quote preceded by
// three backslashes. The assignment class allowed one backslash, so the value was neither matched nor
// redacted.
func TestValueAfterANestedEscapedQuoteIsRecognised(t *testing.T) {
	v := valueFixture(201, 22, alnum)
	for _, c := range []struct{ name, command string }{
		{"bash -c", `bash -c "export API_` + `KEY=\"` + v + `\" && ./run"`},
		{"ssh", `ssh host "export TO` + `KEN=\"` + v + `\" && ./run"`},
		{"password colon", `sh -c "echo pass` + `word: \"` + v + `\""`},
	} {
		text := nestedQuoteArgs(t, c.command)
		var got string
		for _, cl := range Classes {
			if cl.Name == "api-key-assignment" {
				got = cl.Re.FindString(text)
			}
		}
		if got == "" {
			t.Errorf("%s: api-key-assignment does not recognise %s", c.name, text)
			continue
		}
		if !strings.HasSuffix(got, v) {
			t.Errorf("%s: matched %q, want it to end with the value %q", c.name, got, v)
		}
	}
}

// TestHeaderClassesAfterANestedEscapedQuote: the Basic header, the registry auth field and the cookie
// header take an optional quote around their name and value. In a JSON document that sits inside a
// double-quoted shell string those quotes carry three backslashes each, so the classes saw nothing.
func TestHeaderClassesAfterANestedEscapedQuote(t *testing.T) {
	b64 := valueFixture(211, 22, alnum) + "=="
	sid := valueFixture(212, 32, alnum)
	for _, c := range []struct{ class, command string }{
		{"basic-auth", `sh -c "echo '{\"Authorization\": \"Ba` + `sic ` + b64 + `\"}' > h.json"`},
		{"auth-field", `sh -c "echo '{\"auths\":{\"r.invalid\":{\"au` + `th\":\"` + b64 + `\"}}}' > c.json"`},
		{"cookie-header", `sh -c "echo '{\"Coo` + `kie\": \"session=` + sid + `\"}' > h.json"`},
	} {
		text := nestedQuoteArgs(t, c.command)
		found := false
		for _, got := range Matching(text) {
			if got == c.class {
				found = true
			}
		}
		if !found {
			t.Errorf("%s does not recognise %s", c.class, text)
		}
	}
}
