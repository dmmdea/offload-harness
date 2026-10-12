package main

// GPU-F9: a wrapped command whose OWN card pin reaches outside the cards its lease holds is moved to the leased
// card, never refused. The pin is the program's own (ComfyUI and the render runner turn COMFY_CUDA_DEVICE or
// COMFY_CARD_UUID into the card they run on, and reset CUDA_VISIBLE_DEVICES from it), so the lease cannot confine
// it by setting CUDA_VISIBLE_DEVICES; the wrapper rewrites the program's own variable, in the kind the program
// used, and says so on one line. Everything here runs against a fake card table: no step opens a card.
//
// The box is the reference three-card one: nvidia-smi index 1 is the display card and ComfyUI's device 0, index 0 is
// device 1, index 2 is device 2 (fixtureCards, spec "1,0,2").

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

const (
	cardZero = "gpu-aaaa0000-x" // nvidia index 0, ComfyUI device 1
	cardOne  = "gpu-bbbb0000-x" // nvidia index 1, ComfyUI device 0 (the display card)
	cardTwo  = "gpu-cccc0000-x" // nvidia index 2, ComfyUI device 2
)

// repinEnv joins the environment entries a confinement adds, so a failure message shows them.
func repinEnv(c confinement) string { return strings.Join(c.Env, "|") }

// envHas reports whether the child environment carries the entry exactly.
func envHas(c confinement, entry string) bool {
	for _, e := range c.Env {
		if e == entry {
			return true
		}
	}
	return false
}

// envHasKey reports whether the child environment carries any entry for the variable.
func envHasKey(c confinement, key string) bool {
	for _, e := range c.Env {
		if strings.HasPrefix(e, key+"=") {
			return true
		}
	}
	return false
}

func TestConfineWrappedRepinsAComfyCudaDevicePinOutsideTheLease(t *testing.T) {
	cards := fixtureCards()
	// The lease holds nvidia index 0 (ComfyUI device 1); the command pins device 2 (nvidia index 2).
	c := confineWrapped(true, []string{cardZero}, []string{"python", "run.py"},
		envMap(map[string]string{"COMFY_CUDA_DEVICE": "2"}), cards)
	if !envHas(c, "COMFY_CUDA_DEVICE=1") {
		t.Fatalf("the child must be started on the held card's ComfyUI device (1), got env %q note %q", repinEnv(c), c.Note)
	}
	if envHasKey(c, "COMFY_CARD_UUID") {
		t.Fatalf("an index pin stays an index pin: a uuid beside it makes the launch a keyed instance and the render layer refuses the pair: %q", repinEnv(c))
	}
	for _, want := range []string{"COMFY_CUDA_DEVICE=2", "COMFY_CUDA_DEVICE=1", cardZero, "re-pinned"} {
		if !strings.Contains(c.Note, want) {
			t.Errorf("the one line must name the old pin, the new pin and the lease (%q missing): %q", want, c.Note)
		}
	}
	if strings.Contains(c.Note, "trespass") {
		t.Errorf("a pin that was moved onto the lease is no longer a trespass: %q", c.Note)
	}
	if strings.Count(c.Note, "\n") != 0 {
		t.Errorf("the re-pin is told on one line: %q", c.Note)
	}
}

func TestConfineWrappedRepinsAComfyCardUUIDPinOutsideTheLease(t *testing.T) {
	cards := fixtureCards()
	c := confineWrapped(true, []string{cardZero}, []string{"python", "run.py"},
		envMap(map[string]string{"COMFY_CARD_UUID": "GPU-cccc0000-x"}), cards)
	if !envHas(c, "COMFY_CARD_UUID=GPU-aaaa0000-x") {
		t.Fatalf("a uuid pin is re-pinned to the held card's driver uuid, got env %q note %q", repinEnv(c), c.Note)
	}
	if envHasKey(c, "COMFY_CUDA_DEVICE") {
		t.Fatalf("a uuid pin never gets an index beside it (COMFY-INSTANCE-CONFLICT): %q", repinEnv(c))
	}
	if !envHas(c, "CUDA_VISIBLE_DEVICES=GPU-aaaa0000-x") {
		t.Errorf("the lease still pins the rest of the command as it did: %q", repinEnv(c))
	}
	for _, want := range []string{"COMFY_CARD_UUID=GPU-cccc0000-x", "COMFY_CARD_UUID=GPU-aaaa0000-x", "re-pinned"} {
		if !strings.Contains(c.Note, want) {
			t.Errorf("the one line must name the old pin and the new pin (%q missing): %q", want, c.Note)
		}
	}
	// A uuid the table does not know cannot be shown to be inside the lease either.
	c = confineWrapped(true, []string{cardZero}, []string{"python"}, envMap(map[string]string{"COMFY_CARD_UUID": "GPU-ffff9999-nowhere"}), cards)
	if !envHas(c, "COMFY_CARD_UUID=GPU-aaaa0000-x") {
		t.Fatalf("an unknown uuid is re-pinned too: env %q note %q", repinEnv(c), c.Note)
	}
}

