package gpuactivity

// driverevents.go: the NVIDIA driver's own fault events for the leased cards (C-83-B, A-128 part 3).
//
// A card that faults under a lease leaves one line in the driver's log: on Windows an event in the System log from
// provider nvlddmkm (id 153, "Error occurred on GPUID: b500", is the one that matters here), on Linux an
// "NVRM: Xid (PCI:0000:b5:00): 79, ..." line in the kernel log. Until now nothing showed them: sessions found the
// events by hand, weeks after the fact (181 of id 153 since 2026-09-22 on the reference box). `gpu status` and
// offload_status's gpu_lease.activity now carry the recent ones for the cards the lease holds.
//
// SHOWN AS FACTS. A driver fault and a kill look alike in the log, and telling them apart takes A-128's kill test, so
// an event is never a verdict input and never moves the lease's verdict. A reader that cannot read the log is a note,
// never an error, and an event that names no card this box can place is counted and not listed, so an empty list is
// not read as "no faults".
//
// READ-ONLY, HIDDEN, BOUNDED. Every child starts through internal/winexec (no console window), the Windows read is one
// PowerShell from System32 with a profile-less, non-interactive, read-only Get-WinEvent over a 24 h window and a cap,
// and the whole read is bounded at 8 s. The answer is cached for 60 s (failures too), so a status poll does not start a
// process per call.
//
// ATTRIBUTION is by PCI bus and device: the record's GPUID is the bus and device in hex ("b500" = bus b5, device 00),
// the kernel line names them as PCI:0000:b5:00, and nvidia-smi's pci.bus_id ends B5:00.0. The bus table is a second
// small nvidia-smi query made here, so gpuprobe's device sample stays as it is. The PCI domain is ignored: a box with
// two NVIDIA domains is not a case this fleet has.

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/winexec"
)

// DriverEvent is one event the NVIDIA driver logged.
type DriverEvent struct {
	// At is when the driver logged it (zero when the log line carried no readable time).
	At time.Time `json:"at"`
	// Source is "nvlddmkm" (the Windows System log provider) or "Xid" (the Linux kernel log's NVRM Xid line).
	Source string `json:"source"`
	// ID is the Windows event id or the Xid code.
	ID int `json:"id"`
	// Bus is the card's PCI bus and device the event names, lower-case ("b5:00"); "" when the event names none.
	Bus string `json:"bus,omitempty"`
	// Card is the nvidia-smi index of the card the bus belongs to, and UUID its uuid; UUID is "" when the event could
	// not be placed on a card (then Card means nothing).
	Card int    `json:"card"`
	UUID string `json:"uuid,omitempty"`
	// Text is the driver's message, without the Windows device object path.
	Text string `json:"text,omitempty"`
}

// Line is the one-line rendering `gpu status` prints.
func (e DriverEvent) Line() string {
	when := "time unknown"
	if !e.At.IsZero() {
		when = e.At.Local().Format("2006-01-02 15:04:05 MST")
	}
	s := fmt.Sprintf("%s card %d (%s) %s %d", when, e.Card, e.Bus, e.Source, e.ID)
	if e.Text != "" {
		s += ": " + e.Text
	}
	return s
}

// BusCard is one row of the PCI bus table: the card at a bus and device.
type BusCard struct {
	Index int
	UUID  string
}

// ErrNoDriverEventSource says this OS has no driver-event source: the snapshot treats it as "off", with no key and no note.
var ErrNoDriverEventSource = errors.New("gpuactivity: no driver event source on this OS")

const (
	// driverEventWindow is how far back the readers look.
	driverEventWindow = 24 * time.Hour
	// driverEventTimeout bounds one whole read (the child process and the bus query).
	driverEventTimeout = 8 * time.Second
	// driverEventTTL is how long one answer (or failure) is reused.
	driverEventTTL = 60 * time.Second
	// maxWinEvents caps the Windows read: a storm cannot make the child's output unbounded.
	maxWinEvents = 500
	// maxDriverEventsShown caps the list a View carries (newest first).
	maxDriverEventsShown = 20
	// maxEventText caps one event's message.
	maxEventText = 200
)

