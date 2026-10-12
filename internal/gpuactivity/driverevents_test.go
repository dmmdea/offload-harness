package gpuactivity

// driverevents_test.go: the NVIDIA driver's own fault events for the leased cards (C-83-B, A-128 part 3).
//
// A card that faults under a lease leaves one line in the driver's log (Windows: the System log, provider nvlddmkm,
// event 153 "Error occurred on GPUID: b500"; Linux: "NVRM: Xid (PCI:0000:b5:00): 79, ..." in the kernel log), and
// nothing in `gpu status` showed it: sessions found the events by hand, weeks late. They are shown as facts under the
// leased cards, never as a verdict input (a fault and a kill look alike in the log; telling them apart takes A-128's
// kill test), and a reader that cannot read the log is a note and never an error.
//
// The Windows fixture (testdata/nvlddmkm-153.xml) is two real records hand-reduced from the log: the host name is a
// placeholder (the repository is public).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

const (
	busTable = "0, GPU-aaaa0000-x, 00000000:17:00.0\n1, GPU-bbbb0000-x, 00000000:65:00.0\n2, GPU-cccc0000-x, 00000000:B5:00.0\n"

	// A journalctl -o short-unix line and a dmesg --time-format iso line, with a placeholder host.
	journalXid = "1791709549.098666 testhost kernel: NVRM: Xid (PCI:0000:b5:00): 79, pid=1234, name=python3, GPU has fallen off the bus.\n"
	dmesgXid   = "2026-10-11T04:05:49,123456-05:00 kern  :err   : NVRM: Xid (PCI:0000:65:00): 13, Graphics Exception: ESR 0x404600=0x80000000\n"
)

