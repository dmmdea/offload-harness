package servingtmpl

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// display-cap-unseen: a llama-swap file states no VRAM footprint, so audit-yaml cannot judge an entry against
// gpu_display_cap_pct. What it can do is stop passing in silence over the entries that may sit on the display
// card: proxy: seats (their cards are set in the engine's own launcher), entries with no device pin, and entries
// pinned onto the card the driver flags as the display card. They are named in ONE aggregated note, never a
// violation. The display card is the driver's reading by UUID (Card.Display), never a remembered index.

// unseenBox is a three-card box of 15.9 GiB cards whose display card is the one at displayIdx.
func unseenBox(displayIdx int) []gpuprobe.Card {
	cards := []gpuprobe.Card{
		{UUID: pinCard5060A, NvidiaIndex: 0, Name: "NVIDIA GeForce RTX 5060 Ti", VRAMTotalGiB: 15.9, VRAMFreeGiB: 15},
		{UUID: pinCard5070, NvidiaIndex: 1, Name: "NVIDIA GeForce RTX 5070 Ti", VRAMTotalGiB: 15.9, VRAMFreeGiB: 15},
		{UUID: pinCard5060B, NvidiaIndex: 2, Name: "NVIDIA GeForce RTX 4090", VRAMTotalGiB: 15.9, VRAMFreeGiB: 15},
	}
	if displayIdx >= 0 {
		cards[displayIdx].Display = true
	}
	return cards
}

func proxyEntry(name string) string {
	return "  " + name + ":\n    ttl: 300\n    proxy: http://192.0.2.10:18797\n    cmd: /opt/seats/run-" + name + ".sh\n    cmdStop: /opt/seats/stop-" + name + ".sh\n"
}

func plainEntry(name string) string {
	return "  " + name + ":\n    ttl: 300\n    cmd: /bin/llama-server -m m.gguf -ngl 99 --port ${PORT}\n"
}

func report(t *testing.T, cfg string, cards []gpuprobe.Card) unseenReport {
	t.Helper()
	r, ok := unseenEntries(cfg, cards)
	if !ok {
		t.Fatalf("the file did not parse:\n%s", cfg)
	}
	return r
}

func TestUnseenSortsEntriesIntoTheFourGroupsAndNamesNoneThatPinOffTheDisplayCard(t *testing.T) {
	cfg := pinCfg(
		pinEntry("a-on-display", pinCard5070, true),   // UUID of the display card
		pinEntry("b-by-pci-index", "1", true),         // index 1 under PCI_BUS_ID is the display card
		pinEntry("c-off-display", pinCard5060A, true), // UUID of another card
		pinEntry("d-index-off", "2", true),            // index 2 under PCI_BUS_ID is another card
		pinEntry("e-unmapped", "1", false),            // an index with no PCI order cannot be mapped
		proxyEntry("f-vllm"),
		plainEntry("g-free"),
	)
	r := report(t, cfg, unseenBox(1))
	if want := []unseenPin{{"a-on-display", 1}, {"b-by-pci-index", 1}}; !reflect.DeepEqual(r.Pinned, want) {
		t.Errorf("pinned onto the display card = %+v, want %+v (the entries on other cards are not named)", r.Pinned, want)
	}
	if want := []string{"f-vllm"}; !reflect.DeepEqual(r.Proxy, want) {
		t.Errorf("proxy seats = %v, want %v", r.Proxy, want)
	}
	if want := []string{"g-free"}; !reflect.DeepEqual(r.Unpinned, want) {
		t.Errorf("unpinned = %v, want %v", r.Unpinned, want)
	}
	if want := []string{"e-unmapped"}; !reflect.DeepEqual(r.Unmapped, want) {
		t.Errorf("an index with no CUDA_DEVICE_ORDER=PCI_BUS_ID is named unmappable, never guessed: %v, want %v", r.Unmapped, want)
	}
	if len(r.Cards) != 1 || r.Cards[0].Index != 1 || r.Cards[0].UUID != pinCard5070 {
		t.Errorf("the display card is card 1 on this reading: %+v", r.Cards)
	}
}

