package fleetnode

// A job's state, result data and error text go to a reader who holds the bearer: the poll answers 401 to anyone else and
// the jobs feed shows such a row without its error. What marks a job for that is JobView.Gated, set by gatedJob from the
// task type, and the only task type left out is an agent run, which has its own marker (JobView.Agent). If one more task
// type were left out, a reader without the bearer could read that job's output paths and error text, and nothing else
// would notice: tokenGated is pinned for every type (bearer_every_lane_test.go), and the feed test dispatches image-gen
// only. So the rule is held here for every task type, and over the wire for a run-graph job.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

func TestEveryTaskTypeButAnAgentRunIsMaskedFromAReaderWithoutTheBearer(t *testing.T) {
	agent := string(core.TaskAgentRun)
	types := append([]string{agent, VisionTask, TextTask, ForecastTask, STTUploadTask, ComposeProjectTask, MediaJobTask}, everyTaskType...)
	for _, token := range []string{"", "s3cret"} {
		for _, tt := range types {
			want := tt != agent
			if got := gatedJob(config.Config{FleetAuthToken: token}, tt); got != want {
				t.Errorf("gatedJob(token %q, %q) = %v, want %v", token, tt, got, want)
			}
		}
	}
}

func TestAFailedRunGraphJobIsHiddenFromAReaderWithoutTheBearerOnThePollAndTheFeed(t *testing.T) {
	const nodePath = "/srv/node-render/out/rg-mask-1/a.png"
	r := &fakeRunner{fn: func(context.Context, core.Request) core.Result {
		return core.Result{OK: false, Reason: "graph failed writing " + nodePath}
	}}
	s, _ := newTestServer(t, tokenCfg(holderKey), r, authOpts(false))
	hold := map[string]string{"Authorization": "Bearer " + holderKey}
	body := `{"job_id":"rg-mask-1","task_type":"run-graph","payload":{"graph":` + plainGraph + `}}`
	if rec := do(t, s, http.MethodPost, "/fleet/dispatch", body, hold); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if m := pollDone(t, s, "rg-mask-1", hold); m["state"] != "error" {
		t.Fatalf("the job did not fail: %v", m)
	}
	for name, hdr := range map[string]map[string]string{"no bearer": nil, "wrong bearer": {"Authorization": "Bearer " + wrongKey}} {
		rec := do(t, s, http.MethodGet, "/fleet/jobs/rg-mask-1", "", hdr)
		if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), nodePath) {
			t.Errorf("%s: poll of a run-graph job = %d, want 401 and no error text (body %s)", name, rec.Code, rec.Body.String())
		}
		feed := do(t, s, http.MethodGet, "/fleet/jobs?limit=10", "", hdr)
		var out struct {
			Jobs []jobFeedRow `json:"jobs"`
		}
		if feed.Code != http.StatusOK || json.Unmarshal(feed.Body.Bytes(), &out) != nil {
			t.Fatalf("%s: feed status = %d (body %s)", name, feed.Code, feed.Body.String())
		}
		seen := false
		for _, row := range out.Jobs {
			if row.ID == "rg-mask-1" {
				seen = true
				if row.Error != "" || strings.Contains(feed.Body.String(), nodePath) {
					t.Errorf("%s: the feed shows a run-graph job's error text: %+v", name, row)
				}
			}
		}
		if !seen {
			t.Errorf("%s: the feed has no row for the job: %s", name, feed.Body.String())
		}
	}
	if m := decodeMap(t, do(t, s, http.MethodGet, "/fleet/jobs/rg-mask-1", "", hold)); !strings.Contains(m["error"].(string), nodePath) {
		t.Errorf("a reader with the bearer should see the whole error text: %v", m)
	}
}
