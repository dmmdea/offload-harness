package compeval

import (
	"strings"
	"testing"
)

// Fixtures are assembled at run time from pieces, so no literal in the tree is itself
// credential-shaped.

const (
	tailAlnum  = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	tailB64url = tailAlnum + "_-"
	tailPunct  = tailAlnum + "./+=@~:"
)

// tailFake returns n characters of alphabet, the same for the same seed.
func tailFake(seed, n int, alphabet string) string {
	var b strings.Builder
	x := uint32(seed)*2654435761 + 12345
	for i := 0; i < n; i++ {
		x = x*1664525 + 1013904223
		b.WriteByte(alphabet[int(x>>16)%len(alphabet)])
	}
	return b.String()
}

func tailJWT() string {
	return "ey" + "J" + tailFake(1, 20, tailB64url) + "." + tailFake(2, 30, tailB64url) + "." + tailFake(3, 40, tailB64url)
}

// anyRun reports the first run of n characters of secret that appears in out.
func anyRun(secret, out string, n int) (string, bool) {
	for i := 0; i+n <= len(secret); i++ {
		if strings.Contains(out, secret[i:i+n]) {
			return secret[i : i+n], true
		}
	}
	return "", false
}

type tailCase struct{ name, prefix, value, suffix string }

func tailCases() []tailCase {
	return []tailCase{
		{"access token jwt", "access_" + "token=", tailJWT(), ""},
		{"id token jwt colon", "id_" + "token: ", tailJWT(), ""},
		{"aws secret", "AWS_" + "SECRET_ACCESS_KEY=", "wJalrXUtnFEMI/K7MDENG/" + tailFake(4, 18, tailAlnum), ""},
		{"bearer with dot", "Authorization: Bearer ", tailFake(5, 1, tailAlnum) + "bcdefgh.ijklmnop.qrstuvwxyz", ""},
		{"bearer with slash", "Authorization: Bearer ", tailFake(6, 1, tailAlnum) + "bcdefgh/ijklmnop/qrstuvwxyz", ""},
		{"bearer with plus", "Authorization: Bearer ", tailFake(7, 1, tailAlnum) + "bcdefgh+ijklmnop+qrstuvwxyz==", ""},
		{"bearer with every mark", "Authorization: Bearer ", tailFake(8, 1, tailAlnum) + "ab/cd.ef+gh=ij@kl~mn:" + tailFake(9, 20, tailPunct), ""},
		{"secret slash", "SEC" + "RET=", "ab/" + "cdefghijklmnopqrstuvwx", ""},
		{"quoted value", "api_" + "key=\"", tailFake(10, 1, tailAlnum) + "b/c.d+e=f@g~h:" + tailFake(11, 16, tailPunct), "\", next"},
		{"url query", "https://host.invalid/x?" + "to" + "ken=", tailFake(12, 1, tailAlnum) + "b/c.d+e=f@g~h:" + tailFake(13, 16, tailPunct), "&page=2"},
	}
}

// TestRedactorLeavesNoTailOfAValue: after redaction no run of 8 characters of a credential value is
// left in the text, and the text around the value is kept.
func TestRedactorLeavesNoTailOfAValue(t *testing.T) {
	for _, c := range tailCases() {
		in := "before " + c.prefix + c.value + c.suffix + "\nafter line"
		out := NewRedactor().Redact(in)
		if run, found := anyRun(c.value, out, 8); found {
			t.Errorf("%s: a run of the value survived redaction (%q): %q", c.name, run, out)
		}
		if !strings.HasPrefix(out, "before ") || !strings.HasSuffix(out, "\nafter line") {
			t.Errorf("%s: redaction ate the surrounding text: %q", c.name, out)
		}
		if c.suffix != "" && !strings.Contains(out, c.suffix) {
			t.Errorf("%s: redaction ate the text after the value (%q): %q", c.name, c.suffix, out)
		}
		if left := PIIClasses(out); len(left) != 0 {
			t.Errorf("%s: %v still match after redaction: %q", c.name, left, out)
		}
	}
}

