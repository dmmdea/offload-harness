package fleetnode

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// The stt upload door has the media-job door's CT-53 defect: it took one of its upload slots first and then
// read the body up to the cap, so a client that declared an oversize Content-Length (8 GiB here) and sent
// 20 bytes held a slot, unanswered, until the 10-minute window ended; two such clients held both slots and
// every real upload queued behind them for the slot wait and got a 503. A declared length over the cap is
// now refused after the bearer check and before the slot, the body unread, with Connection: close. The
// door's status for an upload over its cap is 400 (its body-too-large answer has always been one, and the
// client of this door reads every refusal the same way), so the early refusal keeps that status and wording.

func sttDeclaredServer(t *testing.T) (*Server, string, *countingListener) {
	t.Helper()
	s, _ := newTestServer(t, sttCfg(t, "tok"), &sttRunner{res: sttOK}, authOpts(false))
	addr, cl := serveCounting(t, s)
	return s, addr, cl
}

func TestASTTUploadDeclaredOverTheCapIsAnsweredWithoutReadingItsBody(t *testing.T) {
	s, addr, _ := sttDeclaredServer(t)
	conn := dialDeclaredTo(t, addr, STTUploadPath, declaredOversize, 20, true)
	start := time.Now()
	resp := readAnswer(t, conn, bufio.NewReader(conn), 2*time.Second)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d (%s), want the door's 400 for a body over its cap", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "request body too large") || !strings.Contains(string(body), "fleet_stt_upload_max_mb") {
		t.Errorf("the refusal must say the body is too large and name the cap key: %s", body)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("answered after %s, want at once", d)
	}
	if n := len(s.sttUploadSlots); n != 0 {
		t.Errorf("%d slot(s) still held after the refusal", n)
	}
}

// Both slots are the node's whole upload capacity: a client that only declares a big body must not be able
// to queue for one, and its refusal must not wait behind real uploads.
func TestASTTUploadDeclaredOverTheCapIsAnsweredWhileEverySlotIsHeld(t *testing.T) {
	s, addr, _ := sttDeclaredServer(t)
	for i := 0; i < sttUploadInFlightMax; i++ {
		s.sttUploadSlots <- struct{}{} // uploads mid-body
	}
	conn := dialDeclaredTo(t, addr, STTUploadPath, declaredOversize, 20, true)
	resp := readAnswer(t, conn, bufio.NewReader(conn), 2*time.Second)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d (%s), want the 400 refusal while every slot is held, not a 503 after the slot wait", resp.StatusCode, b)
	}
	if n := len(s.sttUploadSlots); n != sttUploadInFlightMax {
		t.Errorf("%d slot(s) held after the refusal, want the other uploads' %d untouched", n, sttUploadInFlightMax)
	}
	for i := 0; i < sttUploadInFlightMax; i++ {
		<-s.sttUploadSlots
	}
}

// A client that declares a body over the cap and then streams it is not read: the server has taken under
// 64 KiB (the header buffer) when the refusal is out, however fast the client writes.
func TestAStreamedOversizeSTTUploadIsNotDrained(t *testing.T) {
	_, addr, cl := sttDeclaredServer(t)
	conn := dialDeclaredTo(t, addr, STTUploadPath, declaredOversize, 0, true)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		chunk := []byte(strings.Repeat("x", 32<<10))
		for i := 0; i < 4096; i++ { // up to 128 MiB, or until the server shuts the connection
			_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if _, err := conn.Write(chunk); err != nil {
				return
			}
		}
	}()
	resp := readAnswer(t, conn, bufio.NewReader(conn), 3*time.Second)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	time.Sleep(200 * time.Millisecond) // anything the server was still going to read
	if n := cl.read.Load(); n >= 64<<10 {
		t.Errorf("the server read %d bytes of a body it had already refused, want under 64 KiB", n)
	}
	_ = conn.Close()
	wg.Wait()
}

// The refusal ends the connection: the body that was refused is not read, so nothing more can be asked on it.
func TestTheSTTOversizeAnswerClosesTheConnection(t *testing.T) {
	_, addr, _ := sttDeclaredServer(t)
	conn := dialDeclaredTo(t, addr, STTUploadPath, declaredOversize, 20, true)
	br := bufio.NewReader(conn)
	resp := readAnswer(t, conn, br, 2*time.Second)
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("reading the refusal's body: %v", err)
	}
	resp.Body.Close()
	if !resp.Close {
		t.Errorf("the refusal did not carry Connection: close (resp.Close = false)")
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		t.Errorf("after the refusal the connection was not closed by the server: next read = %v, want EOF", err)
	}
}

// Guard: the bearer is still checked first (an unauthorized caller with an oversize declared length gets 401,
// never the refusal that would tell it the cap), and a body of exactly the cap is still read.
func TestASTTUploadDeclaredOversizeStillMeetsTheBearerFirstAndTheCapIsStrict(t *testing.T) {
	_, addr, _ := sttDeclaredServer(t)
	conn := dialDeclaredTo(t, addr, STTUploadPath, declaredOversize, 20, false)
	resp := readAnswer(t, conn, bufio.NewReader(conn), 2*time.Second)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unauthenticated declared-oversize request: status %d, want 401", resp.StatusCode)
	}

	cfg := sttCfg(t, "")
	cfg.FleetSTTUploadMaxMB = 1
	s, _ := newTestServer(t, cfg, &sttRunner{res: sttOK}, authOpts(true))
	limit := STTUploadBodyCap(cfg)
	at := do(t, s, http.MethodPost, STTUploadPath, strings.Repeat("x", int(limit)), nil)
	if at.Code != http.StatusBadRequest || !strings.Contains(at.Body.String(), "malformed stt body") {
		t.Fatalf("a body of exactly the cap (%d bytes): status %d (%s), want it read and refused as malformed", limit, at.Code, at.Body.String())
	}
	over := do(t, s, http.MethodPost, STTUploadPath, strings.Repeat("x", int(limit)+1), nil)
	if over.Code != http.StatusBadRequest || !strings.Contains(over.Body.String(), "request body too large") {
		t.Fatalf("a body one byte over the cap: status %d (%s), want the too-large refusal", over.Code, over.Body.String())
	}
	if n := len(s.sttUploadSlots); n != 0 {
		t.Errorf("%d slot(s) held after both requests returned", n)
	}
}
