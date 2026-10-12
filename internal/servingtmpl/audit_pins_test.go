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

// A UUID is matched without regard to case, and one that names no card is reported in either: a
// lower-case token must not fall out of the rule just because it was typed that way.
func TestAuditPinsReadsAUUIDInEitherCase(t *testing.T) {
	const absent = "GPU-dddd0000-0000-0000-0000-000000000000"
	for name, pin := range map[string]string{
		"upper-case prefix": absent,
		"lower-case prefix": strings.ToLower(absent),
		"mixed-case prefix": "Gpu-dddd0000-0000-0000-0000-000000000000",
	} {
		t.Run(name, func(t *testing.T) {
			vs := AuditPins(pinCfg(pinEntry("seat", pin, true)), threeCardBox())
			if len(vs) != 1 || vs[0].Rule != "pin-unresolved" || vs[0].Where != "seat" {
				t.Fatalf("a UUID that names no card is pin-unresolved in any case, got:\n%s", Violations(vs))
			}
			if strings.Contains(vs[0].Text, "neither a plain index nor a GPU- UUID") {
				t.Errorf("the token is a UUID in any case, not a word the rule cannot read: %s", vs[0].Text)
			}
		})
	}
}

// pinEntryEnvAndCmd pins the same variable twice: in the env list and inline in the command.
func pinEntryEnvAndCmd(name, envPin, cmdPin string) string {
	return "  " + name + ":\n    ttl: 300\n    env: [\"CUDA_VISIBLE_DEVICES=" + envPin + "\", CUDA_DEVICE_ORDER=PCI_BUS_ID]\n" +
		"    cmd: env CUDA_VISIBLE_DEVICES=" + cmdPin + " /bin/llama-server -m m.gguf -ngl 99 --port ${PORT}\n"
}

// A setting inline in the command is applied over the env list, so the command's value is the one in
// force; the rule reads that one in both directions.
func TestAuditPinsTheCommandPinOverridesTheEnvPin(t *testing.T) {
	t.Run("an index in the command over a UUID in the env", func(t *testing.T) {
		vs := AuditPins(pinCfg(pinEntryEnvAndCmd("seat", pinCard5060A, "0,2")), threeCardBox())
		if len(vs) != 1 || vs[0].Rule != "pin-by-index" {
			t.Fatalf("the command's index is the pin in force:\n%s", Violations(vs))
		}
	})
	t.Run("a UUID in the command over an index in the env", func(t *testing.T) {
		if vs := AuditPins(pinCfg(pinEntryEnvAndCmd("seat", "0,2", pinCard5060A)), threeCardBox()); len(vs) != 0 {
			t.Fatalf("the command's UUID is the pin in force; the env's index is overridden:\n%s", Violations(vs))
		}
	})
}

// -1 hides every card. It is not a pin on a card, so no pin rule judges it (on a box where two cards
// share a name, reading it as an index would call it ambiguous).
func TestAuditPinsDoesNotReadTheHideAllValueAsAnIndex(t *testing.T) {
	for _, pci := range []bool{false, true} {
		if vs := AuditPins(pinCfg(pinEntry("seat", "-1", pci)), threeCardBox()); len(vs) != 0 {
			t.Fatalf("pci=%v: -1 names no card and is no index:\n%s", pci, Violations(vs))
		}
	}
}

