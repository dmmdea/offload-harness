package fleetnode

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// CT-53. A media-job request that DECLARES a body over the cap (Content-Length) is refused with 413 at
// once, after the bearer check and before the upload slot, without reading a byte of the body. Before,
// the handler took the one slot first and then drained the body up to the cap before it answered, so a
// client that declared 8 GiB and sent 20 bytes held the node's only media-job slot, and got no answer,
// until the 15-minute window ended (seen live, 2026-10-08).

// countingListener counts the bytes the server reads from every connection it accepts.
type countingListener struct {
	net.Listener
	read atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: c, n: &l.read}, nil
}

type countingConn struct {
	net.Conn
	n *atomic.Int64
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// CloseWrite keeps the server's FIN-then-wait close on a wrapped connection (net/http only does it for a
// connection that has CloseWrite), so the test sees the close a real TCP connection gets.
func (c *countingConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// serveCounting serves s on a loopback listener and returns its address and the server-side read counter.
func serveCounting(t *testing.T, s *Server) (string, *countingListener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cl := &countingListener{Listener: ln}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(cl) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String(), cl
}

// declaredOversize is a Content-Length no cap reaches (8 GiB).
const declaredOversize = int64(8) << 30

// dialDeclared connects and writes the headers of a POST to the media-job door that declares `declared`
// body bytes, then `sent` bytes of body, then nothing.
func dialDeclared(t *testing.T, addr string, declared int64, sent int, auth bool) net.Conn {
	t.Helper()
	return dialDeclaredTo(t, addr, MediaJobPath, declared, sent, auth)
}

// dialDeclaredTo is dialDeclared for any door's path.
func dialDeclaredTo(t *testing.T, addr, path string, declared int64, sent int, auth bool) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var b strings.Builder
	fmt.Fprintf(&b, "POST %s HTTP/1.1\r\nHost: node\r\nContent-Type: application/json\r\nContent-Length: %d\r\n", path, declared)
	if auth {
		b.WriteString("Authorization: Bearer tok\r\n")
	}
	b.WriteString("\r\n")
	b.WriteString(strings.Repeat("x", sent))
	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(conn, b.String()); err != nil {
		t.Fatalf("write request: %v", err)
	}
	return conn
}

// readAnswer reads one response within `within`; it fails the test when none arrives.
func readAnswer(t *testing.T, conn net.Conn, br *bufio.Reader, within time.Duration) *http.Response {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(within))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("no answer within %s: %v", within, err)
	}
	return resp
}

func TestAMediaJobDeclaredOverTheCapIsAnsweredWithoutReadingItsBody(t *testing.T) {
	s, _ := newTestServer(t, mediaJobCfg(t), &inputRunner{}, nil)
	addr, _ := serveCounting(t, s)
	conn := dialDeclared(t, addr, declaredOversize, 20, true)
	start := time.Now()
	resp := readAnswer(t, conn, bufio.NewReader(conn), 2*time.Second)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d (%s), want 413", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "fleet_media_inputs_max_mb") {
		t.Errorf("the refusal must name the cap key: %s", body)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("answered after %s, want at once", d)
	}
	if s.mediaJobSlots != nil && len(s.mediaJobSlots) != 0 {
		t.Errorf("%d slot(s) still held after the refusal", len(s.mediaJobSlots))
	}
}

// The slot is the node's only media-job slot: a client that only declares a big body must not be able to
// queue for it, or hold it, and a refusal must not wait behind a real upload.
func TestAMediaJobDeclaredOverTheCapIsAnsweredWhileAnotherUploadHoldsTheSlot(t *testing.T) {
	s, _ := newTestServer(t, mediaJobCfg(t), &inputRunner{}, nil)
	addr, _ := serveCounting(t, s)
	for i := 0; i < mediaJobInFlightMax; i++ {
		s.mediaJobSlots <- struct{}{} // another upload is mid-body
	}
	conn := dialDeclared(t, addr, declaredOversize, 20, true)
	resp := readAnswer(t, conn, bufio.NewReader(conn), 2*time.Second)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d (%s), want 413 while another upload holds the slot", resp.StatusCode, b)
	}
	if n := len(s.mediaJobSlots); n != mediaJobInFlightMax {
		t.Errorf("%d slot(s) held after the refusal, want the other upload's %d untouched", n, mediaJobInFlightMax)
	}
	for i := 0; i < mediaJobInFlightMax; i++ {
		<-s.mediaJobSlots
	}
}

// A client that declares a body over the cap and then streams it must not be read: the server has taken
// under 64 KiB (the header buffer) when the 413 is out, however fast the client writes.
func TestAStreamedOversizeMediaJobIsNotDrained(t *testing.T) {
	s, _ := newTestServer(t, mediaJobCfg(t), &inputRunner{}, nil)
	addr, cl := serveCounting(t, s)
	defer func() { t.Logf("the server read %d bytes of the body", cl.read.Load()) }()
	conn := dialDeclared(t, addr, declaredOversize, 0, true)
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
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", resp.StatusCode)
	}
	time.Sleep(200 * time.Millisecond) // anything the server was still going to read
	if n := cl.read.Load(); n >= 64<<10 {
		t.Errorf("the server read %d bytes of a body it had already refused, want under 64 KiB", n)
	}
	_ = conn.Close()
	wg.Wait()
}

