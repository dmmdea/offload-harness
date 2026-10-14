package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// gpu_on_overdue (plan P11, ADR 0085) is what the harness does about a lease that outlived its
// window while somebody waits for its cards. Unset keeps the behaviour from before the key existed
// (nobody is asked); "request" asks the lease's holder to yield; "hold" says so in words. Load
// installs it where every holder's tick reads it.

func writeOnOverdueConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	full := `{"state_dir": "` + filepath.ToSlash(dir) + `"` + body + `}`
	if err := os.WriteFile(p, []byte(full), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func resetOnOverdue(t *testing.T) {
	t.Helper()
	gpulease.SetDefaultOnOverdue("")
	t.Cleanup(func() { gpulease.SetDefaultOnOverdue("") })
}

func TestGPUOnOverdueUnsetKeepsTheBehaviourFromBeforeTheKey(t *testing.T) {
	resetOnOverdue(t)
	if got := Default().OnOverduePolicy(); got != gpulease.OnOverdueUnset {
		t.Fatalf("the default policy = %q, want the unset one (nobody is asked)", got)
	}
	// An earlier load that asked for request must not leak into a load that does not say.
	if _, err := Load(writeOnOverdueConfig(t, `, "gpu_on_overdue": "request"`)); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(writeOnOverdueConfig(t, ``)); err != nil {
		t.Fatal(err)
	}
	if got := gpulease.InstalledOnOverdue(); got != gpulease.OnOverdueUnset {
		t.Fatalf("a config that does not set the key installs %q, want the unset policy", got)
	}
}

func TestGPUOnOverdueRequestAndHoldInstall(t *testing.T) {
	for raw, want := range map[string]gpulease.OnOverdue{"request": gpulease.OnOverdueRequest, "hold": gpulease.OnOverdueHold} {
		resetOnOverdue(t)
		c, err := Load(writeOnOverdueConfig(t, `, "gpu_on_overdue": "`+raw+`"`))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got := c.OnOverduePolicy(); got != want {
			t.Errorf("%s: the config reads %q, want %q", raw, got, want)
		}
		if got := gpulease.InstalledOnOverdue(); got != want {
			t.Errorf("%s: Load installed %q, want %q", raw, got, want)
		}
	}
}

// The repo's validators trim and lower-case ("a copy-pasted \" Always\" is accepted").
func TestGPUOnOverdueIsTrimmedAndLowerCased(t *testing.T) {
	resetOnOverdue(t)
	c, err := Load(writeOnOverdueConfig(t, `, "gpu_on_overdue": " Request "`))
	if err != nil {
		t.Fatalf("a copy-pasted value with a space and a capital is valid: %v", err)
	}
	if c.OnOverduePolicy() != gpulease.OnOverdueRequest || gpulease.InstalledOnOverdue() != gpulease.OnOverdueRequest {
		t.Fatalf("read %q, installed %q; both want request", c.OnOverduePolicy(), gpulease.InstalledOnOverdue())
	}
}

// Anything else is refused by name, and because a one-shot verb goes on after a load error (and
// Load installs on every exit), the value that gets installed is the one that asks nobody.
func TestGPUOnOverdueRefusesUnknownValueAndInstallsHold(t *testing.T) {
	for _, raw := range []string{"yield", "kill", "takeover", "reques", "true"} {
		resetOnOverdue(t)
		c, err := Load(writeOnOverdueConfig(t, `, "gpu_on_overdue": "`+raw+`"`))
		if err == nil {
			t.Errorf("%q was accepted: there is no automatic mode, and a typo must not pass", raw)
			continue
		}
		for _, want := range []string{"gpu_on_overdue", raw, "request", "hold"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal of %q must name %q: %v", raw, want, err)
			}
		}
		if got := gpulease.InstalledOnOverdue(); got != gpulease.OnOverdueHold {
			t.Errorf("%q: the policy installed after the refusal is %q, want hold (never the permissive direction)", raw, got)
		}
		if got := c.OnOverduePolicy(); got != gpulease.OnOverdueHold {
			t.Errorf("%q: the value Load returns reads %q, want hold", raw, got)
		}
	}
}

// loadArmed returns early when fleet_queue_holder is refused; the policy must be installed before
// that, or a config with two faults would run the permissive default.
func TestGPUOnOverdueIsInstalledEvenWhenAnotherKeyIsRefused(t *testing.T) {
	resetOnOverdue(t)
	_, err := Load(writeOnOverdueConfig(t, `, "gpu_on_overdue": "hold", "fleet_queue_holder": "http://203.0.113.9:8080"`))
	if err == nil || !strings.Contains(err.Error(), "fleet_queue_holder") {
		t.Fatalf("setup: the fleet_queue_holder must be refused: %v", err)
	}
	if got := gpulease.InstalledOnOverdue(); got != gpulease.OnOverdueHold {
		t.Fatalf("installed %q, want hold: the install must not sit behind another key's refusal", got)
	}
	_, err = Load(writeOnOverdueConfig(t, `, "gpu_on_overdue": "request", "fleet_queue_holder": "http://203.0.113.9:8080"`))
	if err == nil {
		t.Fatal("setup: the fleet_queue_holder must be refused")
	}
	if got := gpulease.InstalledOnOverdue(); got != gpulease.OnOverdueRequest {
		t.Fatalf("installed %q, want request", got)
	}
}

func TestOnOverduePolicyOfAHandBuiltConfig(t *testing.T) {
	for raw, want := range map[string]gpulease.OnOverdue{
		"": gpulease.OnOverdueUnset, "request": gpulease.OnOverdueRequest, " REQUEST ": gpulease.OnOverdueRequest,
		"hold": gpulease.OnOverdueHold, "kill": gpulease.OnOverdueHold,
	} {
		if got := (Config{GPUOnOverdue: raw}).OnOverduePolicy(); got != want {
			t.Errorf("%q reads %q, want %q", raw, got, want)
		}
	}
}