// The display card is the driver's reading, not index 1: when the monitor moves, the same file reads differently.
func TestUnseenFollowsTheMonitorNotTheIndex(t *testing.T) {
	cfg := pinCfg(pinEntry("seat", pinCard5060B, true), pinEntry("other", "1", true))
	moved := report(t, cfg, unseenBox(2)) // the monitor is on card 2 now
	if want := []unseenPin{{"seat", 2}}; !reflect.DeepEqual(moved.Pinned, want) {
		t.Fatalf("with the monitor on card 2 the entry pinned to card 2's UUID is named, and index 1 is not: %+v", moved.Pinned)
	}
	home := report(t, cfg, unseenBox(1)) // the monitor is on card 1
	if want := []unseenPin{{"other", 1}}; !reflect.DeepEqual(home.Pinned, want) {
		t.Fatalf("with the monitor on card 1 the index-1 entry is named and the card-2 UUID entry is not: %+v", home.Pinned)
	}
	// The note names the card of this reading.
	vs := AuditDisplayCapUnseen(cfg, unseenBox(2))
	if len(vs) != 1 || vs[0].Where != "card 2" || !strings.Contains(vs[0].Text, "00000003") {
		t.Fatalf("the note names card 2 and its UUID tail: %+v", vs)
	}
}

// A single-card box and a headless one have no flagged display card, so there is nothing to say; the same file on
// a box with a display card is not silent.
func TestUnseenIsSilentWithoutAFlaggedDisplayCard(t *testing.T) {
	cfg := pinCfg(proxyEntry("seat"), plainEntry("free"), pinEntry("pinned", "1", true))
	single, _ := gpuprobe.BuildCards([]gpuprobe.Device{
		{Index: 0, UUID: pinCard5060A, Name: "NVIDIA GeForce RTX 5060 Ti", TotalGiB: 15.9, FreeGiB: 15, DisplayAttached: true},
	}, "")
	if single[0].Display {
		t.Fatal("fixture: a single card is never flagged as the display card")
	}
	if vs := AuditDisplayCapUnseen(cfg, single); len(vs) != 0 {
		t.Errorf("a single-card box is silent: %+v", vs)
	}
	if vs := AuditDisplayCapUnseen(cfg, unseenBox(-1)); len(vs) != 0 {
		t.Errorf("a headless multi-card box is silent: %+v", vs)
	}
	if vs := AuditDisplayCapUnseen(cfg, nil); len(vs) != 0 {
		t.Errorf("no card table is silent here (the caller says so): %+v", vs)
	}
	// The positive control: a flagged display card makes the same file speak.
	vs := AuditDisplayCapUnseen(cfg, unseenBox(1))
	if len(vs) != 1 || vs[0].Rule != "display-cap-unseen" {
		t.Fatalf("control: with a display card the same file gets the note: %+v", vs)
	}
}

func TestUnseenNoteTextNamesTheEntriesTheCardTheCapAndWhatIsNotJudged(t *testing.T) {
	cfg := pinCfg(pinEntry("on-card", pinCard5070, true), proxyEntry("vllm-seat"), plainEntry("free-run"), pinEntry("by-index", "1", false))
	vs := AuditDisplayCapUnseen(cfg, unseenBox(1))
	if len(vs) != 1 {
		t.Fatalf("exactly one aggregated note, got %d: %+v", len(vs), vs)
	}
	v := vs[0]
	if v.Rule != "display-cap-unseen" || v.Where != "card 1" {
		t.Errorf("rule/where = %q (%q), want display-cap-unseen (card 1)", v.Rule, v.Where)
	}
	for _, want := range []string{
		"on-card", "vllm-seat", "free-run", "by-index", // every entry named
		"card 1", "00000002", "15.9 GiB", // the display card of this reading: index, UUID tail, total
		"60", "9.5 GiB", // the default cap and its budget on that card
		"no VRAM footprint", "nothing is judged", "not summed", // what the audit does not do
		"cannot be mapped", // the index without PCI order
		"gpu status",       // where the live reading is
		"reads no config",  // the cap is the default, not the operator's
	} {
		if !strings.Contains(v.Text, want) {
			t.Errorf("the note must contain %q:\n%s", want, v.Text)
		}
	}
	if strings.Contains(v.Text, "\n") {
		t.Errorf("one line per finding:\n%s", v.Text)
	}
	// A note is not a violation: the string a caller prints reads as a rule with its place.
	if !strings.HasPrefix(v.String(), "display-cap-unseen (card 1): ") {
		t.Errorf("String() = %q", v.String())
	}
}

