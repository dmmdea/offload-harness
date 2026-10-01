package mcpserver

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// fakeNIM is an OpenAI-compatible NIM: it counts every request it receives.
func fakeNIM(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/models") {
			fmt.Fprint(w, `{"object":"list","data":[{"id":"m"}]}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"model":"m","usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func nimServer(t *testing.T, policy string, bases []string) (*Server, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Home = t.TempDir()
	cfg.StateDir = t.TempDir()
	cfg.NIMEndpoint = "https://integrate.api.nvidia.com/v1"
	cfg.NIMBasePolicy = policy
	cfg.NIMBases = bases
	return New(pipeline.New(cfg, nil, nil, nil)), cfg.StateDir
}

func auditRows(t *testing.T, stateDir string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(stateDir, "nim-base-audit.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	defer f.Close()
	var rows []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		rows = append(rows, sc.Text())
	}
	return rows
}

// S-30 under "enforce": a caller-named base outside the allowlist is refused
// before any request leaves, and the refusal is counted.
func TestOffloadNIMEnforceRefusesAnUnlistedBaseBeforeAnyRequest(t *testing.T) {
	var hits atomic.Int64
	nim := fakeNIM(t, &hits)
	s, stateDir := nimServer(t, "enforce", nil)
	res, err := s.handleNIM(context.Background(), callReq(fmt.Sprintf(`{"prompt":"secret plan text","base":%q}`, nim.URL+"/v1?leak=secret-plan")))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	reason, _ := m["reason"].(string)
	if m["deferred"] != true || !strings.Contains(reason, "base refused") {
		t.Fatalf("enforce must defer an unlisted base: %v", m)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d request(s) reached the unlisted base under enforce", n)
	}
	rows := auditRows(t, stateDir)
	if len(rows) != 1 || !strings.Contains(rows[0], `"mode":"enforce"`) {
		t.Fatalf("audit rows = %v, want one enforce row", rows)
	}
	if strings.Contains(rows[0], "secret-plan") || strings.Contains(rows[0], "/v1") {
		t.Fatalf("the audit row must keep only scheme, host and port: %s", rows[0])
	}
}

// S-30 under "audit" (the default): the call runs, the result says it would be
// refused, and a would-refuse row is written for the promotion decision.
func TestOffloadNIMAuditRunsButCountsAnUnlistedBase(t *testing.T) {
	var hits atomic.Int64
	nim := fakeNIM(t, &hits)
	s, stateDir := nimServer(t, "", nil) // default policy
	res, err := s.handleNIM(context.Background(), callReq(fmt.Sprintf(`{"prompt":"hello","base":%q}`, nim.URL+"/v1")))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	note, _ := m["base_policy"].(string)
	if m["content"] != "ok" || !strings.HasPrefix(note, "audit:") {
		t.Fatalf("audit mode must run the call and say it would be refused: %v", m)
	}
	if hits.Load() == 0 {
		t.Fatal("audit mode must not block the call")
	}
	rows := auditRows(t, stateDir)
	if len(rows) != 1 || !strings.Contains(rows[0], `"mode":"audit"`) {
		t.Fatalf("audit rows = %v, want one audit row", rows)
	}
}

// A base the operator listed runs with no note and no audit row, under
// either policy.
func TestOffloadNIMListedBaseRunsCleanly(t *testing.T) {
	var hits atomic.Int64
	nim := fakeNIM(t, &hits)
	for _, policy := range []string{"audit", "enforce"} {
		s, stateDir := nimServer(t, policy, []string{nim.URL + "/v1"})
		res, err := s.handleNIM(context.Background(), callReq(fmt.Sprintf(`{"prompt":"hello","base":%q}`, nim.URL+"/v1")))
		if err != nil {
			t.Fatal(err)
		}
		m := decodeResult(t, res)
		if m["content"] != "ok" || m["base_policy"] != nil {
			t.Fatalf("policy %s: a listed base must run cleanly: %v", policy, m)
		}
		if rows := auditRows(t, stateDir); len(rows) != 0 {
			t.Fatalf("policy %s: a listed base must write no audit row: %v", policy, rows)
		}
	}
}

// nimServerDefaultRoot leaves state_dir unset, as the live hosts' configs do, so
// the audit row has to find the machine-wide state root the GPU lease uses.
func nimServerDefaultRoot(t *testing.T, envRoot string) *Server {
	t.Helper()
	t.Setenv("LOCAL_OFFLOAD_STATE_DIR", envRoot)
	cfg := config.Default()
	cfg.Home = t.TempDir()
	cfg.StateDir = ""
	cfg.NIMEndpoint = "https://integrate.api.nvidia.com/v1"
	return New(pipeline.New(cfg, nil, nil, nil))
}

// SF-05: with state_dir unset every would-refuse row was dropped with "no
// state_dir" (live proof 2026-10-01), so the promotion to enforce had nothing
// to count. The row lands in the resolved state root instead.
func TestOffloadNIMAuditRowLandsInTheDefaultStateRoot(t *testing.T) {
	var hits atomic.Int64
	nim := fakeNIM(t, &hits)
	root := t.TempDir()
	s := nimServerDefaultRoot(t, root)
	res, err := s.handleNIM(context.Background(), callReq(fmt.Sprintf(`{"prompt":"hello","base":%q}`, nim.URL+"/v1")))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	note, _ := m["base_policy"].(string)
	if m["content"] != "ok" || !strings.HasPrefix(note, "audit:") {
		t.Fatalf("audit mode must run the call and say it would be refused: %v", m)
	}
	if strings.Contains(note, "not written") {
		t.Fatalf("the would-refuse row was dropped: %s", note)
	}
	rows := auditRows(t, root)
	if len(rows) != 1 || !strings.Contains(rows[0], `"mode":"audit"`) {
		t.Fatalf("audit rows in the default state root = %v, want one audit row", rows)
	}
}

// A state root the resolver refuses (a cloud-sync folder) still lets the
// audit-mode call run, and the note names why the row was not written.
func TestOffloadNIMAuditSaysWhyTheRowWasNotWritten(t *testing.T) {
	var hits atomic.Int64
	nim := fakeNIM(t, &hits)
	synced := filepath.Join(t.TempDir(), "Dropbox", "state")
	s := nimServerDefaultRoot(t, synced)
	res, err := s.handleNIM(context.Background(), callReq(fmt.Sprintf(`{"prompt":"hello","base":%q}`, nim.URL+"/v1")))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	note, _ := m["base_policy"].(string)
	if m["content"] != "ok" || !strings.Contains(note, "not written") || !strings.Contains(note, "cloud-sync") {
		t.Fatalf("a refused state root must leave the call running and say why no row was written: %v", m)
	}
	if rows := auditRows(t, synced); len(rows) != 0 {
		t.Fatalf("no row may be written under a refused state root: %v", rows)
	}
}
