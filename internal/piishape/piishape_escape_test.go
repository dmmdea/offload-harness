package piishape

import (
	"strings"
	"testing"
)

// escapeSamples are one credential per class. Every fixture is assembled from pieces at run time so
// no literal in the tree is itself credential-shaped.
func escapeSamples() []struct{ class, text string } {
	v := valueFixture(71, 15, alnum)
	return []struct{ class, text string }{
		{"api-key-assignment", "PASS" + "WORD=" + v},
		{"api-key-assignment", "TO" + "KEN=" + v},
		{"api-key-assignment", "SEC" + "RET=" + v},
		{"api-key-assignment", "api_" + "key=" + v},
		{"api-key-assignment", "pass" + "word: " + v},
		{"bearer-token", "Bear" + "er " + valueFixture(72, 24, alnum)},
		{"aws-access-key", "AK" + "IA" + strings.ToUpper(valueFixture(73, 16, "abcdefghijklmnopqrstuvwxyz0123456789"))},
		{"github-token", "gh" + "p_" + valueFixture(74, 26, alnum)},
		{"provider-key", "s" + "k-" + valueFixture(75, 26, alnum)},
		{"jwt", "ey" + "J" + valueFixture(76, 12, b64url) + "." + valueFixture(77, 12, b64url) + "." + valueFixture(78, 12, b64url)},
	}
}

// TestAKeyWordAfterAJSONEscapeIsRecognised: tool-call arguments are JSON text, so a newline there is the
// two characters backslash and n, and the key word that starts the next line follows a word character.
// A word boundary does not see it, so a secret at the start of a line in escaped text went unmatched.
func TestAKeyWordAfterAJSONEscapeIsRecognised(t *testing.T) {
	for _, lead := range []string{`\n`, `\r\n`, `\t`, `\u000a`} {
		for _, s := range escapeSamples() {
			text := `{"path":".env","content":"APP=1` + lead + s.text + lead + `OTHER=2"}`
			found := false
			for _, got := range Matching(text) {
				if got == s.class {
					found = true
				}
			}
			if !found {
				t.Errorf("after %q the class %s does not match %q", lead, s.class, text)
			}
		}
	}
}
