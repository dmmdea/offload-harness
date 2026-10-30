package main

// mcp_startup_capture_test.go: the long-running processes take their startup identity (version, build hash, image stat)
// as the FIRST thing they do (D-36). buildinfo.CaptureStartup hashes the executable at the path os.Executable names; on
// Windows that path keeps its original name after a rename-swap, so every millisecond between the process starting
// and the capture is a chance for a deploy to land and for the capture to hash the NEW file, which then reads as the
// image this process runs: stale_image stays false for the life of the process and its rows carry the wrong hash.
// openPipeline (bbolt, caches, the reloader) is the long stretch; nothing may come before the capture but the
// function's own entry. Two processes live that long: the MCP server (runMCP) and the fleet node (runFleetServe, whose
// answers carry harness_build_sha256 of the image it runs).

import (
	"strings"
	"testing"
)

// funcBody is the text of the top-level function name in main.go, from its signature to its closing brace.
func funcBody(t *testing.T, name string) string {
	t.Helper()
	text := mainSplitSource(t)
	start := strings.Index(text, "\nfunc "+name+"(")
	if start < 0 {
		t.Fatalf("main.go has no %s", name)
	}
	body := text[start+1:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	return body
}

// capturesBefore fails unless buildinfo.CaptureStartup() is called in the function's body before every call in later.
func capturesBefore(t *testing.T, name string, later ...string) {
	t.Helper()
	body := funcBody(t, name)
	capture := strings.Index(body, "buildinfo.CaptureStartup()")
	if capture < 0 {
		t.Fatalf("%s must call buildinfo.CaptureStartup() itself, before it opens anything", name)
	}
	for _, l := range later {
		at := strings.Index(body, l)
		if at < 0 {
			t.Fatalf("%s no longer calls %s: update this pin", name, l)
		}
		if capture > at {
			t.Errorf("buildinfo.CaptureStartup() comes after %s in %s: a deploy landing in between is hashed as the image this process runs", l, name)
		}
	}
}

func TestRunMCPCapturesTheStartupImageBeforeOpeningAnything(t *testing.T) {
	// mcpserver.New is in the list because the stale-binary guard starts from the identity captured here: a New that ran
	// before the capture would start the guard from a stat taken after whatever was opened first.
	capturesBefore(t, "runMCP", "loadCfgWithSource(", "openPipeline(", "StartReloader(", "mcpserver.New(")
}

// A fleet node is as long-lived as an MCP server: its agent answers stamp harness_build_sha256 (pipeline/agenttask.go),
// and a node whose first answer came after a rename-swap deploy used to hash the NEW file and stamp a build it was not
// running.
func TestRunFleetServeCapturesTheStartupImageBeforeOpeningAnything(t *testing.T) {
	capturesBefore(t, "runFleetServe", "loadCfgWithSource(", "fleetServeConfigGate(", "SweepOrphanedPipelineJobs(")
}
