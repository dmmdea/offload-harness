package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewAllowlistRejectsNonBareHosts(t *testing.T) {
	for _, bad := range []string{
		"https://example.com", // scheme
		"example.com:8080",    // port
		"example.com/path",    // path
		"user@example.com",    // userinfo
		`\\host\share`,        // UNC
		"ex*.com",             // mid-label wildcard
		"*",                   // bare star
		"*.",                  // empty wildcard
		"*.com",               // whole-TLD wildcard (single-label base)
		"allow_ed.com",        // underscore (non-LDH)
		"аllowed.com",         // Cyrillic homograph (non-ASCII)
	} {
		if _, err := NewAllowlist([]string{bad}); err == nil {
			t.Errorf("NewAllowlist(%q) = nil err, want rejection", bad)
		}
	}
}

func TestAllowlistExactMatchAndCanonicalization(t *testing.T) {
	a, err := NewAllowlist([]string{"Allowed.COM", " example.org "})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"allowed.com":          true,
		"Allowed.COM":          true, // case-folded
		"allowed.com.":         true, // trailing FQDN-root dot
		"ALLOWED.COM.":         true,
		"example.org":          true,
		"evil.com":             false,
		"allowed.com.evil.com": false, // suffix confusion
		"evil.com.allowed.com": false, // prefix confusion
		"sub.allowed.com":      false, // bare entry = exact, NO implicit subdomain
		"xallowed.com":         false,
	}
	for host, want := range cases {
		if got := a.permits(host); got != want {
			t.Errorf("permits(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestAllowlistWildcardBoundaryAnchored(t *testing.T) {
	a, err := NewAllowlist([]string{"*.wild.com"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"a.wild.com":        true,
		"deep.sub.wild.com": true,
		"A.WILD.COM":        true,  // case-folded
		"wild.com":          false, // apex NOT matched by *.
		"evil-wild.com":     false, // requires a real label boundary
		"wild.com.evil.com": false,
		"notwild.com":       false,
	}
	for host, want := range cases {
		if got := a.permits(host); got != want {
			t.Errorf("permits(%q) [*.wild.com] = %v, want %v", host, got, want)
		}
	}
}

func TestAllowlistRejectsNonASCIIAndHidden(t *testing.T) {
	a, err := NewAllowlist([]string{"allowed.com"})
	if err != nil {
		t.Fatal(err)
	}
	// homograph / hidden-char / non-LDH hosts must NEVER match the ASCII entry.
	for _, h := range []string{
		"аllowed.com",   // Cyrillic 'а' + llowed.com
		"allow​ed.com",  // zero-width space
		"allowed.com ",  // line separator
		"allowed_com",   // underscore
		"allowed com",   // space
		"::1",           // IPv6 literal (colon)
		"allowed.com@x", // embedded '@'
	} {
		if a.permits(h) {
			t.Errorf("permits(%q) = true, want false (non-ASCII/non-LDH must be rejected)", h)
		}
	}
}

func TestAllowlistIPv4LiteralAllowed(t *testing.T) {
	a, err := NewAllowlist([]string{"93.184.216.34"})
	if err != nil {
		t.Fatal(err)
	}
	if !a.permits("93.184.216.34") {
		t.Error("public IPv4 literal entry should match itself")
	}
	if a.permits("93.184.216.35") {
		t.Error("a different IPv4 must not match")
	}
}

func TestPolicyFetchDefaultDenyAll(t *testing.T) {
	for _, unattended := range []bool{false, true} {
		p := NewPolicy(unattended, nil) // no egress configured => deny-all
		if d, _ := p.Decide(Action{Kind: ActFetch, Path: "example.com"}); d != Deny {
			t.Errorf("default ActFetch (unattended=%v) = %q, want deny", unattended, d)
		}
	}
}

func TestPolicyFetchAllowlistGate(t *testing.T) {
	allow, _ := NewAllowlist([]string{"example.com"})
	p := NewPolicyWithEgress(true, nil, allow)
	if d, _ := p.Decide(Action{Kind: ActFetch, Path: "example.com"}); d != Allow {
		t.Error("allowlisted host should be allowed")
	}
	if d, _ := p.Decide(Action{Kind: ActFetch, Path: "evil.com"}); d != Deny {
		t.Error("non-allowlisted host should be denied")
	}
}

// Fetch is binary allow/deny — the allowlist IS the pre-authorization, so it must
// never resolve to Ask (which would deny-and-queue unattended, contradicting the
// "this host is allowed" contract the operator set).
func TestPolicyFetchNeverAsks(t *testing.T) {
	allow, _ := NewAllowlist([]string{"example.com"})
	p := NewPolicyWithEgress(false /* attended */, nil, allow)
	if d, _ := p.Decide(Action{Kind: ActFetch, Path: "example.com"}); d != Allow {
		t.Errorf("attended allowlisted fetch = %q, want allow (never ask)", d)
	}
}

func TestPolicyFetchAudited(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	allow, _ := NewAllowlist([]string{"example.com"})
	p := NewPolicyWithEgress(true, NewAuditLog(auditPath), allow)
	p.Decide(Action{Kind: ActFetch, Path: "example.com"}) // allow
	p.Decide(Action{Kind: ActFetch, Path: "evil.com"})    // deny
	b, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"kind":"fetch"`) {
		t.Errorf("fetch decisions not tagged kind=fetch: %s", s)
	}
	if !strings.Contains(s, `"decision":"allow"`) || !strings.Contains(s, `"decision":"deny"`) {
		t.Errorf("both allow and deny fetch decisions should be audited: %s", s)
	}
}