// The refusal ends the connection: the client has been told the body will not be read, so the server
// does not wait for it, and a client library that keeps writing sees a closed connection rather than a
// connection the next request would be stuck behind.
func TestTheOversizeAnswerClosesTheConnection(t *testing.T) {
	s, _ := newTestServer(t, mediaJobCfg(t), &inputRunner{}, nil)
	addr, _ := serveCounting(t, s)
	conn := dialDeclared(t, addr, declaredOversize, 20, true)
	br := bufio.NewReader(conn)
	resp := readAnswer(t, conn, br, 2*time.Second)
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("reading the refusal's body: %v", err)
	}
	resp.Body.Close()
	if !resp.Close {
		t.Errorf("the 413 did not carry Connection: close (resp.Close = false)")
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		t.Errorf("after the 413 the connection was not closed by the server: next read = %v, want EOF", err)
	}
}

// The check is the declared length against the cap, strictly over: a body of exactly the cap is read
// (and refused for what it is), one byte over is the 413.
func TestAMediaJobDeclaredExactlyAtTheCapIsStillRead(t *testing.T) {
	cfg := mediaJobCfg(t)
	cfg.FleetMediaInputsMaxMB = 1
	limit := MediaJobBodyCap(cfg)
	s, _ := newTestServer(t, cfg, &inputRunner{}, nil)
	at := do(t, s, http.MethodPost, MediaJobPath, strings.Repeat("x", int(limit)), bearer())
	if at.Code != http.StatusBadRequest || !strings.Contains(at.Body.String(), "malformed media-job body") {
		t.Fatalf("a body of exactly the cap (%d bytes): status %d (%s), want it read and refused as malformed (400)", limit, at.Code, at.Body.String())
	}
	over := do(t, s, http.MethodPost, MediaJobPath, strings.Repeat("x", int(limit)+1), bearer())
	if over.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a body one byte over the cap: status %d (%s), want 413", over.Code, over.Body.String())
	}
	if len(s.mediaJobSlots) != 0 {
		t.Errorf("%d slot(s) held after both requests returned", len(s.mediaJobSlots))
	}
}

// failingBody is a request body no code may read.
type failingBody struct{ reads atomic.Int32 }

func (b *failingBody) Read([]byte) (int, error) { b.reads.Add(1); return 0, io.EOF }
func (b *failingBody) Close() error             { return nil }

func declaredRequest(body io.ReadCloser, declared int64, hdr map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, MediaJobPath, nil)
	req.Body, req.ContentLength = body, declared
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return req
}

// At the handler, not the wire: the 413 comes with Connection: close, the body is never touched, and the
// bearer is checked first, so the door's refusals keep their order (403 closed, 401 no bearer, then 413).
func TestADeclaredOversizeMediaJobBodyIsNeverReadAndTheDoorChecksComeFirst(t *testing.T) {
	cfg := mediaJobCfg(t)
	s, _ := newTestServer(t, cfg, &inputRunner{}, nil)

	body := &failingBody{}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, declaredRequest(body, declaredOversize, bearer()))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d (%s), want 413", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Connection"); got != "close" {
		t.Errorf("Connection = %q, want close", got)
	}
	if n := body.reads.Load(); n != 0 {
		t.Errorf("the body was read %d time(s) after a declared-oversize refusal", n)
	}

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, declaredRequest(&failingBody{}, declaredOversize, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no bearer, declared oversize: status %d, want 401 (the bearer is checked before the size)", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, declaredRequest(&failingBody{}, declaredOversize, map[string]string{"Authorization": "Bearer wrong"}))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong bearer, declared oversize: status %d, want 401", rec.Code)
	}

	closed := mediaJobCfg(t)
	closed.FleetMediaInputs = false
	sc, _ := newTestServer(t, closed, &inputRunner{}, nil)
	rec = httptest.NewRecorder()
	sc.Handler().ServeHTTP(rec, declaredRequest(&failingBody{}, declaredOversize, bearer()))
	if rec.Code != http.StatusForbidden {
		t.Errorf("closed door, declared oversize: status %d, want 403 (the door is checked before the size)", rec.Code)
	}
}

// The reader is also the last line for a caller that reaches it with a declared-oversize body: it
// returns the typed error the handler maps to 413 without reading the body, instead of draining it.
func TestReadMediaJobBodyDoesNotDrainADeclaredOversizeBody(t *testing.T) {
	body := &failingBody{}
	r := declaredRequest(body, 100, nil)
	var maxErr *http.MaxBytesError
	if _, err := readMediaJobBody(r, 10); !errors.As(err, &maxErr) {
		t.Fatalf("err = %v, want the typed MaxBytesError", err)
	} else if maxErr.Limit != 10 {
		t.Errorf("MaxBytesError.Limit = %d, want the cap (10)", maxErr.Limit)
	}
	if n := body.reads.Load(); n != 0 {
		t.Errorf("a declared-oversize body was read %d time(s) before it was refused", n)
	}
}