// Seams, so a test reads a canned log on any OS.
var (
	driverEventsGOOS = runtime.GOOS
	driverNow        = time.Now
	// runChild starts one child process through internal/winexec (hidden on Windows) and returns its stdout; a
	// non-zero exit carries the child's stderr in the error.
	runChild = func(ctx context.Context, name string, args ...string) (string, error) {
		out, err := winexec.CommandContext(ctx, name, args...).Output()
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) && len(ee.Stderr) > 0 {
				return string(out), fmt.Errorf("%w: %s", err, squeeze(string(ee.Stderr), 300))
			}
			return string(out), err
		}
		return string(out), nil
	}
)

// squeeze collapses every whitespace run to one space and caps the result at max runes.
func squeeze(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "..."
	}
	return s
}

// --- parsers ---

var (
	gpuIDRE = regexp.MustCompile(`(?i)GPUID:\s*([0-9a-f]{2})([0-9a-f]{2})\b`)
	pciRE   = regexp.MustCompile(`(?i)PCI:(?:[0-9a-f]{4}:)?([0-9a-f]{2}):([0-9a-f]{2})`)
	xidRE   = regexp.MustCompile(`(?i)NVRM: Xid \(PCI:(?:[0-9a-f]{4}:)?([0-9a-f]{2}):([0-9a-f]{2})(?:\.[0-9a-f])?\):\s*(\d+)\s*(.*)$`)
	busIDRE = regexp.MustCompile(`(?i)(?:[0-9a-f]+:)?([0-9a-f]{2}):([0-9a-f]{2})\.[0-9a-f]\s*$`)
)

// busOf reads the PCI bus and device an event's message names, "" when it names none.
func busOf(text string) string {
	if m := gpuIDRE.FindStringSubmatch(text); m != nil {
		return strings.ToLower(m[1]) + ":" + strings.ToLower(m[2])
	}
	if m := pciRE.FindStringSubmatch(text); m != nil {
		return strings.ToLower(m[1]) + ":" + strings.ToLower(m[2])
	}
	return ""
}

// winEventXML is the part of a Windows event record the parser reads.
type winEventXML struct {
	System struct {
		Provider struct {
			Name string `xml:"Name,attr"`
		} `xml:"Provider"`
		EventID     int `xml:"EventID"`
		TimeCreated struct {
			SystemTime string `xml:"SystemTime,attr"`
		} `xml:"TimeCreated"`
	} `xml:"System"`
	EventData struct {
		Data []string `xml:"Data"`
	} `xml:"EventData"`
}

