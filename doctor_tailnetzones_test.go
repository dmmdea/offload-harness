package main

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// ADR 0074: the tailnet guard admits dotted hostnames under a LIST of zones, so doctor says
// which zones this config named and which key each came from. It is the first row to read
// when a roster entry by name is refused.
func TestDoctorShowsTheConfiguredTailnetZones(t *testing.T) {
	cfg := config.Default()
	cfg.TailnetSuffix = " TAILNNNNNN.ts.net. "
	cfg.TailnetSuffixes = []string{"tailmmmmmm.ts.net", "", "tailnnnnnn.ts.net"}
	var b strings.Builder
	writeTailnetZonesSection(&b, cfg)
	want := "tailnet zones: tailnnnnnn.ts.net (tailnet_suffix), tailmmmmmm.ts.net (tailnet_suffixes[0])\n"
	if b.String() != want {
		t.Fatalf("zones row =\n%q\nwant\n%q (a zone named twice is one zone, an unset slot is skipped)", b.String(), want)
	}
}

// A config naming no zone prints nothing: a green doctor stays as short as it was.
func TestDoctorTailnetZonesRowIsAbsentWithNoZone(t *testing.T) {
	var b strings.Builder
	writeTailnetZonesSection(&b, config.Default())
	if b.Len() != 0 {
		t.Fatalf("no zone configured must print nothing, got %q", b.String())
	}
}

// The row is part of the pure-config band, so it reaches the real doctor output.
func TestDoctorRunPrintsTheTailnetZonesRow(t *testing.T) {
	srv := fakeSwap(t, defaultAliasIDs())
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.TailnetSuffix = "tailnnnnnn.ts.net"
	cfg.TailnetSuffixes = []string{"tailmmmmmm.ts.net"}
	var out strings.Builder
	if err := doctorRun(cfg, nil, &out); err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "tailnet zones: tailnnnnnn.ts.net (tailnet_suffix), tailmmmmmm.ts.net (tailnet_suffixes[0])") {
		t.Fatalf("doctor output lacks the zones row:\n%s", out.String())
	}
}
