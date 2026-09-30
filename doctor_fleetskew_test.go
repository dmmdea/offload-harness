package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// Fleet version skew in doctor (security standard L0, register R-06): one row
// per remote, OK / SKEW / UNKNOWN / UNREACHABLE; nothing at all without
// remotes. Informational only.
func TestDoctorFleetSkewRows(t *testing.T) {
	cfg := config.Default()
	cfg.DelegateRemotes = []string{"http://192.0.2.1:18811", "http://192.0.2.2:18811", "http://192.0.2.3:18811", "http://192.0.2.4:18811"}
	answers := map[string]struct {
		v   string
		err error
	}{
		"http://192.0.2.1:18811": {"1.2.3", nil},
		"http://192.0.2.2:18811": {"1.2.2", nil},
		"http://192.0.2.3:18811": {"", nil},
		"http://192.0.2.4:18811": {"", errors.New("connection refused")},
	}
	var b strings.Builder
	writeFleetSkewSection(&b, cfg, "1.2.3", func(base string) (string, error) {
		a := answers[base]
		return a.v, a.err
	})
	out := b.String()
	for _, want := range []string{
		"fleet versions (this binary 1.2.3):",
		"OK           http://192.0.2.1:18811 1.2.3",
		"SKEW         http://192.0.2.2:18811 runs 1.2.2 (this binary 1.2.3)",
		"UNKNOWN      http://192.0.2.3:18811",
		"UNREACHABLE  http://192.0.2.4:18811 — connection refused",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor fleet rows lack %q:\n%s", want, out)
		}
	}
	cfg.DelegateRemotes = nil
	b.Reset()
	writeFleetSkewSection(&b, cfg, "1.2.3", func(string) (string, error) { t.Fatal("no remotes: nothing to read"); return "", nil })
	if b.Len() != 0 {
		t.Fatalf("no remotes must print nothing, got %q", b.String())
	}
}
