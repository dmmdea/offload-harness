package fleetview

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dmmdea/offload-harness/internal/netguard"

	"net/http"
)

// RenderTop is the pure, tested core of the `top` terminal view: a plain-text
// rendering of an Overview snapshot, independent of Bubble Tea so it can be
// unit-tested without a TTY. It builds three sections — a header line, a
// per-node table, and JOBS/ERRORS feeds — mirroring fleet-ui's page but for
// headless boxes with no browser.
func RenderTop(o Overview, width int) string {
	var b strings.Builder

	reachable := 0
	for _, n := range o.Nodes {
		if n.Reachable {
			reachable++
		}
	}
	delegation := "off"
	if o.DelegationEnabled {
		delegation = "on"
	}
	fmt.Fprintf(&b, "Fleet overview  %d/%d reachable  delegation %s\n\n", reachable, len(o.Nodes), delegation)

	fmt.Fprintf(&b, "%-20s %-18s %-4s %-5s %-13s %-5s %-11s %s\n",
		"NODE", "SEAT", "RES", "GPU%", "VRAM free/total", "CPU%", "RAM", "RUN/QUEUED")
	for _, n := range o.Nodes {
		if !n.Reachable {
			name := n.NodeID
			if name == "" {
				name = n.Base
			}
			fmt.Fprintf(&b, "%-20s unreachable: %s\n", name, n.ProbeError)
			continue
		}
		name := n.NodeID
		if name == "" {
			name = n.Base
		}
		res := "no"
		if n.AgentResident {
			res = "yes"
		}
		gpu := "?"
		if n.GpuUtilKnown {
			gpu = fmt.Sprintf("%d%%", n.GpuUtil)
		}
		vram := fmt.Sprintf("%.1f/%.1f", n.VramFree, n.VramTotal)
		ram := fmt.Sprintf("%.1f/%.1f", n.RamUsed, n.RamTotal)
		runq := fmt.Sprintf("%d/%d", n.JobsRunning, n.JobsQueued)
		fmt.Fprintf(&b, "%-20s %-18s %-4s %-5s %-13s %-5d %-11s %s\n",
			name, n.AgentSeat, res, gpu, vram, n.HostCPU, ram, runq)
	}

	b.WriteString("\nJOBS\n")
	for _, j := range recentJobs(o, 15) {
		fmt.Fprintf(&b, "%-6s %-20s %-12s %-10s %-8s %s\n",
			jobAge(j.wallMs), j.node, str(j.job, "task"), str(j.job, "model"),
			str(j.job, "state"), str(j.job, "id"))
	}

	b.WriteString("\nERRORS\n")
	errs := o.Errors
	if len(errs) > 10 {
		errs = errs[len(errs)-10:]
	}
	for i := len(errs) - 1; i >= 0; i-- {
		e := errs[i]
		fmt.Fprintf(&b, "%-8s %-8s %-20s %s\n", e.Severity, e.Node, e.Source, e.Message)
	}

	return b.String()
}

// jobRow pairs one job map with the node it came from, for the cross-node
// JOBS feed.
type jobRow struct {
	node   string
	job    map[string]any
	wallMs float64
}

// recentJobs flattens every node's Jobs slice into one feed capped at n
// entries. Each node's Jobs slice already arrives newest-first from the
// poller (internal/fleetview's collector), so this just interleaves nodes in
// order and truncates — no re-sort needed, and jobs carry no absolute
// timestamp to sort by anyway (only accepted_at, on the node's own clock).
func recentJobs(o Overview, n int) []jobRow {
	var rows []jobRow
	for _, node := range o.Nodes {
		name := node.NodeID
		if name == "" {
			name = node.Base
		}
		for _, j := range node.Jobs {
			wall, _ := j["wall_ms"].(float64)
			rows = append(rows, jobRow{node: name, job: j, wallMs: wall})
			if len(rows) >= n {
				return rows
			}
		}
	}
	return rows
}

// jobAge renders a job's wall time as a short duration string; jobs carry no
// absolute timestamp usable against the delegator's clock, so this reports
// the job's own duration rather than time-since-now.
func jobAge(wallMs float64) string {
	if wallMs <= 0 {
		return "-"
	}
	return (time.Duration(wallMs) * time.Millisecond).String()
}

// topModel is the Bubble Tea client of a running fleet-ui: it polls
// GET <ui>/api/overview on an interval and renders RenderTop. Kept separate
// from RenderTop so the render logic stays pure and unit-testable.
type topModel struct {
	ui       string
	interval time.Duration
	client   *http.Client
	ov       Overview
	err      error
	width    int
}

type tickMsg time.Time
type overviewMsg struct {
	ov  Overview
	err error
}

// NewTop constructs the `top` Bubble Tea program's model. ui is the fleet-ui
// base URL; interval is the poll period.
func NewTop(ui string, interval time.Duration) tea.Model {
	return topModel{
		ui:       strings.TrimRight(ui, "/"),
		interval: interval,
		width:    100,
		client:   &http.Client{Transport: netguard.SafeTransport(nil), Timeout: 5 * time.Second},
	}
}

func (m topModel) Init() tea.Cmd {
	return tea.Batch(m.fetch(), tea.Tick(m.interval, func(t time.Time) tea.Msg { return tickMsg(t) }))
}

func (m topModel) fetch() tea.Cmd {
	return func() tea.Msg {
		resp, err := m.client.Get(m.ui + "/api/overview")
		if err != nil {
			return overviewMsg{err: err}
		}
		defer resp.Body.Close()
		var ov Overview
		if err := json.NewDecoder(resp.Body).Decode(&ov); err != nil {
			return overviewMsg{err: err}
		}
		return overviewMsg{ov: ov}
	}
}

func (m topModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.KeyMsg:
		if v.String() == "q" || v.String() == "ctrl+c" {
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width = v.Width
	case tickMsg:
		return m, tea.Batch(m.fetch(), tea.Tick(m.interval, func(t time.Time) tea.Msg { return tickMsg(t) }))
	case overviewMsg:
		m.ov, m.err = v.ov, v.err
	}
	return m, nil
}

func (m topModel) View() string {
	if m.err != nil {
		return "fleet-ui unreachable at " + m.ui + ": " + m.err.Error() + "\n(q to quit)\n"
	}
	return RenderTop(m.ov, m.width) + "\nq quit\n"
}
