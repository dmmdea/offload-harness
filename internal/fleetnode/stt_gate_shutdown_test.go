// A node that shuts down while a job waits at the stt gate (D18) must leave nothing half-open: the
// waiter's PAIR card closes failed with the reason, and a PULLED waiter is handed back to the queue
// holder with a nack. Both branches sit between the gate error and the early return, where a deleted
// line leaves every other stt test green.
package fleetnode

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetqueue"
	"github.com/dmmdea/offload-harness/internal/netguard"
)

const sttShutdownCardReason = "the node shut down while this job waited for the stt slot"

// cardFramesOf returns one job's card frames by state, waiting for the emitter first.
func (pn *pairNode) cardFramesOf(jobID string) map[string]map[string]any {
	pn.e.Wait()
	pn.mu.Lock()
	defer pn.mu.Unlock()
	out := map[string]map[string]any{}
	for _, f := range pn.frames {
		wi := f["params"].(map[string]any)["workloadInfo"].(map[string]any)
		if wi["id"] == jobID {
			out[wi["state"].(string)] = wi
		}
	}
	return out
}

// Pushed: job one holds the only stt slot, job two waits behind it. The drain cancels both; job two
// never reaches the pipeline and its card ends queued -> failed with the shutdown reason.
func TestSTTUploadWaiterShutDownAtTheGateClosesItsCardFailed(t *testing.T) {
	pn := newPairNode(t, true)
	cfg := sttCfg(t, "")
	r := &sttRunner{res: sttOK, gate: make(chan struct{}), entered: make(chan string, 4)}
	s, jobs := newTestServer(t, cfg, r, pairOpts(pn))
	uploadSTT(t, s, "stt-hold", askerHeaders)
	<-r.entered
	uploadSTT(t, s, "stt-wait", askerHeaders)
	sttWaitFor(t, "the second upload waiting at the gate", func() bool { return s.sttGate.waiting() == 1 })

	jobs.DrainAndStop(200 * time.Millisecond)

	card := pn.cardFramesOf("stt-wait")
	if card["queued"] == nil || card["running"] != nil || card["completed"] != nil {
		t.Fatalf("the waiter's card = %v, want queued then failed, never running", card)
	}
	if card["failed"] == nil || card["failed"]["error"] != sttShutdownCardReason {
		t.Fatalf("the waiter's card = %v, want it closed failed with %q", card, sttShutdownCardReason)
	}
	if _, _, _, _, started := r.snapshot(); len(started) != 1 {
		t.Fatalf("runs started = %v: the waiter must never have reached the pipeline", started)
	}
}

// Pulled: the same shutdown on a claimed job. Its card closes failed and the holder is told to
// requeue it at once (a nack) instead of waiting out the lease.
func TestPulledSTTWaiterShutDownAtTheGateIsNackedAndItsCardClosed(t *testing.T) {
	pn := newPairNode(t, true)
	cfg := sttCfg(t, "tok")
	r := &sttRunner{res: sttOK, gate: make(chan struct{}), entered: make(chan string, 4)}
	s, jobs := newTestServer(t, cfg, r, pairOpts(pn))

	var mu sync.Mutex
	next := 0
	var nacks []map[string]any
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/fleet/queue/claim":
			mu.Lock()
			next++
			n := next
			mu.Unlock()
			if n > 2 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode(fleetqueue.Job{
				ID: "pulled-" + string(rune('0'+n)), TaskType: "stt", Asker: "node-q", PairCard: core.PairCardNode,
				Payload: json.RawMessage(`{"audio":"/node/local/a.wav"}`),
			})
		case strings.HasSuffix(req.URL.Path, "/nack"):
			var m map[string]any
			_ = json.NewDecoder(req.Body).Decode(&m)
			mu.Lock()
			nacks = append(nacks, m)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer holder.Close()
	client := &http.Client{Timeout: 5 * time.Second, Transport: netguard.SafeTransport(nil)}
	// Claim one at a time: the run goroutine of a claimed job starts after claimOne returns, so two
	// back-to-back claims could reach the gate in either order. The first must hold the slot before
	// the second is claimed, which makes pulled-2 the waiter the shutdown then nacks.
	if _, ok := s.claimOne(context.Background(), client, holder.URL, "testnode", cfg); !ok {
		t.Fatal("claimOne #1 did not claim")
	}
	if got := <-r.entered; got != "pulled-1" {
		t.Fatalf("first run = %s, want pulled-1", got)
	}
	if _, ok := s.claimOne(context.Background(), client, holder.URL, "testnode", cfg); !ok {
		t.Fatal("claimOne #2 did not claim")
	}
	sttWaitFor(t, "the second pulled job waiting at the gate", func() bool { return s.sttGate.waiting() == 1 })

	jobs.DrainAndStop(200 * time.Millisecond)

	sttWaitFor(t, "the waiter's nack", func() bool { mu.Lock(); defer mu.Unlock(); return len(nacks) >= 1 })
	mu.Lock()
	got := nacks[0]
	mu.Unlock()
	if got["job_id"] != "pulled-2" {
		t.Fatalf("nacked %v, want the waiting job pulled-2", got["job_id"])
	}
	card := pn.cardFramesOf("pulled-2")
	if card["failed"] == nil || card["failed"]["error"] != sttShutdownCardReason || card["running"] != nil {
		t.Fatalf("the pulled waiter's card = %v, want queued then failed with %q", card, sttShutdownCardReason)
	}
}

