package gpuactivity

// faultcontext.go: the restarts and sleep transitions the OS logged, so `gpu faults` can label an incident that is
// the driver tearing down or coming up rather than a card failing under work.
//
// A driver logs faults when the machine shuts down, boots, sleeps or wakes: the reference box's 30 days hold 15
// restarts, and incidents sit on two of them. Such an incident is still listed, labelled with its context, but it does
// not count towards NVSentinel's repeat rule: five restarts in a day are not a card failing five times.
//
// WINDOWS (System log): a restart opens at User32 1074 (a process asked for it), EventLog 6006 (clean stop) or
// Kernel-General 13 (OS shutdown) and closes at the next EventLog 6005 or Kernel-General 12 (the boot); EventLog 6008
// and Kernel-Power 41 (logged at boot after an unexpected shutdown) are points. A sleep opens at Kernel-Power 42 and
// closes at Kernel-Power 107 or Power-Troubleshooter 1. Kernel-Power 566 is NOT read: it is a session transition
// (unlock, input, display on), 244 of them in the reference box's 30 days, and would label unrelated faults.
//
// LINUX (journal): each boot's first entry is a point (`journalctl --list-boots`); a suspend or hibernation opens at
// the kernel's "PM: suspend entry" and closes at "PM: suspend exit". The end of a boot is not read as a shutdown: the
// journal does not tell a clean stop from a crash, and a fault just before a crash is exactly the one to keep.
//
// An incident is in context when it overlaps a span widened by a minute on each side. Opening events less than 10
// minutes apart are one opening, so a restart asked for an hour ahead (1074 with a timer) does not cover that hour.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// ContextRestart and ContextSleepWake are the incident labels.
	ContextRestart   = "restart"
	ContextSleepWake = "sleep-wake"
	// contextSlack widens each span on both sides.
	contextSlack = time.Minute
	// contextGroupGap joins opening events into one opening.
	contextGroupGap = 10 * time.Minute
	// maxContextRecords caps the Windows read (the reference box: 87 records in 30 days without Kernel-Power 566).
	maxContextRecords = 2000
	// maxIncidentContext caps the events an incident carries.
	maxIncidentContext = 4
)

// ContextEvent is one restart or sleep transition the OS logged. No message text is kept: a restart record names the
// machine and the user that asked for it.
type ContextEvent struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Source string    `json:"source"`
	ID     int       `json:"id,omitempty"`
}

// contextRole says what one record does to a span: opens it, closes it, or stands alone.
type contextRole int

const (
	roleNone contextRole = iota
	roleOpen
	roleClose
	rolePoint
)

// winContextRole is the role and kind of a Windows System log record.
func winContextRole(source string, id int) (contextRole, string) {
	switch {
	case source == "User32" && id == 1074, source == "EventLog" && id == 6006, source == "Microsoft-Windows-Kernel-General" && id == 13:
		return roleOpen, ContextRestart
	case source == "EventLog" && id == 6005, source == "Microsoft-Windows-Kernel-General" && id == 12:
		return roleClose, ContextRestart
	case source == "EventLog" && id == 6008, source == "Microsoft-Windows-Kernel-Power" && id == 41:
		return rolePoint, ContextRestart
	case source == "Microsoft-Windows-Kernel-Power" && id == 42:
		return roleOpen, ContextSleepWake
	case source == "Microsoft-Windows-Kernel-Power" && id == 107, source == "Microsoft-Windows-Power-Troubleshooter" && id == 1:
		return roleClose, ContextSleepWake
	}
	return roleNone, ""
}

