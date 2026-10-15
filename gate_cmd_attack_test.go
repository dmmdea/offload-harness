package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// brokenWriter is an output stream that always fails, like a closed pipe.
type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("write: broken pipe") }

// A failed write of the answer is a gate failure like any other: exit 2 with a "gate:" line, never
// the bare exit 1 that is reserved for "the verdict is not ADOPT".
func TestGateCmdWriteErrorsExitWithTheGateCode(t *testing.T) {
	f := freezeSuperiority(t, 16, nil)
	input := gateWrite(t, f.dir, "input.json", map[string]any{
		"baseline_hash": f.hash,
		"candidate":     gateArm("cand", 16, 1),
		"incumbent":     gateArm("inc", 16, 0),
		"judge":         map[string]string{"kind": "deterministic", "identity": "acceptance-dsl"},
	})
	freezeOut := filepath.Join(f.dir, "fresh-baseline.json")
	prereg := gateWrite(t, f.dir, "prereg2.json", gatePreregFor(16))
	for name, args := range map[string][]string{
		"schema":  {"schema"},
		"check":   {"check", "--baseline", f.baseline},
		"verdict": {"verdict", "--baseline", f.baseline, "--input", input},
		"freeze":  {"freeze", "--prereg", prereg, "--corpus", f.corpus, "--out", freezeOut},
	} {
		err := gateMain(args, brokenWriter{}, io.Discard)
		if err == nil {
			t.Errorf("%s: a failed write must be an error", name)
			continue
		}
		code, line := exitStatus(err)
		if code != 2 || !strings.HasPrefix(line, "gate: ") {
			t.Errorf("%s: want exit 2 and a \"gate: \" line, got %d %q", name, code, line)
		}
	}
}

// The other verbs reached from the dispatcher parse their flags with flag.ExitOnError, which exits
// 0 and prints the usage for -h. The gate matches: -h is a request for help, not a failure, and
// a bad flag or a missing argument stays exit 2.
func TestGateCmdHelpExitsZeroAndOddInputsExitTwo(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"help"}, {"verdict", "-h"}, {"freeze", "-h"}, {"check", "-h"}, {"schema", "-h"}} {
		out, errs, code := runGateCmd(t, args...)
		if code != 0 {
			t.Errorf("%v: want exit 0, got %d (stderr %q)", args, code, errs)
		}
		if !strings.Contains(out+errs, "usage") && !strings.Contains(out+errs, "Usage") {
			t.Errorf("%v: -h must print a usage, got stdout %q stderr %q", args, out, errs)
		}
	}

	dir := t.TempDir()
	w := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	missing := filepath.Join(dir, "missing")
	for name, args := range map[string][]string{
		"no arguments":           {},
		"unknown subcommand":     {"bogus"},
		"verdict without flags":  {"verdict"},
		"verdict missing files":  {"verdict", "--baseline", missing, "--input", missing + "2"},
		"check an empty file":    {"check", "--baseline", w("empty.json", "")},
		"check a null document":  {"check", "--baseline", w("null.json", "null")},
		"check a directory":      {"check", "--baseline", dir},
		"verdict unknown flag":   {"verdict", "--bogus"},
		"verdict extra argument": {"verdict", "--baseline", "a", "--input", "b", "extra"},
		"dangling --config":      {"--config"},
		"freeze missing corpus":  {"freeze", "--prereg", w("p.json", "{}"), "--corpus", missing, "--out", filepath.Join(dir, "o.json")},
	} {
		out, errs, code := runGateCmd(t, args...)
		if code != 2 {
			t.Errorf("%s: want exit 2, got %d (stdout %q stderr %q)", name, code, out, errs)
		}
		if out != "" {
			t.Errorf("%s: a refusal prints no result, got %q", name, out)
		}
	}
}
