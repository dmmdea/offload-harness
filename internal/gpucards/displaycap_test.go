package gpucards

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// The display card's row carries a live verdict of gpu_display_cap_pct: the VRAM in use now (the desktop and every
// process counted, which is how the allocator counts it) against the share of the card the cap allows, by the
// allocator's own arithmetic (gpulease.DisplayBudgetGiB). It needs no knowledge of which seat is pinned where, so
// it covers a seat the allocator never placed. Fixtures are synthetic: 16.0 GiB cards, plain numbers.

// capTable is a three-card box whose card 1 is the display card, with displayFree GiB free of 16.0 on it.
func capTable(displayFree float64, mutate ...func(*[]gpuprobe.Device)) []gpuprobe.Card {
	devs := []gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa0000-x", Name: "T", TotalGiB: 16, FreeGiB: 15},
		{Index: 1, UUID: "GPU-bbbb0000-x", Name: "T", TotalGiB: 16, FreeGiB: displayFree, DisplayActive: true},
		{Index: 2, UUID: "GPU-cccc0000-x", Name: "T", TotalGiB: 16, FreeGiB: 16},
	}
	for _, m := range mutate {
		m(&devs)
	}
	c, _ := gpuprobe.BuildCards(devs, "")
	return c
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.011 }

func TestRowsCarryTheDisplayCapOnTheDisplayCardOnly(t *testing.T) {
	rows := RowsWithCap(capTable(3.0), nil, 60, 0)
	d := rows[1].DisplayCap
	if d == nil {
		t.Fatalf("the display card's row must carry the cap verdict: %+v", rows[1])
	}
	if d.CapPct != 60 || !near(d.BudgetGiB, 9.6) || !near(d.UsedGiB, 13) || !near(d.UsedPct, 81.3) {
		t.Errorf("13.0 of 16.0 GiB in use against a 60 percent cap (9.6 GiB): cap_pct 60 budget 9.6 used 13.0 used_pct 81.3, got %+v", *d)
	}
	if !d.OverCap || !near(d.OverByGiB, 3.4) || d.Closed || d.Unknown != "" {
		t.Errorf("13.0 GiB in use against a 9.6 GiB budget is over by 3.4, and neither closed nor unknown: %+v", *d)
	}
	for _, i := range []int{0, 2} {
		if rows[i].DisplayCap != nil {
			t.Errorf("card %d is not the display card and carries no cap verdict: %+v", rows[i].Index, *rows[i].DisplayCap)
		}
		b, err := json.Marshal(rows[i])
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "display_cap") {
			t.Errorf("card %d's JSON must not carry the key display_cap: %s", rows[i].Index, b)
		}
	}
	// The positive control: the display card's JSON does carry the key, with the verdict a consumer branches on.
	b, _ := json.Marshal(rows[1])
	var back struct {
		DisplayCap map[string]any `json:"display_cap"`
	}
	if err := json.Unmarshal(b, &back); err != nil || back.DisplayCap["over_cap"] != true || back.DisplayCap["cap_pct"] != float64(60) {
		t.Fatalf("the display card's row JSON must carry display_cap with over_cap true and cap_pct 60 (%v): %s", err, b)
	}
	for _, k := range []string{"budget_gb", "used_gb", "used_pct", "over_by_gb"} {
		if _, ok := back.DisplayCap[k]; !ok {
			t.Errorf("display_cap lacks the key %q: %s", k, b)
		}
	}
}

// The comparison is the VRAM IN USE against the budget, and landing exactly on the budget is within it.
func TestDisplayCapComparesUseToTheBudgetAndAnExactFitIsWithin(t *testing.T) {
	for _, tc := range []struct {
		name     string
		free     float64
		wantOver bool
		wantBy   float64
	}{
		{"well under", 10, false, 0},             // 6.0 in use of a 9.6 budget
		{"exactly on the budget", 6.4, false, 0}, // 9.6 in use
		{"a hair over", 6.3, true, 0.1},          // 9.7 in use
		{"far over", 3.0, true, 3.4},
		{"the whole card in use", 0, true, 6.4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := RowsWithCap(capTable(tc.free), nil, 60, 0)[1].DisplayCap
			if d == nil {
				t.Fatal("no verdict on the display card")
			}
			if d.OverCap != tc.wantOver || !near(d.OverByGiB, tc.wantBy) {
				t.Errorf("free %.1f of 16.0 at a 60 percent cap: over_cap %v over_by %.2f, want %v %.2f (%+v)", tc.free, d.OverCap, d.OverByGiB, tc.wantOver, tc.wantBy, *d)
			}
		})
	}
}