func TestUnseenNamesAtMostSixEntriesPerGroup(t *testing.T) {
	build := func(n int) string {
		var entries []string
		for i := 1; i <= n; i++ {
			entries = append(entries, proxyEntry(fmt.Sprintf("p%d", i)))
		}
		return pinCfg(entries...)
	}
	six := AuditDisplayCapUnseen(build(6), unseenBox(1))
	if len(six) != 1 || !strings.Contains(six[0].Text, "p1, p2, p3, p4, p5, p6") || strings.Contains(six[0].Text, "more") {
		t.Fatalf("six names are all named, with no 'more': %+v", six)
	}
	nine := AuditDisplayCapUnseen(build(9), unseenBox(1))
	if len(nine) != 1 {
		t.Fatalf("one note: %+v", nine)
	}
	txt := nine[0].Text
	if !strings.Contains(txt, "p1, p2, p3, p4, p5, p6") || strings.Contains(txt, "p7") || !strings.Contains(txt, "and 3 more") {
		t.Errorf("nine proxy seats name six and count three more:\n%s", txt)
	}
}

// What a pin cannot put on a card is left to the rules that own it: -1 hides every card, an empty value is cvd-empty's,
// a MIG slice or an unresolved UUID or a typo is A-87's. None is named here; the entry that IS on the card still is.
func TestUnseenLeavesOtherRulesPinsAlone(t *testing.T) {
	cfg := pinCfg(
		pinEntry("hidden", "-1", true),
		pinEntry("mig", "MIG-aaaa0000-0000", true),
		pinEntry("unresolved", "GPU-dddd0000-0000-0000-0000-000000000000", true),
		pinEntry("typo", "gpu_abc", true),
		pinEntry("on-card", pinCard5070, true),
	)
	r := report(t, cfg, unseenBox(1))
	if want := []unseenPin{{"on-card", 1}}; !reflect.DeepEqual(r.Pinned, want) {
		t.Errorf("only the entry that is on the display card is named: %+v", r.Pinned)
	}
	if len(r.Unmapped)+len(r.Unpinned)+len(r.Proxy) != 0 {
		t.Errorf("the other pins are not named in any group: %+v", r)
	}
}

// The pin is the one llama-swap runs: macros expanded, and a setting inline in the command wins over the env list.
func TestUnseenReadsThePinTheWayTheAuditDoes(t *testing.T) {
	cfg := "macros:\n  onDisplay: CUDA_VISIBLE_DEVICES=" + pinCard5070 + " CUDA_DEVICE_ORDER=PCI_BUS_ID\n" +
		"models:\n" +
		"  via-macro:\n    ttl: 300\n    cmd: ${onDisplay} /bin/llama-server -m m.gguf --port ${PORT}\n" +
		// env pins the display card, the command overrides it with another card: not on the display card.
		"  cmd-wins-off:\n    ttl: 300\n    env: [\"CUDA_VISIBLE_DEVICES=" + pinCard5070 + "\", CUDA_DEVICE_ORDER=PCI_BUS_ID]\n    cmd: CUDA_VISIBLE_DEVICES=" + pinCard5060A + " /bin/llama-server -m m.gguf --port ${PORT}\n" +
		// env pins another card, the command overrides it with the display card: on it.
		"  cmd-wins-on:\n    ttl: 300\n    env: [\"CUDA_VISIBLE_DEVICES=" + pinCard5060A + "\", CUDA_DEVICE_ORDER=PCI_BUS_ID]\n    cmd: CUDA_VISIBLE_DEVICES=" + pinCard5070 + " /bin/llama-server -m m.gguf --port ${PORT}\n"
	r := report(t, cfg, unseenBox(1))
	if want := []unseenPin{{"cmd-wins-on", 1}, {"via-macro", 1}}; !reflect.DeepEqual(r.Pinned, want) {
		t.Errorf("pinned onto the display card = %+v, want %+v", r.Pinned, want)
	}
	if len(r.Unpinned) != 0 {
		t.Errorf("every entry here pins a device: %v", r.Unpinned)
	}
}

