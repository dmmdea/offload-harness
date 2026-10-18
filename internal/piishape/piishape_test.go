package piishape

import (
	"reflect"
	"strings"
	"testing"
)

// Every fixture below is assembled from pieces so that no literal in the tree is
// itself credential-shaped (the repo's pre-push secret scan reads this file).
var fixtures = map[string]string{
	"private-key-block":  "-----BEGIN " + "RSA PRIVATE KEY-----",
	"bearer-token":       "Authorization: Bearer " + strings.Repeat("a", 24),
	"api-key-assignment": "api_" + "key = " + strings.Repeat("x", 16),
	"aws-access-key":     "AKI" + "A" + strings.Repeat("A", 16),
	"github-token":       "gh" + "p_" + strings.Repeat("a", 24),
	"email":              "dev" + "@" + "example.invalid",
}

// TestMatchingNamesEveryClass: each credential- or identity-shaped fixture is
// reported under its own class name.
func TestMatchingNamesEveryClass(t *testing.T) {
	for class, text := range fixtures {
		got := Matching("log line before\n" + text + "\nlog line after")
		found := false
		for _, g := range got {
			if g == class {
				found = true
			}
		}
		if !found {
			t.Errorf("Matching(%s fixture) = %v, want it to name %q", class, got, class)
		}
	}
}

// TestMatchingIsNilForPlainBuildOutput: ordinary compiler and test output names no class.
func TestMatchingIsNilForPlainBuildOutput(t *testing.T) {
	plain := "ok  \tgithub.com/example/pkg\t0.412s\n--- FAIL: TestThing (0.00s)\n    thing_test.go:41: got 3, want 4\nFAIL\n"
	if got := Matching(plain); got != nil {
		t.Errorf("Matching(plain build output) = %v, want nil", got)
	}
}

// TestMatchingIsSorted: the answer is sorted, so two callers print the same line.
func TestMatchingIsSorted(t *testing.T) {
	got := Matching(fixtures["github-token"] + "\n" + fixtures["aws-access-key"] + "\n" + fixtures["email"])
	want := []string{"aws-access-key", "email", "github-token"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Matching = %v, want %v", got, want)
	}
}

// TestClassesAreTheVetTable: the six classes the corpus vet has always refused keep
// their names and order; the classes added later are appended at the end.
func TestClassesAreTheVetTable(t *testing.T) {
	var names []string
	for _, c := range Classes {
		names = append(names, c.Name)
	}
	want := []string{"email", "private-key-block", "bearer-token", "api-key-assignment", "aws-access-key", "github-token", "provider-key", "jwt", "basic-auth", "auth-field", "cookie-header"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("class names = %v, want %v", names, want)
	}
}

// fixtureSecret is a fake credential value built at run time, so no literal in the
// tree is itself credential-shaped.
func fixtureSecret(n int) string { return strings.Repeat("q7", n/2) }

// fixtureJWT is three base64url segments, the first starting with the JWT header prefix.
func fixtureJWT() string {
	return "ey" + "J" + strings.Repeat("h", 12) + "." + "ey" + "J" + strings.Repeat("p", 12) + "." + strings.Repeat("s", 20)
}

// TestWidenedClassesMatch: the credential shapes the first table missed. Each case names
// the class that must report it.
func TestWidenedClassesMatch(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		class string
	}{
		{"env github token", "GITHUB" + "_TOKEN=" + fixtureSecret(24), "api-key-assignment"},
		{"env openai key", "OPENAI" + "_API_KEY=" + fixtureSecret(24), "api-key-assignment"},
		{"env db password", "DB" + "_PASSWORD=" + fixtureSecret(16), "api-key-assignment"},
		{"env aws secret", "AWS" + "_SECRET_ACCESS_KEY=" + fixtureSecret(30), "api-key-assignment"},
		{"env exported and quoted", "export STRIPE" + "_SECRET=\"" + fixtureSecret(20) + "\"", "api-key-assignment"},
		{"env colon form", "service_" + "token: " + fixtureSecret(20), "api-key-assignment"},
		{"env after a non-word char", "(MY" + "_API_KEY=" + fixtureSecret(20) + ")", "api-key-assignment"},
		{"bare openai key", "key is s" + "k-" + strings.Repeat("A1", 20), "provider-key"},
		{"bare openai project key", "s" + "k-proj-" + strings.Repeat("Ab_-9", 8), "provider-key"},
		{"bare anthropic key", "s" + "k-ant-api03-" + strings.Repeat("Zy_-8", 8), "provider-key"},
		{"jwt alone", fixtureJWT(), "jwt"},
		{"jwt after a header name", "Cookie: session=" + fixtureJWT() + "; path=/", "jwt"},
		{"jwt in a json value", "{\"id_token\":\"" + fixtureJWT() + "\"}", "jwt"},
	}
	for _, c := range cases {
		got := Matching("before\n" + c.text + "\nafter")
		found := false
		for _, g := range got {
			if g == c.class {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: Matching = %v, want it to name %q", c.name, got, c.class)
		}
	}
}

// TestWidenedClassesStayQuietOnOrdinaryText: names and shapes that look near a credential
// but are not one must stay clean, or the reducer and the corpus vet refuse good output.
func TestWidenedClassesStayQuietOnOrdinaryText(t *testing.T) {
	clean := []struct{ name, text string }{
		{"token limit setting", "MAX_TOKENS=4096"},
		{"long numeric token limit", "MAX_TOKENS=4096 CONTEXT_TOKENS=131072"},
		{"token counter", "token_count: 12"},
		{"password prose", "the password field is required"},
		{"secret prose", "the secret is stored elsewhere and the token expires"},
		{"git sha", "commit 3f786850e387550fdab836ed7e6dc881de23001b"},
		{"uuid", "request 123e4567-e89b-12d3-a456-426614174000 done"},
		{"base64 blob", "blob " + strings.Repeat("QUJDREVGR0hJSktM", 4) + "=="},
		{"dotted words", "pkg.subpkg.helper.function_name called"},
		{"go test line", "ok  \tgithub.com/example/pkg/sub\t0.412s\n--- FAIL: TestTokenizer (0.00s)"},
		{"scikit learn", "s" + "k-learn"},
		{"scikit learn requirement", "scikit-learn==1.4.2 and s" + "k-learn-compatible-estimators-guide"},
		{"risk word", "risk-assessment-and-mitigation-plan-for-release"},
		{"secret dir path", "/tmp/secret_dir/file.txt"},
		{"token dir path", "cd /var/lib/token_store && ls"},
		{"tokenizer name", "tokenizer: sentencepiece-model-version-two"},
		{"password field flag", "PASSWORD_FIELD_REQUIRED=true"},
	}
	for _, c := range clean {
		if got := Matching(c.text); got != nil {
			t.Errorf("%s: Matching(%q) = %v, want nil", c.name, c.text, got)
		}
	}
}
