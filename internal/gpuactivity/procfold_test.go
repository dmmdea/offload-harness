package gpuactivity

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The lease view lists the processes on the cards. On a Windows box with a screen, nvidia-smi
// types every window of the desktop as a process on the display card and sizes none of them
// (used_memory [N/A]); the 3-card box listed 31 such rows, and the one process that mattered
// (a python on a work card, unsized as well) was a needle in them (F18, 2026-10-09). The rows
// the display card owns and nvidia-smi cannot size are one count per card; every other row stays.

const (
	foldWork0   = "GPU-1111aaaa-2222-3333-4444-555566667777" // a work card
	foldDisplay = "GPU-8888bbbb-9999-cccc-dddd-eeeeffff0000" // the card the monitor is on
	foldWork2   = "GPU-3333cccc-0000-0000-0000-000000000000" // a work card
)

// foldCards is the 3-card box; the monitor's card carries the flags given.
func foldCards(displayActive, displayAttached bool) []GPU {
	return []GPU{
		{Index: 0, UUID: foldWork0, Name: "RTX A", UtilPct: 90, UtilKnown: true, MemTotalMiB: 16311},
		{Index: 1, UUID: foldDisplay, Name: "RTX B", UtilPct: 4, UtilKnown: true, MemTotalMiB: 16303, DisplayActive: displayActive, DisplayAttached: displayAttached},
		{Index: 2, UUID: foldWork2, Name: "RTX C", UtilPct: 0, UtilKnown: true, MemTotalMiB: 16311},
	}
}

