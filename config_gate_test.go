package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// badEndpointSource loads a real config file whose endpoint dials the discard
// port and returns the Source the CLI entry points would see. Going through
// config.LoadWithSource rather than a hand-built Source is the point: these
// tests assert on the text a real refusal produces, not on a stand-in.
func badEndpointSource(t *testing.T) config.Source {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"endpoint":"http://127.0.0.1:9"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, src := config.LoadWithSource(p)
	if src.LoadErr == nil {
		t.Fatalf("precondition: a :9 endpoint must fail validation")
	}
	return src
}

// TestFleetServeRefusesAnInvalidConfig (review of #361, blocker 2a): a fleet
// NODE that would dial a dead endpoint must not join the fleet. Every other
// entry point degrades and warns; this one cannot, because a node that accepts
// dispatches it can never serve turns every delegator's placement decision into
// a wasted wall.
//
// The decision is a function so the test drives THE THING THAT DECIDES, not a
// process: an exit-code test on a spawned binary proves the same fact far more
// slowly and tells you nothing about why.
func TestFleetServeRefusesAnInvalidConfig(t *testing.T) {
	err := fleetServeConfigGate(badEndpointSource(t))
	if err == nil {
		t.Fatal("fleet-serve must refuse to start on a config that failed validation")
	}
	for _, want := range []string{"endpoint", "127.0.0.1:9", ":9"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q must name %q — the operator has to know which key", err, want)
		}
	}
	// A clean load, a missing file and no file at all are all serveable: a fresh
	// box with no config is a legitimate node, and that path already warns.
	for _, src := range []config.Source{
		{Path: "C:/real.json"},
		{Path: "C:/typo.json", NotFound: true},
		{},
	} {
		if err := fleetServeConfigGate(src); err != nil {
			t.Errorf("fleet-serve must start for %+v; got %v", src, err)
		}
	}
}

// TestDoctorPrintsTheValidationErrorVerbatim (review of #361, addendum): the
// refusal text is the only thing that names the key AND the value. doctor kept
// the Source and printed only the generic one-liner, so the descriptive error
// never reached the operator through the one verb they run to find out what is
// wrong — and doctor exited 0 on a config the loader had refused.
func TestDoctorPrintsTheValidationErrorVerbatim(t *testing.T) {
	src := badEndpointSource(t)
	var buf bytes.Buffer
	tainted := doctorConfigRow(src, &buf)
	if !tainted {
		t.Fatal("a config that failed validation must make doctor exit non-zero")
	}
	got := buf.String()
	for _, want := range []string{"FAIL", "endpoint", "127.0.0.1:9", ":9"} {
		if !strings.Contains(got, want) {
			t.Errorf("doctor's config row must contain %q; got:\n%s", want, got)
		}
	}
	// A clean source prints the ordinary disclosure and nothing else.
	var ok bytes.Buffer
	if doctorConfigRow(config.Source{Path: "C:/real.json"}, &ok) {
		t.Fatal("a loaded config is not a doctor failure")
	}
	if strings.Contains(ok.String(), "FAIL") {
		t.Errorf("no FAIL row expected for a loaded config; got %q", ok.String())
	}
}

// TestLoadErrDisclosureIsTrue (review of #361, blocker 2c): both disclosures
// claimed "running on BUILT-IN DEFAULTS; machine bindings are inactive" for a
// file that FAILED validation. That is false — Load returns the file's settings
// with only the five composite keys stripped, so the process runs on the file,
// which is precisely why the refusal matters. Saying "defaults" sent an operator
// looking for a path problem while the real one was a named key in the file they
// were already reading.
func TestLoadErrDisclosureIsTrue(t *testing.T) {
	src := badEndpointSource(t)

	var warn bytes.Buffer
	if !config.WarnOnDefaults(src, &warn) {
		t.Fatal("a failed validation must still warn")
	}
	if strings.Contains(warn.String(), "BUILT-IN DEFAULTS") {
		t.Errorf("the load-error warning must not claim built-in defaults:\n%s", warn.String())
	}
	for _, want := range []string{"endpoint", ":9", src.Path} {
		if !strings.Contains(warn.String(), want) {
			t.Errorf("the load-error warning must name %q; got:\n%s", want, warn.String())
		}
	}

	line := config.SourceLine(src)
	if strings.Contains(line, "BUILT-IN DEFAULTS") {
		t.Errorf("doctor's config line must not claim built-in defaults: %q", line)
	}
	for _, want := range []string{src.Path, "endpoint"} {
		if !strings.Contains(line, want) {
			t.Errorf("doctor's config line must name %q; got %q", want, line)
		}
	}
}
