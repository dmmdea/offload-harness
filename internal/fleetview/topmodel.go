package fleetview

import (
	"encoding/json"
	"fmt"
	"sort"
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
// headless boxes with no browser. width bounds the free-text columns (probe
// errors, error messages) so a long message can't blow a narrow terminal's
// line wrapping.
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
			fmt.Fprintf(&b, "%-20s unreachable: %s\n", name, truncate(n.ProbeError, width-34))
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
		wall := ""
		if w, _ := j.job["wall_ms"].(float64); w > 0 {
			wall = fmt.Sprintf("%.1fs", w/1000)
		}
		fmt.Fprintf(&b, "%-6s %-20s %-12s %-10s %-8s %s\n",
			jobAge(j.acceptedAt), j.node, str(j.job, "task"), str(j.job, "model"),
			str(j.job, "state"), wall)
	}

	b.WriteString("\nERRORS\n")
	errs := o.Errors
	if len(errs) > 10 {
		errs = errs[len(errs)-10:]
	}
	for i := len(errs) - 1; i >= 0; i-- {
		e := errs[i]
		fmt.Fprintf(&b, "%-8s %-8s %-20s %s\n", e.Severity, e.Node, e.Source, truncate(e.Message, width-39))
	}

	return b.String()
}

// truncate clips s to at most w runes, appending an ellipsis when it does —
// used to keep free-text columns (probe errors, error messages) from
// blowing past the terminal width RenderTop was asked to fit. w<=0 (unknown
// or too-narrow width) disables truncation rather than mangling the text.
func truncate(s string, w int) string {
	r := []rune(s)
	if w <= 0 || len(r) <= w {
		return s
	}
	if w == 1 {
		return string(r[:1])
	}
	return string(r[:w-1]) + "…"
}

// jobRow pairs one job map with the node it came from and its accepted_at
// (unix seconds, cross-node comparable — unlike wall_ms it is not a
// per-job duration but a shared clock reading), for the cross-node JOBS feed.
type jobRow struct {
	node       string
	job        map[string]any
	acceptedAt float64
}

// recentJobs flattens every node's Jobs slice into one feed, sorted newest
// `accepted_at` first across ALL nodes, capped at n entries. This mirrors
// ui.html's `rows.sort((a,b)=>b.t-a.t)` — accepted_at is a real unix
// timestamp (unlike wall_ms, a per-job duration), so it is safe to compare
// across nodes. A job missing accepted_at sorts as if it happened at epoch 0
// (oldest), never as most-recent.
func recentJobs(o Overview, n int) []jobRow {
	var rows []jobRow
	for _, node := range o.Nodes {
		name := node.NodeID
		if name == "" {
			name = node.Base
		}
		for _, j := range node.Jobs {
			at, _ := j["accepted_at"].(float64)
			rows = append(rows, jobRow{node: name, job: j, acceptedAt: at})
		}
	}
	sort.SliceStable(rows, func(i, k int) bool { return rows[i].acceptedAt > rows[k].acceptedAt })
	if len(rows) > n {
		rows = rows[:n]
	}
	return rows
}

// jobAge renders time-since-accepted the way ui.html's `ago` does: seconds
// under a minute, minutes under an hour, else hours. acceptedAt<=0 (missing)
// renders as "-" rather than a nonsensical multi-decade age.
func jobAge(acceptedAt float64) string {
	if acceptedAt <= 0 {
		return "-"
	}
	s := time.Now().Unix() - int64(acceptedAt)
	if s < 0 {
		s = 0
	}
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm", s/60)
	default:
		return fmt.Sprintf("%dh", s/3600)
	}
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