// Four uploads at the node, one slot (D18), and a cap of one text execution slot: the three that
// run or wait at the stt gate hold no fleet slot, so a capped job still starts at once and health
// still reports the slot idle.
func TestSTTUploadsWaitingAtTheGateDoNotHoldAFleetSlot(t *testing.T) {
	cfg := sttCfg(t, "")
	cfg.FleetMaxConcurrentJobs = 1
	cfg.FleetMaxQueueDepth = -1
	r := &sttRunner{res: sttOK, gate: make(chan struct{}), entered: make(chan string, 8)}
	s, jobs := newTestServer(t, cfg, r, authOpts(true))
	for _, id := range []string{"stt-s1", "stt-s2", "stt-s3", "stt-s4"} {
		uploadSTT(t, s, id, nil)
	}
	<-r.entered
	sttWaitFor(t, "three uploads waiting at the gate", func() bool { return s.sttGate.waiting() == 3 })

	if h := healthOf(t, s); h.Saturation == nil || !h.Saturation.IdleSlot {
		t.Fatalf("idle_slot = %+v with only stt uploads in flight: they must not count against fleet_max_concurrent_jobs", h.Saturation)
	}
	finish := blockingJob(t, jobs, "agent-1", AcceptSpec{Task: "agent"}) // fails the test if it never starts
	finish()
	close(r.gate)
}

