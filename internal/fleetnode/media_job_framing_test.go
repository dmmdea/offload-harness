package fleetnode

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// What the media-job door answers for a body whose length on the wire is not the one it declared. The
// docs said a body "longer than it declared" is cut off at the cap and answered 413; over a socket that
// never happens, because net/http hands a handler exactly Content-Length bytes and reads whatever the
// client sent past them as its next request. These two tests pin what does happen, so the docs cannot say
// otherwise again.

// A client that declares 4 bytes and sends 10 has the 4 judged as its body (here: not a media-job body, a
// 400), not a 413. The excess is the server's business as the start of a next request, never the handler's.
func TestAMediaJobBodyLongerThanItDeclaredIsJudgedAtItsDeclaredLength(t *testing.T) {
	s, _ := newTestServer(t, mediaJobCfg(t), &inputRunner{}, nil)
	addr, _ := serveCounting(t, s)
	conn := dialDeclared(t, addr, 4, 10, true)
	resp := readAnswer(t, conn, bufio.NewReader(conn), 3*time.Second)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "malformed media-job body") {
		t.Fatalf("status %d (%s), want the 4 declared bytes judged as the body (400 malformed media-job body), not a 413", resp.StatusCode, b)
	}
}

// A body with NO declared length (chunked) is the case the reader's cap is for: it is cut off at the cap and
// answered 413, and the slot it held is free again.
func TestAChunkedMediaJobBodyOverTheCapIsCutOffAtTheCapWith413(t *testing.T) {
	cfg := mediaJobCfg(t)
	cfg.FleetMediaInputsMaxMB = 1
	limit := MediaJobBodyCap(cfg)
	s, _ := newTestServer(t, cfg, &inputRunner{}, nil)
	addr, _ := serveCounting(t, s)
	// A reader of unknown length makes the client send Transfer-Encoding: chunked (ContentLength -1 server side).
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+MediaJobPath, io.MultiReader(bytes.NewReader(bytes.Repeat([]byte("x"), int(limit)+1024))))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("chunked upload over the cap: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d (%s), want 413 for a chunked body past the cap", resp.StatusCode, b)
	}
	if n := len(s.mediaJobSlots); n != 0 {
		t.Errorf("%d slot(s) held after the refusal", n)
	}
}