// winContextScriptFor reads the restart and sleep records of the last hours from the System log, newest first,
// capped, as XML (the same shape ParseWinEvents reads).
func winContextScriptFor(hours, max int) string {
	ms := int64(hours) * 3600 * 1000
	xp := "*[System[((Provider[@Name='User32'] and EventID=1074) or (Provider[@Name='EventLog'] and (EventID=6005 or EventID=6006 or EventID=6008)) or (Provider[@Name='Microsoft-Windows-Kernel-General'] and (EventID=12 or EventID=13)) or (Provider[@Name='Microsoft-Windows-Kernel-Power'] and (EventID=41 or EventID=42 or EventID=107)) or (Provider[@Name='Microsoft-Windows-Power-Troubleshooter'] and EventID=1)) and TimeCreated[timediff(@SystemTime) <= " + fmt.Sprint(ms) + "]]]"
	return fmt.Sprintf(`$ErrorActionPreference='Stop'; [Console]::OutputEncoding=[System.Text.Encoding]::UTF8; try { Get-WinEvent -LogName System -FilterXPath "%s" -MaxEvents %d | ForEach-Object { $_.ToXml() } } catch { if ($_.FullyQualifiedErrorId -like 'NoMatchingEventsFound*') { exit 0 }; [Console]::Error.WriteLine($_.Exception.Message); exit 2 }`, xp, max)
}

// Seam: the context read, so a test reads canned records on any OS.
var readContextFn = readContextEvents

// readContextEvents reads the window's restart and sleep records, each with its role. A short read returns what it
// read and the reason.
func readContextEvents(ctx context.Context, now time.Time, window time.Duration) ([]roledEvent, error) {
	rctx, cancel := context.WithTimeout(ctx, faultReadTimeout)
	defer cancel()
	hours := int((window + time.Hour - 1) / time.Hour)
	cutoff := now.Add(-window)
	var out []roledEvent
	keep := func(r roledEvent) {
		if !r.At.IsZero() && !r.At.Before(cutoff) {
			out = append(out, r)
		}
	}
	switch driverEventsGOOS {
	case "windows":
		raw, err := runChild(rctx, powershellPath(), "-NoProfile", "-NonInteractive", "-Command", winContextScriptFor(hours, maxContextRecords))
		if err != nil {
			return nil, fmt.Errorf("reading the System log: %w", err)
		}
		evs, perr := ParseWinEvents(raw)
		for _, e := range evs {
			if role, kind := winContextRole(e.Source, e.ID); role != roleNone {
				keep(roledEvent{ContextEvent{At: e.At, Kind: kind, Source: e.Source, ID: e.ID}, role})
			}
		}
		if perr != nil {
			return out, fmt.Errorf("the System log read stopped part way (%v), so older restarts may be missing", perr)
		}
		return out, nil
	case "linux":
		var errs []string
		if raw, err := runChild(rctx, "journalctl", "--list-boots", "--no-pager", "-o", "json"); err != nil {
			errs = append(errs, fmt.Sprintf("boots: %v", err))
		} else if boots, perr := ParseBootList(raw); perr != nil {
			errs = append(errs, fmt.Sprintf("boots: %v", perr))
		} else {
			for _, b := range boots {
				keep(roledEvent{b, rolePoint})
			}
		}
		raw, err := runChild(rctx, "journalctl", "--no-pager", "-o", "short-unix", "--since", fmt.Sprintf("%d hours ago", hours), "_TRANSPORT=kernel", "--grep", `PM: (suspend|hibernation) (entry|exit)`)
		// --grep exits 1 with "-- No entries --" when nothing matches: that is an empty answer, not a failure.
		if err != nil && !strings.Contains(raw, "-- No entries --") {
			errs = append(errs, fmt.Sprintf("suspend lines: %v", err))
		} else {
			for _, r := range ParsePMLines(raw) {
				keep(r)
			}
		}
		if len(errs) > 0 {
			return out, errors.New(strings.Join(errs, "; "))
		}
		return out, nil
	}
	return nil, ErrNoDriverEventSource
}

// roledEvent is a context record with what it does to a span.
type roledEvent struct {
	ContextEvent
	role contextRole
}

