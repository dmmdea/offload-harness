package fleetnode

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Resource bound (ADR 0077, mirroring the stt upload door's): a media-job body is read and decoded before the
// admission gates, so every concurrent upload could hold a bundle-sized body. The node holds at most
// mediaJobInFlightMax of them at once; a caller over the bound waits for a slot and is answered a
// re-placeable 503 when the wait runs out.

func mjStillBody(t *testing.T, id string) string {
	t.Helper()
	return string(mediaPayload("video-gen", `{"prompt":"p"}`, packFiles(t, map[string][]byte{"s.png": mjPNG}),
		map[string]string{"still": "s.png"}, func(p *MediaJobPayload) { p.JobID = id }))
}

func TestMediaJobInFlightMaxIsTheSTTDoorsDefault(t *testing.T) {
	if mediaJobInFlightMax != sttUploadInFlightMax || mediaJobInFlightMax < 1 {
		t.Fatalf("mediaJobInFlightMax = %d, want the stt door's %d", mediaJobInFlightMax, sttUploadInFlightMax)
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
