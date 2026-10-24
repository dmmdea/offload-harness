package gpuactivity

// faults.go: `gpu faults`, the driver's fault history on this machine read as incidents (C-83-B facts, never a
// verdict input), whatever holds the lease.
//
// The lease view (driverevents.go) shows the last 24 h for the cards a lease holds. This report reads up to 30
// days for every card, adds Windows Error Reporting's live kernel dumps (LiveKernelEvent) on Windows, classifies
// each event against a known-error table (knownerrors.go) and groups the events into INCIDENTS the way NVIDIA's
// NVSentinel does: records less than 3 minutes apart are one burst. Two of NVSentinel's analyzer rules are
// applied as labelled readings, never as actions: the same class in 5 or more bursts on one card within 24 h
// (RepeatedXIDErrorOnSameGPU, which NVSentinel answers with CONTACT_SUPPORT; Xid 31 and 45 are left out as it
// leaves them out), and NVIDIA's Xid 13 workflow over the GPC/TPC units the graphics exceptions name.
//
// PLACEMENT is by the bus the record names (GPUID on Windows, PCI on Linux), as in the lease view. Windows id 13
// records name no card: they are counted in their burst, and the burst's cards are the ones its GPUID-bearing
// records name. The card table is today's: a card moved to another slot since an event would be misnamed.
//
// LIVE KERNEL DUMPS are WER records (Application log, id 1001, event name LiveKernelEvent). WER files one report
// many times (the reference box: 2464 records for 25 dumps in 30 days) and its record time is when the report was
// queued, days after the fault for some. So the dumps are counted once per dump file, and the fault time is read
// from the dump's file name (WATCHDOG-20261013-1921.dmp, local time to the minute); only a report with no dump
// file falls back to the queue time, and says so. They name no card.
//
// READ-ONLY and BOUNDED like the lease view: hidden children, one profile-less PowerShell per log, 20 s per read,
// a cap of 5000 records per log (a read that hits it says older events may be missing).

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// FaultWindowMax is the longest window `gpu faults` reads.
	FaultWindowMax = 30 * 24 * time.Hour
	// faultBurstGap ends a burst: records further apart than this are two incidents (NVSentinel's burst window).
	faultBurstGap = 3 * time.Minute
	// faultRepeatWindow and faultRepeatBursts are NVSentinel's RepeatedXIDErrorOnSameGPU rule.
	faultRepeatWindow = 24 * time.Hour
	faultRepeatBursts = 5
	// maxFaultRecords caps each windowed log read (the newest this many).
	maxFaultRecords = 5000
	// faultReadTimeout bounds each windowed log read (measured: 30 days of nvlddmkm in 0.8 s, of WER in 2.0 s).
	faultReadTimeout = 20 * time.Second
)

// FaultCard is one card of the table with its faults in the window.
type FaultCard struct {
	Index int    `json:"index"`
	UUID  string `json:"uuid"`
	Bus   string `json:"bus"`
	Name  string `json:"name,omitempty"`
	// RecoveryAction is nvidia-smi's gpu_recovery_action ("None" = nothing pending); "" when the driver does not
	// report it.
	RecoveryAction string `json:"recovery_action,omitempty"`
	// Events counts the records placed on this card, ByClass splits them, Incidents counts the bursts that name it.
	Events    int          `json:"events"`
	ByClass   []ClassCount `json:"by_class,omitempty"`
	Incidents int          `json:"incidents"`
	Last      time.Time    `json:"last,omitempty"`
}

// ClassCount is how many records of one class.
type ClassCount struct {
	Class string `json:"class"`
	Count int    `json:"count"`
}

// IncidentCard is a card a burst's records name: the table's index and uuid, or Index -1 and no uuid when the bus
// is not in today's table.
type IncidentCard struct {
	Index  int    `json:"index"`
	UUID   string `json:"uuid,omitempty"`
	Bus    string `json:"bus"`
	Events int    `json:"events"`
}