func fixtureXML(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "nvlddmkm-153.xml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The record's GPUID is the card's PCI bus and device in hex: "b500" is bus b5, device 00, which is the card whose
// nvidia-smi pci.bus_id ends B5:00.0. The device object path in the record's first Data is noise and is dropped.
func TestParseWinEventNvlddmkm153AttributesTheEventToItsBus(t *testing.T) {
	evs, err := ParseWinEvents(fixtureXML(t))
	if err != nil {
		t.Fatalf("ParseWinEvents: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("two records in the fixture, got %d: %+v", len(evs), evs)
	}
	first := evs[0]
	if first.Source != "nvlddmkm" || first.ID != 153 || first.Bus != "b5:00" {
		t.Errorf("first event = %+v, want source nvlddmkm, id 153, bus b5:00", first)
	}
	if want := time.Date(2026, 10, 11, 9, 5, 49, 361824100, time.UTC); !first.At.Equal(want) {
		t.Errorf("first event time = %v, want %v", first.At, want)
	}
	if first.Text != "Error occurred on GPUID: b500" {
		t.Errorf("first event text = %q, want the message without the device object path", first.Text)
	}
	if evs[1].Bus != "65:00" || evs[1].ID != 153 {
		t.Errorf("second event = %+v, want bus 65:00 id 153", evs[1])
	}
	if strings.Contains(first.Text, "host") || strings.Contains(first.Text, `\Device`) {
		t.Errorf("the text carries the computer name or the device path: %q", first.Text)
	}
}

// The reader's output is a sequence of records, not one document; a record that is not XML ends the read with the
// events parsed so far and an error, never a panic.
func TestParseWinEventsReadsASequenceAndStopsOnNoise(t *testing.T) {
	evs, err := ParseWinEvents(fixtureXML(t) + "\nnot xml at all <<<")
	if err == nil {
		t.Fatal("trailing noise must be reported")
	}
	if len(evs) != 2 {
		t.Fatalf("the two good records before the noise must survive, got %+v", evs)
	}
	if evs, err := ParseWinEvents(""); err != nil || len(evs) != 0 {
		t.Fatalf("an empty read is no events and no error, got %+v, %v", evs, err)
	}
}

func TestParseKernelLogXidLine(t *testing.T) {
	evs := ParseKernelLog("1791709548.5 testhost kernel: audit: not an xid line\n" + journalXid + dmesgXid)
	if len(evs) != 2 {
		t.Fatalf("two Xid lines among three, got %d: %+v", len(evs), evs)
	}
	j := evs[0]
	if j.Source != "Xid" || j.ID != 79 || j.Bus != "b5:00" || !strings.Contains(j.Text, "GPU has fallen off the bus") {
		t.Errorf("journal line = %+v", j)
	}
	if want := time.Unix(1791709549, 98666000).UTC(); !j.At.Equal(want) {
		t.Errorf("journal line time = %v, want %v", j.At, want)
	}
	d := evs[1]
	if d.Source != "Xid" || d.ID != 13 || d.Bus != "65:00" || !strings.Contains(d.Text, "Graphics Exception") {
		t.Errorf("dmesg line = %+v", d)
	}
	if want := time.Date(2026, 10, 11, 9, 5, 49, 123456000, time.UTC); !d.At.Equal(want) {
		t.Errorf("dmesg line time = %v, want %v", d.At, want)
	}
	if strings.Contains(j.Text+d.Text, "testhost") {
		t.Errorf("the host name must not ride in the text: %q %q", j.Text, d.Text)
	}
}

func TestParseBusTableKeysTheCardByBusAndDeviceInAnyCase(t *testing.T) {
	table := ParseBusTable(busTable + "garbage\n")
	if len(table) != 3 {
		t.Fatalf("three cards, got %+v", table)
	}
	if c := table["b5:00"]; c.Index != 2 || c.UUID != "GPU-cccc0000-x" {
		t.Errorf("bus b5:00 = %+v, want card 2 GPU-cccc0000-x (the table says B5:00.0 upper-case)", c)
	}
	if c := table["17:00"]; c.Index != 0 {
		t.Errorf("bus 17:00 = %+v, want card 0", c)
	}
}

func TestAttributeEventsNamesTheCardAndLeavesAnUnknownBusUnplaced(t *testing.T) {
	evs := []DriverEvent{{Bus: "b5:00", ID: 153}, {Bus: "ff:00", ID: 153}, {ID: 13}}
	got := AttributeEvents(evs, ParseBusTable(busTable))
	if got[0].Card != 2 || got[0].UUID != "GPU-cccc0000-x" {
		t.Errorf("a known bus is placed: %+v", got[0])
	}
	if got[1].UUID != "" || got[2].UUID != "" {
		t.Errorf("a bus the table does not list, and an event with no bus, stay unplaced: %+v %+v", got[1], got[2])
	}
	if evs[0].UUID != "" {
		t.Error("AttributeEvents must not rewrite its input")
	}
}

// --- the readers ---

// driverSeams points the readers at canned output. child gets the program and its arguments.
func driverSeams(t *testing.T, goos string, child func(name string, args []string) (string, error), smi func(args []string) (string, error)) {
	t.Helper()
	oldGOOS, oldChild, oldSMI, oldNow := driverEventsGOOS, runChild, smiRun, driverNow
	driverEventsGOOS = goos
	runChild = func(_ context.Context, name string, args ...string) (string, error) { return child(name, args) }
	smiRun = func(_ context.Context, args ...string) (string, error) { return smi(args) }
	driverNow = func() time.Time { return time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC) }
	resetDriverEvents()
	t.Cleanup(func() {
		driverEventsGOOS, runChild, smiRun, driverNow = oldGOOS, oldChild, oldSMI, oldNow
		resetDriverEvents()
	})
}

func smiBusTable(args []string) (string, error) {
	if len(args) > 0 && strings.Contains(args[0], "pci.bus_id") {
		return busTable, nil
	}
	return "", errors.New("unexpected nvidia-smi query: " + strings.Join(args, " "))
}

func TestReadDriverEventsOnWindowsReadsTheSystemLogAndPlacesTheCards(t *testing.T) {
	xml := fixtureXML(t)
	driverSeams(t, "windows", func(string, []string) (string, error) { return xml, nil }, smiBusTable)
	evs, err := ReadDriverEvents(context.Background())
	if err != nil {
		t.Fatalf("ReadDriverEvents: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("two events, got %+v", evs)
	}
	if evs[0].Card != 2 || evs[0].UUID != "GPU-cccc0000-x" || evs[1].Card != 1 || evs[1].UUID != "GPU-bbbb0000-x" {
		t.Errorf("the events must name their cards by the bus table: %+v", evs)
	}
}

// The Windows read is one read-only, windowless PowerShell from System32: no profile, no prompt, a 24 h window, a cap,
// the record as XML; a log with no matching event is an empty answer, not a failure.
func TestTheWindowsReaderIsOneReadOnlySystem32PowerShell(t *testing.T) {
	var name string
	var args []string
	calls := 0
	driverSeams(t, "windows", func(n string, a []string) (string, error) { calls++; name, args = n, a; return "", nil }, smiBusTable)
	if _, err := ReadDriverEvents(context.Background()); err != nil {
		t.Fatalf("an empty read is not a failure: %v", err)
	}
	if calls != 1 {
		t.Fatalf("one child process per read, got %d", calls)
	}
	if !strings.HasSuffix(strings.ToLower(filepath.ToSlash(name)), "/system32/windowspowershell/v1.0/powershell.exe") {
		t.Errorf("the program must be the System32 PowerShell by full path, got %q", name)
	}
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{" -NoProfile ", " -NonInteractive ", "Get-WinEvent", "ProviderName='nvlddmkm'", "LogName='System'", "AddHours(-24)", "-MaxEvents", "ToXml()", "NoMatchingEventsFound"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the PowerShell command must contain %q:\n%s", want, joined)
		}
	}
	for _, forbidden := range []string{"Set-", "Remove-", "Clear-", "Stop-", "Restart-", "New-", "Out-File", "wevtutil", "Invoke-"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("the reader must be read-only, but the command contains %q:\n%s", forbidden, joined)
		}
	}
}

