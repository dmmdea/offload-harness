package gpuactivity

// driverevents_fixround_test.go: the review round on the driver-event reader (C-83-B fix round). Each test pins a
// way the reader gave a wrong or silent answer:
//
//   - a GPUID is the bus and device in UNPADDED hex of (bus<<8 | device): a card on bus 01 logs "GPUID: 100", which
//     the four-digit match missed, so a one-card box (a laptop's dGPU, a small desktop) placed none of its events.
//     testdata/nvlddmkm-153-lowbus.xml is two real records from such a box, hand-reduced (the host name and the
//     device object are placeholders: the repository is public). Two spellings occur: "Error occurred on GPUID: 100"
//     and "UCodeReset TDR occurred on GPUID:100" (no space after the colon);
//   - a card table that cannot be read left every event unplaced and counted as "names no card", with no note;
//   - a read that stopped part way (a record that is not XML after good ones) showed the head as the whole;
//   - on Linux `journalctl -k` implies -b, so a fault before a reboot was invisible under a "last 24 h" label.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// busOf reads the GPUID in any of its spellings to the "bus:device" the card table is keyed by.
func TestBusOfReadsEveryGPUIDSpelling(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"Error occurred on GPUID: b500", "b5:00"},
		{"Error occurred on GPUID: B500", "b5:00"},
		{"Error occurred on GPUID: 6500", "65:00"},
		{"Error occurred on GPUID: 2100", "21:00"},
		{"Error occurred on GPUID: 100", "01:00"},         // bus 01, device 00: three digits, unpadded
		{"UCodeReset TDR occurred on GPUID:100", "01:00"}, // no space after the colon
		{"Error occurred on GPUID: 0100", "01:00"},        // a padded spelling reads the same
		{"Error occurred on GPUID: 308", "03:08"},         // a non-zero device
		{"Error occurred on GPUID: a00", "0a:00"},
		{"Error occurred on GPUID: 8", "00:08"}, // bus 00
		{"NVRM: Xid (PCI:0000:01:00): 79, x", "01:00"},
		{"Graphics Exception: ESR 0x55a7b0=0xb040020", ""},
		{"Error occurred on GPUID: zz", ""},
		{"Error occurred on GPUID: 12345", ""}, // five digits is not a bus and a device
	} {
		if got := busOf(tc.text); got != tc.want {
			t.Errorf("busOf(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

// The real shape from a one-card box: the record's GPUID has no leading zero, and the card table's key does.
func TestParseWinEventsPlacesACardOnALowBus(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "nvlddmkm-153-lowbus.xml"))
	if err != nil {
		t.Fatal(err)
	}
	evs, err := ParseWinEvents(string(b))
	if err != nil || len(evs) != 2 {
		t.Fatalf("two records: %+v %v", evs, err)
	}
	for _, e := range evs {
		if e.Bus != "01:00" || e.ID != 153 || e.Source != "nvlddmkm" {
			t.Errorf("a GPUID of 100 is bus 01 device 00: %+v", e)
		}
		if strings.Contains(e.Text, "host") || strings.Contains(e.Text, `\Device`) {
			t.Errorf("the text carries the computer name or the device path: %q", e.Text)
		}
	}
	placed := AttributeEvents(evs, ParseBusTable("0, GPU-dddd0000-x, 00000000:01:00.0\n"))
	for _, e := range placed {
		if e.UUID != "GPU-dddd0000-x" || e.Card != 0 {
			t.Errorf("the card at 00000000:01:00.0 owns the event: %+v", e)
		}
	}
}

// A card table that cannot be read is said, with its cause: the events survive, unplaced, and the error names the
// table (it is not "no card can be placed for these").
func TestReadDriverEventsSaysWhenTheCardTableCannotBeRead(t *testing.T) {
	driverSeams(t, "windows", func(string, []string) (string, error) { return fixtureXML(t), nil },
		func([]string) (string, error) { return "", errors.New("nvidia-smi: boom") })
	evs, err := ReadDriverEvents(context.Background())
	if len(evs) != 2 {
		t.Fatalf("the events survive a missing card table: %+v", evs)
	}
	for _, e := range evs {
		if e.UUID != "" {
			t.Errorf("no table, no placement: %+v", e)
		}
	}
	if !errors.Is(err, ErrCardTableUnreadable) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("the failure must say the card table could not be read, with its cause, got %v", err)
	}
}