// A redactor that stops short of a value (the defect this file's first test pins) leaves its tail
// behind, and the vet cannot see a tail: ".payload.signature" matches no class. The residual gate
// must still notice, because the redactor knows which values it was handed.
func TestResidualGateCatchesATailTheVetCannotSee(t *testing.T) {
	for _, c := range tailCases() {
		x := NewRedactor()
		x.Redact("before " + c.prefix + c.value + c.suffix)
		// What the old redactor wrote: the head of the value replaced, the rest kept.
		cut := len(c.value) / 3
		leaky := "before [redacted-api-key-assignment-1]" + c.value[cut:] + c.suffix
		if tail := c.value[cut:]; len(tail) < 12 {
			t.Fatalf("%s: fixture tail is shorter than 12 characters", c.name)
		}
		got := x.Residual(leaky)
		if !contains(got, "value-tail") {
			t.Errorf("%s: the residual gate missed a surviving tail (got %v): %q", c.name, got, leaky)
		}
	}
}

// Clean output passes the gate, and a 12-character piece of an email address is not a credential
// value: a URL on the same domain must not refuse a harvest.
func TestResidualGateStaysQuietOnRedactedOutput(t *testing.T) {
	for _, c := range tailCases() {
		x := NewRedactor()
		out := x.Redact("before " + c.prefix + c.value + c.suffix + "\nafter line")
		if got := x.Residual(out); len(got) != 0 {
			t.Errorf("%s: the gate refused clean redacted output: %v", c.name, got)
		}
	}
	x := NewRedactor()
	out := x.Redact("mail " + "someone" + "@" + "example.invalid" + " and see https://example.invalid/page")
	if got := x.Residual(out); len(got) != 0 {
		t.Errorf("the gate refused a url on an email's domain: %v: %q", got, out)
	}
}

// The gate also reports the vet classes it always did.
func TestResidualGateStillReportsVetClasses(t *testing.T) {
	got := NewRedactor().Residual("tok " + "ghp_" + strings.Repeat("c", 24))
	if !contains(got, "github-token") {
		t.Errorf("Residual lost the vet classes: %v", got)
	}
}

// Full consumption makes a tail impossible: for any value that starts with a letter or digit and
// then carries any of the characters the value class allows, the redacted text holds no 12-character
// piece of the value and the gate reports nothing.
func TestWholeValueConsumptionLeavesNoTailForAnyValue(t *testing.T) {
	prefixes := []string{"api_" + "key=", "GITHUB_" + "TOKEN: ", "service-" + "secret = \"", "DB_PASS" + "WORD=", "x_" + "token='", "Authorization: Bearer ", "bearer  "}
	ends := []string{"", " tail", "\" tail", "', tail", "; tail", "&tail=1", "\ntail"}
	for seed := 0; seed < 400; seed++ {
		value := tailFake(seed, 1, tailAlnum) + tailFake(seed+1000, 15+seed%45, tailPunct)
		prefix, end := prefixes[seed%len(prefixes)], ends[seed%len(ends)]
		in := "head " + prefix + value + end
		x := NewRedactor()
		out := x.Redact(in)
		if run, found := anyRun(value, out, 12); found {
			t.Fatalf("seed %d: a 12-character piece of %q survived (%q): %q", seed, value, run, out)
		}
		if got := x.Residual(out); len(got) != 0 {
			t.Fatalf("seed %d: the gate reports %v on %q (from %q)", seed, got, out, in)
		}
	}
}

// A value that holds an earlier class's placeholder must not turn the placeholder text into a
// "credential piece" that refuses every later output.
func TestPlaceholderInsideAValueIsNotATail(t *testing.T) {
	x := NewRedactor()
	out := x.Redact("SEC" + "RET=ab/" + "someone" + "@" + "example.invalid" + "/xyz-more and mail " + "other" + "@" + "example.invalid")
	if got := x.Residual(out); len(got) != 0 {
		t.Fatalf("gate refused %q: %v", out, got)
	}
	again := x.Redact("later mail " + "third" + "@" + "example.invalid")
	if got := x.Residual(again); len(got) != 0 {
		t.Fatalf("gate refused ordinary redacted text %q: %v", again, got)
	}
}
