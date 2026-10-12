package servingtmpl

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// A-87: a pin on identical cards is by UUID, and every UUID names a card.
//
// An index is the card's place on the PCI bus. On a box with two cards of the same model, nothing
// in a config that says `CUDA_VISIBLE_DEVICES=0,2` records which of the two identical cards was
// meant, and a power loss or a BIOS reset that reorders enumeration moves the seat to the other
// one without a word. A UUID names the card. These rules read a live file against the box's own
// card table (gpuprobe.ReadCards); the severity (a hand-maintained file fails, a verbatim render
// only notes) is the caller's, because only it holds the stamp.

const (
	pinCard5060A = "GPU-aaaa1111-0000-0000-0000-000000000001"
	pinCard5070  = "GPU-bbbb2222-0000-0000-0000-000000000002"
	pinCard5060B = "GPU-cccc3333-0000-0000-0000-000000000003"
)

// threeCardBox is a two-identical-cards-plus-one box: two 5060 Ti and, between them on the bus, the
// display card.
func threeCardBox() []gpuprobe.Card {
	return []gpuprobe.Card{
		{UUID: pinCard5060A, NvidiaIndex: 0, Name: "NVIDIA GeForce RTX 5060 Ti"},
		{UUID: pinCard5070, NvidiaIndex: 1, Name: "NVIDIA GeForce RTX 5070 Ti", Display: true},
		{UUID: pinCard5060B, NvidiaIndex: 2, Name: "NVIDIA GeForce RTX 5060 Ti"},
	}
}

func distinctCardsBox() []gpuprobe.Card {
	return []gpuprobe.Card{
		{UUID: pinCard5060A, NvidiaIndex: 0, Name: "NVIDIA GeForce RTX 5060 Ti"},
		{UUID: pinCard5070, NvidiaIndex: 1, Name: "NVIDIA GeForce RTX 5070 Ti"},
		{UUID: pinCard5060B, NvidiaIndex: 2, Name: "NVIDIA GeForce RTX 4090"},
	}
}

// pinEntry is a one-entry config. pci adds CUDA_DEVICE_ORDER=PCI_BUS_ID to the env, which is what
// makes a CUDA index the nvidia-smi index.
func pinEntry(name, pin string, pci bool) string {
	env := `"CUDA_VISIBLE_DEVICES=` + pin + `"`
	if pci {
		env += ", CUDA_DEVICE_ORDER=PCI_BUS_ID"
	}
	return "  " + name + ":\n    ttl: 300\n    env: [" + env + "]\n    cmd: /bin/llama-server -m m.gguf -ngl 99 --port ${PORT}\n"
}

func pinCfg(entries ...string) string { return "models:\n" + strings.Join(entries, "") }

func TestAuditPinsRefusesAnIndexPinOnIdenticalCards(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  string
	}{
		{"both identical cards, PCI order", pinCfg(pinEntry("seat", "0,2", true))},
		{"one of the identical cards, PCI order", pinCfg(pinEntry("seat", "2", true))},
		{"the other identical card, PCI order", pinCfg(pinEntry("seat", "0", true))},
		// With no CUDA_DEVICE_ORDER the index follows CUDA's default (fastest first), which the card
		// table cannot map, so any plain index is the weaker form: it cannot be shown to be unambiguous.
		{"any index, no PCI order, while two cards share a name", pinCfg(pinEntry("seat", "1", false))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vs := AuditPins(tc.cfg, threeCardBox())
			if len(vs) != 1 {
				t.Fatalf("want exactly one finding, got %d:\n%s", len(vs), Violations(vs))
			}
			if vs[0].Rule != "pin-by-index" || vs[0].Where != "seat" {
				t.Errorf("finding = rule %q where %q, want pin-by-index (seat)", vs[0].Rule, vs[0].Where)
			}
			for _, w := range []string{"CUDA_VISIBLE_DEVICES", "NVIDIA GeForce RTX 5060 Ti", "cards 0 and 2", "UUID"} {
				if !strings.Contains(vs[0].Text, w) {
					t.Errorf("the finding must carry %q, got: %s", w, vs[0].Text)
				}
			}
		})
	}
}

// The unique card's index names exactly one card; nothing is ambiguous about it.
func TestAuditPinsLeavesAUniqueCardsIndexAlone(t *testing.T) {
	if vs := AuditPins(pinCfg(pinEntry("display", "1", true)), threeCardBox()); len(vs) != 0 {
		t.Fatalf("index 1 is the only 5070 Ti and names it unambiguously, flagged:\n%s", Violations(vs))
	}
}

func TestAuditPinsAcceptsUUIDPinsOnIdenticalCards(t *testing.T) {
	for name, pin := range map[string]string{
		"both UUIDs":                     pinCard5060A + "," + pinCard5060B,
		"one UUID":                       pinCard5060B,
		"a UUID prefix CUDA accepts":     "GPU-cccc3333",
		"a lower-case UUID":              strings.ToLower(pinCard5060A),
		"a UUID beside the display card": pinCard5060A + "," + pinCard5070,
	} {
		t.Run(name, func(t *testing.T) {
			if vs := AuditPins(pinCfg(pinEntry("seat", pin, true)), threeCardBox()); len(vs) != 0 {
				t.Fatalf("a UUID names the card; flagged:\n%s", Violations(vs))
			}
		})
	}
}

func TestAuditPinsAllowsIndexPinsWhenNoNamesRepeat(t *testing.T) {
	cfg := pinCfg(pinEntry("a", "0", true), pinEntry("b", "0,1,2", true), pinEntry("c", "2", false))
	if vs := AuditPins(cfg, distinctCardsBox()); len(vs) != 0 {
		t.Fatalf("three different card models: an index names one card, flagged:\n%s", Violations(vs))
	}
}