// The card-table query has its own budget. One 8 s bound over both children let a slow Get-WinEvent leave nvidia-smi
// nothing, which is exactly when a fault has just happened.
func TestTheCardTableQueryHasItsOwnBudget(t *testing.T) {
	old := driverEventTimeout
	driverEventTimeout = 40 * time.Millisecond
	t.Cleanup(func() { driverEventTimeout = old })
	smiSawLiveContext := false
	oldSMI, oldChild, oldGOOS, oldNow := smiRun, runChild, driverEventsGOOS, driverNow
	t.Cleanup(func() {
		smiRun, runChild, driverEventsGOOS, driverNow = oldSMI, oldChild, oldGOOS, oldNow
		resetDriverEvents()
	})
	driverEventsGOOS = "windows"
	driverNow = func() time.Time { return time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC) }
	resetDriverEvents()
	// A slow log read that spends the whole of its own budget and still answers.
	runChild = func(ctx context.Context, _ string, _ ...string) (string, error) {
		<-ctx.Done()
		return fixtureXML(t), nil
	}
	smiRun = func(ctx context.Context, _ ...string) (string, error) {
		smiSawLiveContext = ctx.Err() == nil
		return busTable, nil
	}
	evs, err := ReadDriverEvents(context.Background())
	if err != nil || len(evs) != 2 {
		t.Fatalf("the slow read still answers: %+v %v", evs, err)
	}
	if !smiSawLiveContext {
		t.Fatal("the card-table query ran on a context the log read had already used up")
	}
	if evs[0].UUID == "" {
		t.Errorf("with its own budget the table places the events: %+v", evs[0])
	}
}

// A good answer is held for a minute; one that could not place its events because the table failed is not: the
// failure is most likely at the moment of a fault (a reset driver answers nvidia-smi late), so the next poll tries again.
func TestACardTableFailureIsRetriedSoonNotHeldForAMinute(t *testing.T) {
	calls := 0
	smiFails := true
	driverSeams(t, "windows", func(string, []string) (string, error) { calls++; return fixtureXML(t), nil },
		func(args []string) (string, error) {
			if smiFails {
				return "", errors.New("nvidia-smi: driver resetting")
			}
			return smiBusTable(args)
		})
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	driverNow = func() time.Time { return now }
	if _, err := ReadDriverEvents(context.Background()); !errors.Is(err, ErrCardTableUnreadable) {
		t.Fatalf("first read: %v", err)
	}
	now = now.Add(driverEventRetryTTL / 2)
	_, _ = ReadDriverEvents(context.Background())
	if calls != 1 {
		t.Fatalf("a poll inside the retry window must not start the log reader again, started %d times", calls)
	}
	smiFails = false
	now = now.Add(driverEventRetryTTL)
	evs, err := ReadDriverEvents(context.Background())
	if err != nil || calls != 2 || len(evs) != 2 || evs[0].UUID == "" {
		t.Fatalf("past the retry window the whole read is made again and places the events: %d starts, %+v %v", calls, evs, err)
	}
}

// The events a failed table left unplaced are not "events that name no card": the note says the table was the cause,
// and the count that blames the event itself (an id 13 Graphics Exception carries no GPUID) stays out of it.
func TestSnapshotSaysWhenEventsCouldNotBePlacedAndDoesNotBlameThem(t *testing.T) {
	root := leaseCards(t, "gpu-cccc0000-x")
	events := []DriverEvent{{At: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC), Source: "nvlddmkm", ID: 153, Bus: "b5:00", Text: "Error occurred on GPUID: b500"}}
	readErr := fmt.Errorf("%w (nvidia-smi: boom): 1 driver event(s) in the last 24 h were not placed on cards", ErrCardTableUnreadable)
	v := Snapshot(context.Background(), driverOptions(root, events, readErr, nil))
	if !strings.Contains(v.DriverEventsNote, "card table") || !strings.Contains(v.DriverEventsNote, "boom") {
		t.Fatalf("the note must name the unreadable card table and its cause, got %q", v.DriverEventsNote)
	}
	if v.DriverEventsUnattributed != 0 {
		t.Errorf("events a failed table left unplaced are not events that name no card: counted %d", v.DriverEventsUnattributed)
	}
	text := strings.Join(v.Lines(), "\n")
	if strings.Contains(text, "name no card") {
		t.Errorf("the lines must not blame the events for what the table did:\n%s", text)
	}
	if !strings.Contains(text, "card table") {
		t.Errorf("the text view carries the note:\n%s", text)
	}
}

