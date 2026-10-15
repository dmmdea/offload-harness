package piishape

import (
	"strings"
	"testing"
)

// valueFixture builds a fake credential value from an alphabet at run time, so no literal in the
// tree is itself credential-shaped. The same seed always gives the same value.
func valueFixture(seed, n int, alphabet string) string {
	var b strings.Builder
	x := uint32(seed)*2654435761 + 12345
	for i := 0; i < n; i++ {
		x = x*1664525 + 1013904223
		b.WriteByte(alphabet[int(x>>16)%len(alphabet)])
	}
	return b.String()
}

const (
	alnum   = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	punct   = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789./+=@~:"
	b64url  = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-"
	nameKey = "SEC" + "RET"
)

// wholeValue returns a value that starts with a letter or digit and carries '.', '/', '+', '=',
// '@', '~' and ':' after that.
func wholeValue(seed, n int) string {
	return valueFixture(seed, 1, alnum) + "ab/cd.ef+gh=ij@kl~mn:" + valueFixture(seed+1, n, punct)
}

// TestAssignmentValueIsMatchedWhole: the vet class must match the credential's entire value, so
// that a redactor deriving from it leaves no tail. Each value starts with [A-Za-z0-9_-] and then
// carries the characters the first table stopped at.
func TestAssignmentValueIsMatchedWhole(t *testing.T) {
	jwt := "ey" + "J" + valueFixture(1, 20, b64url) + "." + valueFixture(2, 30, b64url) + "." + valueFixture(3, 40, b64url)
	cases := []struct{ name, prefix, value string }{
		{"access token jwt", "access_" + "token=", jwt},
		{"id token jwt colon", "id_" + "token: ", jwt},
		{"aws secret", "AWS_" + nameKey + "_ACCESS_KEY=", "wJalrXUtnFEMI/K7MDENG/" + valueFixture(4, 18, alnum)},
		{"short head then slash", nameKey + "=", "ab/" + "cdefghijklmnopqrstuvwx"},
		{"quoted", "api_" + "key=\"", wholeValue(5, 20)},
		{"plus and equals", "DB_PASS" + "WORD=", valueFixture(6, 6, alnum) + "+" + valueFixture(7, 10, alnum) + "=="},
	}
	for _, c := range cases {
		text := "before " + c.prefix + c.value + " after"
		var got string
		for _, cl := range Classes {
			if cl.Name == "api-key-assignment" {
				got = cl.Re.FindString(text)
			}
		}
		if got == "" {
			t.Errorf("%s: api-key-assignment does not recognise the assignment at all", c.name)
			continue
		}
		if !strings.HasSuffix(got, c.value) {
			t.Errorf("%s: the match stops early: matched %q, value is %q", c.name, got, c.value)
		}
	}
}

// TestBearerValueIsMatchedWhole: a bearer value with '.', '/', '+' or '=' is matched to the end.
func TestBearerValueIsMatchedWhole(t *testing.T) {
	for i, value := range []string{
		valueFixture(11, 1, alnum) + "bcdefgh/ijklmnop+qrstuvwx",
		valueFixture(12, 1, alnum) + "bcdefgh.ijklmnop.qrstuvwx==",
		wholeValue(13, 24),
	} {
		text := "Authorization: Bearer " + value + "\nnext line"
		var got string
		for _, cl := range Classes {
			if cl.Name == "bearer-token" {
				got = cl.Re.FindString(text)
			}
		}
		if !strings.HasSuffix(got, value) {
			t.Errorf("case %d: the match stops early: matched %q, value is %q", i, got, value)
		}
	}
}

// TestValueEndsAtItsDelimiters: the value stops at whitespace, a quote, a comma, ';' and '&', so
// the text around a credential survives redaction.
func TestValueEndsAtItsDelimiters(t *testing.T) {
	value := valueFixture(21, 1, alnum) + "bc/defghijkl.mnopqrst"
	for _, tail := range []string{" next", "\" next", "', next", "; next", "&page=2", ", next"} {
		text := "api_" + "key=" + value + tail
		var got string
		for _, cl := range Classes {
			if cl.Name == "api-key-assignment" {
				got = cl.Re.FindString(text)
			}
		}
		if !strings.HasSuffix(got, value) {
			t.Errorf("tail %q: matched %q, want it to end with the value %q", tail, got, value)
		}
	}
}

// TestPathsAfterAKeyNameStayQuiet: a value that starts with '/' or '.' is a path, not a secret,
// so the wider value class does not start there. (A relative path that starts with a letter
// does match; that is the safe direction and is not pinned here.)
func TestPathsAfterAKeyNameStayQuiet(t *testing.T) {
	for _, text := range []string{
		"token_store=/var/lib/x/y/z/long/path",
		"secret: /etc/ssl/certs/ca-bundle",
		"password_file=./config/passwords.txt",
		"MAX_TOKENS=4096 and token_count=1234",
	} {
		if got := Matching(text); got != nil {
			t.Errorf("Matching(%q) = %v, want nil", text, got)
		}
	}
}

// TestValueStopsAtAJSONEscape: Args are JSON text, so a backslash ends a value. A class that ran
// through it would take the closing quote or the next command with it.
func TestValueStopsAtAJSONEscape(t *testing.T) {
	v := valueFixture(51, 24, alnum)
	for _, c := range []struct{ name, text, class string }{
		{"bearer", `{"command":"curl -H \"Authorization: Bearer ` + v + `\" x"}`, "bearer-token"},
		{"assignment", `{"command":"export TO` + `KEN=` + v + `\nls"}`, "api-key-assignment"},
		{"quoted assignment", `{"command":"export API_` + `KEY=\"` + v + `\" && ls"}`, "api-key-assignment"},
	} {
		var got string
		for _, cl := range Classes {
			if cl.Name == c.class {
				got = cl.Re.FindString(c.text)
			}
		}
		if got == "" {
			t.Errorf("%s: %s does not recognise it", c.name, c.class)
			continue
		}
		if !strings.HasSuffix(got, v) {
			t.Errorf("%s: matched %q, want it to end with the value %q", c.name, got, v)
		}
	}
}

// TestSymbolStartingValuesAreRecognised: a value may start with a symbol.
func TestSymbolStartingValuesAreRecognised(t *testing.T) {
	for _, first := range []string{"!", "$", "#", "%", "(", "*", "^", "~", "@"} {
		text := "DB_PASS" + "WORD=" + first + valueFixture(61, 20, alnum)
		if got := Matching(text); len(got) == 0 {
			t.Errorf("value starting with %q: not recognised", first)
		}
	}
}