// The index a pin is rewritten to is the card table's ComfyUI order (what --cuda-device counts), never the
// nvidia-smi index the lease and the table are keyed by: on this box the two disagree for every card but one.
func TestRepinUsesTheCardTablesComfyOrderNotTheNvidiaIndex(t *testing.T) {
	cards := fixtureCards()
	for _, tc := range []struct {
		held, outsidePin, want string
	}{
		{cardZero, "2", "COMFY_CUDA_DEVICE=1"}, // nvidia index 0 is ComfyUI device 1
		{cardOne, "2", "COMFY_CUDA_DEVICE=0"},  // nvidia index 1 is ComfyUI device 0
		{cardTwo, "0", "COMFY_CUDA_DEVICE=2"},  // nvidia index 2 is ComfyUI device 2
	} {
		c := confineWrapped(true, []string{tc.held}, []string{"python"}, envMap(map[string]string{"COMFY_CUDA_DEVICE": tc.outsidePin}), cards)
		if !envHas(c, tc.want) {
			t.Errorf("held %s, pin %s: want %s, got env %q note %q", tc.held, tc.outsidePin, tc.want, repinEnv(c), c.Note)
		}
	}
}

func TestConfineWrappedRepinsAnUnresolvablePin(t *testing.T) {
	cards := fixtureCards()
	// ComfyUI device 7 names no card on this box: it cannot be shown to stay inside, so it is moved too.
	c := confineWrapped(true, []string{cardZero}, []string{"python"}, envMap(map[string]string{"COMFY_CUDA_DEVICE": "7"}), cards)
	if !envHas(c, "COMFY_CUDA_DEVICE=1") {
		t.Fatalf("an index no card has is re-pinned to the held card: env %q note %q", repinEnv(c), c.Note)
	}
	if !strings.Contains(c.Note, "COMFY_CUDA_DEVICE=7") {
		t.Errorf("the line names the pin it replaced: %q", c.Note)
	}
	// Not a list of device indices at all: that is the program's own error to report, not a card to move.
	c = confineWrapped(true, []string{cardZero}, []string{"python"}, envMap(map[string]string{"COMFY_CUDA_DEVICE": "abc"}), cards)
	if envHasKey(c, "COMFY_CUDA_DEVICE") {
		t.Errorf("a value that is no device index is not rewritten: %q", repinEnv(c))
	}
}