// A malformed record after good ones ends the read: the good ones are returned AND the stop is said, not a silent
// short list that reads as the whole set.
func TestAPartialWindowsReadReturnsTheEventsAndSaysItStopped(t *testing.T) {
	driverSeams(t, "windows", func(string, []string) (string, error) { return fixtureXML(t) + "\n<Event><broken", nil }, smiBusTable)
	evs, err := ReadDriverEvents(context.Background())
	if len(evs) != 2 {
		t.Fatalf("the two good records survive: %+v", evs)
	}
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("the read stopped part way and must say so, got %v", err)
	}
}

// What the snapshot does with a partial read: the events show, and a note says the list may be short.
func TestSnapshotNotesAnIncompleteReadAndStillShowsTheEvents(t *testing.T) {
	root := leaseCards(t, "gpu-cccc0000-x")
	v := Snapshot(context.Background(), driverOptions(root, []DriverEvent{ev(2, cardC, 153, "on card 2")}, errors.New("the event log read stopped after 1 record(s)"), nil))
	if len(v.DriverEvents) != 1 {
		t.Fatalf("the events that were read are shown: %+v", v.DriverEvents)
	}
	if !strings.Contains(v.DriverEventsNote, "incomplete") || !strings.Contains(v.DriverEventsNote, "stopped after 1") {
		t.Fatalf("a partial read carries a note saying the list may be short, got %q", v.DriverEventsNote)
	}
	if m := v.Map(); m["driver_events_note"] != v.DriverEventsNote {
		t.Errorf("the note rides in the map: %v", m)
	}
}

// journalctl -k implies -b (the current boot), so a fault that preceded a reboot - and a Xid such as 79, a card off
// the bus, often ends in one - was invisible under a "last 24 h" label. The kernel transport as a match covers every
// boot the journal holds; the window is the --since.
func TestTheLinuxReaderReadsEveryBootInTheWindow(t *testing.T) {
	var args []string
	driverSeams(t, "linux", func(n string, a []string) (string, error) {
		if n == "journalctl" {
			args = a
		}
		return journalXid, nil
	}, smiBusTable)
	if _, err := ReadDriverEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	joined := " " + strings.Join(args, " ") + " "
	for _, forbidden := range []string{" -k ", " --dmesg ", " -b ", " --boot "} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("journalctl%s implies the current boot only:%s", forbidden, joined)
		}
	}
	for _, want := range []string{" _TRANSPORT=kernel ", " --since ", " short-unix "} {
		if !strings.Contains(joined, want) {
			t.Errorf("journalctl must be asked for%swithin the window:%s", want, joined)
		}
	}
}

// The dmesg fallback is this boot's ring buffer only: when it is the source, the read says so.
func TestTheDmesgFallbackIsSaidToBeThisBootOnly(t *testing.T) {
	driverSeams(t, "linux", func(n string, _ []string) (string, error) {
		if n == "journalctl" {
			return "", errors.New("journalctl: not permitted")
		}
		return dmesgXid, nil
	}, smiBusTable)
	evs, err := ReadDriverEvents(context.Background())
	if len(evs) != 1 {
		t.Fatalf("dmesg answers: %+v", evs)
	}
	if err == nil || !strings.Contains(err.Error(), "this boot") || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("the fallback must say it reads this boot only, and why journalctl was not used, got %v", err)
	}
}