func TestAWindowsReaderFailureCarriesTheReason(t *testing.T) {
	driverSeams(t, "windows", func(string, []string) (string, error) { return "", errors.New("Get-WinEvent: access denied") }, smiBusTable)
	if _, err := ReadDriverEvents(context.Background()); err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("a failed read must return its reason, got %v", err)
	}
}

func TestReadDriverEventsOnLinuxReadsTheKernelLogAndFallsBackToDmesg(t *testing.T) {
	var names []string
	driverSeams(t, "linux", func(n string, a []string) (string, error) {
		names = append(names, n)
		if n == "journalctl" {
			if !strings.Contains(strings.Join(a, " "), "-k") || !strings.Contains(strings.Join(a, " "), "short-unix") {
				t.Errorf("journalctl must read the kernel log with unix times: %v", a)
			}
			return "", errors.New("journalctl: not permitted")
		}
		return dmesgXid, nil
	}, smiBusTable)
	evs, err := ReadDriverEvents(context.Background())
	if err != nil {
		t.Fatalf("dmesg answers when journalctl cannot: %v", err)
	}
	if strings.Join(names, ",") != "journalctl,dmesg" {
		t.Errorf("journalctl first, dmesg on its failure, got %v", names)
	}
	if len(evs) != 1 || evs[0].ID != 13 || evs[0].UUID != "GPU-bbbb0000-x" {
		t.Errorf("the dmesg Xid is placed on bus 65:00 = card 1: %+v", evs)
	}

	names = nil
	driverSeams(t, "linux", func(n string, a []string) (string, error) { names = append(names, n); return journalXid, nil }, smiBusTable)
	evs, err = ReadDriverEvents(context.Background())
	if err != nil || len(evs) != 1 || evs[0].ID != 79 || evs[0].Card != 2 || strings.Join(names, ",") != "journalctl" {
		t.Errorf("journalctl alone when it answers: %+v %v (calls %v)", evs, err, names)
	}
}

func TestBothLinuxReadersFailingIsOneErrorNamingBoth(t *testing.T) {
	driverSeams(t, "linux", func(n string, _ []string) (string, error) { return "", errors.New(n + " failed") }, smiBusTable)
	_, err := ReadDriverEvents(context.Background())
	if err == nil || !strings.Contains(err.Error(), "journalctl failed") || !strings.Contains(err.Error(), "dmesg failed") {
		t.Fatalf("the error must name both readers, got %v", err)
	}
}

func TestAnOSWithNoSourceSaysSo(t *testing.T) {
	driverSeams(t, "darwin", func(string, []string) (string, error) { t.Fatal("no child on an OS with no source"); return "", nil }, smiBusTable)
	if _, err := ReadDriverEvents(context.Background()); !errors.Is(err, ErrNoDriverEventSource) {
		t.Fatalf("want ErrNoDriverEventSource, got %v", err)
	}
}

