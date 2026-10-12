package main

// mcp_startup_capture_test.go: the MCP server takes its startup identity (version, build hash, image stat) as the
// FIRST thing runMCP does (D-36). buildinfo.CaptureStartup hashes the executable at the path os.Executable names; on
// Windows that path keeps its original name after a rename-swap, so every millisecond between the process starting
// and the capture is a chance for a deploy to land and for the capture to hash the NEW file, which then reads as the
// image this process runs: stale_image stays false for the life of the process and its rows carry the wrong hash.
// openPipeline (bbolt, caches, the reloader) is the long stretch; nothing may come before the capture but the
// function's own entry.

import (
	"os"
	"strings"
	"testing"
)

func TestRunMCPCapturesTheStartupImageBeforeOpeningAnything(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(src), "\r\n", "\n")
	start := strings.Index(text, "\nfunc runMCP(")
	if start < 0 {
		t.Fatal("main.go has no runMCP")
	}
	body := text[start+1:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	capture := strings.Index(body, "buildinfo.CaptureStartup()")
	if capture < 0 {
		t.Fatal("runMCP must call buildinfo.CaptureStartup() itself, before it opens anything")
	}
	for _, later := range []string{"loadCfgWithSource(", "openPipeline(", "StartReloader("} {
		at := strings.Index(body, later)
		if at < 0 {
			t.Fatalf("runMCP no longer calls %s: update this pin", later)
		}
		if capture > at {
			t.Errorf("buildinfo.CaptureStartup() comes after %s in runMCP: a deploy landing in between is hashed as the image this process runs", later)
		}
	}
}
