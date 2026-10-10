package fleetnode

// The class a failed run's lane filed its deferral under rides the job's poll beside the reason, as
// `err_class`. The box that asked closes the call's PAIR card on it, quiet for a call another job held
// back (gpu_busy, gpu_queued) and failed for one that ran and broke (pairworkloads.CardOutcome), and it
// can only tell them apart by the class: the reason is a sentence for a person. The media remote lane
// used to rebuild the deferral from `error` alone, so a held card read as a failed one on the asker's
// dashboard, the reading the pair-close fix of 2026-10-09 exists to remove.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

func TestAFailedJobPublishesTheErrClassItsLaneFiledBesideTheReason(t *testing.T) {
	for _, class := range []string{"gpu_busy", "gpu_queued", "timeout"} {
		t.Run(class, func(t *testing.T) {
			reason := "the lane said " + class
			fr := &fakeRunner{fn: func(context.Context, core.Request) core.Result {
				return core.Deferf(reason, "", core.Meta{ErrClass: class})
			}}
			s, _ := newTestServer(t, imageCfg(), fr, nil)
			id := "ec-" + class
			do(t, s, http.MethodPost, "/fleet/dispatch", `{"job_id":"`+id+`","task_type":"image-gen","payload":{"prompt":"hi"}}`, nil)
			m := pollJob(t, s, id, JobError)
			if m["error"] != reason || m["err_class"] != class {
				t.Fatalf("poll = %v, want error %q and err_class %q", m, reason, class)
			}
			if _, present := m["data"]; present {
				t.Fatalf("a failed job carries no data: %v", m)
			}
		})
	}
}

// Nothing else changes on the wire: a failure whose lane filed no class (the common case, and every
// failure of a node too old to know the field), and a job that succeeded, publish exactly the keys they
// always did, so an asker that does not read err_class cannot tell the difference.
func TestAJobWithNoClassPublishesTheSameKeysAsBefore(t *testing.T) {
	keys := func(m map[string]any) []string {
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return ks
	}
	fr := &fakeRunner{fn: func(_ context.Context, req core.Request) core.Result {
		if req.Input == "fail" {
			return core.Deferf("no image-gen route configured", "", core.Meta{})
		}
		return core.Result{OK: true, Data: json.RawMessage(`{"image_path":"x.png"}`)}
	}}
	s, _ := newTestServer(t, imageCfg(), fr, nil)
	do(t, s, http.MethodPost, "/fleet/dispatch", `{"job_id":"nc-fail","task_type":"image-gen","payload":{"prompt":"fail"}}`, nil)
	do(t, s, http.MethodPost, "/fleet/dispatch", `{"job_id":"nc-ok","task_type":"image-gen","payload":{"prompt":"ok"}}`, nil)
	failed, done := pollJob(t, s, "nc-fail", JobError), pollJob(t, s, "nc-ok", JobDone)
	if got := fmt.Sprint(keys(failed)); got != "[error job_id state]" {
		t.Errorf("an unclassified failure publishes %s, want [error job_id state]: %v", got, failed)
	}
	if got := fmt.Sprint(keys(done)); got != "[data job_id state]" {
		t.Errorf("a job that succeeded publishes %s, want [data job_id state]: %v", got, done)
	}
}

// The store carries the class from the run's error to the job's view whatever wraps it on the way, and
// a plain error (every other failure of a run: a panic's text, a drain, a refused slot) carries none.
func TestTheStoreKeepsTheClassOfAFailedRunAndNothingElse(t *testing.T) {
	jobs := NewJobs(time.Hour, 0)
	t.Cleanup(func() { jobs.DrainAndStop(2 * time.Second) })
	runs := map[string]error{
		"direct":  &classedError{msg: "held", class: "gpu_busy"},
		"wrapped": fmt.Errorf("lane: %w", &classedError{msg: "held", class: "gpu_queued"}),
		"plain":   errors.New("boom"),
	}
	for id, err := range runs {
		err := err
		jobs.Accept(id, func(context.Context) (json.RawMessage, error) { return nil, err })
	}
	want := map[string]string{"direct": "gpu_busy", "wrapped": "gpu_queued", "plain": ""}
	deadline := time.Now().Add(5 * time.Second)
	for id, class := range want {
		for {
			v, ok := jobs.Get(id)
			if ok && Terminal(v.State) {
				if v.State != JobError || v.ErrClass != class {
					t.Errorf("%s: state %s class %q, want error with class %q", id, v.State, v.ErrClass, class)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("job %s never finished", id)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}