// desktopRows is n windows of the desktop: distinct pids, memory unknown, on the given card.
func desktopRows(n int, card string) []GPUProcess {
	apps := []string{"explorer.exe", "chrome.exe", "msedgewebview2.exe", "slack.exe", "discord.exe"}
	rows := make([]GPUProcess, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, GPUProcess{PID: 20000 + i, Name: `C:\Program Files\desk\` + apps[i%len(apps)], GPUUUID: card})
	}
	return rows
}

// survivors are the rows that must outlive any amount of desktop: the real holder (unsized, on a
// work card: WDDM sizes no process), a sized process on a work card, and a sized process that
// sits on the display card.
func survivors() []GPUProcess {
	return []GPUProcess{
		{PID: 900, Name: `C:\Python\python.exe`, GPUUUID: foldWork0},
		{PID: 901, Name: "/opt/llama/llama-server", UsedMiB: 9000, UsedKnown: true, GPUUUID: foldWork2},
		{PID: 902, Name: `C:\Tools\capture.exe`, UsedMiB: 120, UsedKnown: true, GPUUUID: foldDisplay},
	}
}

// mapJSON is View.Map() as a reader sees it: marshalled and decoded.
func mapJSON(t *testing.T, v View) map[string]any {
	t.Helper()
	b, err := json.Marshal(v.Map())
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func listedPIDs(t *testing.T, m map[string]any) []int {
	t.Helper()
	rows, ok := m["gpu_processes"].([]any)
	if !ok {
		t.Fatalf("gpu_processes must be a list, got %T (%v)", m["gpu_processes"], m["gpu_processes"])
	}
	var pids []int
	for _, r := range rows {
		pids = append(pids, int(r.(map[string]any)["pid"].(float64)))
	}
	return pids
}

func TestMapFoldsADisplayCardsUnsizedProcessesIntoOneCountPerCard(t *testing.T) {
	procs := append(desktopRows(31, foldDisplay), survivors()...)
	// A row that names no card is not provably the desktop's: it stays.
	procs = append(procs, GPUProcess{PID: 903, Name: `C:\mystery.exe`})
	m := mapJSON(t, View{At: time.Now(), GPUs: foldCards(true, false), Processes: procs})

	if got, want := fmt.Sprint(listedPIDs(t, m)), "[900 901 902 903]"; got != want {
		t.Errorf("listed processes = %s, want %s (the real holder, a sized process on a work card, a sized process on the display card, an unattributed row)", got, want)
	}
	sum, ok := m["display_card_processes_unknown"].([]any)
	if !ok || len(sum) != 1 {
		t.Fatalf("one summary for the one display card, got %v", m["display_card_processes_unknown"])
	}
	row := sum[0].(map[string]any)
	if row["index"] != float64(1) || row["gpu_uuid"] != foldDisplay || row["name"] != "RTX B" || row["count"] != float64(31) {
		t.Errorf("the summary names the card and counts its desktop: %v", row)
	}
	// The wire carries no row of the desktop any more.
	b, _ := json.Marshal(m)
	for _, app := range []string{"explorer.exe", "chrome.exe", "slack.exe"} {
		if strings.Contains(string(b), app) {
			t.Errorf("the folded desktop (%s) is still listed row by row:\n%s", app, b)
		}
	}
}

// Unsized rows on a card that drives no screen are the lease holder's candidates, however many:
// folding them is the failure the summary exists to avoid, in the other direction.
func TestMapDoesNotFoldUnsizedProcessesOnAWorkCard(t *testing.T) {
	procs := append(desktopRows(5, foldDisplay), desktopRows(4, foldWork0)...)
	m := mapJSON(t, View{At: time.Now(), GPUs: foldCards(true, false), Processes: procs})
	if n := len(listedPIDs(t, m)); n != 4 {
		t.Errorf("the 4 unsized rows on the work card stay listed, got %d", n)
	}
	sum := m["display_card_processes_unknown"].([]any)
	if len(sum) != 1 || sum[0].(map[string]any)["count"] != float64(5) {
		t.Errorf("only the display card's 5 fold: %v", sum)
	}
}

// A sized process is information, wherever it runs: only the unsizable fold.
func TestMapDoesNotFoldASizedProcessOnTheDisplayCard(t *testing.T) {
	procs := []GPUProcess{{PID: 7, Name: "game.exe", UsedMiB: 3000, UsedKnown: true, GPUUUID: foldDisplay}}
	m := mapJSON(t, View{At: time.Now(), GPUs: foldCards(true, false), Processes: procs})
	if got := listedPIDs(t, m); len(got) != 1 || got[0] != 7 {
		t.Errorf("a sized process on the display card is listed: %v", got)
	}
	if _, ok := m["display_card_processes_unknown"]; ok {
		t.Errorf("nothing was folded, so there is no summary key: %v", m["display_card_processes_unknown"])
	}
}

// The monitor's card is the display card with the screen asleep too: display_active reads
// Disabled on every card then (display.go, measured 2026-10-03) and only display_attached marks it.
func TestMapFoldsOnTheMonitorsCardWithTheScreenAsleep(t *testing.T) {
	procs := append(desktopRows(12, foldDisplay), survivors()[0])
	for name, cards := range map[string][]GPU{
		"screen lit":      foldCards(true, true),
		"screen asleep":   foldCards(false, true),
		"only display on": foldCards(true, false),
	} {
		m := mapJSON(t, View{At: time.Now(), GPUs: cards, Processes: procs})
		if got := fmt.Sprint(listedPIDs(t, m)); got != "[900]" {
			t.Errorf("%s: listed = %s, want [900]", name, got)
		}
		if sum, _ := m["display_card_processes_unknown"].([]any); len(sum) != 1 || sum[0].(map[string]any)["count"] != float64(12) {
			t.Errorf("%s: summary = %v", name, m["display_card_processes_unknown"])
		}
	}
	// No card flagged at all (a headless box, a driver that reports neither field): nothing is
	// known to be the desktop, so nothing folds.
	m := mapJSON(t, View{At: time.Now(), GPUs: foldCards(false, false), Processes: procs})
	if n := len(listedPIDs(t, m)); n != 13 {
		t.Errorf("with no display card flagged every row is listed, got %d", n)
	}
	if _, ok := m["display_card_processes_unknown"]; ok {
		t.Errorf("no display card, no summary: %v", m["display_card_processes_unknown"])
	}
}

// A one-card box has no work card besides its display card: its processes ARE the work.
func TestMapFoldsNothingOnAOneCardBox(t *testing.T) {
	one := []GPU{{Index: 0, UUID: foldDisplay, Name: "RTX B", UtilPct: 90, UtilKnown: true, MemTotalMiB: 8192, DisplayActive: true, DisplayAttached: true}}
	m := mapJSON(t, View{At: time.Now(), GPUs: one, Processes: desktopRows(6, foldDisplay)})
	if n := len(listedPIDs(t, m)); n != 6 {
		t.Errorf("a one-card box lists all 6, got %d", n)
	}
	if _, ok := m["display_card_processes_unknown"]; ok {
		t.Errorf("a one-card box folds nothing: %v", m["display_card_processes_unknown"])
	}
}

// Everything folded still answers "a sample was taken": an empty list, not a missing key, and a
// pid listed twice for one card is one process.
func TestMapAnswersAnEmptyListWhenEverythingFoldsAndCountsPIDsOnce(t *testing.T) {
	procs := append(desktopRows(3, foldDisplay), desktopRows(1, foldDisplay)...) // pid 20000 twice
	m := mapJSON(t, View{At: time.Now(), GPUs: foldCards(true, false), Processes: procs})
	if rows, ok := m["gpu_processes"].([]any); !ok || len(rows) != 0 {
		t.Errorf("gpu_processes must be [] (a sample was taken, nothing is worth a row), got %v", m["gpu_processes"])
	}
	if sum := m["display_card_processes_unknown"].([]any); len(sum) != 1 || sum[0].(map[string]any)["count"] != float64(3) {
		t.Errorf("3 distinct processes, one listed twice: %v", sum)
	}
}

// No sample, no key: what a view without processes always answered.
func TestMapOmitsProcessKeysWithoutASample(t *testing.T) {
	m := mapJSON(t, View{At: time.Now(), GPUs: foldCards(true, false)})
	for _, k := range []string{"gpu_processes", "display_card_processes_unknown"} {
		if _, ok := m[k]; ok {
			t.Errorf("a view with no process sample must not carry %q", k)
		}
	}
}

// Two screens, two cards: one summary per card, in card order.
func TestMapSummarisesEachDisplayCardSeparately(t *testing.T) {
	cards := foldCards(true, false)
	cards[2].DisplayActive = true // the third card drives a screen as well
	procs := append(desktopRows(4, foldWork2), desktopRows(2, foldDisplay)...)
	m := mapJSON(t, View{At: time.Now(), GPUs: cards, Processes: procs})
	sum := m["display_card_processes_unknown"].([]any)
	if len(sum) != 2 || sum[0].(map[string]any)["index"] != float64(1) || sum[0].(map[string]any)["count"] != float64(2) ||
		sum[1].(map[string]any)["index"] != float64(2) || sum[1].(map[string]any)["count"] != float64(4) {
		t.Errorf("one summary per display card, lowest card first: %v", sum)
	}
}

// The busy-outside note names who is on the cards. Sorted by name and cut at six, thirty window
// rows pushed the one process that was not the desktop into "and N more"; the desktop is counted
// instead, and a note without a display card is what it always was.
func TestBusyOutsideNoteCountsTheDesktopAndNamesTheRest(t *testing.T) {
	seat := SeatState{Name: "agent-pool"}
	procs := append(desktopRows(30, foldDisplay), GPUProcess{PID: 900, Name: `C:\Python\python.exe`, GPUUUID: foldWork0})
	verdict, note := Assess(View{At: time.Now(), Seat: seat, GPUs: foldCards(true, false), Processes: procs})
	if verdict != VerdictBusyOutside {
		t.Fatalf("verdict = %s (%s)", verdict, note)
	}
	for _, want := range []string{"on the cards: python.exe (pid 900)", "30 desktop processes on the display card (card 1, RTX B), memory unknown (WDDM)"} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q:\n%s", want, note)
		}
	}
	for _, bad := range []string{"explorer.exe", "chrome.exe", "more"} {
		if strings.Contains(note, bad) {
			t.Errorf("note must not list the desktop (%q):\n%s", bad, note)
		}
	}
	// One desktop process reads in the singular; with nothing else on the cards the note is the count alone.
	_, one := Assess(View{At: time.Now(), Seat: seat, GPUs: foldCards(true, false), Processes: desktopRows(1, foldDisplay)})
	if !strings.Contains(one, "; 1 desktop process on the display card (card 1, RTX B), memory unknown (WDDM)") || strings.Contains(one, "on the cards:") {
		t.Errorf("a lone desktop process reads in the singular and has no \"on the cards:\" list:\n%s", one)
	}
	// Seven unsized processes on a work card are all work: still named, cut at six as before.
	_, work := Assess(View{At: time.Now(), Seat: seat, GPUs: foldCards(true, false), Processes: desktopRows(7, foldWork0)})
	if !strings.Contains(work, "and 1 more") || strings.Contains(work, "desktop process") {
		t.Errorf("work-card processes are never folded:\n%s", work)
	}
}
