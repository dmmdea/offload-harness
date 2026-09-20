package fleetnode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The job record carries the run's liveness report while it runs, and the
// poll body publishes it beside wall_sec with the two bounds twinned at the
// top level (0.131.0).
func TestJobViewCarriesProgressWhileRunning(t *testing.T) {
	j := NewJobs(time.Hour, 4)
	id := "job-lv"
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	if !j.AcceptAgent(id, func(ctx context.Context) (json.RawMessage, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return json.RawMessage(`{}`), nil
	}) {
		t.Fatal("not accepted")
	}
	<-started
	if v, _ := j.Get(id); v.Progress != nil {
		t.Fatal("no report yet: progress must be nil")
	}
	p := core.LiveProgress{Step: 3, TokensOut: 949, TokS: 1.2, Phase: "decoding", LastProgressMs: 1789938000000, AllowanceMs: 60000, CeilingSec: 2700}
	j.SetProgress(id, p)
	v, ok := j.Get(id)
	if !ok || v.Progress == nil || *v.Progress != p {
		t.Fatalf("view = %+v", v)
	}
	// the copy is the caller's, not the record's
	p.TokensOut = 1000
	if v2, _ := j.Get(id); v2.Progress.TokensOut != 949 {
		t.Fatal("SetProgress must copy")
	}

	rec := httptest.NewRecorder()
	writeJobView(rec, http.StatusOK, v)
	var body struct {
		State             string             `json:"state"`
		Progress          *core.LiveProgress `json:"progress"`
		StallAllowanceSec int                `json:"stall_allowance_sec"`
		CeilingSec        int                `json:"ceiling_sec"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.State != string(JobRunning) || body.Progress == nil || body.Progress.TokensOut != 949 || body.StallAllowanceSec != 60 || body.CeilingSec != 2700 {
		t.Fatalf("poll body = %s", rec.Body.String())
	}
}

func TestSetProgressIgnoresATerminalJob(t *testing.T) {
	j := NewJobs(time.Hour, 4)
	id := "job-done"
	if !j.AcceptAgent(id, func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }) {
		t.Fatal("not accepted")
	}
	if v, still := j.WaitTerminal(context.Background(), id, 5*time.Second); !still || !Terminal(v.State) {
		t.Fatalf("job did not finish: %+v", v)
	}
	j.SetProgress(id, core.LiveProgress{TokensOut: 1})
	if v, _ := j.Get(id); v.Progress != nil {
		t.Fatal("a terminal job must not take a late report")
	}
	// a poll body without a report carries no progress keys at all
	rec := httptest.NewRecorder()
	v, _ := j.Get(id)
	writeJobView(rec, http.StatusOK, v)
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	for _, k := range []string{"progress", "stall_allowance_sec", "ceiling_sec"} {
		if _, present := raw[k]; present {
			t.Fatalf("%q must be omitted when nothing was reported: %s", k, rec.Body.String())
		}
	}
}
