package main

import (
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// fixedTime keeps the rendered report byte-stable in tests.
var fixedTime = time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)

func sampleInput() reportInput {
	return reportInput{
		Version: "9.9.9", Host: "somebox", OS: "linux", Arch: "amd64",
		Generated: "2026-07-27 09:00 UTC", ConfigSource: "/etc/offload/config.json",
		Endpoint: "http://127.0.0.1:11436", Health: "OK",
		Profile: "amd-rdna3", Backend: "vulkan", ManifestPath: "/opt/offload-stack/installed.json",
		Aliases: []aliasVerdict{
			{Key: "model", Alias: "offload-e4b", State: "OK"},
			{Key: "vision_model", Alias: "", State: "unset"},
			{Key: "stt_model", Alias: "whisper-stt", State: "**MISSING**"},
		},
		Routes: []mediacap.Route{
			{Name: "generate_image", Engine: "sdcpp", State: mediacap.Configured, Detail: "sdcpp_bin=/opt/sd-cli"},
			{Name: "generate_video", Engine: "comfyui", State: mediacap.BoundButMissing, Detail: "videogen_script=/opt/x.mjs: not found"},
			{Name: "run_graph", Engine: "comfyui", State: mediacap.NotConfigured, Detail: "run_graph_script is unset"},
			{Name: "comfyui", Engine: "runtime", State: mediacap.NotConfigured, Detail: "comfy_dir is unset", Prereq: true},
		},
	}
}

// TestReportStatesEveryRouteAndAliasVerdict: the whole point of handing this to a
// collaborator is that they do not have to be asked follow-up questions. Every route
// and every alias must appear with its verdict.
func TestReportStatesEveryRouteAndAliasVerdict(t *testing.T) {
	got := renderReport(sampleInput())
	for _, want := range []string{
		"generate_image", "generate_video", "run_graph", "comfyui",
		string(mediacap.Configured), string(mediacap.BoundButMissing), string(mediacap.NotConfigured),
		"offload-e4b", "whisper-stt", "**MISSING**", "amd-rdna3", "vulkan", "9.9.9", "somebox",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "_(prereq)_") {
		t.Error("a prereq row must be marked as one — it explains why a CONFIGURED route can still fail")
	}
}

// TestReportSurfacesTheActionableSubset: a collaborator should not have to read a
// table to learn something is broken. Only BOUND-BUT-MISSING is actionable — a route
// this box never bound is a legitimate machine.
func TestReportSurfacesTheActionableSubset(t *testing.T) {
	got := renderReport(sampleInput())
	i := strings.Index(got, "Needs attention (1)")
	if i < 0 {
		t.Fatalf("no actionable section:\n%s", got)
	}
	tail := got[i:]
	if !strings.Contains(tail, "generate_video") {
		t.Error("the bound-but-absent route must be named in the actionable section")
	}
	for _, notActionable := range []string{"run_graph", "generate_image"} {
		if strings.Contains(tail, notActionable) {
			t.Errorf("%s is not a fault and must not be listed as one", notActionable)
		}
	}
}

// TestReportOnACleanBoxHasNoActionableSection: no false alarms.
func TestReportOnACleanBoxHasNoActionableSection(t *testing.T) {
	in := sampleInput()
	in.Routes = []mediacap.Route{
		{Name: "generate_image", Engine: "sdcpp", State: mediacap.Configured, Detail: "sdcpp_bin=/opt/sd-cli"},
		{Name: "run_graph", Engine: "comfyui", State: mediacap.NotConfigured, Detail: "unset"},
	}
	if got := renderReport(in); strings.Contains(got, "Needs attention") {
		t.Errorf("a box with nothing broken must not be told it has a problem:\n%s", got)
	}
}

// TestReportNamesTheTierOrSaysItCannot: guessing a hardware tier is worse than
// admitting there is no installer manifest — the tier drives every serving decision.
func TestReportNamesTheTierOrSaysItCannot(t *testing.T) {
	in := sampleInput()
	in.Profile, in.Backend = "", ""
	in.ManifestNote = "no installer manifest at that path"
	got := renderReport(in)
	if !strings.Contains(got, "UNKNOWN") || !strings.Contains(got, "no installer manifest") {
		t.Errorf("an unknown tier must say so, with the reason:\n%s", got)
	}
}

// TestReportEscapesPipesInPaths: a Windows path or an error text containing "|"
// would otherwise shear the Markdown table apart in the reply they paste back.
func TestReportEscapesPipesInPaths(t *testing.T) {
	in := sampleInput()
	in.Routes = []mediacap.Route{{Name: "media", Engine: "ffmpeg", State: mediacap.Configured, Detail: "ffmpeg_path=C:/a|b/ffmpeg.exe"}}
	got := renderReport(in)
	if !strings.Contains(got, `C:/a\|b/ffmpeg.exe`) {
		t.Errorf("pipe in a binding must be escaped:\n%s", got)
	}
}

// TestGatherReportSurvivesADeadEndpoint: the report is for the moment things are
// broken, so an endpoint that is down must still produce a usable document — the
// media routes are pure config + filesystem and do not need the endpoint at all.
func TestGatherReportSurvivesADeadEndpoint(t *testing.T) {
	cfg := config.Default()
	cfg.Endpoint = "http://127.0.0.1:1" // nothing listens on port 1
	routes := []mediacap.Route{{Name: "generate_image", Engine: "sdcpp", State: mediacap.Configured, Detail: "sdcpp_bin=/opt/sd-cli"}}
	in := gatherReport(cfg, config.Source{}, routes, fixedTime)
	if !strings.HasPrefix(in.Health, "DOWN") {
		t.Errorf("health = %q, want a DOWN verdict", in.Health)
	}
	if len(in.Aliases) != 0 {
		t.Errorf("aliases cannot be verified against a dead endpoint, got %v", in.Aliases)
	}
	got := renderReport(in)
	if !strings.Contains(got, "generate_image") {
		t.Errorf("media routes must still be reported when the endpoint is down:\n%s", got)
	}
	if !strings.Contains(got, "BUILT-IN DEFAULTS") {
		t.Errorf("a report built on defaults must disclose it — the machine's real bindings are inactive:\n%s", got)
	}
}

// TestGatherReportReadsTheLiveRoster: with a healthy endpoint every configured alias
// is measured against /v1/models, using doctor's own alias set.
func TestGatherReportReadsTheLiveRoster(t *testing.T) {
	cfg := config.Default()
	srv := fakeSwap(t, []string{cfg.Model, cfg.TriageModel})
	cfg.Endpoint = srv.URL
	in := gatherReport(cfg, config.Source{Path: "/tmp/config.json"}, nil, fixedTime)
	if in.Health != "OK" {
		t.Fatalf("health = %q", in.Health)
	}
	byKey := map[string]string{}
	for _, a := range in.Aliases {
		byKey[a.Key] = a.State
	}
	if byKey["model"] != "OK" {
		t.Errorf("a served alias must read OK, got %q", byKey["model"])
	}
	if byKey["escalation_model"] != "**MISSING**" {
		t.Errorf("an alias absent from the live roster must read MISSING, got %q", byKey["escalation_model"])
	}
	if in.ConfigSource != "/tmp/config.json" {
		t.Errorf("config source = %q, want the resolved path with no console padding", in.ConfigSource)
	}
}