// A proxy: entry is a proxy seat whatever its own env says: the pin on its stub does not govern the engine behind it.
func TestUnseenCallsAProxyEntryAProxySeatEvenWithAPinOnItsStub(t *testing.T) {
	cfg := pinCfg("  seat:\n    ttl: 300\n    proxy: http://192.0.2.10:18797\n    env: [\"CUDA_VISIBLE_DEVICES=" + pinCard5060A + "\"]\n    cmd: /opt/seats/run.sh\n")
	r := report(t, cfg, unseenBox(1))
	if !reflect.DeepEqual(r.Proxy, []string{"seat"}) || len(r.Pinned) != 0 {
		t.Errorf("a proxy entry is a proxy seat: %+v", r)
	}
}

// Two monitors, two display cards: each is named, and an entry says which one it is pinned onto.
func TestUnseenNamesEveryDisplayCard(t *testing.T) {
	cards := unseenBox(1)
	cards[0].Display = true
	cfg := pinCfg(pinEntry("on-zero", pinCard5060A, true), pinEntry("on-one", pinCard5070, true), pinEntry("on-two", pinCard5060B, true))
	r := report(t, cfg, cards)
	if len(r.Cards) != 2 || r.Cards[0].Index != 0 || r.Cards[1].Index != 1 {
		t.Fatalf("both display cards: %+v", r.Cards)
	}
	if want := []unseenPin{{"on-one", 1}, {"on-zero", 0}}; !reflect.DeepEqual(r.Pinned, want) {
		t.Errorf("pinned = %+v, want %+v", r.Pinned, want)
	}
	vs := AuditDisplayCapUnseen(cfg, cards)
	if len(vs) != 1 || vs[0].Where != "cards 0, 1" || !strings.Contains(vs[0].Text, "on-zero (card 0)") || !strings.Contains(vs[0].Text, "on-one (card 1)") {
		t.Errorf("the note names both cards and which each entry is on: %+v", vs)
	}
}

// A reading that could not say which card the monitor is on is not guessed: one note says so, and names no entry.
func TestUnseenSaysWhenTheReadingCouldNotSayWhichCardIsTheMonitors(t *testing.T) {
	cfg := pinCfg(pinEntry("on-card", pinCard5070, true), proxyEntry("vllm-seat"))
	unknown := unseenBox(1)
	for i := range unknown {
		unknown[i].DisplayUnknown = true
	}
	vs := AuditDisplayCapUnseen(cfg, unknown)
	if len(vs) != 1 || vs[0].Rule != "display-cap-unseen" || vs[0].Where != "no display card known" {
		t.Fatalf("one note: %+v", vs)
	}
	if !strings.Contains(vs[0].Text, "could not say which card the monitor is on") {
		t.Errorf("the note must say why nothing was looked at: %s", vs[0].Text)
	}
	if strings.Contains(vs[0].Text, "on-card") || strings.Contains(vs[0].Text, "vllm-seat") {
		t.Errorf("no entry is named on a reading that cannot say which card is the monitor's: %s", vs[0].Text)
	}
	// The positive control: the same file on a known reading names them.
	known := AuditDisplayCapUnseen(cfg, unseenBox(1))
	if len(known) != 1 || !strings.Contains(known[0].Text, "on-card") || !strings.Contains(known[0].Text, "vllm-seat") {
		t.Errorf("control: %+v", known)
	}
}

