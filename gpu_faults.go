package main

// `gpu faults`: the NVIDIA driver's fault history on this machine, read as incidents with their known-error class
// and NVIDIA's recommended action (internal/gpuactivity/faults.go), and with --jobs the ledger rows that ran here
// during each incident. Lease-independent: every card, up to 30 days. Facts only, never a lease verdict input
// (C-83-B); a read that is short says so.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// faultJobSlack widens an incident on both sides when the ledger is joined: a job that faulted is recorded when it
// ends, and the driver's records can trail the fault by a minute.
const faultJobSlack = 2 * time.Minute

// readFaultsFn is the report reader, a variable so a test prints a synthetic history.
var readFaultsFn = gpuactivity.ReadFaults

// faultJob is one ledger row that ran on this node during an incident.
type faultJob struct {
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Task      string    `json:"task"`
	Door      string    `json:"door,omitempty"`
	Route     string    `json:"route,omitempty"`
	ModelTier string    `json:"model_tier,omitempty"`
	LatencyMs int64     `json:"latency_ms"`
	JobID     string    `json:"job_id,omitempty"`
	ErrClass  string    `json:"err_class,omitempty"`
	Deferred  bool      `json:"deferred,omitempty"`
}

// faultIncidentOut is an incident as --json prints it: the incident's fields, plus its jobs when --jobs asked.
type faultIncidentOut struct {
	gpuactivity.Incident
	Jobs []faultJob `json:"jobs,omitempty"`
}

