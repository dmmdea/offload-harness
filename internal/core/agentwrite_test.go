package core

import (
	"strings"
	"testing"
)

// TestValidateWriteRootAcceptsOnlySafeRelativeDirs. The rules are the strictest
// platform's on EVERY platform, because a contract is validated on the
// delegator and again on a node that may be a different operating system.
func TestValidateWriteRootAcceptsOnlySafeRelativeDirs(t *testing.T) {
	// "a//b" and "a/./b" are in the OK set deliberately: path.Clean normalizes
	// them to "a/b", which is a safe path. Refusing a caller's redundant
	// separator would be pedantry, not safety.
	ok := []string{"", ".", "work", "internal/pipeline", "a/b/c", "./work", "a//b", "a/./b"}
	for _, r := range ok {
		if err := ValidateWriteRoot(r); err != nil {
			t.Errorf("write_root %q refused: %v", r, err)
		}
	}
	bad := map[string]string{
		"..":           "escapes",
		"../out":       "escapes",
		"a/../../out":  "escapes",
		"/etc":         "RELATIVE",
		"C:/Windows":   "RELATIVE",
		`C:x`:          "RELATIVE",
		`\\host\share`: "RELATIVE",
		"a/.git":       ".git",
		"a/.GIT":       ".git",
		"a/.git.":      "trailing",
		"nul":          "device",
		"a/nul.d":      "device",
		"a/com1":       "device",
		"a/trailing ":  "trailing",
		"a/trailing.":  "trailing",
		"a\x00b":       "NUL",
	}
	for r, want := range bad {
		err := ValidateWriteRoot(r)
		if err == nil {
			t.Errorf("write_root %q was accepted; it must be refused", r)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("write_root %q refused with %q, want a reason mentioning %q", r, err.Error(), want)
		}
	}
}

// TestContractValidateRejectsABadWriteRoot: the contract-level door, so a bad
// write_root dies before the network on the delegator and again at decode on
// the node.
func TestContractValidateRejectsABadWriteRoot(t *testing.T) {
	c := AgentContract{SchemaVersion: AgentWireSchemaVersion, Goal: "g", WriteRoot: "../out"}
	if err := c.Validate(); err == nil {
		t.Fatal("a contract with an escaping write_root validated")
	}
	c.WriteRoot = "work"
	if err := c.Validate(); err != nil {
		t.Fatalf("a contract with a safe write_root was refused: %v", err)
	}
}

// TestDiffAcceptanceVerbsParse: the unfalsifiable shapes are parse ERRORS, the
// same rule the rest of the DSL keeps.
func TestDiffAcceptanceVerbsParse(t *testing.T) {
	for _, s := range []string{"diff_touches:internal/", "diff_max_files:1", "diff_max_files:0"} {
		if _, err := ParseAcceptanceCheck(s); err != nil {
			t.Errorf("%q should parse: %v", s, err)
		}
	}
	for _, s := range []string{"diff_touches:", "diff_max_files:", "diff_max_files:-1", "diff_max_files:two"} {
		if _, err := ParseAcceptanceCheck(s); err == nil {
			t.Errorf("%q parsed; it verifies nothing or is malformed and must be refused", s)
		}
	}
	// A Windows-authored prefix must match a diff, whose paths are always
	// slash-separated.
	chk, err := ParseAcceptanceCheck(`diff_touches:internal\pipeline`)
	if err != nil {
		t.Fatal(err)
	}
	if chk.Arg != "internal/pipeline" {
		t.Errorf("prefix = %q, want it normalized to forward slashes", chk.Arg)
	}
}