func TestConfineWrappedLeavesAnArgvPinAndKeepsTheNote(t *testing.T) {
	cards := fixtureCards()
	held := []string{cardZero}
	// The program's own argv is not rewritten: a --cuda-device outside the lease keeps today's note.
	for _, args := range [][]string{
		{"python", "main.py", "--cuda-device", "2"},
		{"python", "main.py", "--cuda-device=2"},
	} {
		c := confineWrapped(true, held, args, noEnv, cards)
		if len(c.Env) != 0 || !strings.Contains(c.Note, "not confined") || !strings.Contains(c.Note, cardTwo) || strings.Contains(c.Note, "re-pinned") {
			t.Errorf("%v: an argv pin keeps today's report-only note: env %q note %q", args, repinEnv(c), c.Note)
		}
	}
	// With an argv pin and the env pin both outside, the argv one is what the program obeys: nothing is rewritten.
	c := confineWrapped(true, held, []string{"python", "main.py", "--cuda-device", "2"},
		envMap(map[string]string{"COMFY_CUDA_DEVICE": "2"}), cards)
	if envHasKey(c, "COMFY_CUDA_DEVICE") || !strings.Contains(c.Note, "not confined") {
		t.Errorf("argv wins over the env pin, so the env pin is not rewritten and the note stays: env %q note %q", repinEnv(c), c.Note)
	}
	// A --cuda-device in COMFY_EXTRA_ARGS is the program's own argv too: not rewritten, today's behaviour.
	c = confineWrapped(true, held, []string{"python"}, envMap(map[string]string{"COMFY_EXTRA_ARGS": "--cuda-device 2"}), cards)
	if envHasKey(c, "COMFY_EXTRA_ARGS") || envHasKey(c, "COMFY_CUDA_DEVICE") || envHasKey(c, "COMFY_CARD_UUID") {
		t.Errorf("COMFY_EXTRA_ARGS is the program's argv and is not rewritten: %q", repinEnv(c))
	}
}

func TestConfineWrappedKeepsAPinInsideTheHeldCards(t *testing.T) {
	cards := fixtureCards()
	// An index pin inside the lease is the command's own business: nothing added, nothing said.
	c := confineWrapped(true, []string{cardZero}, []string{"python"}, envMap(map[string]string{"COMFY_CUDA_DEVICE": "1"}), cards)
	if len(c.Env) != 0 || c.Note != "" {
		t.Errorf("an index pin inside the lease is left alone: env %q note %q", repinEnv(c), c.Note)
	}
	// A uuid pin inside the lease: the lease pins the rest as before, the uuid is untouched, nothing is said.
	c = confineWrapped(true, []string{cardZero}, []string{"python"}, envMap(map[string]string{"COMFY_CARD_UUID": "GPU-aaaa0000-x"}), cards)
	if envHasKey(c, "COMFY_CARD_UUID") || envHasKey(c, "COMFY_CUDA_DEVICE") || c.Note != "" {
		t.Errorf("a uuid pin inside the lease is left alone: env %q note %q", repinEnv(c), c.Note)
	}
}

// A blank COMFY_CUDA_DEVICE next to a card uuid is how the render layer starts a uuid-bound launch (it blanks the index
// on purpose): that command starts as it always did, with no index entry added to the child.
func TestBlankComfyCudaDeviceWithAForeignCardVariableStartsUnchanged(t *testing.T) {
	cards := fixtureCards()
	c := confineWrapped(true, []string{cardZero}, []string{"python"},
		envMap(map[string]string{"COMFY_CUDA_DEVICE": "", "COMFY_CARD_UUID": "GPU-aaaa0000-x"}), cards)
	if envHasKey(c, "COMFY_CUDA_DEVICE") || envHasKey(c, "COMFY_CARD_UUID") || c.Note != "" {
		t.Fatalf("a blank index with an in-lease uuid starts unchanged: env %q note %q", repinEnv(c), c.Note)
	}
	// And a blank index with an OUTSIDE uuid is re-pinned as a uuid, without ever writing an index.
	c = confineWrapped(true, []string{cardZero}, []string{"python"},
		envMap(map[string]string{"COMFY_CUDA_DEVICE": "  ", "COMFY_CARD_UUID": "GPU-cccc0000-x"}), cards)
	if !envHas(c, "COMFY_CARD_UUID=GPU-aaaa0000-x") || envHasKey(c, "COMFY_CUDA_DEVICE") {
		t.Fatalf("an outside uuid is re-pinned by uuid only: %q", repinEnv(c))
	}
}