// A pin that names no card leaves the seat on fewer cards than it was sized for, or on none: CUDA hides
// every device from the first pin it cannot resolve. A UUID is not the only form that can fail to
// resolve.
func TestAuditPinsRefusesAPinThatNamesNoCard(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pin   string
		pci   bool
		cards []gpuprobe.Card
		want  []string // what the finding must say
	}{
		{"an index past the last card, PCI order", "7", true, distinctCardsBox(), []string{"index 7", "0, 1, 2"}},
		{"an index past the last card, no PCI order", "7", false, distinctCardsBox(), []string{"index 7", "0, 1, 2"}},
		{"the first index past the end", "3", true, distinctCardsBox(), []string{"index 3"}},
		{"a valid index then one past the end", "0,7", true, distinctCardsBox(), []string{"index 7"}},
		{"a valid UUID then an index past the end", pinCard5060A + ",9", true, distinctCardsBox(), []string{"index 9"}},
		{"a word", "gpu_abc", true, distinctCardsBox(), []string{`"gpu_abc"`, "neither a plain index nor a GPU- UUID"}},
		{"a UUID with a non-ASCII hyphen", "GPU\u2010aaaa1111-0000", true, distinctCardsBox(), []string{"neither a plain index nor a GPU- UUID"}},
		{"a bare UUID without its GPU- prefix", "aaaa1111-0000-0000-0000-000000000001", false, distinctCardsBox(), []string{"neither a plain index nor a GPU- UUID"}},
		{"an index past the end on identical cards", "7", true, threeCardBox(), []string{"index 7"}},
		// Without PCI order every plain index is called ambiguous while two cards share a name; one that names
		// no card is not an ambiguous card, it is no card.
		{"an index past the end on identical cards, no PCI order", "7", false, threeCardBox(), []string{"index 7"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vs := AuditPins(pinCfg(pinEntry("seat", tc.pin, tc.pci)), tc.cards)
			if len(vs) != 1 {
				t.Fatalf("want exactly one finding (an index that names no card is not also ambiguous), got %d:\n%s", len(vs), Violations(vs))
			}
			if vs[0].Rule != "pin-unresolved" || vs[0].Where != "seat" {
				t.Errorf("finding = rule %q where %q, want pin-unresolved (seat)", vs[0].Rule, vs[0].Where)
			}
			for _, w := range append([]string{"CUDA_VISIBLE_DEVICES=" + tc.pin, "names no card"}, tc.want...) {
				if !strings.Contains(vs[0].Text, w) {
					t.Errorf("the finding must carry %q, got: %s", w, vs[0].Text)
				}
			}
		})
	}
}

// What the box's table can and cannot judge stays unjudged: an in-range index, a full list, a MIG
// instance id (a slice of a data-centre card, which a table of whole cards cannot resolve), and the
// separators around a token.
func TestAuditPinsLeavesWhatItCanJudgeAlone(t *testing.T) {
	for name, pin := range map[string]string{
		"the last index":         "2",
		"every index":            "0,1,2",
		"a MIG instance id":      "MIG-aaaa1111-0000-0000-0000-000000000001",
		"an index then a MIG id": "0,MIG-aaaa1111-0000-0000-0000-000000000001",
	} {
		t.Run(name, func(t *testing.T) {
			if vs := AuditPins(pinCfg(pinEntry("seat", pin, true)), distinctCardsBox()); len(vs) != 0 {
				t.Fatalf("flagged:\n%s", Violations(vs))
			}
		})
	}
}

// With a card missing from the bus (indices 0 and 2 only) an index is valid when a card has it under
// PCI order; the box's card count is the limit when the order is CUDA's own.
func TestAuditPinsReadsAnIndexAgainstTheCardsThatAreThere(t *testing.T) {
	cards := []gpuprobe.Card{
		{UUID: pinCard5060A, NvidiaIndex: 0, Name: "NVIDIA GeForce RTX 5060 Ti"},
		{UUID: pinCard5060B, NvidiaIndex: 2, Name: "NVIDIA GeForce RTX 4090"},
	}
	if vs := AuditPins(pinCfg(pinEntry("seat", "2", true)), cards); len(vs) != 0 {
		t.Fatalf("under PCI order a card has nvidia-smi index 2:\n%s", Violations(vs))
	}
	if vs := AuditPins(pinCfg(pinEntry("seat", "1", true)), cards); len(vs) != 1 || vs[0].Rule != "pin-unresolved" {
		t.Fatalf("under PCI order no card has nvidia-smi index 1:\n%s", Violations(vs))
	}
	if vs := AuditPins(pinCfg(pinEntry("seat", "1", false)), cards); len(vs) != 0 {
		t.Fatalf("under CUDA's own order two cards are devices 0 and 1:\n%s", Violations(vs))
	}
	if vs := AuditPins(pinCfg(pinEntry("seat", "2", false)), cards); len(vs) != 1 || vs[0].Rule != "pin-unresolved" {
		t.Fatalf("under CUDA's own order two cards leave no device 2:\n%s", Violations(vs))
	}
}

// A pin the rules cannot read as a card still pins: the caller must read the box's table for it, or
// the audit would print OK for a file that leaves a seat on no device.
func TestPinsCUDADevicesCountsAPinItCannotReadAsACard(t *testing.T) {
	for name, pin := range map[string]string{"a word": "gpu_abc", "an index past nothing": "7", "a list with a word": "0,x"} {
		if !PinsCUDADevices(pinCfg(pinEntry("seat", pin, true))) {
			t.Errorf("%s: CUDA_VISIBLE_DEVICES=%s pins, so the table is needed", name, pin)
		}
	}
	if PinsCUDADevices(pinCfg(pinEntry("seat", "-1", true))) {
		t.Error("-1 names no card and nothing to compare: no table is needed for it")
	}
}
