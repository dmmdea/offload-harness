package main

import (
	"os"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// A lease on cards that the wrapped command is not confined to leaves the held cards idle
// and lets the job run on CUDA's default fastest-first card, which on the reference box is
// the display card (the card the allocator refuses to hand out). So a reservation that
// NAMED or ALLOCATED its cards confines the command to them, unless the command pins
// itself. Environment entries are asserted on what the child process actually sees.

func envValue(file, key string) string {
	b, err := os.ReadFile(file)
	if err != nil {
		return "<no file: " + err.Error() + ">"
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(ln, key+"="); ok {
			return strings.TrimRight(v, "\r")
		}
	}
	return "<missing>"
}

func clearCardPins(t *testing.T) {
	t.Helper()
	t.Setenv("CUDA_VISIBLE_DEVICES", "")
	t.Setenv("CUDA_DEVICE_ORDER", "")
	t.Setenv("COMFY_CUDA_DEVICE", "")
}

func TestGPUReserveDevicesPinsTheChildToTheHeldCards(t *testing.T) {
	cfg, _ := scopedLeaseFixture(t)
	useCardTable(t, "")
	clearCardPins(t)
	out := t.TempDir() + "/env.txt"
	t.Setenv("LO_HELPER_ENV_OUT", out)
	t.Setenv("LO_HELPER_SLEEP_MS", "0")
	if err := runGPUReserve(append([]string{"--config", cfg, "--class", "media", "--devices", "2", "--wait", "0"}, envHelperCmd(out)...)); err != nil {
		t.Fatal(err)
	}
	// The UUID as the driver reports it ("GPU-..."), not the lower-cased lease id.
	if got := envValue(out, "cuda_visible"); got != "GPU-cccc0000-x" {
		t.Fatalf("the child must see only the held card, by its driver UUID: %q", got)
	}
	if got := envValue(out, "cuda_order"); got != "PCI_BUS_ID" {
		t.Fatalf("CUDA_DEVICE_ORDER=PCI_BUS_ID: %q", got)
	}
}

func TestGPUReserveCardsPinsTheChildToEveryAllocatedCard(t *testing.T) {
	cfg, _ := scopedLeaseFixture(t)
	useCardTable(t, "")
	clearCardPins(t)
	out := t.TempDir() + "/env.txt"
	t.Setenv("LO_HELPER_ENV_OUT", out)
	t.Setenv("LO_HELPER_SLEEP_MS", "0")
	if err := runGPUReserve(append([]string{"--config", cfg, "--class", "media", "--cards", "2", "--wait", "0"}, envHelperCmd(out)...)); err != nil {
		t.Fatal(err)
	}
	// Two non-display cards (the display card is never auto-assigned): 0 and 2.
	if got := envValue(out, "cuda_visible"); got != "GPU-aaaa0000-x,GPU-cccc0000-x" {
		t.Fatalf("the child must see exactly the allocated cards: %q", got)
	}
}

// Not confined: a whole-node lease (nothing to confine to), and a lease whose set came from
// the command's own pin (the command already says where it runs).
func TestGPUReserveDoesNotPinAWholeNodeOrACommandDerivedLease(t *testing.T) {
	cfg, _ := scopedLeaseFixture(t)
	useCardTable(t, "1,0,2")
	clearCardPins(t)
	out := t.TempDir() + "/env.txt"
	t.Setenv("LO_HELPER_ENV_OUT", out)
	t.Setenv("LO_HELPER_SLEEP_MS", "0")
	if err := runGPUReserve(append([]string{"--config", cfg, "--class", "media", "--whole-node", "--wait", "0"}, envHelperCmd(out)...)); err != nil {
		t.Fatal(err)
	}
	if got := envValue(out, "cuda_visible"); got != "" {
		t.Fatalf("a whole-node lease sets no pin: %q", got)
	}

	t.Setenv("COMFY_CUDA_DEVICE", "2") // ComfyUI order 1,0,2: nvidia index 2
	if err := runGPUReserve(append([]string{"--config", cfg, "--class", "media", "--wait", "0"}, envHelperCmd(out)...)); err != nil {
		t.Fatal(err)
	}
	if got := envValue(out, "cuda_visible"); got != "" {
		t.Fatalf("a command that pins itself is left to its own pin: %q", got)
	}
	if got := envValue(out, "devices"); got != "gpu-cccc0000-x" {
		t.Fatalf("setup: the lease must still be derived from the pin: %q", got)
	}
}

func fixtureCards() []gpuprobe.Card {
	cards, _ := gpuprobe.BuildCards([]gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
		{Index: 1, UUID: "GPU-bbbb0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16, DisplayActive: true},
		{Index: 2, UUID: "GPU-cccc0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
	}, "1,0,2")
	return cards
}

func TestConfineWrappedRules(t *testing.T) {
	cards := fixtureCards()
	held := []string{"gpu-cccc0000-x"}

	// Explicit set, command names no card: pinned.
	c := confineWrapped(true, held, []string{"python", "run.py"}, noEnv, cards)
	if strings.Join(c.Env, "|") != "CUDA_VISIBLE_DEVICES=GPU-cccc0000-x|CUDA_DEVICE_ORDER=PCI_BUS_ID" || c.Note != "" {
		t.Fatalf("pin the command to the held card: %+v", c)
	}
	// Not an explicit set (derived from the command, or whole node): untouched.
	if c := confineWrapped(false, held, []string{"python", "run.py"}, noEnv, cards); len(c.Env) != 0 || c.Note != "" {
		t.Fatalf("a derived or whole-node lease is not confined by the wrapper: %+v", c)
	}
	if c := confineWrapped(true, nil, []string{"python"}, noEnv, cards); len(c.Env) != 0 {
		t.Fatalf("no cards, nothing to pin to: %+v", c)
	}
	// Card table gone at launch: the driver UUID is rebuilt from the lease id, never skipped.
	if c := confineWrapped(true, held, []string{"python"}, noEnv, nil); strings.Join(c.Env, "|") != "CUDA_VISIBLE_DEVICES=GPU-cccc0000-x|CUDA_DEVICE_ORDER=PCI_BUS_ID" {
		t.Fatalf("without a table the UUID is rebuilt from the lease id: %+v", c)
	}

	// The command pins itself INSIDE the held set: left alone, no note.
	inside := func(k string) string {
		if k == "COMFY_CUDA_DEVICE" {
			return "2" // ComfyUI order 1,0,2 -> nvidia index 2
		}
		return ""
	}
	if c := confineWrapped(true, held, []string{"python"}, inside, cards); len(c.Env) != 0 || c.Note != "" {
		t.Fatalf("a pin inside the held set is the command's own business: %+v", c)
	}
	// The command pins itself OUTSIDE the held set: not overridden (ComfyUI overwrites
	// CUDA_VISIBLE_DEVICES from --cuda-device itself), but said out loud.
	outside := func(k string) string {
		if k == "COMFY_CUDA_DEVICE" {
			return "1" // ComfyUI order 1,0,2 -> nvidia index 0
		}
		return ""
	}
	c = confineWrapped(true, held, []string{"python"}, outside, cards)
	if len(c.Env) != 0 || !strings.Contains(c.Note, "gpu-aaaa0000-x") || !strings.Contains(c.Note, "not confined") {
		t.Fatalf("a pin outside the held set must be reported, naming the card: %+v", c)
	}
	// A pin that cannot be resolved cannot be confirmed to be inside: said, too. The pin
	// is the program's own (ComfyUI resets the variable from --cuda-device), so it is not
	// overridden.
	unresolvedFlag := []string{"python", "main.py", "--cuda-device", "7"}
	if c := confineWrapped(true, held, unresolvedFlag, noEnv, cards); len(c.Env) != 0 || !strings.Contains(c.Note, "cannot") {
		t.Fatalf("an own pin by argv that cannot be resolved is reported, not overridden: %+v", c)
	}

	// An INHERITED CUDA_VISIBLE_DEVICES is the operator's shell, not the program's choice:
	// when it reaches outside the held cards, or cannot be resolved, it is REPLACED with the
	// held cards (the override is effective: the child sees only what the wrapper left in
	// its environment) and the wrapper says so.
	heldEnv := "CUDA_VISIBLE_DEVICES=GPU-cccc0000-x|CUDA_DEVICE_ORDER=PCI_BUS_ID"
	inherited := func(v string) func(string) string {
		return func(k string) string {
			if k == "CUDA_VISIBLE_DEVICES" {
				return v
			}
			return ""
		}
	}
	// Bare indices, counted in ComfyUI order (declared 1,0,2): 0,1,2 is every card.
	c = confineWrapped(true, held, []string{"python"}, inherited("0,1,2"), cards)
	if strings.Join(c.Env, "|") != heldEnv || !strings.Contains(c.Note, "replaced") || !strings.Contains(c.Note, "gpu-aaaa0000-x") {
		t.Fatalf("an inherited pin that reaches outside the lease is replaced, and said: %+v", c)
	}
	// A pin naming a card nobody has cannot be confirmed inside: replaced, and said.
	c = confineWrapped(true, held, []string{"python"}, inherited("GPU-ffff9999-nowhere"), cards)
	if strings.Join(c.Env, "|") != heldEnv || !strings.Contains(c.Note, "cannot") || !strings.Contains(c.Note, "replaced") {
		t.Fatalf("an inherited pin that cannot be resolved is replaced, and said: %+v", c)
	}
	// Bare indices on a box whose ComfyUI order was never declared cannot be resolved either.
	undeclared, _ := gpuprobe.BuildCards([]gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
		{Index: 2, UUID: "GPU-cccc0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
	}, "")
	if c := confineWrapped(true, held, []string{"python"}, inherited("0,1,2"), undeclared); strings.Join(c.Env, "|") != heldEnv || c.Note == "" {
		t.Fatalf("unresolvable bare indices are replaced, and said: %+v", c)
	}
	// An inherited pin inside the held set is tighter than the lease: left alone, no note.
	if c := confineWrapped(true, held, []string{"python"}, inherited("GPU-cccc0000-x"), cards); len(c.Env) != 0 || c.Note != "" {
		t.Fatalf("an inherited pin inside the lease is the operator's business: %+v", c)
	}
	// A pin that is partly outside is outside.
	if c := confineWrapped(true, held, []string{"python"}, inherited("GPU-cccc0000-x,GPU-aaaa0000-x"), cards); strings.Join(c.Env, "|") != heldEnv {
		t.Fatalf("a pin that includes a card outside the lease is replaced: %+v", c)
	}
}

// The shell's own CUDA_VISIBLE_DEVICES must not let the job run on cards the lease does not
// hold: through the real verb, the child sees the held card and not the shell's.
func TestGPUReserveReplacesAnInheritedPinThatReachesOutsideTheLease(t *testing.T) {
	cfg, _ := scopedLeaseFixture(t)
	useCardTable(t, "1,0,2")
	clearCardPins(t)
	t.Setenv("CUDA_VISIBLE_DEVICES", "0,1,2")
	out := t.TempDir() + "/env.txt"
	t.Setenv("LO_HELPER_ENV_OUT", out)
	t.Setenv("LO_HELPER_SLEEP_MS", "0")
	if err := runGPUReserve(append([]string{"--config", cfg, "--class", "media", "--devices", "2", "--wait", "0"}, envHelperCmd(out)...)); err != nil {
		t.Fatal(err)
	}
	if got := envValue(out, "cuda_visible"); got != "GPU-cccc0000-x" {
		t.Fatalf("the child must see only the held card, not the shell's 0,1,2: %q", got)
	}
}
