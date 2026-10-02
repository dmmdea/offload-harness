// The progress heartbeat of offload_transcribe (register C-89, stage 2).
//
// A transcription of long audio is silent for minutes, like a delegation: nothing is sent
// to the MCP client until the whole call returns. The reporter the delegation doors use
// (progress.go) now also serves a call with no subtasks, as a bare liveness signal: an
// opening notification and a heartbeat every progressHeartbeat while the call runs, and only
// when the request carried a progress token. These tests drive the real tool over an
// in-memory MCP transport, through the real pipeline, the real ffmpeg convert and a whisper
// stand-in that takes a while to answer.

package mcpserver

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// silentWav writes one second of 16 kHz mono 16-bit silence and returns its path.
func silentWav(t *testing.T) string {
	t.Helper()
	const rate = 16000
	data := make([]byte, rate*2)
	var b bytes.Buffer
	le := binary.LittleEndian
	b.WriteString("RIFF")
	_ = binary.Write(&b, le, uint32(36+len(data)))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, le, uint32(16))
	_ = binary.Write(&b, le, uint16(1)) // PCM
	_ = binary.Write(&b, le, uint16(1)) // mono
	_ = binary.Write(&b, le, uint32(rate))
	_ = binary.Write(&b, le, uint32(rate*2))
	_ = binary.Write(&b, le, uint16(2))
	_ = binary.Write(&b, le, uint16(16))
	b.WriteString("data")
	_ = binary.Write(&b, le, uint32(len(data)))
	b.Write(data)
	path := filepath.Join(t.TempDir(), "silence.wav")
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// slowWhisperServer is an offload server whose whisper upstream answers after `answerAfter`.
// Its media directory, home and ledger are temp directories: the call writes its transcript
// there, never to the machine's own.
func slowWhisperServer(t *testing.T, answerAfter time.Duration) *Server {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH; this test needs a real convert to reach the STT call")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(answerAfter)
		_, _ = w.Write([]byte(`{"language":"en","duration":1,"text":"hello","segments":[{"id":0,"start":0,"end":1,"text":"hello"}]}`))
	}))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	cfg := config.Default()
	cfg.Home = home
	cfg.LedgerPath = filepath.Join(home, "ledger.jsonl")
	cfg.MediaDir = filepath.Join(home, "media")
	cfg.FFmpegPath = ffmpeg
	cfg.Endpoint = srv.URL
	cfg.STTModel = "test-stt"
	cfg.STTUnloadAfter = false // nothing here is about the unload
	return New(pipeline.New(cfg, nil, nil, nil))
}

// TestTranscribeSendsAHeartbeatWhileItRuns: a request with a progress token hears the call
// open and then hears from it every heartbeat until the transcript is ready — each
// notification carrying the caller's token and a strictly increasing progress value, and
// none after the call has returned.
func TestTranscribeSendsAHeartbeatWhileItRuns(t *testing.T) {
	old := progressHeartbeat
	progressHeartbeat = 40 * time.Millisecond
	t.Cleanup(func() { progressHeartbeat = old })

	s := slowWhisperServer(t, 400*time.Millisecond)
	res, log := callOverMCP(t, s, "offload_transcribe", map[string]any{"audio": silentWav(t)}, "tr-1")
	if res.IsError || !strings.Contains(callText(t, res), `"gist":"hello"`) {
		t.Fatalf("the call itself did not transcribe: %+v", res)
	}

	got := log.all()
	if len(got) < 4 {
		t.Fatalf("%d progress notification(s) for a call that outlasts ten heartbeats, want an opening one and at least three heartbeats: %v", len(got), progressMessages(got))
	}
	if !strings.Contains(got[0].Message, "offload_transcribe started") {
		t.Errorf("the first notification %q should say the call started", got[0].Message)
	}
	var beats int
	last := 0.0
	for i, p := range got {
		if p.ProgressToken != "tr-1" {
			t.Fatalf("notification %d carries token %v, want the caller's tr-1", i, p.ProgressToken)
		}
		if p.Progress <= last {
			t.Fatalf("notification %d progress %v does not increase past %v: the spec asks for a strictly increasing value", i, p.Progress, last)
		}
		last = p.Progress
		if strings.Contains(p.Message, "still working") && strings.Contains(p.Message, "offload_transcribe") {
			beats++
		}
	}
	if beats < 3 {
		t.Errorf("%d heartbeat(s) during the call, want at least 3: %v", beats, progressMessages(got))
	}

	// Nothing arrives after the call has returned: the token is dead by then.
	n := len(log.all())
	time.Sleep(150 * time.Millisecond)
	if after := len(log.all()); after != n {
		t.Fatalf("%d notification(s) arrived after the call returned", after-n)
	}
}

// TestTranscribeSendsNoProgressWithoutAToken: the heartbeat is opt-in, like the delegation
// reporter. A request with no progress token gets exactly what it always got.
func TestTranscribeSendsNoProgressWithoutAToken(t *testing.T) {
	old := progressHeartbeat
	progressHeartbeat = 40 * time.Millisecond
	t.Cleanup(func() { progressHeartbeat = old })

	s := slowWhisperServer(t, 300*time.Millisecond)
	res, log := callOverMCP(t, s, "offload_transcribe", map[string]any{"audio": silentWav(t)}, nil)
	if res.IsError || !strings.Contains(callText(t, res), `"gist":"hello"`) {
		t.Fatalf("the call itself did not transcribe: %+v", res)
	}
	time.Sleep(100 * time.Millisecond)
	if got := log.all(); len(got) != 0 {
		t.Fatalf("%d progress notification(s) without a progress token: %v", len(got), progressMessages(got))
	}
}