// An event older than the window is not "recent"; one with no readable time is kept (an unknown time must not read as
// "long ago").
func TestReadDriverEventsDropsWhatIsOutsideTheWindow(t *testing.T) {
	old := "2026-10-09T09:05:49,000000+00:00 kern  :err   : NVRM: Xid (PCI:0000:b5:00): 79, long ago\n"
	undated := "kern  :err   : NVRM: Xid (PCI:0000:b5:00): 31, no time on this line\n"
	driverSeams(t, "linux", func(string, []string) (string, error) { return old + undated + dmesgXid, nil }, smiBusTable)
	evs, err := ReadDriverEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []int
	for _, e := range evs {
		ids = append(ids, e.ID)
	}
	if len(ids) != 2 || ids[0] != 31 || ids[1] != 13 {
		t.Fatalf("the two-day-old event goes, the undated and the recent stay, got %v", ids)
	}
}

// A card table that cannot be read leaves the events unplaced; the events themselves are not lost.
func TestReadDriverEventsKeepsTheEventsWhenTheCardTableCannotBeRead(t *testing.T) {
	driverSeams(t, "windows", func(string, []string) (string, error) { return fixtureXML(t), nil },
		func([]string) (string, error) { return "", errors.New("nvidia-smi: not found") })
	evs, err := ReadDriverEvents(context.Background())
	if err != nil || len(evs) != 2 {
		t.Fatalf("the events survive a missing card table: %+v %v", evs, err)
	}
	for _, e := range evs {
		if e.UUID != "" {
			t.Errorf("no table, no placement: %+v", e)
		}
	}
}

// 60 s: a status poll must not start PowerShell every time.
func TestReadDriverEventsIsCachedForSixtySeconds(t *testing.T) {
	calls := 0
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	driverSeams(t, "windows", func(string, []string) (string, error) { calls++; return fixtureXML(t), nil }, smiBusTable)
	driverNow = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if _, err := ReadDriverEvents(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("three reads inside a minute started the reader %d times, want 1", calls)
	}
	now = now.Add(59 * time.Second)
	_, _ = ReadDriverEvents(context.Background())
	if calls != 1 {
		t.Fatalf("59 s later is still the cached answer, reader started %d times", calls)
	}
	now = now.Add(2 * time.Second)
	_, _ = ReadDriverEvents(context.Background())
	if calls != 2 {
		t.Fatalf("past 60 s the log is read again, reader started %d times", calls)
	}
}

// A failure is cached for the same minute: a broken reader must not start a process per status call either.
func TestAReaderFailureIsCachedToo(t *testing.T) {
	calls := 0
	driverSeams(t, "windows", func(string, []string) (string, error) { calls++; return "", errors.New("boom") }, smiBusTable)
	for i := 0; i < 3; i++ {
		if _, err := ReadDriverEvents(context.Background()); err == nil {
			t.Fatal("the failure must come back each time")
		}
	}
	if calls != 1 {
		t.Fatalf("a failing reader was started %d times in a minute, want 1", calls)
	}
}

// --- the snapshot ---

var (
	cardA = "GPU-aaaa0000-x"
	cardB = "GPU-bbbb0000-x"
	cardC = "GPU-cccc0000-x"
)

func driverOptions(root string, events []DriverEvent, err error, reads *int) Options {
	return Options{
		StateDir:  root,
		SampleGPU: true,
		Sampler: func(context.Context) ([]GPU, error) {
			return []GPU{{Index: 0, UUID: cardA, Name: "Card", MemTotalMiB: 16384, UtilKnown: true}, {Index: 1, UUID: cardB, Name: "Card", MemTotalMiB: 16384, UtilKnown: true}, {Index: 2, UUID: cardC, Name: "Card", MemTotalMiB: 16384, UtilKnown: true}}, nil
		},
		ProcSampler: func(context.Context) ([]GPUProcess, error) { return nil, nil },
		DriverEvents: func(context.Context) ([]DriverEvent, error) {
			if reads != nil {
				*reads++
			}
			return events, err
		},
	}
}

func leaseCards(t *testing.T, devices ...string) string {
	t.Helper()
	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render", Devices: devices, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	return root
}

func ev(card int, uuid string, id int, text string) DriverEvent {
	return DriverEvent{At: time.Date(2026, 10, 11, 9, 0, card, 0, time.UTC), Source: "nvlddmkm", ID: id, Bus: "b5:00", Card: card, UUID: uuid, Text: text}
}

