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

// TestSeatLauncherMountsTheCacheServerAtAnAddressOnThePinnedNetwork (register B-55). The store host answers mDNS for its .local
// name from two responders: one pinned to its wired NIC (the right answer) and another agent that answers on every interface
// with that interface's own address (a Wi-Fi address on the same /24, container bridges, a VPN address). The name resolver
// returns ONE of them, so the mount landed on the Wi-Fi path (writes ~24 MB/s, so the launcher degraded L2 under
// SEAT_L2_MIN_MBPS), on an unreachable bridge address, or on the VPN address that refuses the pinned source: every start for
// three days ran without L2. The launcher now collects all the answers, mounts the first one inside the pinned source
// address's network, and mounts by name exactly as before whenever it cannot.
func TestSeatLauncherMountsTheCacheServerAtAnAddressOnThePinnedNetwork(t *testing.T) {
	out := kvStoreShellRun(t)
	requirePassLines(t, out, []string{
		"an off-network answer first and an on-network one second: the mount uses the on-network address, one line names every answer and the choice, the status file records it",
		"an explicit source address picks the address the same way",
		"the FIRST answer inside the network wins",
		"a share path below the share name is kept",
		"the helper fails: the mount is by name exactly as before, the reason is printed, the status line is unchanged",
		"the helper finds no answer: the mount is by name, the reason is printed",
		"the helper exits 0 with nothing usable: the mount is by name",
		"no answer on the source address's network: the mount is by name and the line lists what was heard",
		"lines that are not IPv4 addresses are ignored",
		"a helper that hangs is cut off at 3 s and the mount is by name",
		"a name that is not .local is not looked up",
		"an address literal is mounted as given",
		"an nfs mount is untouched",
		"no source pin: the mount is by name, as before, and nothing is looked up",
		"a chosen address that does not mount: the seat degrades, the message keeps its text and names the address",
		"the write-floor degrade keeps its text and names the address the share was mounted at",
		"a /28 pinned address: the answer inside the /28 is mounted, the one outside it (same /24) is passed over",
		"a /28 pinned address: an answer only elsewhere in its /24 is no answer on its network, the mount is by name",
		"a /16 pinned address: an answer in its /16 beyond its /24 is mounted",
		"the helper prints each A answer of the asked name once, in arrival order, and ignores other names, other types and junk",
		"no answer: the helper prints nothing, exits non-zero and ends by itself inside 3 s",
		"nothing listening at the destination: same, inside 3 s",
		"the launcher with the real embedded helper, no stand-in: the on-network answer is mounted",
		"the helper's default destination is the mDNS group and port, it asks 2 or 3 times and listens for about 1 s",
		"the first query is lost: the next one still brings the answer",
		"an answer that takes 0.7 s is heard inside the window",
	})
}

// TestSeatLauncherLeavesAnAlreadyMountedCacheServerShareAlone (register B-55, the other half). The address is chosen when the
// share is mounted, which is once per WSL session; llama-swap restarts the seat on a swap and the next start finds the share
// mounted. That start must not mount again, must not unmount (another seat may be using the share, and with two addresses of the
// store on one network a new pick at every start could trade a good mount for the Wi-Fi one) and must not ask mDNS; it says where
// the share is and how to choose again, the status file records the address, and a write-floor degrade names it.
func TestSeatLauncherLeavesAnAlreadyMountedCacheServerShareAlone(t *testing.T) {
	out := kvStoreShellRun(t)
	requirePassLines(t, out, []string{
		"a share already mounted by name: no mount, no unmount, no lookup; the line names its source and server address and how to choose again; the status file records the address",
		"a share already mounted at an address an earlier start chose: left alone, the address is recorded, no advice to unmount",
		"a share already mounted that findmnt cannot describe: left alone, said so, no address recorded",
		"a mounted share whose name is not .local: untouched and unreported, as before",
		"a mounted share with no source pin: untouched and unreported, as before",
		"a mounted nfs share: untouched and unreported, as before",
		"the write-floor degrade of an already-mounted share keeps its text and says the share was mounted at that address before this start",
	})
}
