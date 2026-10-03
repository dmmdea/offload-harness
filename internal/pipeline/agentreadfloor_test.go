package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// readEnvChat is an assistant turn that calls read_file on a secret-material file.
func readEnvChat() string {
	return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"r1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\".env\"}"}}]},"finish_reason":"tool_calls"}]}`
}

// TestContractDoorAppliesTheReadFloor (register SF-07): a delegated or fleet
// contract whose seat reads a secret file gets the file under warn (the default,
// recorded) and a refusal under enforce (recorded); the node's agent_read_floor
// reaches the loop, with the trail attached by audit_all_doors=warn.
func TestContractDoorAppliesTheReadFloor(t *testing.T) {
	for _, tc := range []struct {
		floor    string
		decision string
		refused  bool
	}{{"", "warn", false}, {"enforce", "deny", true}} {
		t.Run("floor="+tc.floor, func(t *testing.T) {
			home := t.TempDir()
			homeAt(t, home)
			var mu sync.Mutex
			seen := ""
			fake := &agentFake{
				rosterIDs: []string{agentTestSeat},
				loop:      func(int64) string { return doneChat("read it") },
				repack:    func(int64) string { return `{"answer":"done"}` },
			}
			fake.loopStream = func(n int64, body map[string]any, w http.ResponseWriter, _ *http.Request) {
				if n == 1 {
					_, _ = w.Write([]byte(readEnvChat()))
					return
				}
				mu.Lock()
				seen = toolObservations(body)
				mu.Unlock()
				_, _ = w.Write([]byte(doneChat("read it")))
			}
			srv := fake.server(t)
			defer srv.Close()
			p := writeDoorPipeline(t, srv.URL, true)
			p.cfg.AuditAllDoors, p.cfg.AgentReadFloor = "warn", tc.floor
			c := writeContract(".")
			c.Context = append(c.Context, core.ContextDoc{Name: ".env", Text: "TOKEN=abc123\n"})
			wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, c)))
			if wire.Deferred {
				t.Fatalf("deferred: %s", wire.Reason)
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Contains(seen, "abc123") == tc.refused {
				t.Errorf("the secret reached the seat = %v, want %v (tool results: %q)", strings.Contains(seen, "abc123"), !tc.refused, seen)
			}
			if tc.refused && !strings.Contains(seen, "read refused") {
				t.Errorf("the seat was not told why the read was refused: %q", seen)
			}
			found := false
			for _, row := range readAuditRows(t, home) {
				if row["kind"] == "read" && row["path"] == ".env" && row["decision"] == tc.decision {
					found = true
				}
			}
			if !found {
				t.Errorf("no %q read row for .env on the trail", tc.decision)
			}
		})
	}
}

func readAuditRows(t *testing.T, home string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	b, err := os.ReadFile(home + "/.local-offload/agent-audit.jsonl")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, m)
	}
	return rows
}