// A display card whose total was not read has no budget to state; it says so rather than print a number.
func TestUnseenStatesNoBudgetWhenTheTotalWasNotRead(t *testing.T) {
	cfg := pinCfg(proxyEntry("seat"))
	cards := unseenBox(1)
	cards[1].VRAMTotalGiB = 0
	vs := AuditDisplayCapUnseen(cfg, cards)
	if len(vs) != 1 || !strings.Contains(vs[0].Text, "total VRAM was not read") || strings.Contains(vs[0].Text, "9.5") {
		t.Fatalf("no total, no budget: %+v", vs)
	}
	if vs := AuditDisplayCapUnseen(cfg, unseenBox(1)); len(vs) != 1 || !strings.Contains(vs[0].Text, "9.5 GiB") {
		t.Errorf("control: with a total the budget is stated: %+v", vs)
	}
}

func TestUnseenIgnoresAFileThatNeitherPinsNorProxiesNorIsEmptyOfEntries(t *testing.T) {
	if _, ok := unseenEntries("models: [", unseenBox(1)); ok {
		t.Error("a file that does not parse is no report (Audit owns the yaml rule)")
	}
	if vs := AuditDisplayCapUnseen("models: [", unseenBox(1)); len(vs) != 0 {
		t.Errorf("a file that does not parse is silent here: %+v", vs)
	}
	if vs := AuditDisplayCapUnseen("models: {}\n", unseenBox(1)); len(vs) != 0 {
		t.Errorf("no entries, nothing to name: %+v", vs)
	}
}

func TestHasProxyEntry(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  string
		want bool
	}{
		{"a proxy seat", pinCfg(plainEntry("a"), proxyEntry("b")), true},
		{"only plain entries", pinCfg(plainEntry("a"), pinEntry("b", "0", true)), false},
		{"an empty proxy value", pinCfg("  a:\n    ttl: 300\n    proxy: \"\"\n    cmd: x\n"), false},
		{"a file that does not parse", "models: [", false},
		{"no models", "macros: {}\n", false},
	} {
		if got := HasProxyEntry(tc.cfg); got != tc.want {
			t.Errorf("%s: HasProxyEntry = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A single-card box has no display card to be on or off (its only card is its work card), and the note is silent
// there, as the docs say, even on a reading that could not say which card the monitor is on: gpuprobe marks every
// device of such a reading unknown, a one-card box included, and with one card there is nothing to tell apart.
func TestUnseenIsSilentOnASingleCardBoxEvenWhenTheReadingCouldNotTell(t *testing.T) {
	cfg := pinCfg(proxyEntry("seat"), plainEntry("free"))
	dev := func(i int, uuid string) gpuprobe.Device {
		return gpuprobe.Device{Index: i, UUID: uuid, Name: "NVIDIA GeForce RTX 5060 Ti", TotalGiB: 15.9, FreeGiB: 15, AttachedUnknown: true}
	}
	one, _ := gpuprobe.BuildCards([]gpuprobe.Device{dev(0, pinCard5060A)}, "")
	if len(one) != 1 || !one[0].DisplayUnknown || one[0].Display {
		t.Fatalf("fixture: one card, unknown reading, never flagged: %+v", one)
	}
	if vs := AuditDisplayCapUnseen(cfg, one); len(vs) != 0 {
		t.Errorf("a single-card box is silent even on a reading that could not say which card the monitor is on: %+v", vs)
	}
	// The positive control: the same kind of reading on two cards is not silent (the monitor could be on either).
	two, _ := gpuprobe.BuildCards([]gpuprobe.Device{dev(0, pinCard5060A), dev(1, pinCard5070)}, "")
	vs := AuditDisplayCapUnseen(cfg, two)
	if len(vs) != 1 || vs[0].Where != "no display card known" {
		t.Errorf("control: two cards of an unknown reading get the one note saying so: %+v", vs)
	}
}
