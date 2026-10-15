package piishape

import (
	"strings"
	"testing"
)

// assignFormSamples are a quoted credential literal in the ways source code, a config dump and a failing
// test's printed values assign or pass one: Go's := and typed declarations, a comparison, Python's typed
// and prefixed strings, an environment subscript or call, a hash literal, a quoted JSON key. Built from
// pieces at run time so no literal in the tree is credential-shaped. The literal is the last thing in
// each text before its closers.
func assignFormSamples(v string) []struct{ name, text string } {
	return []struct{ name, text string }{
		{"go short declaration", `password := "` + v + `"`},
		{"go token declaration", `token := "` + v + `"`},
		{"typescript typed", `const token: string = "` + v + `"`},
		{"typescript generic type", `const token: Record<string, string> = "` + v + `"`},
		{"go var typed", `var token string = "` + v + `"`},
		{"python typed", `password: str = "` + v + `"`},
		{"python bytes", `password = b"` + v + `"`},
		{"python raw", `password = r'` + v + `'`},
		{"comparison", `if password == "` + v + `" {`},
		{"environ subscript", `os.environ["API_TOKEN"] = "` + v + `"`},
		{"setenv call", `t.Setenv("API_TOKEN", "` + v + `")`},
		{"hash rocket", `{'password' => '` + v + `'}`},
		{"quoted json key", `{"token": "` + v + `"}`},
		{"quoted json key, escaped", `{\"token\": \"` + v + `\"}`},
	}
}

// TestQuotedLiteralAssignmentsAreRecognised: the assignment class took only = or : straight after the
// name, so a Go :=, a type annotation, a comparison, a prefixed string, a call or a hash left the
// literal unmatched. The literal is matched whole and the closers behind it are not part of the match.
func TestQuotedLiteralAssignmentsAreRecognised(t *testing.T) {
	v := valueFixture(501, 22, alnum)
	for _, s := range assignFormSamples(v) {
		var got string
		for _, cl := range Classes {
			if cl.Name == "api-key-assignment" {
				got = cl.Re.FindString(s.text)
			}
		}
		if got == "" {
			t.Errorf("%s: api-key-assignment does not recognise %q", s.name, s.text)
			continue
		}
		if !strings.HasSuffix(got, v) {
			t.Errorf("%s: matched %q, want it to end with the value %q", s.name, got, v)
		}
	}
}

// TestCodeThatHandlesACredentialWithoutHoldingOneStaysQuiet: the wider operators need a quoted literal of
// 12 or more characters. An identifier, a call, a short or empty literal, a name that only contains the
// key word and prose around the words are not matches (Go source assigns to token-named variables all day).
func TestCodeThatHandlesACredentialWithoutHoldingOneStaysQuiet(t *testing.T) {
	for _, text := range []string{
		`token := strings.TrimSpace(line)`,
		`password := os.Getenv("DB_PASSWORD")`,
		`token := <-tokens`,
		`if token == "" {`,
		`password := ""`,
		`password := "short"`,
		`password: str = "short"`,
		`tokenizer := "abcdefghijklmnop"`,
		`token_count = "abcdefghijklmnop"`,
		`max_tokens: "abcdefghijklmnop"`,
		`"token_count": "1234567890123"`,
		`func (c *Client) SetToken(token string) {`,
		"Token string `json:\"token\"`",
		`the token is "abcdefghijklmnop" today`,
		`req.Header.Set("Authorization", token)`,
		`t.Setenv("API_TOKEN", "x")`,
		`password required: "abcdefghijklmnop"`,
		`token type: "service-account-key"`,
		`the credentials file: "/etc/ssl/certs/ca-bundle"`,
	} {
		if got := Matching(text); got != nil {
			t.Errorf("Matching(%q) = %v, want nil", text, got)
		}
	}
}