// Incident is one burst of driver records.
type Incident struct {
	Start   time.Time      `json:"start"`
	End     time.Time      `json:"end"`
	Events  int            `json:"events"`
	Cards   []IncidentCard `json:"cards"`
	Classes []ClassCount   `json:"classes"`
	// Unplaced counts the burst's records that name no card (Windows id 13 names none).
	Unplaced int `json:"unplaced"`
	// Units is how many GPC/TPC units the burst's graphics exceptions name, and Reading NVIDIA's Xid 13 workflow
	// applied to them ("" when fewer than two records name a unit).
	Units   int    `json:"units,omitempty"`
	Reading string `json:"reading,omitempty"`
	// LiveKernel are the live kernel dumps whose time falls in the burst (a minute before it to two after).
	LiveKernel []LiveKernelEvent `json:"live_kernel,omitempty"`
}

// LiveKernelEvent is one live kernel dump Windows Error Reporting filed.
type LiveKernelEvent struct {
	// At is the fault time: from the dump's file name (AtSource "dump-name", local minute) or, with no dump file,
	// the first time WER queued the report (AtSource "report-queue", which can be days late).
	At       time.Time `json:"at"`
	AtSource string    `json:"at_source"`
	Code     string    `json:"code"`
	// Dump is the dump's folder and file under LiveKernelReports; Reports how many WER records carried it.
	Dump    string `json:"dump,omitempty"`
	Reports int    `json:"reports"`
	Name    string `json:"name"`
}