// Resource bound (review): an upload's body is read and decoded before the admission gates, so the
// node holds at most sttUploadInFlightMax of them at once. A caller over the bound waits for a slot
// and is served when one frees; it is not refused while a burst drains.
func TestSTTUploadsInFlightAreBoundedAndWaitForASlot(t *testing.T) {
	cfg := sttCfg(t, "")
	s, _ := newTestServer(t, cfg, &sttRunner{res: sttOK}, authOpts(true))
	for i := 0; i < sttUploadInFlightMax; i++ {
		s.sttUploadSlots <- struct{}{}
	}
	done := make(chan int, 1)
	go func() {
		done <- do(t, s, http.MethodPost, STTUploadPath, sttBody("stt-slot", []byte("OggS"), nil), nil).Code
	}()
	select {
	case code := <-done:
		t.Fatalf("an upload with every slot taken was answered %d at once: the in-flight bound is not applied", code)
	case <-time.After(200 * time.Millisecond):
	}
	<-s.sttUploadSlots // one upload finishes
	select {
	case code := <-done:
		if code != http.StatusAccepted {
			t.Fatalf("the waiting upload got %d, want 202 once a slot freed", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting upload was never served after a slot freed")
	}
	// A request that completes gives its slot back: one more goes straight through.
	<-s.sttUploadSlots
	if rec := do(t, s, http.MethodPost, STTUploadPath, sttBody("stt-slot-2", []byte("OggS"), nil), nil); rec.Code != http.StatusAccepted {
		t.Fatalf("upload after the slots freed: %d (%s)", rec.Code, rec.Body.String())
	}
	if n := len(s.sttUploadSlots); n != 0 {
		t.Fatalf("%d upload slot(s) still held after every request returned", n)
	}
}

// A caller that waits past the slot wait gets the re-placeable 503, with Retry-After.
func TestSTTUploadSlotWaitEndsInARePlaceable503(t *testing.T) {
	old := sttUploadSlotWait
	sttUploadSlotWait = 50 * time.Millisecond
	defer func() { sttUploadSlotWait = old }()
	s, _ := newTestServer(t, sttCfg(t, ""), &sttRunner{res: sttOK}, authOpts(true))
	for i := 0; i < sttUploadInFlightMax; i++ {
		s.sttUploadSlots <- struct{}{}
	}
	rec := do(t, s, http.MethodPost, STTUploadPath, sttBody("stt-late", []byte("OggS"), nil), nil)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d, Retry-After %q (body %s), want a 503 the asker can re-place", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	if _, known := s.jobs.Get("stt-late"); known {
		t.Fatal("a refused upload was admitted")
	}
}

// The 503 must reach a REAL client: net/http arms the blanket write timeout at header-read, so a slot
// wait that outlasts it would be answered with a bare connection reset. The recorder test above cannot
// see that; this one serves the node behind an http.Server whose blanket is shorter than the wait.
func TestSTTUploadSlotWaitAnswerOutlivesTheBlanketWriteTimeout(t *testing.T) {
	old := sttUploadSlotWait
	sttUploadSlotWait = 600 * time.Millisecond
	defer func() { sttUploadSlotWait = old }()
	s, _ := newTestServer(t, sttCfg(t, ""), &sttRunner{res: sttOK}, authOpts(true))
	for i := 0; i < sttUploadInFlightMax; i++ {
		s.sttUploadSlots <- struct{}{}
	}
	base := serveOn(t, s, 200*time.Millisecond)
	resp, err := http.Post(base+STTUploadPath, "application/json", strings.NewReader(sttBody("stt-late", []byte("OggS"), nil)))
	if err != nil {
		t.Fatalf("the slot-wait 503 never reached the caller (cut at the blanket write timeout): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d, Retry-After %q, want a re-placeable 503", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

// The decode never copies the audio: parsing an 8 MiB base64 body allocates a small fraction of it,
// and the audio field aliases the body. Two decoded copies of an 85 MiB body were the peak the
// review measured; this holds the line at none.
func TestParseSTTUploadDoesNotCopyTheAudio(t *testing.T) {
	body := []byte(sttBody("stt-big", make([]byte, 6<<20), nil))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	p, err := parseSTTUpload(body)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 1<<20 {
		t.Fatalf("parsing a %d byte body allocated %d bytes: the audio is being copied", len(body), grown)
	}
	if len(p.AudioB64) < 6<<20 || &p.AudioB64[0] != &body[bytes.Index(body, []byte(`"audio_b64":"`))+len(`"audio_b64":"`)] {
		t.Fatal("the audio field does not alias the body")
	}
	// Escapes still decode (an encoder that writes \/ for /).
	q, err := parseSTTUpload([]byte(`{"job_id":"e","audio_b64":"ab\/d"}`))
	if err != nil || string(q.AudioB64) != "ab/d" {
		t.Fatalf("escaped value = %q, %v", q.AudioB64, err)
	}
	if _, err := parseSTTUpload([]byte(`{"job_id":"e","audio_b64":"x","path":"/p"}`)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field err = %v", err)
	}
	if _, err := parseSTTUpload([]byte(`{"job_id":"e","audio_b64":7}`)); err == nil {
		t.Fatal("a non-string audio_b64 was accepted")
	}
}
