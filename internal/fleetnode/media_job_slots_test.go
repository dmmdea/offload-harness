package fleetnode

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

// Resource bound (ADR 0077; the same shape as the stt upload door's, with its own bound): a media-job body is
// read and decoded before the admission gates, so every concurrent upload could hold a bundle-sized body. The
// node holds at most mediaJobInFlightMax of them at once (one); a caller over the bound waits for a slot and is
// answered a capacity 503 with Retry-After when the wait runs out.

func mjStillBody(t *testing.T, id string) string {
	t.Helper()
	return string(mediaPayload("video-gen", `{"prompt":"p"}`, packFiles(t, map[string][]byte{"s.png": mjPNG}),
		map[string]string{"still": "s.png"}, func(p *MediaJobPayload) { p.JobID = id }))
}

// The bound was the stt upload door's 2, copied unscaled. A media-job body is far larger than an stt upload's:
// at the 256 MiB default cap one body holds the base64 text (a third larger than the bundle) beside the decoded
// bundle, about 0.58 GiB, so the node-wide peak is slots x that figure. The door takes ONE body at a time, which
// keeps its peak at the "under about 0.6 GiB" the config key and the docs promise (review of 0.172.0, C5S1).
func TestMediaJobDoorHoldsOneBodyAtATimeSoItsPeakIsTheDocumentedFigure(t *testing.T) {
	const gib = int64(1) << 30
	var def config.Config // the defaults: fleet_media_inputs_max_mb unset = 256
	perSlot := MediaJobBodyCap(def) + def.EffectiveMediaInputsMaxBytes()
	if perSlot < gib/2 || perSlot > 6*gib/10 {
		t.Fatalf("one slot holds %d bytes at the default cap, outside the documented 0.5 to 0.6 GiB: the figure in the docs is stale", perSlot)
	}
	if peak := perSlot * mediaJobInFlightMax; peak > 6*gib/10 {
		t.Fatalf("mediaJobInFlightMax = %d puts the door's peak at %.2f GiB at the default cap, over the documented 0.6 GiB (one slot is %.2f GiB)",
			mediaJobInFlightMax, float64(peak)/float64(gib), float64(perSlot)/float64(gib))
	}
	s, _ := newTestServer(t, mediaJobCfg(t), &inputRunner{}, nil)
	if cap(s.mediaJobSlots) != mediaJobInFlightMax || mediaJobInFlightMax != 1 {
		t.Fatalf("the server holds %d slot(s), mediaJobInFlightMax = %d, want one", cap(s.mediaJobSlots), mediaJobInFlightMax)
	}
	if mediaJobInFlightMax >= sttUploadInFlightMax {
		t.Errorf("the media-job door (%d) must hold fewer bodies than the stt upload door (%d): its bodies are several times larger", mediaJobInFlightMax, sttUploadInFlightMax)
	}
}