// A card table that cannot give a name (an older driver, a test double) cannot show two cards to be
// identical, so it never makes an index ambiguous.
func TestAuditPinsNeverCallsNamelessCardsIdentical(t *testing.T) {
	cards := []gpuprobe.Card{{UUID: pinCard5060A, NvidiaIndex: 0}, {UUID: pinCard5060B, NvidiaIndex: 1}}
	if vs := AuditPins(pinCfg(pinEntry("seat", "0,1", true)), cards); len(vs) != 0 {
		t.Fatalf("two cards with no name are not identical cards:\n%s", Violations(vs))
	}
}

func TestAuditPinsRefusesAUuidThatNamesNoCard(t *testing.T) {
	cfg := pinCfg(
		pinEntry("good", pinCard5060A, true),
		pinEntry("moved", "GPU-dddd0000-0000-0000-0000-000000000000", true),
		pinEntry("mixed", pinCard5060B+",GPU-eeee0000-0000-0000-0000-000000000000", true),
	)
	vs := AuditPins(cfg, threeCardBox())
	if len(vs) != 2 {
		t.Fatalf("want a finding for moved and mixed, got %d:\n%s", len(vs), Violations(vs))
	}
	for i, want := range []string{"mixed", "moved"} {
		if vs[i].Rule != "pin-unresolved" || vs[i].Where != want {
			t.Errorf("finding %d = rule %q where %q, want pin-unresolved (%s)", i, vs[i].Rule, vs[i].Where, want)
		}
		if !strings.Contains(vs[i].Text, "names no card") || !strings.Contains(vs[i].Text, "GPU-") {
			t.Errorf("the finding must say which pin names no card, got: %s", vs[i].Text)
		}
	}
}

// What llama-swap runs is the entry with its macros substituted, and a later setting of the same
// variable wins; the rule reads the pin that is in force.
func TestAuditPinsReadsThePinThatIsInForce(t *testing.T) {
	t.Run("a pin inside a macro", func(t *testing.T) {
		cfg := "macros:\n  pin: \"CUDA_VISIBLE_DEVICES=0,2\"\nmodels:\n  seat:\n    ttl: 300\n    env: [\"${pin}\", CUDA_DEVICE_ORDER=PCI_BUS_ID]\n    cmd: /bin/llama-server -m m.gguf\n"
		if vs := AuditPins(cfg, threeCardBox()); len(vs) != 1 || vs[0].Where != "seat" {
			t.Fatalf("a macro's pin is the entry's pin:\n%s", Violations(vs))
		}
	})
	t.Run("the last env setting wins", func(t *testing.T) {
		cfg := "models:\n  seat:\n    ttl: 300\n    env: [CUDA_VISIBLE_DEVICES=0, CUDA_DEVICE_ORDER=PCI_BUS_ID, \"CUDA_VISIBLE_DEVICES=" + pinCard5060A + "\"]\n    cmd: /bin/llama-server -m m.gguf\n"
		if vs := AuditPins(cfg, threeCardBox()); len(vs) != 0 {
			t.Fatalf("the earlier index is overridden by the later UUID:\n%s", Violations(vs))
		}
	})
	t.Run("a pin set inline in the command", func(t *testing.T) {
		cfg := "models:\n  seat:\n    ttl: 300\n    env: [CUDA_DEVICE_ORDER=PCI_BUS_ID]\n    cmd: env CUDA_VISIBLE_DEVICES=2 /bin/llama-server -m m.gguf\n"
		if vs := AuditPins(cfg, threeCardBox()); len(vs) != 1 || vs[0].Rule != "pin-by-index" {
			t.Fatalf("an inline pin is a pin:\n%s", Violations(vs))
		}
	})
	t.Run("an entry that pins nothing", func(t *testing.T) {
		cfg := "models:\n  seat:\n    ttl: 300\n    cmd: /bin/llama-server -m m.gguf\n"
		if vs := AuditPins(cfg, threeCardBox()); len(vs) != 0 {
			t.Fatalf("no pin, no finding:\n%s", Violations(vs))
		}
	})
	t.Run("an empty pin is the cvd-empty rule's, not this one", func(t *testing.T) {
		cfg := "models:\n  seat:\n    ttl: 300\n    env: [\"CUDA_VISIBLE_DEVICES=\"]\n    cmd: /bin/llama-server -m m.gguf\n"
		if vs := AuditPins(cfg, threeCardBox()); len(vs) != 0 {
			t.Fatalf("Audit owns the empty pin:\n%s", Violations(vs))
		}
	})
}

// Without a card table there is nothing to compare with: the rule says nothing, and the caller says
// so (a silent pass would read as a clean audit).
func TestAuditPinsWithoutACardTableFindsNothing(t *testing.T) {
	if vs := AuditPins(pinCfg(pinEntry("seat", "0,2", true)), nil); len(vs) != 0 {
		t.Fatalf("no card table, no verdict:\n%s", Violations(vs))
	}
}

func TestPinsCUDADevicesSaysWhetherAnyEntryPins(t *testing.T) {
	if !PinsCUDADevices(pinCfg(pinEntry("seat", "0", true))) {
		t.Error("an entry with CUDA_VISIBLE_DEVICES pins")
	}
	if PinsCUDADevices("models:\n  seat:\n    ttl: 300\n    cmd: /bin/llama-server -m m.gguf\n") {
		t.Error("a config with no pin does not")
	}
	if PinsCUDADevices("models: [unclosed") {
		t.Error("an unparseable document pins nothing")
	}
}
