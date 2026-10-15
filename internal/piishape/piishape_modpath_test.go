package piishape

import "testing"

// A Go module path pinned to a version (golang.org/toolchain@v0.0.1-go1.27.2.linux-amd64) reads like an
// address to the email pattern: a local part, an @, dotted labels and a final run of letters. Every stack
// trace and module-cache path in a Go build or test log carries one, so the reducer would keep those logs
// raw and the harvest would rewrite them. A domain cannot start with a v and a digit; a module version
// always does.
func TestAGoModuleVersionIsNotAnAddress(t *testing.T) {
	quiet := []string{
		"\t/opt/go/pkg/mod/golang.org/toolchain" + "@" + "v0.0.1-go1.27.2.linux-amd64/src/runtime/proc.go:283 +0x28",
		"C:/go/pkg/mod/golang.org/toolchain" + "@" + "v0.0.1-go1.27.2.windows-amd64/src/testing/testing.go:2961 +0x34a",
		"github.com/example/mod" + "@" + "v1.2.3-rc.1.linux",
	}
	for _, s := range quiet {
		if got := Matching(s); got != nil {
			t.Errorf("Matching(%q) = %v, want nil: a module version is not an address", s, got)
		}
	}
	addresses := []string{
		"dev" + "@" + "example.invalid",
		"ops" + "@" + "vendor.example",
		"ops" + "@" + "V2mail.example",
		"build bot <ci" + "@" + "v-team.example> pushed",
	}
	for _, s := range addresses {
		got := Matching(s)
		if len(got) != 1 || got[0] != "email" {
			t.Errorf("Matching(%q) = %v, want [email]", s, got)
		}
	}
}