func runGPUFaults(args []string) error {
	fs := flag.NewFlagSet("gpu faults", flag.ExitOnError)
	fs.String("config", "", "config file path")
	days := fs.Int("days", 7, "how many days back to read (at most 30)")
	jobs := fs.Bool("jobs", false, "add the ledger rows that ran on this node during each incident")
	asJSON := fs.Bool("json", false, "emit JSON")
	_ = fs.Parse(args)
	if *days < 1 || *days > 30 {
		return fmt.Errorf("gpu faults: --days must be 1 to 30 (got %d)", *days)
	}
	rep, err := readFaultsFn(context.Background(), time.Duration(*days)*24*time.Hour)
	if errors.Is(err, gpuactivity.ErrNoDriverEventSource) {
		return errors.New("gpu faults: this OS has no NVIDIA driver log to read (Windows System log or Linux kernel log)")
	}
	if err != nil {
		return fmt.Errorf("gpu faults: %w", err)
	}
	var jobRows []ledger.Entry
	jobsNote, self := "", ""
	if *jobs {
		cfg := loadCfg(fs)
		self = faultSelfNode(cfg)
		rows, _, lerr := ledger.ReadProduction(cfg.LedgerPath)
		if lerr != nil {
			jobsNote = fmt.Sprintf("the ledger could not be read (%v)", lerr)
		}
		jobRows = rows
	}
	incs := make([]faultIncidentOut, len(rep.Incidents))
	for i, in := range rep.Incidents {
		incs[i] = faultIncidentOut{Incident: in}
		if *jobs {
			incs[i].Jobs = jobsDuring(jobRows, self, in.Start.Add(-faultJobSlack), in.End.Add(faultJobSlack))
		}
	}
	if *asJSON {
		out := map[string]any{
			"since": rep.Since, "until": rep.Until, "window_days": rep.WindowDays, "events": rep.Events,
			"cards": rep.Cards, "incidents": incs, "repeats": rep.Repeats, "known_errors": rep.Known,
			"live_kernel_events": rep.LiveKernel, "notes": rep.Notes, "untimed": rep.Untimed,
			"facts_only": "driver events are facts, never a lease verdict input (C-83-B)",
		}
		if *jobs {
			out["jobs_joined"] = true
			if jobsNote != "" {
				out["jobs_note"] = jobsNote
			}
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	printFaults(rep, incs, *jobs, jobsNote)
	return nil
}

// faultSelfNode is this machine's node id as fleet-serve derives it: fleet_node_id, else the OS hostname.
func faultSelfNode(cfg config.Config) string {
	if id := strings.TrimSpace(cfg.FleetNodeID); id != "" {
		return id
	}
	h, _ := os.Hostname()
	return strings.TrimSpace(h)
}

// ranHere says whether a ledger row's work ran on this node. An inner row (its outer row stands for the job) is
// skipped and a row that took no time did no work; a deferral that ran for a while is kept (a job a fault ended
// is recorded as deferred); a row that names a remote node ran there unless that node is
// this one; a delegate row names its node in its tier ("<node>:<seat>"); any other row ran here unless its route
// was remote.
func ranHere(r ledger.Entry, self string) bool {
	if r.ParentJobID != "" || r.LatencyMs <= 0 || r.TS == 0 {
		return false
	}
	if r.Node != "" || r.NodeID != "" {
		return self != "" && (strings.EqualFold(r.Node, self) || strings.EqualFold(r.NodeID, self))
	}
	if r.Task == "agent_delegate" {
		node, _, ok := strings.Cut(r.ModelTier, ":")
		return ok && self != "" && strings.EqualFold(node, self)
	}
	return r.Route != "remote"
}

// jobsDuring is the ledger rows whose work ran on this node and overlaps [from, to]. A row is recorded when its call
// ends, so it ran from ts-latency to ts.
func jobsDuring(rows []ledger.Entry, self string, from, to time.Time) []faultJob {
	var out []faultJob
	for _, r := range rows {
		if !ranHere(r, self) {
			continue
		}
		end := time.Unix(r.TS, 0)
		start := end.Add(-time.Duration(r.LatencyMs) * time.Millisecond)
		if end.Before(from) || start.After(to) {
			continue
		}
		out = append(out, faultJob{Start: start.UTC(), End: end.UTC(), Task: r.Task, Door: r.Door, Route: r.Route, ModelTier: r.ModelTier, LatencyMs: r.LatencyMs, JobID: r.JobID, ErrClass: r.ErrClass, Deferred: r.Deferred})
	}
	return out
}

func printFaults(rep gpuactivity.FaultReport, incs []faultIncidentOut, jobs bool, jobsNote string) {
	ts := func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") }
	fmt.Printf("GPU driver faults on this machine, last %d day(s) (since %s): %d driver record(s) in %d incident(s), %d live kernel dump(s)\n",
		rep.WindowDays, rep.Since.Local().Format("2006-01-02 15:04 MST"), rep.Events, len(rep.Incidents), len(rep.LiveKernel))
	fmt.Println("Facts, never a lease verdict input. Actions are NVIDIA's datacenter guidance; Windows ids are read as the Xid they match (see known errors).")

	fmt.Println("\ncards:")
	if len(rep.Cards) == 0 {
		fmt.Println("  none read (see notes)")
	}
	for _, c := range rep.Cards {
		ra := c.RecoveryAction
		if ra == "" {
			ra = "not reported"
		}
		line := fmt.Sprintf("  card %d  %s  %s  recovery action: %s  ", c.Index, c.Bus, c.Name, ra)
		if c.Events == 0 {
			line += "no records placed on it"
		} else {
			line += fmt.Sprintf("%d record(s) (%s), %d incident(s), last %s", c.Events, classList(c.ByClass), c.Incidents, ts(c.Last))
		}
		fmt.Println(line)
	}

	if len(rep.Repeats) > 0 {
		fmt.Println("\nrepeats (NVSentinel's RepeatedXIDErrorOnSameGPU: one class in 5 or more bursts on one card within 24 h; its action is CONTACT_SUPPORT):")
		for _, r := range rep.Repeats {
			fmt.Printf("  %s %s: %d bursts from %s to %s\n", cardLabel(r.Card, r.Bus), r.Class, r.Bursts, ts(r.From), ts(r.To))
		}
	}

	fmt.Println("\nincidents (records less than 3 minutes apart are one burst), newest first:")
	if len(incs) == 0 {
		fmt.Println("  none in the window")
	}
	for _, in := range incs {
		var cards []string
		for _, c := range in.Cards {
			cards = append(cards, cardLabel(c.Index, c.Bus))
		}
		where := "no record names a card"
		if len(cards) > 0 {
			where = strings.Join(cards, ", ")
		}
		line := fmt.Sprintf("  %s +%ds  %s  %s", ts(in.Start), int(in.End.Sub(in.Start).Seconds()), where, classList(in.Classes))
		if in.Unplaced > 0 && len(in.Cards) > 0 {
			line += fmt.Sprintf("  (%d record(s) name no card)", in.Unplaced)
		}
		fmt.Println(line)
		if in.Reading != "" {
			fmt.Println("      xid 13 reading: " + in.Reading)
		}
		for _, l := range in.LiveKernel {
			fmt.Printf("      live dump: %s %s at %s (%s)\n", l.Code, l.Name, l.At.Local().Format("15:04"), l.Dump)
		}
		if jobs {
			if jobsNote != "" {
				fmt.Println("      jobs: " + jobsNote)
			} else if len(in.Jobs) == 0 {
				fmt.Println("      jobs: no ledger row ran here in this window (2 min either side)")
			}
			for _, j := range in.Jobs {
				fmt.Printf("      job: %s..%s %s door=%s route=%s tier=%s %.1fs%s\n", j.Start.Local().Format("15:04:05"), j.End.Local().Format("15:04:05"), j.Task, dash(j.Door), dash(j.Route), dash(j.ModelTier), float64(j.LatencyMs)/1000, errSuffix(j.ErrClass, j.Deferred))
			}
		}
	}

	if len(rep.LiveKernel) > 0 {
		fmt.Println("\nlive kernel dumps (Windows Error Reporting, one per dump; time from the dump's file name, local minute):")
		for _, l := range rep.LiveKernel {
			when := l.At.Local().Format("2006-01-02 15:04")
			if l.AtSource != "dump-name" {
				when += " (report queue time, not the fault time)"
			}
			fmt.Printf("  %s  %s %s  %d report(s)  %s\n", when, l.Code, l.Name, l.Reports, l.Dump)
		}
	}

	fmt.Println("\nknown errors seen:")
	if len(rep.Known) == 0 {
		fmt.Println("  none")
	}
	for _, k := range rep.Known {
		basis := k.Basis
		if k.XidAnalogue > 0 {
			basis += fmt.Sprintf(", read as Xid %d", k.XidAnalogue)
		}
		fmt.Printf("  %s  %s  [%s/%s, %s]\n", k.Class, k.Name, k.Domain, k.Severity, basis)
		fmt.Printf("      %s\n", k.Meaning)
		if k.NVIDIAAction != "" {
			act := "NVIDIA immediate action: " + k.NVIDIAAction
			if k.NVIDIAActionMeaning != "" {
				act += " (" + k.NVIDIAActionMeaning + ")"
			}
			if k.NVIDIAInvestigate != "" {
				act += "; investigatory: " + k.NVIDIAInvestigate
			}
			fmt.Printf("      %s\n", act)
		}
	}
	if rep.Untimed > 0 {
		fmt.Printf("\n%d record(s) carried no readable time and are not grouped\n", rep.Untimed)
	}
	for _, n := range rep.Notes {
		fmt.Println("note: " + n)
	}
}

func classList(cs []gpuactivity.ClassCount) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, fmt.Sprintf("%s x%d", c.Class, c.Count))
	}
	return strings.Join(parts, ", ")
}

func cardLabel(index int, bus string) string {
	if index < 0 {
		return fmt.Sprintf("bus %s (not in today's card table)", bus)
	}
	return fmt.Sprintf("card %d (%s)", index, bus)
}

func errSuffix(c string, deferred bool) string {
	s := ""
	if deferred {
		s = " deferred"
	}
	if c != "" {
		s += " err=" + c
	}
	return s
}
