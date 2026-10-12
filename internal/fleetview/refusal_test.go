package fleetview

// The errors feed and the delegation table are what an operator reads to decide whether the harness is failing.
// A node that answers a dispatch 503 "queue full" is a place in line: the delegator outwaits it and the subtask
// lands elsewhere. Its corpus row carries a `refusal` marker (internal/delegate delegationLogLine), and the view
// must not turn it into a "delegation" error: thirteen of those in a day read as thirteen failures of a fleet
// whose subtasks all landed.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

func writeShard(t *testing.T, home string, lines ...string) {
	t.Helper()
	dir := filepath.Join(home, "delegation-log")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, time.Now().Format("2006-01-02")+".jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestARefusedAttemptIsNotADelegationError(t *testing.T) {
	home := t.TempDir()
	now := time.Now().Unix()
	row := func(id, node, extra string) string {
		return `{"ts":` + strconv.FormatInt(now, 10) + `,"job_id":"` + id + `","node":"` + node + `","seat":"s","contract":{"goal":"secret goal"},` + extra + `}`
	}
	writeShard(t, home,
		row("agd-refused", "n1", `"deferred":false,"acceptance_pass":false,"error":"dispatch http://n1/fleet/dispatch: status 503: queue full","refusal":{"status":503,"retry_after_sec":9}`),
		row("agd-lost", "n2", `"deferred":true,"defer_class":"capacity","acceptance_pass":false,"placement_reason":"capacity wait: no node had room"`),
		row("agd-ok", "n3", `"deferred":false,"acceptance_pass":true`),
	)
	p := NewPoller(config.Config{Home: home}, nil, time.Hour, 3)
	p.foldDelegationLog()
	ov := p.Snapshot()

	var ids []string
	for _, e := range ov.Errors {
		ids = append(ids, e.Node+":"+e.Message)
		if strings.Contains(e.Message, "queue full") {
			t.Errorf("the errors feed carries the refused attempt as an error: %+v", e)
		}
	}
	if len(ov.Errors) != 1 || !strings.Contains(ov.Errors[0].Message, "capacity") {
		t.Fatalf("errors = %v, want exactly the lost subtask (the refusal is a place in line)", ids)
	}

	// The row stays in the table, marked, so the page can show it as the retry it was.
	var found bool
	for _, d := range ov.Delegations {
		if d["job_id"] == "agd-refused" {
			found = true
			if _, ok := d["refusal"]; !ok {
				t.Errorf("the table row lost its refusal marker: %v", d)
			}
		}
		if _, leaked := d["contract"]; leaked {
			t.Errorf("a table row carries the contract: %v", d)
		}
	}
	if !found {
		t.Fatalf("the refused attempt is missing from the delegation table: %v", ov.Delegations)
	}
}