// ParseWinEvents parses the XML records the Windows reader prints: a sequence of <Event> elements, not one document.
// A stretch that is not XML ends the read with the events parsed so far and an error. The record's own host name,
// user and device object path are not carried: only the provider, id, time and message.
func ParseWinEvents(out string) ([]DriverEvent, error) {
	dec := xml.NewDecoder(strings.NewReader(out))
	var evs []DriverEvent
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return evs, nil
		}
		if err != nil {
			return evs, fmt.Errorf("an event record is not XML: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "Event" {
			continue
		}
		var rec winEventXML
		if err := dec.DecodeElement(&rec, &se); err != nil {
			return evs, fmt.Errorf("an event record is not readable: %w", err)
		}
		var parts []string
		for _, d := range rec.EventData.Data {
			d = strings.TrimSpace(d)
			if d == "" || strings.HasPrefix(d, `\Device\`) {
				continue
			}
			parts = append(parts, d)
		}
		text := squeeze(strings.Join(parts, "; "), maxEventText)
		at, _ := time.Parse(time.RFC3339Nano, rec.System.TimeCreated.SystemTime)
		evs = append(evs, DriverEvent{At: at.UTC(), Source: rec.System.Provider.Name, ID: rec.System.EventID, Bus: busOf(text), Text: text})
	}
}

// ParseKernelLog reads the NVRM Xid lines out of a kernel log: `journalctl -k -o short-unix` lines (a unix time
// first) or `dmesg --time-format iso` lines (an ISO time first). Other lines are ignored; a line whose time cannot
// be read keeps a zero At.
func ParseKernelLog(out string) []DriverEvent {
	var evs []DriverEvent
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		m := xidRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		id, err := strconv.Atoi(m[3])
		if err != nil {
			continue
		}
		fields := strings.Fields(line)
		var at time.Time
		if len(fields) > 0 {
			at = kernelLogTime(fields[0])
		}
		evs = append(evs, DriverEvent{
			At: at, Source: "Xid", ID: id,
			Bus:  strings.ToLower(m[1]) + ":" + strings.ToLower(m[2]),
			Text: squeeze(strings.TrimLeft(m[4], ", "), maxEventText),
		})
	}
	return evs
}

// kernelLogTime reads the first token of a kernel-log line: a unix time with a fraction ("1760199949.098666", read
// as digits so no float rounding) or an ISO time ("2026-10-11T04:05:49,123456-05:00"). Zero when it is neither.
func kernelLogTime(tok string) time.Time {
	if sec, frac, ok := strings.Cut(tok, "."); ok || allDigits(tok) {
		if s, err := strconv.ParseInt(sec, 10, 64); err == nil && allDigits(sec) && s > 1e9 {
			ns := int64(0)
			if ok && allDigits(frac) {
				f := frac
				if len(f) > 9 {
					f = f[:9]
				}
				f += strings.Repeat("0", 9-len(f))
				ns, _ = strconv.ParseInt(f, 10, 64)
			}
			return time.Unix(s, ns).UTC()
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05Z0700"} {
		if t, err := time.Parse(layout, tok); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ParseBusTable parses `nvidia-smi --query-gpu=index,uuid,pci.bus_id --format=csv,noheader` rows into a table keyed by
// the lower-case "bus:device" an event names ("b5:00"). Rows that do not parse are skipped.
func ParseBusTable(out string) map[string]BusCard {
	table := map[string]BusCard{}
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
		table[strings.ToLower(m[1])+":"+strings.ToLower(m[2])] = BusCard{Index: idx, UUID: strings.TrimSpace(f[1])}
	}
	return table
}

// AttributeEvents names the card behind each event's bus. An event whose bus the table does not list, or that names
// none, stays unplaced (UUID ""). The input is not changed.
func AttributeEvents(evs []DriverEvent, table map[string]BusCard) []DriverEvent {
	out := make([]DriverEvent, len(evs))
	copy(out, evs)
	for i := range out {
		if c, ok := table[out[i].Bus]; ok && out[i].Bus != "" {
			out[i].Card, out[i].UUID = c.Index, c.UUID
		}
	}
	return out
}

// --- the readers ---

// winEventScript is the one command the Windows reader runs: read-only Get-WinEvent over the System log's nvlddmkm
// provider for the last 24 h, newest first, capped, each record as XML. A log with no matching event is an empty
// answer (exit 0); any other failure exits 2 with its message on stderr.
const winEventScript = `$ErrorActionPreference='Stop'; [Console]::OutputEncoding=[System.Text.Encoding]::UTF8; try { Get-WinEvent -FilterHashtable @{LogName='System'; ProviderName='nvlddmkm'; StartTime=(Get-Date).AddHours(-24)} -MaxEvents 500 | ForEach-Object { $_.ToXml() } } catch { if ($_.FullyQualifiedErrorId -like 'NoMatchingEventsFound*') { exit 0 }; [Console]::Error.WriteLine($_.Exception.Message); exit 2 }`

// powershellPath is the System32 Windows PowerShell, by full path: the reader must not find a different one on PATH.
func powershellPath() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = os.Getenv("windir")
	}
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
}

func readWindowsEvents(ctx context.Context) ([]DriverEvent, error) {
	out, err := runChild(ctx, powershellPath(), "-NoProfile", "-NonInteractive", "-Command", winEventScript)
	if err != nil {
		return nil, fmt.Errorf("reading the System log: %w", err)
	}
	evs, perr := ParseWinEvents(out)
	if perr != nil && len(evs) == 0 {
		return nil, perr
	}
	return evs, nil
}

func readKernelEvents(ctx context.Context) ([]DriverEvent, error) {
	out, jerr := runChild(ctx, "journalctl", "-k", "--no-pager", "-o", "short-unix", "--since", "24 hours ago")
	if jerr != nil {
		dout, derr := runChild(ctx, "dmesg", "--time-format", "iso")
		if derr != nil {
			return nil, fmt.Errorf("journalctl: %v; dmesg: %v", jerr, derr)
		}
		out = dout
	}
	return ParseKernelLog(out), nil
}

// readDriverEvents is one uncached read: the OS's log, the window, then the card for each event's bus.
func readDriverEvents(ctx context.Context, now time.Time) ([]DriverEvent, error) {
	ctx, cancel := context.WithTimeout(ctx, driverEventTimeout)
	defer cancel()
	var evs []DriverEvent
	var err error
	switch driverEventsGOOS {
	case "windows":
		evs, err = readWindowsEvents(ctx)
	case "linux":
		evs, err = readKernelEvents(ctx)
	default:
		return nil, ErrNoDriverEventSource
	}
	if err != nil {
		return nil, err
	}
	// Inside the window; an event whose time could not be read is kept (an unknown time must not read as "long ago").
	cutoff := now.Add(-driverEventWindow)
	kept := evs[:0:0]
	for _, e := range evs {
		if e.At.IsZero() || !e.At.Before(cutoff) {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}
	// The card for each bus. A table that cannot be read leaves the events unplaced; they are not lost.
	if out, terr := smiRun(ctx, "--query-gpu=index,uuid,pci.bus_id", "--format=csv,noheader"); terr == nil {
		kept = AttributeEvents(kept, ParseBusTable(out))
	}
	return kept, nil
}

// driverCache is the 60 s memory of the last read.
var driverCache struct {
	mu     sync.Mutex
	have   bool
	at     time.Time
	events []DriverEvent
	err    error
}

// resetDriverEvents forgets the cached read (tests).
func resetDriverEvents() {
	driverCache.mu.Lock()
	driverCache.have, driverCache.events, driverCache.err = false, nil, nil
	driverCache.mu.Unlock()
}

// ReadDriverEvents is the production reader behind Options.DriverEvents: this machine's recent driver fault events,
// each placed on its card when the card table allows. Cached for 60 s, a failure included, so a status poll starts no
// process per call. ErrNoDriverEventSource means this OS has no source.
func ReadDriverEvents(ctx context.Context) ([]DriverEvent, error) {
	driverCache.mu.Lock()
	defer driverCache.mu.Unlock()
	now := driverNow()
	if driverCache.have && now.Sub(driverCache.at) < driverEventTTL {
		return append([]DriverEvent(nil), driverCache.events...), driverCache.err
	}
	evs, err := readDriverEvents(ctx, now)
	if ctx.Err() == nil {
		driverCache.have, driverCache.at, driverCache.events, driverCache.err = true, now, evs, err
	}
	return append([]DriverEvent(nil), evs...), err
}

// --- the snapshot's use of them ---

// leasedCards says which cards the live leases hold: all when any lease is whole-node (or none can be told), else the
// lower-case uuids of every lease's devices.
func leasedCards(v View) (all bool, ids map[string]bool) {
	holders := v.Leases
	if len(holders) == 0 && v.Holder != nil {
		holders = []Holder{*v.Holder}
	}
	if len(holders) == 0 {
		return true, nil
	}
	ids = map[string]bool{}
	for _, h := range holders {
		if len(h.Devices) == 0 {
			return true, nil
		}
		for _, d := range h.Devices {
			ids[strings.ToLower(strings.TrimSpace(d))] = true
		}
	}
	return false, ids
}

// readDriverEvents fills the view's driver-event fields from the reader: the events on the leased cards, newest first
// and capped, the count of those that name no card this box can place, or the note that they could not be read.
func (v *View) readDriverEvents(ctx context.Context, read func(context.Context) ([]DriverEvent, error)) {
	evs, err := read(ctx)
	if errors.Is(err, ErrNoDriverEventSource) {
		return
	}
	if err != nil && len(evs) == 0 {
		v.DriverEventsNote = "driver events unavailable: " + err.Error()
		return
	}
	all, ids := leasedCards(*v)
	var mine []DriverEvent
	for _, e := range evs {
		switch {
		case e.UUID == "":
			v.DriverEventsUnattributed++
		case all || ids[strings.ToLower(e.UUID)]:
			mine = append(mine, e)
		}
	}
	sort.SliceStable(mine, func(i, j int) bool { return mine[i].At.After(mine[j].At) })
	if len(mine) > maxDriverEventsShown {
		v.DriverEventsTotal = len(mine)
		mine = mine[:maxDriverEventsShown]
	}
	v.DriverEvents = mine
}
