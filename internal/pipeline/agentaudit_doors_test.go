package pipeline

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// homeAt points every home-directory variable at dir, so agent.DefaultAuditPath
// resolves to <dir>/.local-offload/agent-audit.jsonl ("" when dir is "").
func homeAt(t *testing.T, dir string) {
	t.Helper()
	for _, k := range []string{"USERPROFILE", "HOME"} {
		t.Setenv(k, dir)
	}
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	// These tests read the user-home fallback; TestMain points the offload home somewhere else for every other
	// test (so none writes the operator's ledger or footprint store), and that variable wins over the home.
	t.Setenv("LOCAL_OFFLOAD_HOME", "")
}

func auditRows(t *testing.T, home string) int {
	t.Helper()
	f, err := os.Open(filepath.Join(home, ".local-offload", "agent-audit.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	for sc := bufio.NewScanner(f); sc.Scan(); {
		n++
	}
	return n
}

// writingFake is a seat that writes one file and finishes.
func writingFake() *agentFake {
	return &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(n int64) string {
			if n == 1 {
				return writeCallChat("w1", "util_test.go", "package util\n")
			}
			return doneChat("added the test file")
		},
		repack: func(int64) string { return `{"answer":"added"}` },
	}
}

// TestContractDoorWritesTheBrokerTrailPerAuditMode (register SF-02): a delegated or
// fleet contract that writes a file leaves a broker row under warn and enforce, and
// none under off (today's behaviour: no trail without browse); the result is the
// same green diff in every mode.
func TestContractDoorWritesTheBrokerTrailPerAuditMode(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		wantRows bool
	}{{"", false}, {"off", false}, {"warn", true}, {"enforce", true}} {
		t.Run("mode="+tc.mode, func(t *testing.T) {
			home := t.TempDir()
			homeAt(t, home)
			srv := writingFake().server(t)
			defer srv.Close()
			p := writeDoorPipeline(t, srv.URL, true)
			p.cfg.AuditAllDoors = tc.mode
			wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, writeContract("."))))
			if wire.Deferred {
				t.Fatalf("deferred: %s", wire.Reason)
			}
			if len(wire.DiffFiles) != 1 || wire.DiffFiles[0] != "util_test.go" {
				t.Errorf("diff_files = %v, want [util_test.go] in every audit mode", wire.DiffFiles)
			}
			if got := auditRows(t, home); (got > 0) != tc.wantRows {
				t.Errorf("audit rows = %d, want rows: %v", got, tc.wantRows)
			}
		})
	}
}

// TestContractDoorRefusesEnforceWithoutATrail: enforce with no resolvable audit path
// defers by class config, naming the key, with no seat call; warn runs and says so.
func TestContractDoorRefusesEnforceWithoutATrail(t *testing.T) {
	homeAt(t, "")
	fake := writingFake()
	fake.loop = func(int64) string {
		t.Error("the seat ran a contract under audit_all_doors=enforce with no trail")
		return doneChat("x")
	}
	srv := fake.server(t)
	defer srv.Close()
	p := writeDoorPipeline(t, srv.URL, true)
	p.cfg.AuditAllDoors = "enforce"
	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, writeContract("."))))
	if !wire.Deferred || wire.DeferClass != core.DeferClassConfig || !strings.Contains(wire.Reason, "audit_all_doors") {
		t.Fatalf("want a config-class defer naming audit_all_doors, got deferred=%v class=%q reason=%q", wire.Deferred, wire.DeferClass, wire.Reason)
	}
}

// TestContractDoorWithABrokenTrailPerMode (SF-02 review): the trail's file is a
// directory, so every audit write fails. Under warn the contract runs exactly as with
// no trail (the green diff comes back); under enforce the write is denied and the seat
// is told why, naming the key and the way out.
func TestContractDoorWithABrokenTrailPerMode(t *testing.T) {
	for _, tc := range []struct {
		mode      string
		wantFiles int
	}{{"warn", 1}, {"enforce", 0}} {
		t.Run(tc.mode, func(t *testing.T) {
			home := t.TempDir()
			homeAt(t, home)
			if err := os.MkdirAll(filepath.Join(home, ".local-offload", "agent-audit.jsonl"), 0o755); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			seen := ""
			fake := writingFake()
			fake.loopStream = func(n int64, body map[string]any, w http.ResponseWriter, _ *http.Request) {
				if n == 1 {
					_, _ = w.Write([]byte(writeCallChat("w1", "util_test.go", "package util\n")))
					return
				}
				mu.Lock()
				seen = toolObservations(body)
				mu.Unlock()
				_, _ = w.Write([]byte(doneChat("done")))
			}
			srv := fake.server(t)
			defer srv.Close()
			p := writeDoorPipeline(t, srv.URL, true)
			p.cfg.AuditAllDoors = tc.mode
			wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, writeContract("."))))
			if wire.Deferred {
				t.Fatalf("deferred: %s", wire.Reason)
			}
			if len(wire.DiffFiles) != tc.wantFiles {
				t.Errorf("diff_files = %v, want %d file(s)", wire.DiffFiles, tc.wantFiles)
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.mode == "enforce" && (!strings.Contains(seen, "audit write failed") || !strings.Contains(seen, "audit_all_doors")) {
				t.Errorf("the seat was not told the write was refused by the trail and how to fix it: %q", seen)
			}
		})
	}
}
