package fleetnode

// The jobs feed shows a failed job's error text only to a reader who holds the bearer. That text can carry a path on the
// node's disk, so a media job (which is not an agent run) has to be masked on the feed exactly as an agent run is. The
// job here goes through the real dispatch door, so it is admitted with the marker the feed keys on (JobView.Gated), the
// way a media job is admitted on a running node; a job handed straight to Jobs.Admit carries no such marker.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

func TestTheJobsFeedMasksAFailedMediaJobsErrorFromAReaderWithoutTheBearer(t *testing.T) {
	const nodePath = "/srv/node-render/work/feed-media-1/frame-0007.png"
	r := &fakeRunner{fn: func(context.Context, core.Request) core.Result {
		return core.Result{OK: false, Reason: "render failed writing " + nodePath}
	}}
	s, _ := newTestServer(t, tokenCfg(holderKey), r, authOpts(false))
	hold := map[string]string{"Authorization": "Bearer " + holderKey}

	rec := do(t, s, http.MethodPost, "/fleet/dispatch", `{"job_id":"feed-media-1","task_type":"image-gen","payload":{"prompt":"hi"}}`, hold)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if m := pollDone(t, s, "feed-media-1", hold); m["state"] != "error" {
		t.Fatalf("the job did not fail: %v", m)
	}

	feedRow := func(hdr map[string]string) (jobFeedRow, string) {
		t.Helper()
		rr := do(t, s, http.MethodGet, "/fleet/jobs?limit=10", "", hdr)
		if rr.Code != http.StatusOK {
			t.Fatalf("feed status = %d (body %s)", rr.Code, rr.Body.String())
		}
		var body struct {
			Jobs []jobFeedRow `json:"jobs"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode jobs feed: %v (body %s)", err, rr.Body.String())
		}
		for _, row := range body.Jobs {
			if row.ID == "feed-media-1" {
				return row, rr.Body.String()
			}
		}
		t.Fatalf("the feed has no row for the job: %s", rr.Body.String())
		return jobFeedRow{}, ""
	}

	for name, hdr := range map[string]map[string]string{
		"no bearer":    nil,
		"wrong bearer": {"Authorization": "Bearer " + wrongKey},
	} {
		row, raw := feedRow(hdr)
		if row.Error != "" || strings.Contains(raw, nodePath) || strings.Contains(raw, "/srv/") {
			t.Errorf("%s: the feed shows a media job's error text: row.Error = %q, body = %s", name, row.Error, raw)
		}
		if row.Task != "image-gen" || row.State != "error" || row.Agent {
			t.Errorf("%s: the row's metadata changed: %+v", name, row)
		}
	}

	row, _ := feedRow(hold)
	if !strings.Contains(row.Error, nodePath) {
		t.Errorf("a reader with the bearer should see the whole error text: row.Error = %q", row.Error)
	}
}

// A node with no token on a loopback listener keeps its feed open: the mask is for a node that has a key.
func TestTheJobsFeedOfALoopbackNodeWithNoTokenShowsAFailedMediaJobsError(t *testing.T) {
	r := &fakeRunner{fn: func(context.Context, core.Request) core.Result {
		return core.Result{OK: false, Reason: "render failed on the loopback node"}
	}}
	s, _ := newTestServer(t, tokenCfg(""), r, authOpts(true))
	if rec := do(t, s, http.MethodPost, "/fleet/dispatch", `{"job_id":"feed-open-1","task_type":"image-gen","payload":{"prompt":"hi"}}`, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch status = %d (body %s)", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rr := do(t, s, http.MethodGet, "/fleet/jobs?limit=10", "", nil)
		if strings.Contains(rr.Body.String(), "render failed on the loopback node") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a tokenless loopback node's feed never showed the job's error: %s", rr.Body.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
