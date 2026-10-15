package compeval

import (
	"strings"
	"testing"
)

// The exported redactor must be the harvest's own, not a lookalike: whatever the vet refuses,
// it redacts, and what it leaves behind passes the vet.

func TestExportedRedactorRedactsEveryVetClass(t *testing.T) {
	// Built at run time so no tracked file carries a credential- or identity-shaped literal.
	samples := map[string]string{
		"email":              "mail " + "someone" + "@" + "example.invalid" + " now",
		"bearer-token":       "Authorization: Bearer " + strings.Repeat("a", 24),
		"api-key-assignment": "api" + "_key=" + strings.Repeat("b", 20),
		"aws-access-key":     "id " + "AKIA" + strings.Repeat("A", 16),
		"github-token":       "tok " + "ghp_" + strings.Repeat("c", 24),
		"private-key-block":  "-----BEGIN " + "RSA PRIVATE KEY-----\nMIIB\n-----END " + "RSA PRIVATE KEY-----",
	}
	for _, p := range piiPatterns {
		if _, ok := samples[p.name]; !ok {
			t.Fatalf("vet class %q has no sample here; a class added to the vet table must be covered by this test", p.name)
		}
	}
	for class, in := range samples {
		if got := PIIClasses(in); !contains(got, class) {
			t.Fatalf("sample for %q does not trip the vet (got %v): fix the sample", class, got)
		}
		x := NewRedactor()
		out := x.Redact(in)
		if left := PIIClasses(out); len(left) != 0 {
			t.Errorf("class %q: %v still match after redaction: %q", class, left, out)
		}
		if x.Hits()[class] < 1 {
			t.Errorf("class %q: no hit counted", class)
		}
	}
}

func TestExportedRedactorIsStableAndKeepsDistinctValuesDistinct(t *testing.T) {
	a, b := "one"+"@"+"example.invalid", "two"+"@"+"example.invalid"
	x := NewRedactor()
	first, again, other := x.Redact(a), x.Redact(a), x.Redact(b)
	if first != again {
		t.Fatalf("same value redacted differently: %q vs %q", first, again)
	}
	if first == other {
		t.Fatalf("two different values collapsed to one placeholder: %q", first)
	}
	if got := NewRedactor().Redact(a); got != first {
		t.Fatalf("a fresh redactor numbered differently (%q vs %q): output must be reproducible", got, first)
	}
}

func TestExportedRedactorHitsIsACopy(t *testing.T) {
	x := NewRedactor()
	x.Redact("one" + "@" + "example.invalid")
	h := x.Hits()
	h["email"] = 99
	if x.Hits()["email"] != 1 {
		t.Fatalf("mutating the returned map changed the redactor's own count")
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
