//go:build linux

package vllmseat

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The launcher decisions of seat_fg.sh that only a run can prove (B-54: the KV pool pin and the context length; B-55: the
// address the cache-server share is mounted at) are driven by setup/templates/vllm-seat/seat_fg.kv-and-store.tests.sh. It runs
// the real launcher start to finish from a scratch copy, against stand-ins for every command that would touch the box, and
// reads the argument list vllm was exec'd with, the mount command line, the printed lines and the status file. One run serves
// every test here; each test names the cases of its own leg, because "ALL PASS" says nothing about a case that was deleted.
var (
	kvStoreRunOnce sync.Once
	kvStoreOut     string
	kvStoreErr     error
)

func kvStoreShellRun(t *testing.T) string {
	t.Helper()
	kvStoreRunOnce.Do(func() {
		script := filepath.Join("..", "..", "setup", "templates", "vllm-seat", "seat_fg.kv-and-store.tests.sh")
		var out bytes.Buffer
		cmd := exec.Command("bash", script) // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command — a repository script, a fixed argument list
		cmd.Stdout, cmd.Stderr = &out, &out
		kvStoreErr = cmd.Run()
		kvStoreOut = out.String()
	})
	if strings.Contains(kvStoreOut, "SKIP") {
		t.Skip(strings.TrimSpace(kvStoreOut))
	}
	if kvStoreErr != nil || !strings.Contains(kvStoreOut, "ALL PASS") {
		t.Fatalf("seat launcher KV-pool / store-address behavior (err %v):\n%s", kvStoreErr, kvStoreOut)
	}
	return kvStoreOut
}

func requirePassLines(t *testing.T, out string, want []string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, "PASS "+w+"\n") {
			t.Errorf("the shell test no longer reports %q:\n%s", "PASS "+w, out)
		}
	}
}

// TestSeatLauncherAutoFitsTheContextToAPinnedPoolBelowTheKnob (register B-54). A card that holds ~2.2 GB of desktop graphics
// memory gave pool 1.95 GiB, floored to 2.0, and vLLM refused every start because 163,840 tokens need 2.66 GiB; llama-swap
// restarted the seat on every request (about 98 failed starts in two days). With SEAT_KV_NEED_GIB set and the pinned pool below
// it the launcher passes --max-model-len -1, which makes vLLM pick the largest length that fits the pinned pool.
func TestSeatLauncherAutoFitsTheContextToAPinnedPoolBelowTheKnob(t *testing.T) {
	out := kvStoreShellRun(t)
	requirePassLines(t, out, []string{
		"knob set and the pinned pool below it: --max-model-len -1, one auto-fit line, the pin banner still printed",
		"knob set and the pool above it: SEAT_MAX_LEN kept, nothing printed about it",
		"knob unset and the pool floored: SEAT_MAX_LEN kept and a WARNING names SEAT_KV_NEED_GIB",
		"knob unset and the pool NOT floored: no warning",
		"a non-number knob is a warning and is ignored (the floored-pool warning follows)",
		"knob value '2,66' is not a number and is ignored",
		"knob value '1.2.3' is not a number and is ignored",
		"knob value '.5' is not a number and is ignored",
		"knob value '5.' is not a number and is ignored",
		"knob value '-1' is not a number and is ignored",
		"knob value '2.66 ' is not a number and is ignored",
		"a pool exactly at the knob is enough: SEAT_MAX_LEN kept",
		"a pool computed (not floored) below the knob also auto-fits",
		"no pin (SEAT_KV_HEADROOM_GIB unset): the knob changes nothing",
		"SEAT_MAX_LEN empty: the 131072 default is what the auto-fit line names",
		"SEAT_MAX_LEN empty and the pool enough: the 131072 default is kept",
	})
}