// A pin of k cards under a lease of n >= k cards maps to the first k held cards in ComfyUI order; k > n keeps today's
// note, because there is no card to put the extra ones on.
func TestConfineWrappedRepinsAListOfCardsToTheFirstHeldOnes(t *testing.T) {
	cards := fixtureCards()
	both := []string{cardTwo, cardZero} // lease order is not ComfyUI order: device 2 first, device 1 second
	// "0,1" is nvidia indices 1 and 0: the display card is outside, so the whole list moves to devices 1,2.
	c := confineWrapped(true, both, []string{"python"}, envMap(map[string]string{"COMFY_CUDA_DEVICE": "0,1"}), cards)
	if !envHas(c, "COMFY_CUDA_DEVICE=1,2") {
		t.Fatalf("two cards pinned, two held: the first two held in ComfyUI order, got env %q note %q", repinEnv(c), c.Note)
	}
	// One card pinned, two held: the first held card in ComfyUI order, which is device 1 and not the first in the lease.
	c = confineWrapped(true, both, []string{"python"}, envMap(map[string]string{"COMFY_CUDA_DEVICE": "0"}), cards)
	if !envHas(c, "COMFY_CUDA_DEVICE=1") {
		t.Fatalf("one card pinned: the held card first in ComfyUI order, got env %q note %q", repinEnv(c), c.Note)
	}
	// Three cards pinned, one held: nothing to put the other two on.
	c = confineWrapped(true, []string{cardZero}, []string{"python"}, envMap(map[string]string{"COMFY_CUDA_DEVICE": "0,1,2"}), cards)
	if envHasKey(c, "COMFY_CUDA_DEVICE") || !strings.Contains(c.Note, "not confined") {
		t.Fatalf("a pin of more cards than the lease holds keeps today's note: env %q note %q", repinEnv(c), c.Note)
	}
}

