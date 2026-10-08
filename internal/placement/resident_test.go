package placement

import (
	"strings"
	"testing"
)

// ResidentVerdict is the display layer's guards put to a seat that is ALREADY loaded: admission asks
// them once, this asks them again while the twin sits on the desktop's card. It is the same
// presence rule and the same display card as the guards, with one difference that matters: the
// floor is free(card) >= floor, not free - footprint >= floor, because a loaded twin's footprint is
// already out of the free number, and subtracting it again would read a healthy twin as a violation.

func residentLive(freeOnDisplay float64, pres Presence) Live {
	f := admitting()
	f.free["1"] = freeOnDisplay
	f.pres = &pres
	return f.live()
}

var (
	presAway   = Presence{Mode: "away", Known: true, Away: true, Note: "operator override: away"}
	presLocked = Presence{Mode: "auto", Known: true, Away: true, Locked: true, Note: "console session locked"}
	presDesk   = Presence{Mode: "auto", Known: true, Away: false, IdleSec: 12, Note: "idle 12s < threshold 15m0s"}
	presUnk    = Presence{Mode: "auto", Known: false, Note: "no session is attached to the console"}
	presMode   = Presence{Mode: "present", Known: true, Away: false, Note: "operator override: present (the default)"}
)

func TestResidentVerdictHoldsTheFloorAndThePresenceGuardOfALoadedSeat(t *testing.T) {
	l, _ := displayLayerAndSeat(t) // guards display_floor, presence; floor 4; the twin declares 6.2

	cases := []struct {
		name    string
		free    float64
		pres    Presence
		wantOK  bool
		wantIn  []string // each must appear in the joined reasons when refused
		wantOut []string // none may appear
	}{
		{"away with the floor kept", 6, presAway, true, nil, nil},
		{"console locked with the floor kept", 6, presLocked, true, nil, nil},
		{"free exactly at the floor is kept", 4, presAway, true, nil, nil},
		{"the footprint is not subtracted a second time", 5, presAway, true, nil, []string{"footprint"}},
		{"free under the floor", 3.9, presAway, false, []string{"floor 4.0", "3.9"}, []string{"presence"}},
		{"the operator is at the desk", 12, presDesk, false, []string{"presence", "desk"}, []string{"floor"}},
		{"presence unreadable is not away", 12, presUnk, false, []string{"presence", "unknown"}, nil},
		{"the default mode never opens the card", 12, presMode, false, []string{"presence", "present"}, nil},
		{"both guards refuse, both are named", 3, presDesk, false, []string{"floor", "presence"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reasons := ResidentVerdict(l, residentLive(tc.free, tc.pres))
			joined := strings.Join(reasons, " | ")
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (%s)", ok, tc.wantOK, joined)
			}
			if tc.wantOK && len(reasons) != 0 {
				t.Errorf("a held seat carries no refusal reasons, got %v", reasons)
			}
			for _, w := range tc.wantIn {
				if !strings.Contains(joined, w) {
					t.Errorf("reasons %q must contain %q", joined, w)
				}
			}
			for _, w := range tc.wantOut {
				if strings.Contains(joined, w) {
					t.Errorf("reasons %q must not contain %q", joined, w)
				}
			}
		})
	}
}

// Every input is fail-closed: a seat that is loaded on a card whose floor cannot be read is not shown
// to be safe, so the verdict is a refusal, as the admission guard's is.
func TestResidentVerdictFailsClosedOnAnUnreadableInput(t *testing.T) {
	l, _ := displayLayerAndSeat(t)

	noPresence := admitting().live()
	noPresence.Presence = nil
	if ok, reasons := ResidentVerdict(l, noPresence); ok || !strings.Contains(strings.Join(reasons, " "), "presence") {
		t.Fatalf("no presence reader must refuse by name, got ok=%v %v", ok, reasons)
	}

	noDevices := admitting().live()
	noDevices.DeviceFree = nil
	if ok, reasons := ResidentVerdict(l, noDevices); ok || !strings.Contains(strings.Join(reasons, " "), "unreadable") {
		t.Fatalf("no free-VRAM reader must refuse as unreadable, got ok=%v %v", ok, reasons)
	}

	absent := admitting()
	delete(absent.free, "1")
	if ok, reasons := ResidentVerdict(l, absent.live()); ok || !strings.Contains(strings.Join(reasons, " "), "unreadable") {
		t.Fatalf("a display card the probe does not list must refuse as unreadable, got ok=%v %v", ok, reasons)
	}
}

// The display card is the same card the guard resolves and cross-checks: a UUID pin resolves to its
// index, and a monitor that is somewhere else is a refusal here as at admission.
func TestResidentVerdictReadsTheSameDisplayCardAsTheGuard(t *testing.T) {
	l, _ := displayLayerAndSeat(t)
	l.DisplayDevice = "GPU-8888bbbb"
	f := admitting()
	f.index = map[string]string{"GPU-8888bbbb": "1"}
	f.free["GPU-8888bbbb"] = 6
	live := f.live()
	if ok, reasons := ResidentVerdict(l, live); !ok {
		t.Fatalf("a UUID pin with the floor kept must hold: %v", reasons)
	}
	live.ScreenCards = screenAt("0")
	ok, reasons := ResidentVerdict(l, live)
	if ok || !strings.Contains(strings.Join(reasons, " "), "monitor on index 0") {
		t.Fatalf("the screen cross-check applies to a loaded seat too, got ok=%v %v", ok, reasons)
	}
}

// Only the two guards that are about the desktop are put to a loaded seat. host_ram bounds what a seat
// holds in RAM at load time and is no violation of a seat that is already up; a layer with no guards
// has nothing to violate.
func TestResidentVerdictOnlyEvaluatesTheDesktopGuards(t *testing.T) {
	l, _ := displayLayerAndSeat(t)
	l.Guards = []string{"host_ram"}
	if ok, reasons := ResidentVerdict(l, Live{}); !ok || len(reasons) != 0 {
		t.Fatalf("host_ram is an admission guard; a loaded seat has nothing to violate, got ok=%v %v", ok, reasons)
	}
	l.Guards = nil
	if ok, _ := ResidentVerdict(l, Live{}); !ok {
		t.Fatal("a layer with no guards has nothing to violate")
	}
	l.Guards = []string{"moon_phase"}
	if ok, reasons := ResidentVerdict(l, Live{}); ok || !strings.Contains(strings.Join(reasons, " "), "moon_phase") {
		t.Fatalf("an unknown guard fails closed, as it does at admission, got ok=%v %v", ok, reasons)
	}
}

// PresenceAllows is the one rule behind the guard and the check, so a change to either is a change to both.
func TestPresenceAllowsIsTheGuardsRule(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Presence
		want bool
	}{
		{"away override", presAway, true},
		{"locked console", presLocked, true},
		{"idle past the threshold", Presence{Mode: "auto", Known: true, Away: true, IdleSec: 1800, Note: "idle 30m0s"}, true},
		{"at the desk", presDesk, false},
		{"unknown", presUnk, false},
		{"present", presMode, false},
		{"a mode nobody defined", Presence{Mode: "maybe", Known: true, Away: true}, false},
	} {
		if got, reading := PresenceAllows(tc.p); got != tc.want || reading == "" {
			t.Errorf("%s: PresenceAllows = %v %q, want %v and a reading", tc.name, got, reading, tc.want)
		}
	}
}
