package main

// F35: `gpu --help` answered "unknown gpu subcommand" instead of usage, and `gpu --config <path> status`
// was read as a subcommand named "--config" (the top-level verb hoists a leading --config; the gpu verb
// group did not). The sub-verbs' own flag sets are ExitOnError, so `gpu status --help` already printed its
// flags and exited 0; this is the group in front of them.

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

func TestGPUHelpPrintsUsageOnStdoutAndSucceeds(t *testing.T) {
	for _, arg := range []string{"--help", "-h", "-help", "help"} {
		var err error
		stderr := captureStderr(t, func() {
			out := captureStdout(t, func() { err = runGPU([]string{arg}) })
			if err == nil {
				for _, want := range []string{"local-offload gpu", "status", "cards", "reserve", "release", "doctor", "owner-flags", "--config"} {
					if !strings.Contains(out, want) {
						t.Errorf("gpu %s: the usage lacks %q:\n%s", arg, want, out)
					}
				}
			}
		})
		if err != nil {
			t.Errorf("gpu %s must print usage and succeed (exit 0), got: %v", arg, err)
		}
		if strings.Contains(stderr, "unknown gpu subcommand") {
			t.Errorf("gpu %s answered as if it were an unknown subcommand:\n%s", arg, stderr)
		}
	}
	// Help after a config is still help.
	var err error
	out := captureStdout(t, func() { err = runGPU([]string{"--config", "x.json", "--help"}) })
	if err != nil || !strings.Contains(out, "reserve") {
		t.Errorf("gpu --config x --help: err=%v out=%q", err, out)
	}
}

func TestGPUUsageNamesEverySubcommandTheDispatcherServes(t *testing.T) {
	// The hidden holder is `gpu hold`; every other case of the dispatcher is documented in the usage.
	out := captureStdout(t, func() { _ = runGPU([]string{"--help"}) })
	for _, verb := range gpuVerbs {
		if verb == "hold" {
			continue
		}
		if !strings.Contains(out, "gpu "+verb) {
			t.Errorf("the usage does not name `gpu %s`:\n%s", verb, out)
		}
	}
}

func TestGPUConfigMayPrecedeTheSubcommand(t *testing.T) {
	cfg, m := leaseFixture(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("LOCAL_OFFLOAD_ORIGIN", "")
	l, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "held under the config the test names", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	useQuietStatus(t, nil, nil)
	for _, args := range [][]string{
		{"--config", cfg, "status", "--json"},
		{"--config=" + cfg, "status", "--json"},
		{"status", "--config", cfg, "--json"}, // the form that always worked
	} {
		var runErr error
		out := captureStdout(t, func() { runErr = runGPU(args) })
		if runErr != nil {
			t.Errorf("runGPU(%v): %v", args, runErr)
			continue
		}
		var doc struct {
			Held   bool   `json:"held"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Errorf("runGPU(%v): not one JSON document: %v\n%s", args, err, out)
			continue
		}
		if !doc.Held || !strings.Contains(doc.Reason, "held under the config the test names") {
			t.Errorf("runGPU(%v) did not read the config it was given (state root of the held lease): %+v", args, doc)
		}
	}
}

func TestGPUStillRefusesAnUnknownSubcommandAndABareGPU(t *testing.T) {
	if err := runGPU([]string{"bogus"}); err == nil || !strings.Contains(err.Error(), "unknown gpu subcommand") || !strings.Contains(err.Error(), "--help") {
		t.Errorf("an unknown subcommand is an error that points at --help: %v", err)
	}
	if err := runGPU(nil); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Errorf("a bare `gpu` is still an error with the usage line: %v", err)
	}
	// A dangling --config with nothing after it is not a subcommand either.
	if err := runGPU([]string{"--config", "x.json"}); err == nil {
		t.Error("`gpu --config x.json` names no subcommand")
	}
}

// gpuVerbs must be what runGPU dispatches: a verb added to the switch and not to the list would be missing
// from the usage this file checks against it.
func TestGPUVerbsListIsWhatTheDispatcherServes(t *testing.T) {
	src, err := os.ReadFile("gpu_cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(src), "\r\n", "\n")
	start := strings.Index(text, "func runGPU(args []string) error {")
	end := strings.Index(text, "const gpuUsageLine")
	if start < 0 || end < start {
		t.Fatal("cannot find runGPU in gpu_cmd.go")
	}
	var served []string
	for _, m := range regexp.MustCompile(`(?m)^\tcase "([a-z-]+)":$`).FindAllStringSubmatch(text[start:end], -1) {
		served = append(served, m[1])
	}
	want := append([]string(nil), gpuVerbs...)
	sort.Strings(served)
	sort.Strings(want)
	if strings.Join(served, ",") != strings.Join(want, ",") {
		t.Fatalf("runGPU serves %v, gpuVerbs lists %v", served, want)
	}
}