func TestEventsAreShownOnlyForTheLeasedCards(t *testing.T) {
	root := leaseCards(t, "gpu-cccc0000-x") // the lease sits on card 2
	events := []DriverEvent{ev(0, cardA, 153, "on card 0"), ev(2, strings.ToUpper(cardC), 153, "on card 2"), ev(1, cardB, 153, "on card 1")}
	v := Snapshot(context.Background(), driverOptions(root, events, nil, nil))
	if len(v.DriverEvents) != 1 || v.DriverEvents[0].Text != "on card 2" {
		t.Fatalf("only the leased card's event is shown, got %+v", v.DriverEvents)
	}
}

func TestEveryAttributedEventShowsOnAWholeNodeLease(t *testing.T) {
	root := leaseCards(t) // no devices: the whole node
	events := []DriverEvent{ev(0, cardA, 153, "on card 0"), ev(2, cardC, 13, "on card 2")}
	v := Snapshot(context.Background(), driverOptions(root, events, nil, nil))
	if len(v.DriverEvents) != 2 {
		t.Fatalf("a whole-node lease holds every card, so every placed event shows, got %+v", v.DriverEvents)
	}
}

// Newest first, and a storm cannot bloat a status answer.
func TestDriverEventsAreNewestFirstAndCapped(t *testing.T) {
	root := leaseCards(t)
	var events []DriverEvent
	for i := 0; i < 30; i++ {
		e := ev(2, cardC, 153, "storm")
		e.At = time.Date(2026, 10, 11, 9, 0, i, 0, time.UTC)
		events = append(events, e)
	}
	v := Snapshot(context.Background(), driverOptions(root, events, nil, nil))
	if len(v.DriverEvents) != 20 || v.DriverEventsTotal != 30 {
		t.Fatalf("the list is capped at 20 and says there were 30, got %d / %d", len(v.DriverEvents), v.DriverEventsTotal)
	}
	if !v.DriverEvents[0].At.After(v.DriverEvents[1].At) {
		t.Errorf("newest first: %v then %v", v.DriverEvents[0].At, v.DriverEvents[1].At)
	}
	if want := time.Date(2026, 10, 11, 9, 0, 29, 0, time.UTC); !v.DriverEvents[0].At.Equal(want) {
		t.Errorf("the newest 20 are kept, first is %v, want %v", v.DriverEvents[0].At, want)
	}
}

func TestEventsOfNoKnownCardAreCountedNotShown(t *testing.T) {
	root := leaseCards(t, "gpu-cccc0000-x")
	events := []DriverEvent{ev(2, cardC, 153, "placed"), {At: time.Now(), Source: "nvlddmkm", ID: 13, Text: "no bus on this one"}}
	v := Snapshot(context.Background(), driverOptions(root, events, nil, nil))
	if len(v.DriverEvents) != 1 || v.DriverEventsUnattributed != 1 {
		t.Fatalf("one shown, one counted as unplaced, got %+v / %d", v.DriverEvents, v.DriverEventsUnattributed)
	}
	if m := v.Map(); m["driver_events_unattributed"] != 1 {
		t.Errorf("the count rides in the map so an empty list is not read as no faults: %v", m)
	}
}

// Facts about the leased card: a free card is a place in line, and the log is read only for a lease.
func TestNothingIsReadWhileNoLeaseIsHeldOrTheGPUSampleIsOff(t *testing.T) {
	reads := 0
	v := Snapshot(context.Background(), driverOptions(t.TempDir(), []DriverEvent{ev(2, cardC, 153, "x")}, nil, &reads))
	if reads != 0 || len(v.DriverEvents) != 0 {
		t.Fatalf("no lease, no read: %d reads, %+v", reads, v.DriverEvents)
	}
	root := leaseCards(t, "gpu-cccc0000-x")
	o := driverOptions(root, []DriverEvent{ev(2, cardC, 153, "x")}, nil, &reads)
	o.SampleGPU = false
	if v := Snapshot(context.Background(), o); reads != 0 || len(v.DriverEvents) != 0 {
		t.Fatalf("no GPU sample (a test box, a box with no driver), no read: %d reads, %+v", reads, v.DriverEvents)
	}
}

