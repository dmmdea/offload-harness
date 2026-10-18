package piishape

import (
	"encoding/json"
	"strings"
	"testing"
)

// esc is the terminal escape byte. Colored build and test output (FORCE_COLOR, --color=always, CI) puts a
// control sequence ending in a letter (the 'm' of a color code) right in front of the next word, so there
// is no word boundary between them.
const esc = "\x1b"

// ansiSamples are credentials in colored output, one per class that starts with a word, plus the Java
// system property form. Built from pieces at run time so no literal in the tree is credential-shaped.
func ansiSamples() []struct{ name, class, text string } {
	v := valueFixture(301, 20, alnum)
	b64 := valueFixture(302, 22, alnum) + "=="
	return []struct{ name, class, text string }{
		{"password", "api-key-assignment", esc + "[32mpass" + "word=" + v + esc + "[0m"},
		{"password colon", "api-key-assignment", esc + "[1;34mpass" + "word: " + v},
		{"secret", "api-key-assignment", esc + "[31mSEC" + "RET=" + v},
		{"name then reset then colon", "api-key-assignment", esc + "[36mDB_PASS" + "WORD " + esc + "[0m: " + v},
		{"bearer", "bearer-token", esc + "[33mBear" + "er " + valueFixture(303, 24, alnum)},
		{"github", "github-token", esc + "[33mgh" + "p_" + valueFixture(304, 26, alnum)},
		{"provider", "provider-key", esc + "[33ms" + "k-" + valueFixture(305, 26, alnum)},
		{"basic", "basic-auth", esc + "[33mAuthorization: Ba" + "sic " + b64},
		{"cookie", "cookie-header", esc + "[33mCoo" + "kie: sid=" + valueFixture(306, 32, alnum)},
		{"java property password", "api-key-assignment", "java -Dpass" + "word=" + v + " -jar app.jar"},
		{"java property token", "api-key-assignment", "mvn -DTO" + "KEN=" + v + " test"},
	}
}

// TestAControlSequenceBeforeAKeyWordIsRecognised: the 'm' that ends a color code is a word character, so
// \b never sees the key word after it, and the redactor, the residual gate and the reducer's seat check
// all passed the credential.
func TestAControlSequenceBeforeAKeyWordIsRecognised(t *testing.T) {
	for _, s := range ansiSamples() {
		found := false
		for _, got := range Matching(s.text) {
			if got == s.class {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: %s does not match %q", s.name, s.class, s.text)
		}
	}
}

// TestAControlSequenceWrittenAsAJSONEscapeIsRecognised: in tool-call arguments and JSON tool output the
// escape byte is the six characters backslash, u, 0, 0, 1, b.
func TestAControlSequenceWrittenAsAJSONEscapeIsRecognised(t *testing.T) {
	for _, s := range ansiSamples() {
		b, err := json.Marshal(map[string]string{"content": s.text})
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		if !strings.Contains(text, `\u001b`) {
			continue // the sample carries no escape byte (the Java forms)
		}
		found := false
		for _, got := range Matching(text) {
			if got == s.class {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: %s does not match %s", s.name, s.class, text)
		}
	}
}

// TestAValueEndsAtAControlSequence: a reset code right behind the value is not part of it, so the
// redactor does not eat it.
func TestAValueEndsAtAControlSequence(t *testing.T) {
	v := valueFixture(311, 20, alnum)
	text := esc + "[32mpass" + "word=" + v + esc + "[0m done"
	var got string
	for _, cl := range Classes {
		if cl.Name == "api-key-assignment" {
			got = cl.Re.FindString(text)
		}
	}
	if !strings.HasSuffix(got, v) {
		t.Errorf("matched %q, want it to end with the value %q", got, v)
	}
}

// TestColoredOutputWithoutACredentialStaysQuiet: color codes around ordinary words are not matches.
func TestColoredOutputWithoutACredentialStaysQuiet(t *testing.T) {
	for _, text := range []string{
		esc + "[32mPASS: TestName" + esc + "[0m",
		esc + "[31mFAIL" + esc + "[0m token_count=1234",
		esc + "[33mMAX_TOKENS=4096" + esc + "[0m",
		esc + "[1mtokenizer" + esc + "[0m: loaded",
		esc + "[36mpassword" + esc + "[0m field is required",
		"-Dpassword=short",
	} {
		if got := Matching(text); got != nil {
			t.Errorf("Matching(%q) = %v, want nil", text, got)
		}
	}
}