// A declared display floor stricter than the rest of the card binds, as it does for the allocator.
func TestDisplayCapFloorStricterThanTheCapWins(t *testing.T) {
	// 7.5 GiB in use (free 8.5): within the 9.6 budget of a plain 60 percent cap...
	plain := RowsWithCap(capTable(8.5), nil, 60, 0)[1].DisplayCap
	if plain == nil || plain.OverCap || !near(plain.BudgetGiB, 9.6) {
		t.Fatalf("control: 7.5 GiB in use is within a 9.6 GiB budget: %+v", plain)
	}
	// ...and over the 7.0 GiB budget a declared floor of 9.0 GiB leaves.
	d := RowsWithCap(capTable(8.5), nil, 60, 9)[1].DisplayCap
	if d == nil || !d.OverCap || !near(d.BudgetGiB, 7.0) || !near(d.OverByGiB, 0.5) {
		t.Fatalf("a declared floor of 9.0 GiB on a 16.0 GiB card leaves a 7.0 GiB budget: 7.5 in use is over by 0.5: %+v", d)
	}
}

// At 0 the operator has closed the card ("I need it"). The desktop's own use cannot be told from a seat's, so
// nothing is over: the row says it is closed and keeps the reading.
func TestDisplayCapAtZeroIsClosedNotOver(t *testing.T) {
	d := RowsWithCap(capTable(3.0), nil, 0, 0)[1].DisplayCap
	if d == nil {
		t.Fatal("a closed card still carries its reading")
	}
	if !d.Closed || d.OverCap || d.OverByGiB != 0 || d.CapPct != 0 || d.Unknown != "" || !near(d.UsedGiB, 13) {
		t.Errorf("cap 0: closed true, over_cap false, cap_pct 0, 13.0 GiB read: %+v", *d)
	}
	b, _ := json.Marshal(d)
	if !strings.Contains(string(b), `"closed":true`) || !strings.Contains(string(b), `"over_cap":false`) {
		t.Errorf("the JSON says closed and not over: %s", b)
	}
	// The positive control: the same card at a cap of 60 is over, so the zero case is not "never over".
	if c := RowsWithCap(capTable(3.0), nil, 60, 0)[1].DisplayCap; c == nil || !c.OverCap || c.Closed {
		t.Errorf("control: the same card at 60 percent is over and not closed: %+v", c)
	}
}

func TestDisplayCapNeverGuessesAnUnreadableTotal(t *testing.T) {
	noTotal := capTable(0, func(d *[]gpuprobe.Device) { (*d)[1].TotalGiB, (*d)[1].FreeGiB = 0, 0 })
	d := RowsWithCap(noTotal, nil, 60, 0)[1].DisplayCap
	if d == nil || !strings.Contains(d.Unknown, "total VRAM not given") || d.OverCap || d.Closed {
		t.Fatalf("a display card with no total is unknown, never over: %+v", d)
	}
	// No percentage is made of a missing total: a NaN would not even marshal.
	if _, err := json.Marshal(d); err != nil || d.UsedPct != 0 {
		t.Errorf("an unknown row carries no made-up share (used_pct %v, marshal: %v)", d.UsedPct, err)
	}
	// The positive control: with a total the same row has a verdict and no unknown.
	if c := RowsWithCap(capTable(3.0), nil, 60, 0)[1].DisplayCap; c == nil || c.Unknown != "" {
		t.Errorf("control: a card with a total is judged: %+v", c)
	}
}

// A reading that could not say which card the monitor is on is not judged: the allocator closes such a card
// before it asks the cap, and so does the row, even where display_active happens to flag a card.
func TestDisplayCapNeverJudgesAReadingThatCouldNotSayWhichCardIsTheMonitors(t *testing.T) {
	unknown := capTable(3.0, func(d *[]gpuprobe.Device) {
		for i := range *d {
			(*d)[i].AttachedUnknown = true
		}
	})
	rows := RowsWithCap(unknown, nil, 60, 0)
	if !unknown[1].Display {
		t.Fatal("fixture: display_active still flags card 1 on this reading")
	}
	for _, r := range rows {
		if !r.DisplayUnknown {
			t.Errorf("card %d: the row must say the reading could not tell the monitor's card", r.Index)
		}
		if r.DisplayCap == nil || !strings.Contains(r.DisplayCap.Unknown, "could not say which card the monitor is on") {
			t.Errorf("card %d: every card of such a reading is unknown, got %+v", r.Index, r.DisplayCap)
			continue
		}
		if r.DisplayCap.OverCap || r.DisplayCap.Closed {
			t.Errorf("card %d: no verdict on an unknown reading: %+v", r.Index, *r.DisplayCap)
		}
	}
	// The table says so once, not once per card.
	text := strings.Join(Table(rows), "\n")
	if n := strings.Count(text, "display cap:"); n != 1 {
		t.Errorf("one display cap line for the whole unknown reading, got %d:\n%s", n, text)
	}
	if !strings.Contains(text, "could not say which card the monitor is on") {
		t.Errorf("the line must say why nothing is judged:\n%s", text)
	}
	// The positive control: the same cards with the field known judge card 1.
	if c := RowsWithCap(capTable(3.0), nil, 60, 0)[1].DisplayCap; c == nil || !c.OverCap || c.Unknown != "" {
		t.Errorf("control: a reading that carries display_attached is judged: %+v", c)
	}
}