// A box that wires no source, and an OS that has none, print byte-identically to today.
func TestNoEventSourceMeansNoKey(t *testing.T) {
	root := leaseCards(t, "gpu-cccc0000-x")
	for name, mutate := range map[string]func(*Options){
		"no reader wired": func(o *Options) { o.DriverEvents = nil },
		"an OS with no log": func(o *Options) {
			o.DriverEvents = func(context.Context) ([]DriverEvent, error) { return nil, ErrNoDriverEventSource }
		},
		"a log with no event": func(o *Options) {
			o.DriverEvents = func(context.Context) ([]DriverEvent, error) { return nil, nil }
		},
	} {
		o := driverOptions(root, nil, nil, nil)
		mutate(&o)
		v := Snapshot(context.Background(), o)
		m := v.Map()
		for k := range m {
			if strings.HasPrefix(k, "driver_events") {
				t.Errorf("%s: the map carries %q", name, k)
			}
		}
		for _, ln := range v.Lines() {
			if strings.Contains(strings.ToLower(ln), "driver") {
				t.Errorf("%s: a line mentions the driver: %q", name, ln)
			}
		}
		if v.DriverEventsNote != "" {
			t.Errorf("%s: unexpected note %q", name, v.DriverEventsNote)
		}
	}
}

func TestAReaderFailureIsANoteNotAnError(t *testing.T) {
	root := leaseCards(t, "gpu-cccc0000-x")
	base := Snapshot(context.Background(), driverOptions(root, nil, nil, nil))
	v := Snapshot(context.Background(), driverOptions(root, nil, errors.New("Get-WinEvent: access denied"), nil))
	if !strings.Contains(v.DriverEventsNote, "access denied") || !strings.Contains(v.DriverEventsNote, "driver events") {
		t.Fatalf("the failure rides as a note naming the reason, got %q", v.DriverEventsNote)
	}
	if v.Verdict != base.Verdict || v.Note != base.Note {
		t.Errorf("a reader failure must not move the verdict: %q/%q vs %q/%q", v.Verdict, v.Note, base.Verdict, base.Note)
	}
	if m := v.Map(); m["driver_events_note"] != v.DriverEventsNote {
		t.Errorf("the note rides in the map: %v", m)
	}
	found := false
	for _, ln := range v.Lines() {
		found = found || strings.Contains(ln, "access denied")
	}
	if !found {
		t.Errorf("the text view says why there is nothing to show: %v", v.Lines())
	}
}

// The events never move the verdict (facts, not a verdict input).
func TestDriverEventsNeverMoveTheVerdict(t *testing.T) {
	root := leaseCards(t, "gpu-cccc0000-x")
	base := Snapshot(context.Background(), driverOptions(root, nil, nil, nil))
	v := Snapshot(context.Background(), driverOptions(root, []DriverEvent{ev(2, cardC, 153, "x")}, nil, nil))
	if v.Verdict != base.Verdict || v.Note != base.Note {
		t.Fatalf("an event on the leased card moved the verdict: %q/%q vs %q/%q", v.Verdict, v.Note, base.Verdict, base.Note)
	}
}

func TestViewMapAndLinesCarryTheEvents(t *testing.T) {
	root := leaseCards(t, "gpu-cccc0000-x")
	v := Snapshot(context.Background(), driverOptions(root, []DriverEvent{ev(2, cardC, 153, "Error occurred on GPUID: b500")}, nil, nil))
	raw, err := json.Marshal(v.Map())
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Events []struct {
			Source string `json:"source"`
			ID     int    `json:"id"`
			Bus    string `json:"bus"`
			Card   int    `json:"card"`
			UUID   string `json:"uuid"`
			Text   string `json:"text"`
			At     string `json:"at"`
		} `json:"driver_events"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || len(m.Events) != 1 {
		t.Fatalf("the map carries one driver event: %v\n%s", err, raw)
	}
	if e := m.Events[0]; e.Source != "nvlddmkm" || e.ID != 153 || e.Bus != "b5:00" || e.Card != 2 || e.UUID != cardC || e.At == "" {
		t.Errorf("event JSON = %+v", e)
	}
	text := strings.Join(v.Lines(), "\n")
	for _, want := range []string{"driver events", "card 2", "nvlddmkm 153", "Error occurred on GPUID: b500"} {
		if !strings.Contains(text, want) {
			t.Errorf("the text view must contain %q:\n%s", want, text)
		}
	}
}

// ReadDriverEvents is safe for the concurrent status calls of one MCP server.
func TestReadDriverEventsIsSafeConcurrently(t *testing.T) {
	driverSeams(t, "windows", func(string, []string) (string, error) { return fixtureXML(t), nil }, smiBusTable)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if evs, err := ReadDriverEvents(context.Background()); err != nil || len(evs) != 2 {
				t.Errorf("concurrent read: %+v %v", evs, err)
			}
		}()
	}
	wg.Wait()
}