// TestDiffAcceptanceVerbsEvalAgainstTheWriteSet, including the fail-closed
// posture on an empty one: a cap assertion that passes because nothing was
// written would make a contract that verified nothing read as verified.
func TestDiffAcceptanceVerbsEvalAgainstTheWriteSet(t *testing.T) {
	written := AgentWireResult{
		Output:    "done",
		DiffFiles: []string{"internal/pipeline/a.go", "internal/pipeline/a_test.go"},
	}
	empty := AgentWireResult{Output: "I would change internal/pipeline/a.go."}

	cases := []struct {
		check string
		res   AgentWireResult
		pass  bool
		want  string // fragment of the failure reason
	}{
		{"diff_touches:internal/pipeline", written, true, ""},
		{"diff_touches:cmd/", written, false, "no changed path starts with"},
		{"diff_touches:internal/pipeline", empty, false, "NO write set"},
		{"diff_max_files:2", written, true, ""},
		{"diff_max_files:1", written, false, "changed 2 files"},
		{"diff_max_files:0", written, false, "changed 2 files"},
		{"diff_max_files:5", empty, false, "NO write set"},
	}
	for _, tc := range cases {
		chk, err := ParseAcceptanceCheck(tc.check)
		if err != nil {
			t.Fatalf("%s: %v", tc.check, err)
		}
		pass, reason := chk.Eval(tc.res)
		if pass != tc.pass {
			t.Errorf("%s on %v: pass = %v, want %v (reason %q)", tc.check, tc.res.DiffFiles, pass, tc.pass, reason)
			continue
		}
		if !pass && !strings.Contains(reason, tc.want) {
			t.Errorf("%s: reason %q, want it to mention %q", tc.check, reason, tc.want)
		}
		if pass && reason != "" {
			t.Errorf("%s: a passing check must carry no reason, got %q", tc.check, reason)
		}
	}
}

// TestWriteRootSurvivesTheWireRoundTrip: the field is additive and the decoder
// is tolerant, so a node one release behind simply ignores it — but a node that
// speaks it must see it.
func TestWriteRootSurvivesTheWireRoundTrip(t *testing.T) {
	raw := `{"schema_version":1,"goal":"fix it","write_root":"work","output_schema":{"properties":{"answer":{"type":"string"}}}}`
	c, err := DecodeAgentContract(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if c.WriteRoot != "work" {
		t.Fatalf("write_root = %q after decode, want %q", c.WriteRoot, "work")
	}
	bad := `{"schema_version":1,"goal":"fix it","write_root":"../out"}`
	if _, derr := DecodeAgentContract(strings.NewReader(bad)); derr == nil {
		t.Fatal("the decoder accepted an escaping write_root — never trust the wire")
	}
}

// TestValidateWriteRootIsPlatformIndEpendent is the regression test for the
// defect CI found on 2026-09-14: the absolute/volume-qualified guard was built
// from filepath.IsAbs + filepath.VolumeName, which answer for the platform the
// BINARY was built for. On a Linux node those read "C:/Windows" as an ordinary
// relative directory, so the guard passed it — while the same contract was
// refused on a Windows delegator. A contract is validated on the delegator and
// again on the node, and those two can be different operating systems: a check
// that changes its mind between them is not a check.
//
// The assertions below use no filepath call, so they mean the same thing on
// every platform this ever compiles for.
func TestValidateWriteRootIsPlatformIndependent(t *testing.T) {
	windowsShapes := []string{
		`C:/Windows`,
		`C:\Windows`,
		`c:/windows`,
		`C:x`,
		`\host\share`,
		`//host/share`,
		`work:stream`, // an alternate data stream on NTFS
	}
	for _, r := range windowsShapes {
		if err := ValidateWriteRoot(r); err == nil {
			t.Errorf("write_root %q was accepted; a Windows-shaped absolute, drive-relative or ADS path must be refused on EVERY platform, not only the one whose filepath package recognizes it", r)
		}
	}
	posixShapes := []string{"/etc", "/", "//etc"}
	for _, r := range posixShapes {
		if err := ValidateWriteRoot(r); err == nil {
			t.Errorf("write_root %q was accepted; a POSIX absolute path must be refused on EVERY platform", r)
		}
	}
	// The shapes that must keep working, spelled with both separators — a
	// Windows-authored contract must run on a Linux node and vice versa.
	for _, r := range []string{"work", "internal/pipeline", `internal\pipeline`, "."} {
		if err := ValidateWriteRoot(r); err != nil {
			t.Errorf("write_root %q refused: %v", r, err)
		}
	}
}