// Repeat is NVSentinel's RepeatedXIDErrorOnSameGPU rule met: one class in at least 5 bursts on one card in 24 h.
type Repeat struct {
	Card   int       `json:"card"`
	UUID   string    `json:"uuid,omitempty"`
	Bus    string    `json:"bus"`
	Class  string    `json:"class"`
	Bursts int       `json:"bursts"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
}

// FaultReport is what `gpu faults` shows.
type FaultReport struct {
	Since      time.Time `json:"since"`
	Until      time.Time `json:"until"`
	WindowDays int       `json:"window_days"`
	// Events counts the driver records in the window; Untimed those whose time could not be read (not grouped).
	Events     int               `json:"events"`
	Untimed    int               `json:"untimed,omitempty"`
	Cards      []FaultCard       `json:"cards"`
	Incidents  []Incident        `json:"incidents"`
	Repeats    []Repeat          `json:"repeats,omitempty"`
	Known      []KnownError      `json:"known_errors"`
	LiveKernel []LiveKernelEvent `json:"live_kernel_events,omitempty"`
	Notes      []string          `json:"notes,omitempty"`
}

// Seam: the WER read, so a test reads canned records on any OS.
var readLiveKernelFn = readLiveKernelEvents

// ReadFaults reads this machine's driver fault history for the window (capped at FaultWindowMax). Uncached: each
// call reads the logs. ErrNoDriverEventSource means this OS has no source; a read that is short says so in Notes.
func ReadFaults(ctx context.Context, window time.Duration) (FaultReport, error) {
	if window <= 0 || window > FaultWindowMax {
		window = FaultWindowMax
	}
	now := driverNow()
	rep := FaultReport{Since: now.Add(-window).UTC(), Until: now.UTC(), WindowDays: int((window + 24*time.Hour - 1) / (24 * time.Hour))}
	evs, err := readLogWindow(ctx, now, window, winEventScriptFor(int((window+time.Hour-1)/time.Hour), maxFaultRecords), faultReadTimeout)
	if errors.Is(err, ErrNoDriverEventSource) {
		return rep, err
	}
	if err != nil {
		var issue *readIssue
		if !errors.As(err, &issue) {
			return rep, err
		}
		rep.Notes = append(rep.Notes, "driver log incomplete: "+err.Error())
	}
	if driverEventsGOOS == "windows" && len(evs) >= maxFaultRecords {
		rep.Notes = append(rep.Notes, fmt.Sprintf("the driver log read hit its cap of %d records, so older events in the window are missing", maxFaultRecords))
	}
	cards, terr := readFaultCards(ctx)
	if terr != nil {
		rep.Notes = append(rep.Notes, fmt.Sprintf("%v (nvidia-smi: %v): the events are listed by bus, not placed on cards", ErrCardTableUnreadable, terr))
	}
	var lke []LiveKernelEvent
	if driverEventsGOOS == "windows" {
		var lerr error
		lke, lerr = readLiveKernelFn(ctx, now, window)
		if lerr != nil {
			rep.Notes = append(rep.Notes, "live kernel dumps: "+lerr.Error())
		}
	}
	buildFaultReport(&rep, evs, cards, lke)
	return rep, nil
}

// buildFaultReport fills the report from what was read: placement, incidents, repeats, the known errors seen.
func buildFaultReport(rep *FaultReport, evs []DriverEvent, cards []FaultCard, lke []LiveKernelEvent) {
	byBus := map[string]int{}
	for i, c := range cards {
		byBus[c.Bus] = i
	}
	rep.Events = len(evs)
	known := map[string]KnownError{}
	var timed []DriverEvent
	for _, e := range evs {
		k := ClassifyEvent(e)
		known[k.Class] = k
		if e.At.IsZero() {
			rep.Untimed++
			continue
		}
		timed = append(timed, e)
		if i, ok := byBus[e.Bus]; ok && e.Bus != "" {
			c := &cards[i]
			c.Events++
			c.ByClass = addClass(c.ByClass, k.Class)
			if e.At.After(c.Last) {
				c.Last = e.At
			}
		}
	}
	sort.SliceStable(timed, func(i, j int) bool { return timed[i].At.Before(timed[j].At) })

	// Bursts: oldest first while building.
	var incs []Incident
	units := map[int]map[[2]int]int{}
	for _, e := range timed {
		if len(incs) == 0 || e.At.Sub(incs[len(incs)-1].End) > faultBurstGap {
			incs = append(incs, Incident{Start: e.At, End: e.At})
		}
		n := len(incs) - 1
		in := &incs[n]
		in.End = e.At
		in.Events++
		in.Classes = addClass(in.Classes, ClassOf(e))
		if e.Bus == "" {
			in.Unplaced++
		} else {
			in.Cards = addIncidentCard(in.Cards, e.Bus, cards, byBus)
		}
		if g, t, ok := smUnit(e.Text); ok {
			if units[n] == nil {
				units[n] = map[[2]int]int{}
			}
			units[n][[2]int{g, t}]++
		}
	}
	for n := range incs {
		incs[n].Units = len(units[n])
		incs[n].Reading = xid13Reading(units[n])
		for _, c := range incs[n].Cards {
			if i, ok := byBus[c.Bus]; ok {
				cards[i].Incidents++
			}
		}
	}
	// Live kernel dumps join the burst whose span (a minute before, two after) holds their dump-name time.
	for i := range lke {
		k := ClassifyLiveKernel(lke[i].Code)
		known[k.Class] = k
		lke[i].Name = k.Name
		if lke[i].AtSource != "dump-name" {
			continue
		}
		for n := range incs {
			if !lke[i].At.Before(incs[n].Start.Truncate(time.Minute).Add(-time.Minute)) && !lke[i].At.After(incs[n].End.Add(2*time.Minute)) {
				incs[n].LiveKernel = append(incs[n].LiveKernel, lke[i])
				break
			}
		}
	}
	rep.Repeats = repeatsOf(incs, timed, cards, byBus)
	// Newest first for the reader.
	for i, j := 0, len(incs)-1; i < j; i, j = i+1, j-1 {
		incs[i], incs[j] = incs[j], incs[i]
	}
	rep.Incidents = incs
	if rep.Incidents == nil {
		rep.Incidents = []Incident{}
	}
	sort.SliceStable(lke, func(i, j int) bool { return lke[i].At.After(lke[j].At) })
	rep.LiveKernel = lke
	rep.Cards = cards
	if rep.Cards == nil {
		rep.Cards = []FaultCard{}
	}
	rep.Known = make([]KnownError, 0, len(known))
	for _, k := range known {
		rep.Known = append(rep.Known, k)
	}
	sort.Slice(rep.Known, func(i, j int) bool { return rep.Known[i].Class < rep.Known[j].Class })
}

func addClass(cs []ClassCount, class string) []ClassCount {
	for i := range cs {
		if cs[i].Class == class {
			cs[i].Count++
			return cs
		}
	}
	return append(cs, ClassCount{Class: class, Count: 1})
}

func addIncidentCard(ics []IncidentCard, bus string, cards []FaultCard, byBus map[string]int) []IncidentCard {
	for i := range ics {
		if ics[i].Bus == bus {
			ics[i].Events++
			return ics
		}
	}
	ic := IncidentCard{Index: -1, Bus: bus, Events: 1}
	if i, ok := byBus[bus]; ok {
		ic.Index, ic.UUID = cards[i].Index, cards[i].UUID
	}
	return append(ics, ic)
}

// repeatsOf applies NVSentinel's RepeatedXIDErrorOnSameGPU rule: per card and class, the bursts that carry a
// record of that class placed on that card; 5 or more inside 24 h is a repeat. Xid 31 and 45 are left out, as
// NVSentinel leaves them out. One line per card and class: the 24 h stretch with the most bursts.
func repeatsOf(incs []Incident, timed []DriverEvent, cards []FaultCard, byBus map[string]int) []Repeat {
	type key struct{ bus, class string }
	starts := map[key][]time.Time{}
	n := 0
	for _, e := range timed {
		for n < len(incs)-1 && e.At.After(incs[n].End) {
			n++
		}
		if e.Bus == "" || (e.Source == "Xid" && (e.ID == 31 || e.ID == 45)) {
			continue
		}
		k := key{e.Bus, ClassOf(e)}
		s := starts[k]
		if len(s) == 0 || !s[len(s)-1].Equal(incs[n].Start) {
			starts[k] = append(s, incs[n].Start)
		}
	}
	var out []Repeat
	for k, ts := range starts {
		best, bi, bj := 0, 0, 0
		i := 0
		for j := range ts {
			for ts[j].Sub(ts[i]) > faultRepeatWindow {
				i++
			}
			if j-i+1 > best {
				best, bi, bj = j-i+1, i, j
			}
		}
		if best < faultRepeatBursts {
			continue
		}
		r := Repeat{Card: -1, Bus: k.bus, Class: k.class, Bursts: best, From: ts[bi], To: ts[bj]}
		if c, ok := byBus[k.bus]; ok {
			r.Card, r.UUID = cards[c].Index, cards[c].UUID
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bus != out[j].Bus {
			return out[i].Bus < out[j].Bus
		}
		return out[i].Class < out[j].Class
	})
	return out
}

// --- the card table, with names and the recovery action ---

// readFaultCards reads index, uuid, bus and name of every card, with gpu_recovery_action when the driver knows
// the field: a driver that does not refuses the whole query, which is then made without it.
func readFaultCards(ctx context.Context) ([]FaultCard, error) {
	tctx, cancel := context.WithTimeout(ctx, driverTableTimeout)
	defer cancel()
	out, err := smiRun(tctx, "--query-gpu=index,uuid,pci.bus_id,name,gpu_recovery_action", "--format=csv,noheader")
	if err != nil {
		out, err = smiRun(tctx, "--query-gpu=index,uuid,pci.bus_id,name", "--format=csv,noheader")
		if err != nil {
			return nil, err
		}
	}
	return ParseFaultCards(out), nil
}

// ParseFaultCards parses the card rows readFaultCards asks for (the recovery-action column optional). Rows that do
// not parse are skipped.
func ParseFaultCards(out string) []FaultCard {
	var cards []FaultCard
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) < 3 {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil {
			continue
		}
		m := busIDRE.FindStringSubmatch(strings.TrimSpace(f[2]))
		if m == nil {
			continue
		}
		c := FaultCard{Index: idx, UUID: strings.TrimSpace(f[1]), Bus: strings.ToLower(m[1]) + ":" + strings.ToLower(m[2])}
		if len(f) > 3 {
			c.Name = strings.TrimSpace(f[3])
		}
		if len(f) > 4 {
			if ra := strings.TrimSpace(f[4]); ra != "" && !strings.HasPrefix(ra, "[") {
				c.RecoveryAction = ra
			}
		}
		cards = append(cards, c)
	}
	sort.SliceStable(cards, func(i, j int) bool { return cards[i].Index < cards[j].Index })
	return cards
}

// --- Windows Error Reporting's live kernel dumps ---

// werScriptFor reads the WER records of live kernel dumps for the last hours, newest first, capped, as XML. The
// XPath keeps the read to LiveKernelEvent records (the Application log holds every application's reports).
func werScriptFor(hours, max int) string {
	ms := int64(hours) * 3600 * 1000
	return fmt.Sprintf(`$ErrorActionPreference='Stop'; [Console]::OutputEncoding=[System.Text.Encoding]::UTF8; try { Get-WinEvent -LogName Application -FilterXPath "*[System[Provider[@Name='Windows Error Reporting'] and (EventID=1001) and TimeCreated[timediff(@SystemTime) <= %d]]] and *[EventData[Data[@Name='EventName']='LiveKernelEvent']]" -MaxEvents %d | ForEach-Object { $_.ToXml() } } catch { if ($_.FullyQualifiedErrorId -like 'NoMatchingEventsFound*') { exit 0 }; [Console]::Error.WriteLine($_.Exception.Message); exit 2 }`, ms, max)
}

func readLiveKernelEvents(ctx context.Context, now time.Time, window time.Duration) ([]LiveKernelEvent, error) {
	rctx, cancel := context.WithTimeout(ctx, faultReadTimeout)
	defer cancel()
	hours := int((window + time.Hour - 1) / time.Hour)
	out, err := runChild(rctx, powershellPath(), "-NoProfile", "-NonInteractive", "-Command", werScriptFor(hours, maxFaultRecords))
	if err != nil {
		return nil, fmt.Errorf("reading the Application log: %w", err)
	}
	evs, perr := ParseLiveKernelEvents(out, time.Local)
	cutoff := now.Add(-window)
	kept := evs[:0:0]
	for _, e := range evs {
		if !e.At.Before(cutoff) {
			kept = append(kept, e)
		}
	}
	if perr != nil {
		return kept, fmt.Errorf("the read stopped part way (%v), so older dumps may be missing", perr)
	}
	return kept, nil
}

// werDumpRE reads a live dump's folder, file and time from a WER record's attached files:
// ...\LiveKernelReports\WATCHDOG\WATCHDOG-20261013-1921.dmp (a second dump in the same minute adds "-1").
var werDumpRE = regexp.MustCompile(`(?i)LiveKernelReports\\([^\\\r\n]+)\\([^\\\r\n]*?-(\d{8})-(\d{4})(?:-\d+)?\.dmp)`)

// werEventXML is the part of a WER record the parser reads.
type werEventXML struct {
	System struct {
		TimeCreated struct {
			SystemTime string `xml:"SystemTime,attr"`
		} `xml:"TimeCreated"`
	} `xml:"System"`
	EventData struct {
		Data []struct {
			Name  string `xml:"Name,attr"`
			Value string `xml:",chardata"`
		} `xml:"Data"`
	} `xml:"EventData"`
}

// ParseLiveKernelEvents parses WER records into one LiveKernelEvent per dump file (a report with no dump file is
// one per report id), with the fault time read from the dump's name in loc. Records that are not LiveKernelEvent
// are skipped; a stretch that is not XML ends the read with what was parsed and an error.
func ParseLiveKernelEvents(out string, loc *time.Location) ([]LiveKernelEvent, error) {
	dec := xml.NewDecoder(strings.NewReader(out))
	byKey := map[string]*LiveKernelEvent{}
	var order []string
	var perr error
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			perr = fmt.Errorf("a WER record is not XML: %w", err)
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "Event" {
			continue
		}
		var rec werEventXML
		if err := dec.DecodeElement(&rec, &se); err != nil {
			perr = fmt.Errorf("a WER record is not readable: %w", err)
			break
		}
		d := map[string]string{}
		for _, x := range rec.EventData.Data {
			d[x.Name] = strings.TrimSpace(x.Value)
		}
		if d["EventName"] != "LiveKernelEvent" {
			continue
		}
		queued, _ := time.Parse(time.RFC3339Nano, rec.System.TimeCreated.SystemTime)
		queued = queued.UTC()
		e := LiveKernelEvent{Code: strings.ToLower(d["P1"]), At: queued, AtSource: "report-queue"}
		key := "report:" + d["ReportId"]
		if m := werDumpRE.FindStringSubmatch(d["AttachedFiles"]); m != nil {
			e.Dump = m[1] + `\` + m[2]
			key = "dump:" + strings.ToLower(e.Dump)
			if t, err := time.ParseInLocation("20060102-1504", m[3]+"-"+m[4], loc); err == nil {
				e.At, e.AtSource = t.UTC(), "dump-name"
			}
		}
		if prev, ok := byKey[key]; ok {
			prev.Reports++
			if prev.AtSource == "report-queue" && queued.Before(prev.At) {
				prev.At = queued
			}
			continue
		}
		e.Reports = 1
		byKey[key] = &e
		order = append(order, key)
	}
	evs := make([]LiveKernelEvent, 0, len(order))
	for _, k := range order {
		evs = append(evs, *byKey[k])
	}
	return evs, perr
}