// ParseBootList reads `journalctl --list-boots -o json`: each boot's first entry is a restart point.
func ParseBootList(raw string) ([]ContextEvent, error) {
	var boots []struct {
		FirstEntry int64 `json:"first_entry"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &boots); err != nil {
		return nil, fmt.Errorf("the boot list is not JSON: %w", err)
	}
	out := make([]ContextEvent, 0, len(boots))
	for _, b := range boots {
		if b.FirstEntry > 0 {
			out = append(out, ContextEvent{At: time.UnixMicro(b.FirstEntry).UTC(), Kind: ContextRestart, Source: "boot"})
		}
	}
	return out, nil
}

var pmRE = regexp.MustCompile(`PM: (suspend|hibernation) (entry|exit)`)

// ParsePMLines reads the kernel's suspend and hibernation lines (`journalctl -o short-unix`): entry opens a sleep,
// exit closes it. Other lines (the "-- Boot ... --" separators) are skipped.
func ParsePMLines(raw string) []roledEvent {
	var out []roledEvent
	for _, line := range strings.Split(raw, "\n") {
		m := pmRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		role := roleOpen
		if m[2] == "exit" {
			role = roleClose
		}
		out = append(out, roledEvent{ContextEvent{At: kernelLogTime(fields[0]), Kind: ContextSleepWake, Source: "PM " + m[1], ID: 0}, role})
	}
	return out
}

// contextSpan is one restart or sleep: from its first opening record to its closing one.
type contextSpan struct {
	from, to time.Time
	kind     string
	events   []ContextEvent
}

// contextSpans pairs the records into spans, per kind: opening records less than contextGroupGap apart are one
// opening, which the next closing record of its kind ends; an opening with no close, a close with no opening and a
// point each stand as a span of their own.
func contextSpans(evs []roledEvent) []contextSpan {
	sorted := append([]roledEvent(nil), evs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })
	open := map[string]*contextSpan{}
	var spans []contextSpan
	flush := func(kind string) {
		if s := open[kind]; s != nil {
			spans = append(spans, *s)
			delete(open, kind)
		}
	}
	for _, e := range sorted {
		switch e.role {
		case roleOpen:
			if s := open[e.Kind]; s != nil && e.At.Sub(s.to) <= contextGroupGap {
				s.to = e.At
				s.events = append(s.events, e.ContextEvent)
				continue
			}
			flush(e.Kind)
			open[e.Kind] = &contextSpan{from: e.At, to: e.At, kind: e.Kind, events: []ContextEvent{e.ContextEvent}}
		case roleClose:
			if s := open[e.Kind]; s != nil {
				s.to = e.At
				s.events = append(s.events, e.ContextEvent)
				flush(e.Kind)
				continue
			}
			spans = append(spans, contextSpan{from: e.At, to: e.At, kind: e.Kind, events: []ContextEvent{e.ContextEvent}})
		case rolePoint:
			spans = append(spans, contextSpan{from: e.At, to: e.At, kind: e.Kind, events: []ContextEvent{e.ContextEvent}})
		}
	}
	for _, kind := range []string{ContextRestart, ContextSleepWake} {
		flush(kind)
	}
	return spans
}

// markContext labels each incident that overlaps a span widened by contextSlack, with the span's kind (the first
// span it overlaps, restart before sleep when both) and up to maxIncidentContext of the spans' records.
func markContext(incs []Incident, spans []contextSpan) {
	for n := range incs {
		in := &incs[n]
		for _, s := range spans {
			if in.Start.After(s.to.Add(contextSlack)) || in.End.Before(s.from.Add(-contextSlack)) {
				continue
			}
			if in.Context == "" || (in.Context == ContextSleepWake && s.kind == ContextRestart) {
				in.Context = s.kind
			}
			for _, e := range s.events {
				if len(in.ContextEvents) < maxIncidentContext {
					in.ContextEvents = append(in.ContextEvents, e)
				}
			}
		}
	}
}