// Behaviour, not the constant: while one upload's body is still arriving, a second token holder's request is
// not read; it waits for the slot and is served when the first is done.
func TestASecondMediaJobWaitsWhileTheFirstBodyIsStillArriving(t *testing.T) {
	s, _ := newTestServer(t, mediaJobCfg(t), &inputRunner{}, nil)
	base := serveOn(t, s, 30*time.Second)
	first, second := mjStillBody(t, "mj-first"), mjStillBody(t, "mj-second")
	half := len(first) / 2

	pr, pw := io.Pipe()
	req1, err := http.NewRequest(http.MethodPost, base+MediaJobPath, pr)
	if err != nil {
		t.Fatal(err)
	}
	req1.ContentLength = int64(len(first))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer tok")
	code := func(req *http.Request) <-chan int {
		ch := make(chan int, 1)
		go func() {
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				ch <- -1
				return
			}
			resp.Body.Close()
			ch <- resp.StatusCode
		}()
		return ch
	}
	done1 := code(req1)
	go func() { _, _ = io.WriteString(pw, first[:half]) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(s.mediaJobSlots) != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(s.mediaJobSlots) != 1 {
		t.Fatal("the first upload never took the slot")
	}

	req2, err := http.NewRequest(http.MethodPost, base+MediaJobPath, strings.NewReader(second))
	if err != nil {
		t.Fatal(err)
	}
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer tok")
	done2 := code(req2)
	select {
	case c := <-done2:
		t.Fatalf("a second upload was answered %d while the first body was still arriving: the node holds more than one body at once", c)
	case <-time.After(300 * time.Millisecond):
	}
	if _, known := s.jobs.Get("mj-second"); known {
		t.Fatal("the second job was admitted while the first body was still arriving")
	}

	if _, err := io.WriteString(pw, first[half:]); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	for i, ch := range []<-chan int{done1, done2} {
		select {
		case c := <-ch:
			if c != http.StatusAccepted {
				t.Errorf("upload %d answered %d, want 202", i+1, c)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("upload %d was never answered", i+1)
		}
	}
}

// The slot is taken before the first body byte and released only after the request is answered: a body
// that is still arriving holds it, and it is back when the job is admitted.
func TestMediaJobSlotIsHeldWhileTheBodyIsReadAndReleasedAfter(t *testing.T) {
	s, _ := newTestServer(t, mediaJobCfg(t), &inputRunner{}, nil)
	base := serveOn(t, s, 30*time.Second)
	body := mjStillBody(t, "mj-slot-held")
	half := len(body) / 2
	pr, pw := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, base+MediaJobPath, pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	type answer struct {
		code int
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- answer{err: err}
			return
		}
		resp.Body.Close()
		done <- answer{code: resp.StatusCode}
	}()
	go func() { _, _ = io.WriteString(pw, body[:half]) }()

	deadline := time.Now().Add(5 * time.Second)
	for len(s.mediaJobSlots) != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(s.mediaJobSlots); n != 1 {
		t.Fatalf("slots held while the body is half sent = %d, want 1", n)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(s.mediaJobSlots); n != 1 {
		t.Fatalf("slots held while the body is still arriving = %d, want 1", n)
	}
	if _, err := io.WriteString(pw, body[half:]); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	select {
	case a := <-done:
		if a.err != nil || a.code != http.StatusAccepted {
			t.Fatalf("answer = %d, %v, want 202", a.code, a.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the upload was never answered")
	}
	// The handler returns right after it answers; give its deferred release a moment.
	for len(s.mediaJobSlots) != 0 && time.Now().Before(deadline.Add(10*time.Second)) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(s.mediaJobSlots); n != 0 {
		t.Fatalf("%d slot(s) still held after the job was admitted", n)
	}
}

// A caller over the bound waits for a slot and is served when one frees; it is not refused while a burst
// drains.
func TestMediaJobsInFlightAreBoundedAndWaitForASlot(t *testing.T) {
	s, _ := newTestServer(t, mediaJobCfg(t), &inputRunner{}, nil)
	for i := 0; i < mediaJobInFlightMax; i++ {
		s.mediaJobSlots <- struct{}{}
	}
	done := make(chan int, 1)
	go func() {
		done <- do(t, s, http.MethodPost, MediaJobPath, mjStillBody(t, "mj-wait"), bearer()).Code
	}()
	select {
	case code := <-done:
		t.Fatalf("a media job with every slot taken was answered %d at once: the in-flight bound is not applied", code)
	case <-time.After(200 * time.Millisecond):
	}
	<-s.mediaJobSlots // one upload finishes
	select {
	case code := <-done:
		if code != http.StatusAccepted {
			t.Fatalf("the waiting media job got %d, want 202 once a slot freed", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting media job was never served after a slot freed")
	}
	for i := 0; i < mediaJobInFlightMax-1; i++ {
		<-s.mediaJobSlots
	}
	if n := len(s.mediaJobSlots); n != 0 {
		t.Fatalf("%d slot(s) still held after every request returned", n)
	}
}

// A request over the cap that waits past the slot wait gets the re-placeable 503 with Retry-After (a
// delegator re-places a 503), and is not admitted.
func TestMediaJobOverTheCapGets503WithRetryAfter(t *testing.T) {
	old := mediaJobSlotWait
	mediaJobSlotWait = 50 * time.Millisecond
	defer func() { mediaJobSlotWait = old }()
	s, _ := newTestServer(t, mediaJobCfg(t), &inputRunner{}, nil)
	for i := 0; i < mediaJobInFlightMax; i++ {
		s.mediaJobSlots <- struct{}{}
	}
	rec := do(t, s, http.MethodPost, MediaJobPath, mjStillBody(t, "mj-late"), bearer())
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d, Retry-After %q (body %s), want a 503 the asker can re-place", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "media jobs") {
		t.Errorf("the refusal does not say what is busy: %s", rec.Body.String())
	}
	if _, known := s.jobs.Get("mj-late"); known {
		t.Fatal("a refused media job was admitted")
	}
	// The unauthorized never reach the slot wait: the bearer is checked first.
	if rec := do(t, s, http.MethodPost, MediaJobPath, mjStillBody(t, "mj-anon"), nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no bearer while every slot is taken = %d, want 401", rec.Code)
	}
}

// Every exit of the handler gives its slot back: an admitted job, a malformed body, a refused payload, a
// body over the cap and a wrong content type.
func TestMediaJobSlotIsReleasedOnEveryExit(t *testing.T) {
	cfg := mediaJobCfg(t)
	cfg.FleetMediaInputsMaxMB = 1
	s, _ := newTestServer(t, cfg, &inputRunner{}, nil)
	for name, c := range map[string]struct {
		body string
		hdr  map[string]string
		want int
	}{
		"admitted":      {mjStillBody(t, "mj-ok"), bearer(), http.StatusAccepted},
		"malformed":     {`{"job_id":`, bearer(), http.StatusBadRequest},
		"unknown field": {`{"job_id":"mj-u","task_type":"video-gen","nope":1}`, bearer(), http.StatusBadRequest},
		"oversize":      {`{"job_id":"mj-o","task_type":"video-gen","bundle":"` + strings.Repeat("A", 3<<20) + `"}`, bearer(), http.StatusRequestEntityTooLarge},
		"content type":  {mjStillBody(t, "mj-ct"), map[string]string{"Authorization": "Bearer tok", "Content-Type": "text/plain"}, http.StatusBadRequest},
		"bad task":      {string(mediaPayload("nope", `{}`, nil, nil, func(p *MediaJobPayload) { p.JobID = "mj-bt" })), bearer(), http.StatusBadRequest},
	} {
		if rec := do(t, s, http.MethodPost, MediaJobPath, c.body, c.hdr); rec.Code != c.want {
			t.Errorf("%s: status %d (%s), want %d", name, rec.Code, rec.Body.String(), c.want)
		}
		if n := len(s.mediaJobSlots); n != 0 {
			t.Fatalf("%s: %d slot(s) still held after the request returned", name, n)
		}
	}
}
