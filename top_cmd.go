package main

import (
	"flag"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dmmdea/offload-harness/internal/fleetview"
)

// runTop is the thin verb wrapper around fleetview.NewTop: it is a pure
// client of a running fleet-ui, so — unlike fleet-serve/fleet-ui — it needs
// no config file, no netguard bind check, and no delegator state; on a
// headless box it just points --ui at the delegator's tailnet address.
func runTop(args []string) error {
	fs := flag.NewFlagSet("top", flag.ExitOnError)
	ui := fs.String("ui", "http://127.0.0.1:18813", "fleet-ui base URL to poll")
	interval := fs.Duration("interval", 5*time.Second, "poll interval")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p := tea.NewProgram(fleetview.NewTop(*ui, *interval), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