func TestSectionCarriesTheCapOnTheDisplayRow(t *testing.T) {
	sec := Section(capTable(3.0), "", nil, nil, 60, 0)
	rows, ok := sec["cards"].([]Row)
	if !ok || len(rows) != 3 {
		t.Fatalf("cards = %#v", sec["cards"])
	}
	if rows[1].DisplayCap == nil || !rows[1].DisplayCap.OverCap {
		t.Errorf("the section's display row carries the verdict: %+v", rows[1].DisplayCap)
	}
	if rows[0].DisplayCap != nil || rows[2].DisplayCap != nil {
		t.Errorf("only the display card does")
	}
	// A cap given to Section is the cap judged: at 100 percent the same card is within.
	if all := Section(capTable(3.0), "", nil, nil, 100, 0)["cards"].([]Row); all[1].DisplayCap == nil || all[1].DisplayCap.OverCap {
		t.Errorf("at a cap of 100 the display card is within: %+v", all[1].DisplayCap)
	}
}

const tableHeader = "idx  vram free/tot  display  comfy  util  holder                             name / uuid"

func TestTableSaysWhenTheDisplayCardIsOverItsCap(t *testing.T) {
	lines := Table(RowsWithCap(capTable(3.0), nil, 60, 0))
	if lines[0] != tableHeader {
		t.Errorf("the table keeps its header:\n got %q\nwant %q", lines[0], tableHeader)
	}
	at := -1
	for i, ln := range lines {
		if strings.Contains(ln, "display cap:") {
			if at >= 0 {
				t.Fatalf("one display cap line, got a second at %d:\n%s", i, strings.Join(lines, "\n"))
			}
			at = i
		}
	}
	if at < 0 {
		t.Fatalf("the table must carry a display cap line:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[at-1], "GPU-bbbb0000-x") {
		t.Errorf("the line sits under the display card's own row, got after %q", lines[at-1])
	}
	for _, want := range []string{"13.0 of 16.0 GiB in use (81%)", "gpu_display_cap_pct 60", "allows 9.6 GiB", "OVER by 3.4 GiB", "desktop and every process"} {
		if !strings.Contains(lines[at], want) {
			t.Errorf("the display cap line must contain %q: %s", want, lines[at])
		}
	}
	// The positive control: the same card within its cap says within, not OVER.
	ok := strings.Join(Table(RowsWithCap(capTable(10.0), nil, 60, 0)), "\n")
	if !strings.Contains(ok, "display cap:") || !strings.Contains(ok, "within") || strings.Contains(ok, "OVER") {
		t.Errorf("a card within its cap says within:\n%s", ok)
	}
}

func TestTableSaysWhenTheDisplayCardIsClosed(t *testing.T) {
	text := strings.Join(Table(RowsWithCap(capTable(3.0), nil, 0, 0)), "\n")
	for _, want := range []string{"display cap:", "gpu_display_cap_pct is 0", "closed"} {
		if !strings.Contains(text, want) {
			t.Errorf("a closed card's line must contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "OVER") {
		t.Errorf("a closed card is not over:\n%s", text)
	}
}

// A box with no flagged display card (single card, headless) gets no line: the same rule the allocator uses.
func TestNoDisplayCardNoCapLine(t *testing.T) {
	headless := capTable(3.0, func(d *[]gpuprobe.Device) { (*d)[1].DisplayActive = false })
	rows := RowsWithCap(headless, nil, 60, 0)
	text := strings.Join(Table(rows), "\n")
	if strings.Contains(text, "display cap:") {
		t.Errorf("no card is the display card, so there is no cap line:\n%s", text)
	}
	for _, r := range rows {
		if r.DisplayCap != nil {
			t.Errorf("card %d carries a verdict on a box with no display card: %+v", r.Index, *r.DisplayCap)
		}
	}
	// The positive control: flag the display card and the line appears.
	if text := strings.Join(Table(RowsWithCap(capTable(3.0), nil, 60, 0)), "\n"); !strings.Contains(text, "display cap:") {
		t.Errorf("control: a flagged display card gets the line:\n%s", text)
	}
}

// A holder's command line and the cap line both belong to the display card's row, and neither hides the other.
func TestTableKeepsTheRunningLineAndTheCapLineOfTheDisplayCard(t *testing.T) {
	leases := []gpulease.Info{{Held: true, Epoch: 4, Class: gpulease.ClassMedia, Command: "python film.py", Devices: []string{"gpu-bbbb0000-x"}}}
	text := strings.Join(Table(RowsWithCap(capTable(3.0), leases, 60, 0)), "\n")
	if !strings.Contains(text, "running: python film.py") || !strings.Contains(text, "display cap:") {
		t.Errorf("both lines must be printed:\n%s", text)
	}
}