// With the ComfyUI order undeclared the held card's device index is not known, and a guess would land on the wrong
// card: the pin is left alone and the note says why it was not moved.
func TestConfineWrappedDoesNotGuessAnIndexWhenTheComfyOrderIsNotDeclared(t *testing.T) {
	cards, _ := gpuprobe.BuildCards([]gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
		{Index: 1, UUID: "GPU-bbbb0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
		{Index: 2, UUID: "GPU-cccc0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
	}, "")
	c := confineWrapped(true, []string{cardZero}, []string{"python"}, envMap(map[string]string{"COMFY_CUDA_DEVICE": "1"}), cards)
	if envHasKey(c, "COMFY_CUDA_DEVICE") || !strings.Contains(c.Note, "gpu_comfy_order") {
		t.Fatalf("no declared order, no guess: env %q note %q", repinEnv(c), c.Note)
	}
	// A uuid names its card in every ordering, so it needs no order.
	c = confineWrapped(true, []string{cardZero}, []string{"python"}, envMap(map[string]string{"COMFY_CARD_UUID": "GPU-cccc0000-x"}), cards)
	if !envHas(c, "COMFY_CARD_UUID=GPU-aaaa0000-x") {
		t.Fatalf("a uuid pin is re-pinned without a declared order: %q", repinEnv(c))
	}
}

// The index is read in the order the child runs under. CUDA_DEVICE_ORDER=PCI_BUS_ID in the command's environment makes
// the index nvidia-smi's, so the pin is judged and rewritten in that space.
func TestRepinReadsAndWritesTheIndexInTheOrderTheChildRunsUnder(t *testing.T) {
	cards := fixtureCards()
	c := confineWrapped(true, []string{cardZero}, []string{"python"},
		envMap(map[string]string{"COMFY_CUDA_DEVICE": "2", "CUDA_DEVICE_ORDER": "PCI_BUS_ID"}), cards)
	if !envHas(c, "COMFY_CUDA_DEVICE=0") {
		t.Fatalf("under PCI_BUS_ID the index is nvidia-smi's: pin 2 is outside card 0 and the held card is 0, got %q (%q)", repinEnv(c), c.Note)
	}
	// Pin 0 under PCI_BUS_ID IS the held card (nvidia index 0), although ComfyUI device 0 is not.
	c = confineWrapped(true, []string{cardZero}, []string{"python"},
		envMap(map[string]string{"COMFY_CUDA_DEVICE": "0", "CUDA_DEVICE_ORDER": "PCI_BUS_ID"}), cards)
	if envHasKey(c, "COMFY_CUDA_DEVICE") || c.Note != "" {
		t.Fatalf("an index inside the lease in the order the child reads it is left alone: %q (%q)", repinEnv(c), c.Note)
	}
}

// The shell's CUDA_VISIBLE_DEVICES is the operator's, replaced as before; a program's own pin is judged on its own
// and is moved even when the shell's variable already sits inside the lease, because the program resets
// CUDA_VISIBLE_DEVICES from its own pin.
func TestRepinIsJudgedBesideAnInheritedCudaVisibleDevices(t *testing.T) {
	cards := fixtureCards()
	held := []string{cardZero}
	c := confineWrapped(true, held, []string{"python"},
		envMap(map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-aaaa0000-x", "COMFY_CUDA_DEVICE": "2"}), cards)
	if !envHas(c, "COMFY_CUDA_DEVICE=1") || envHasKey(c, "CUDA_VISIBLE_DEVICES") {
		t.Fatalf("a shell pin inside the lease is left alone and the program's own pin outside is moved: %q (%q)", repinEnv(c), c.Note)
	}
	// The shell's variable reaches outside: replaced as before. The wrapper then also sets nvidia-smi's order for the
	// child, so the index the program reads is nvidia-smi's, and the pin is written in that space.
	c = confineWrapped(true, held, []string{"python"},
		envMap(map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-aaaa0000-x,GPU-cccc0000-x", "COMFY_CUDA_DEVICE": "2"}), cards)
	if !envHas(c, "CUDA_VISIBLE_DEVICES=GPU-aaaa0000-x") || !envHas(c, "CUDA_DEVICE_ORDER=PCI_BUS_ID") || !envHas(c, "COMFY_CUDA_DEVICE=0") {
		t.Fatalf("a replaced shell pin and a moved program pin, the index in the order the child will read it: %q (%q)", repinEnv(c), c.Note)
	}
	if !strings.Contains(c.Note, "replaced") || !strings.Contains(c.Note, "re-pinned") {
		t.Errorf("both things are said: %q", c.Note)
	}
}

// The re-pin only ever adds to what the wrapper did before. For every input that has no program pin outside the lease
// the answer is the old one, byte for byte; oldConfineWrapped is the function as it stood at the 0.182.0 base.
func TestConfineWrappedWithoutAnOutsideProgramPinIsTheOldAnswer(t *testing.T) {
	cards := fixtureCards()
	held := []string{cardTwo}
	for name, tc := range map[string]struct {
		explicit bool
		held     []string
		args     []string
		env      map[string]string
	}{
		"no pin":                    {true, held, []string{"python"}, nil},
		"not explicit":              {false, held, []string{"python"}, map[string]string{"COMFY_CUDA_DEVICE": "0"}},
		"no cards":                  {true, nil, []string{"python"}, map[string]string{"COMFY_CUDA_DEVICE": "0"}},
		"index inside":              {true, held, []string{"python"}, map[string]string{"COMFY_CUDA_DEVICE": "2"}},
		"uuid inside":               {true, held, []string{"python"}, map[string]string{"COMFY_CARD_UUID": "GPU-cccc0000-x"}},
		"blank index":               {true, held, []string{"python"}, map[string]string{"COMFY_CUDA_DEVICE": ""}},
		"argv outside":              {true, held, []string{"python", "main.py", "--cuda-device", "0"}, nil},
		"argv unresolved":           {true, held, []string{"python", "main.py", "--cuda-device", "7"}, nil},
		"argv inside":               {true, held, []string{"python", "main.py", "--cuda-device=2"}, nil},
		"shell pin outside":         {true, held, []string{"python"}, map[string]string{"CUDA_VISIBLE_DEVICES": "0,1,2"}},
		"shell pin inside":          {true, held, []string{"python"}, map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-cccc0000-x"}},
		"shell pin unresolvable":    {true, held, []string{"python"}, map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-ffff9999-nowhere"}},
		"shell pin and argv":        {true, held, []string{"python", "main.py", "--cuda-device", "0"}, map[string]string{"CUDA_VISIBLE_DEVICES": "0,1,2"}},
		"extra args pin":            {true, held, []string{"python"}, map[string]string{"COMFY_EXTRA_ARGS": "--cuda-device 0"}},
		"order only":                {true, held, []string{"python"}, map[string]string{"CUDA_DEVICE_ORDER": "PCI_BUS_ID"}},
		"shell pin outside and idx": {true, held, []string{"python"}, map[string]string{"CUDA_VISIBLE_DEVICES": "0,1,2", "COMFY_CUDA_DEVICE": "2"}},
	} {
		got := confineWrapped(tc.explicit, tc.held, tc.args, envMap(tc.env), cards)
		want := oldConfineWrapped(tc.explicit, tc.held, tc.args, envMap(tc.env), cards)
		if repinEnv(got) != repinEnv(want) || got.Note != want.Note {
			t.Errorf("%s: the answer changed without a program pin outside the lease:\n got env %q note %q\nwant env %q note %q",
				name, repinEnv(got), got.Note, repinEnv(want), want.Note)
		}
	}
}

// oldConfineWrapped is confineWrapped as it was before GPU-F9, kept here as the oracle for the test above.
func oldConfineWrapped(explicit bool, held, cmdArgs []string, env func(string) string, cards []gpuprobe.Card) confinement {
	if !explicit || len(held) == 0 {
		return confinement{}
	}
	pin := confinement{}
	uuids := make([]string, 0, len(held))
	for _, id := range held {
		uuids = append(uuids, driverUUID(id, cards))
	}
	pin.Env = []string{"CUDA_VISIBLE_DEVICES=" + strings.Join(uuids, ","), "CUDA_DEVICE_ORDER=PCI_BUS_ID"}
	if !gpulease.WouldDerive(cmdArgs, env) {
		return pin
	}
	var problem string
	d, err := gpulease.DevicesFromCommand(cmdArgs, env, cards)
	if err != nil {
		problem = fmt.Sprintf("the wrapper cannot confirm the command stays within the cards this lease holds (%s): %v", strings.Join(held, ", "), err)
	} else {
		var outside []string
		for _, id := range d.IDs {
			if !containsID(held, id) {
				outside = append(outside, id)
			}
		}
		if len(outside) == 0 {
			return confinement{}
		}
		problem = fmt.Sprintf("the command pins itself (%s) to %s, outside the cards this lease holds (%s)",
			d.Source, strings.Join(outside, ", "), strings.Join(held, ", "))
	}
	if raw := strings.TrimSpace(env("CUDA_VISIBLE_DEVICES")); raw != "" && raw != "-1" {
		pin.Note = fmt.Sprintf("%s; the inherited CUDA_VISIBLE_DEVICES=%s was replaced with the held cards", problem, raw)
		return pin
	}
	return confinement{Note: problem + ": it is not confined to the lease and may trespass on cards another lease can be granted"}
}

// ---------------------------------------------------------------------------
// Through the real verb: the child starts on the held card.
// ---------------------------------------------------------------------------

// TestHelperRepinDriver runs `gpu reserve` with the arguments in LO_REPIN_ARGS (one per line) on the fake three-card
// table. The wrapper ends with os.Exit(<the child's status>) when the child fails, so the verb cannot run inside the
// test that asserts on it. A no-op unless LO_REPIN_ARGS is set.
func TestHelperRepinDriver(t *testing.T) {
	raw := os.Getenv("LO_REPIN_ARGS")
	if raw == "" {
		return
	}
	useCardTable(t, "1,0,2")
	if err := runGPUReserve(strings.Split(raw, "\n")); err != nil {
		fmt.Fprintln(os.Stderr, "driver: reserve:", err)
		os.Exit(3)
	}
}

// TestHelperRepinChild is the wrapped command: it writes the card variables it was started with and exits 7.
func TestHelperRepinChild(t *testing.T) {
	out := os.Getenv("LO_HELPER_ENV_OUT")
	if out == "" {
		return
	}
	body := "devices=" + os.Getenv("GPU_LEASE_DEVICES") + "\ncuda_visible=" + os.Getenv("CUDA_VISIBLE_DEVICES") +
		"\ncuda_order=" + os.Getenv("CUDA_DEVICE_ORDER") + "\ncomfy_cuda_device=" + os.Getenv("COMFY_CUDA_DEVICE") +
		"\ncomfy_card_uuid=" + os.Getenv("COMFY_CARD_UUID") + "\n"
	_ = os.WriteFile(out, []byte(body), 0o644)
	os.Exit(7)
}

// runRepinReserve runs `gpu reserve <args> -- <repin child>` in a child process with the given environment entries and
// returns the wrapper's exit code, its stderr and the card variables the wrapped command saw.
func runRepinReserve(t *testing.T, cfg string, reserveArgs, env []string) (code int, stderr string, seen string) {
	t.Helper()
	out := t.TempDir() + "/env.txt"
	args := append([]string{"--config", cfg, "--class", "media", "--wait", "0"}, reserveArgs...)
	args = append(args, "--", os.Args[0], "-test.run=^TestHelperRepinChild$", "-test.timeout=60s")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperRepinDriver$", "-test.timeout=120s")
	hideWindow(cmd)
	cmd.Env = append(os.Environ(), "LO_REPIN_ARGS="+strings.Join(args, "\n"), "LO_HELPER_ENV_OUT="+out,
		"CUDA_VISIBLE_DEVICES=", "CUDA_DEVICE_ORDER=", "COMFY_CUDA_DEVICE=", "COMFY_CARD_UUID=")
	cmd.Env = append(cmd.Env, env...)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		code = 0
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatalf("running the wrapper: %v", err)
	}
	b, _ := os.ReadFile(out)
	return code, errBuf.String(), string(b)
}

func seenValue(seen, key string) string {
	for _, ln := range strings.Split(seen, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimRight(ln, "\r"), key+"="); ok {
			return v
		}
	}
	return "<missing>"
}

func TestGPUReserveRepinStartsTheCommandOnTheHeldCard(t *testing.T) {
	cfg, _ := scopedLeaseFixture(t)
	// `gpu reserve --devices 0` (the lease holds nvidia index 0) with a command pinned to ComfyUI device 2.
	code, stderr, seen := runRepinReserve(t, cfg, []string{"--devices", "0"}, []string{"COMFY_CUDA_DEVICE=2"})
	if code != 7 {
		t.Fatalf("the wrapper must start the command and return its exit status (7), got %d:\n%s", code, stderr)
	}
	if got := seenValue(seen, "comfy_cuda_device"); got != "1" {
		t.Fatalf("the command must start on the held card (ComfyUI device 1), saw %q:\n%s\n%s", got, seen, stderr)
	}
	if got := seenValue(seen, "devices"); got != cardZero {
		t.Fatalf("setup: the lease holds nvidia index 0: %q", got)
	}
	if !strings.Contains(stderr, "re-pinned") || !strings.Contains(stderr, "COMFY_CUDA_DEVICE=2") || !strings.Contains(stderr, "COMFY_CUDA_DEVICE=1") {
		t.Errorf("the wrapper prints the re-pin line:\n%s", stderr)
	}
	if strings.Contains(stderr, "trespass") {
		t.Errorf("a re-pinned command is not reported as a trespass:\n%s", stderr)
	}

	// The same through a uuid pin.
	code, stderr, seen = runRepinReserve(t, cfg, []string{"--devices", "0"}, []string{"COMFY_CARD_UUID=GPU-cccc0000-x"})
	if code != 7 || seenValue(seen, "comfy_card_uuid") != "GPU-aaaa0000-x" || seenValue(seen, "comfy_cuda_device") != "" {
		t.Fatalf("a uuid pin outside the lease starts on the held card's uuid: exit %d, saw %q / index %q:\n%s",
			code, seenValue(seen, "comfy_card_uuid"), seenValue(seen, "comfy_cuda_device"), stderr)
	}
}

// A flag-off host, a whole-node lease and a lease derived from the command's own pin never reach the re-pin: the
// command starts with exactly the pin it had.
func TestGPUReserveFlagOffWholeNodeAndDerivedLeasesAreByteForByte(t *testing.T) {
	scopedCfg, _ := scopedLeaseFixture(t)
	offCfg, _ := leaseFixture(t)
	for name, tc := range map[string]struct {
		cfg  string
		args []string
	}{
		"flag off":     {offCfg, nil},
		"whole node":   {scopedCfg, []string{"--whole-node"}},
		"derived pins": {scopedCfg, nil},
	} {
		code, stderr, seen := runRepinReserve(t, tc.cfg, tc.args, []string{"COMFY_CUDA_DEVICE=2", "COMFY_CARD_UUID="})
		if code != 7 {
			t.Fatalf("%s: exit %d, want the child's 7:\n%s", name, code, stderr)
		}
		if got := seenValue(seen, "comfy_cuda_device"); got != "2" {
			t.Errorf("%s: the command's own pin must reach it untouched, saw %q:\n%s", name, got, stderr)
		}
		if got := seenValue(seen, "cuda_visible"); got != "" {
			t.Errorf("%s: no confinement is added here, saw CUDA_VISIBLE_DEVICES=%q", name, got)
		}
		if strings.Contains(stderr, "re-pinned") {
			t.Errorf("%s: nothing is re-pinned here:\n%s", name, stderr)
		}
	}
}

// A pin the wrapper declines to move must still be SAID. An inherited CUDA_VISIBLE_DEVICES that names the held card makes
// the generic confinement empty (it reads the shell's variable first), so the only report of the program's own pin was the
// note the generic confinement carries, and there was none: the command started on cards outside its lease with nothing
// printed. The note is the F34 trespass sentence plus the reason the pin was not moved.
func TestConfineWrappedSaysSoWhenAPinCannotBeMovedAndNothingElseReportsIt(t *testing.T) {
	cards := fixtureCards()
	inherited := map[string]string{"CUDA_VISIBLE_DEVICES": "GPU-aaaa0000-x"}
	// More cards pinned than the lease holds, beside an inherited variable that is inside the lease.
	c := confineWrapped(true, []string{cardZero}, []string{"python"},
		envMap(map[string]string{"CUDA_VISIBLE_DEVICES": inherited["CUDA_VISIBLE_DEVICES"], "COMFY_CUDA_DEVICE": "1,2"}), cards)
	for _, want := range []string{"COMFY_CUDA_DEVICE=1,2", "not re-pinned", "the pin names 2 cards and the lease holds 1", "trespass"} {
		if !strings.Contains(c.Note, want) {
			t.Errorf("a pin that is not moved is reported, with the reason (%q missing): env %q note %q", want, repinEnv(c), c.Note)
		}
	}
	if envHasKey(c, "COMFY_CUDA_DEVICE") {
		t.Errorf("and it is still not rewritten: %q", repinEnv(c))
	}
	// The held card has no known ComfyUI index (the order is undeclared): the same sentence, with that reason.
	noOrder, _ := gpuprobe.BuildCards([]gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
		{Index: 1, UUID: "GPU-bbbb0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
		{Index: 2, UUID: "GPU-cccc0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
	}, "")
	c = confineWrapped(true, []string{cardZero}, []string{"python"},
		envMap(map[string]string{"CUDA_VISIBLE_DEVICES": inherited["CUDA_VISIBLE_DEVICES"], "COMFY_CUDA_DEVICE": "2"}), noOrder)
	for _, want := range []string{"COMFY_CUDA_DEVICE=2", "gpu_comfy_order", "trespass"} {
		if !strings.Contains(c.Note, want) {
			t.Errorf("an undeclared order with an inherited variable is reported too (%q missing): env %q note %q", want, repinEnv(c), c.Note)
		}
	}
}

// A uuid pin that IS moved does not hide an index pin that is not: both are told in the one line.
func TestConfineWrappedTellsAnIndexPinItCannotMoveBesideAUUIDItDidMove(t *testing.T) {
	cards := fixtureCards()
	c := confineWrapped(true, []string{cardZero}, []string{"python"},
		envMap(map[string]string{"COMFY_CARD_UUID": "GPU-cccc0000-x", "COMFY_CUDA_DEVICE": "1,2"}), cards)
	if !envHas(c, "COMFY_CARD_UUID=GPU-aaaa0000-x") {
		t.Fatalf("setup: the uuid pin is moved: env %q note %q", repinEnv(c), c.Note)
	}
	if !strings.Contains(c.Note, "not re-pinned") || !strings.Contains(c.Note, "the pin names 2 cards and the lease holds 1") {
		t.Errorf("the pin that stayed where it was is told too: %q", c.Note)
	}
}
