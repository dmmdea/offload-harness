package main

import (
	"os/exec"
	"runtime"
	"testing"
)

// TestInstallShDryRunRules runs setup/install.tests.sh: install.sh's --llama-bin rule (required
// on every tier but rk3588, whose template has no llama.cpp entry) and its --rknpu-home
// pass-through, driven through --dry-run against a stub harness binary. Linux only, like
// install.sh itself; the script skips itself when jq is absent.
func TestInstallShDryRunRules(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("install.sh is the Linux installer")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	out, err := exec.Command(bash, "setup/install.tests.sh").CombinedOutput()
	if err != nil {
		t.Fatalf("setup/install.tests.sh failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}
