package main

// C-95 finding 2: the shadow note must come from the shared config-loading path,
// not from one verb. The MCP server (agent_delegate, agent_run), acceptance and
// report all load the same config and all build a delegator on it, so each says
// which config's layers pick the seat - once per process, on stderr only (the MCP
// server's stdout is the JSON-RPC stream).

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// shadowedCopy writes a composite config whose agent_model no layer serves (the
// reported config copy) and points $LOCAL_OFFLOAD_CONFIG at it.
func shadowedCopy(t *testing.T, agentModel string) string {
	t.Helper()
	dir := t.TempDir()
	cp := config.CompositeFixture()
	cp.AgentDelegationEnabled = true
	cp.AgentModel = agentModel
	cp.StateDir = filepath.Join(dir, "state")
	cp.GPULockPath = filepath.Join(dir, "gpu.lock")
	p := filepath.Join(dir, "copy.json")
	writeConfigJSON(t, p, cp)
	t.Setenv("LOCAL_OFFLOAD_CONFIG", p)
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("USERPROFILE", filepath.Join(dir, "home"))
	return p
}

func TestSharedConfigLoadSaysShadowOncePerProcess(t *testing.T) {
	p := shadowedCopy(t, "scratch-seat-c95-once")
	stderr := captureStderr(t, func() {
		for i := 0; i < 3; i++ {
			fs := flag.NewFlagSet("x", flag.ContinueOnError)
			fs.String("config", "", "")
			loadCfgWithSource(fs)
			loadCfg(fs)
		}
	})
	if n := strings.Count(stderr, `agent_model "scratch-seat-c95-once"`); n != 1 {
		t.Fatalf("the note must appear exactly once per process, got %d in %q", n, stderr)
	}
	if !strings.Contains(stderr, p) {
		t.Fatalf("the note must name the config file %s: %q", p, stderr)
	}
}

func TestSharedConfigLoadIsSilentWhenAgentModelIsALayerSeat(t *testing.T) {
	shadowedCopy(t, "agent-pool")
	stderr := captureStderr(t, func() {
		fs := flag.NewFlagSet("x", flag.ContinueOnError)
		fs.String("config", "", "")
		loadCfgWithSource(fs)
	})
	if strings.Contains(stderr, "agent_model") {
		t.Fatalf("an agreeing config must not be warned about: %q", stderr)
	}
}

func TestMCPEntryPathSaysShadowOnStderrNotStdout(t *testing.T) {
	shadowedCopy(t, "scratch-seat-c95-mcp")

	// An empty stdin ends the stdio transport at once; stdout is the JSON-RPC stream.
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	inW.Close()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origIn, origOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	var runErr error
	stderr := captureStderr(t, func() { runErr = runMCP([]string{}) })
	os.Stdin, os.Stdout = origIn, origOut
	outW.Close()
	stdout, _ := io.ReadAll(outR)
	_ = runErr

	if n := strings.Count(stderr, `agent_model "scratch-seat-c95-mcp"`); n != 1 {
		t.Fatalf("the MCP entry path must say the layers shadow agent_model exactly once on stderr, got %d in %q", n, stderr)
	}
	if strings.Contains(string(stdout), "agent_model") {
		t.Fatalf("stdout carries JSON-RPC only; the note leaked into it: %q", stdout)
	}
}
