package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestRecoverOrphansDoesNotCloseAnIntentOn401: a 401 at recovery is a statement
// about THIS process's credentials (a config without fleet_auth_token, a rotated
// token), not about the job — and closing the intent on it destroyed the only
// record of work the node might still finish. On 2026-09-29 the pass closed 36 of
// 61 intents this way, and none of the 61 was ever recovered. The intent stays
// open, so a process holding the right token can still collect it, and the
// refusal is reported once for the pass, not once per intent.
func TestRecoverOrphansDoesNotCloseAnIntentOn401(t *testing.T) {
	cfg, root := intentCfg(t)
	wire, _ := json.Marshal(map[string]any{"output": "the answer", "schema_version": 1})
	var authorized atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "done", "data": json.RawMessage(wire)})
	}))
	defer srv.Close()

	l := openIntentLedger(cfg)
	for _, id := range []string{"agd-401a", "agd-401b", "agd-401c"} {
		l.dispatched(id, srv.URL, "work the node may still finish")
	}

	logs := captureLog(t)
	n, err := RecoverOrphans(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("recovered = %d, want 0 (every poll was refused)", n)
	}
	open, _, _ := readOpenIntents(filepath.Join(root, "delegate-intent.jsonl"))
	for _, id := range []string{"agd-401a", "agd-401b", "agd-401c"} {
		if _, ok := open[id]; !ok {
			t.Fatalf("%s was closed on a 401: the node may still hold the job, and a process with the right token could have collected it", id)
		}
	}
	if got := strings.Count(logs.String(), "answered 401"); got != 1 {
		t.Fatalf("the 401 was logged %d times for three intents, want exactly once per pass\nlog:\n%s", got, logs.String())
	}

	// Left open MEANS recoverable: once the credentials are right, the next pass
	// files all three.
	authorized.Store(true)
	n, err = RecoverOrphans(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("second pass recovered %d, want all 3 once the node accepts the token", n)
	}
}

// TestIntentEventsCarryTimestampAndPid: every intent event says WHEN and BY WHICH
// PROCESS it was written. The `ok` events used to carry neither, so the ledger
// could not answer which process ran a recovery or when a job was closed — the
// question the 2026-09-29 diagnosis needed and could not read back.
func TestIntentEventsCarryTimestampAndPid(t *testing.T) {
	cfg, root := intentCfg(t)
	l := openIntentLedger(cfg)
	before := time.Now().Add(-2 * time.Second).Unix()

	l.dispatched("agd-ts1", "http://192.0.2.1:18811", "a goal")
	l.done("agd-ts1", "terminal observed")
	l.dispatched("agd-ts2", "http://192.0.2.1:18811", "another goal")
	l.done("agd-ts2", "withdrawn")
	// The recovery pass writes through the same ledger: an expiry it closes is an
	// event like any other.
	l.append(intentEvent{E: "d", Job: "agd-old", Base: "http://192.0.2.1:18811", Goal: "g", TS: time.Now().Add(-3 * 24 * time.Hour).Unix()})
	if _, err := RecoverOrphans(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(root, "delegate-intent.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().Add(2 * time.Second).Unix()
	events := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var ev struct {
			E   string `json:"e"`
			Job string `json:"job"`
			TS  int64  `json:"ts"`
			PID int    `json:"pid"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("intent line is not JSON: %v (%s)", err, line)
		}
		events++
		if ev.Job == "agd-old" && ev.E == "d" {
			continue // the seeded 3-day-old dispatch keeps the timestamp it was given
		}
		if ev.TS < before || ev.TS > after {
			t.Errorf("%s event for %s has ts %d, want a stamp from this run (%d..%d): %s", ev.E, ev.Job, ev.TS, before, after, line)
		}
		if ev.PID != os.Getpid() {
			t.Errorf("%s event for %s has pid %d, want this process %d: %s", ev.E, ev.Job, ev.PID, os.Getpid(), line)
		}
	}
	if events < 6 {
		t.Fatalf("read %d events, want at least 6 (2 dispatches + 2 closes + the old dispatch + its expiry)", events)
	}
}
