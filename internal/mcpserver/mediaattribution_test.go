package mcpserver

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// A media door that sends its job to a fleet node attributes the call (0.165.0, D5-D11): mediaremote asks
// the Runner it is handed for the attribution handle, and the Runner a door hands it is runTaskAs, not the
// pipeline, so runTaskAs must forward the question to the pipeline or the call writes nothing.
func TestMediaDoorThatRoutesRemoteWritesTheAskersLedgerRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	led, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer led.Close()
	cfg := config.Default()
	cfg.DelegateRemotes = []string{"http://127.0.0.1:1"} // nothing listens: the call is placed, then deferred
	s := New(pipeline.New(cfg, nil, nil, led))
	txt := callVision(t, s.handleGenerateImage, map[string]any{"prompt": "a red door", "route": "remote"})
	if !strings.Contains(txt, `"deferred":true`) {
		t.Fatalf("want a placement defer, got %s", txt)
	}
	rows, err := ledger.ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want ONE asker row for the remote call: %+v", len(rows), rows)
	}
	if r := rows[0]; r.Door != "offload_generate_image" || r.Route != "remote" || !r.CardByCaller || !r.Deferred {
		t.Fatalf("asker row = %+v", r)
	}
}

// A server with no pipeline (the test seam) attributes nothing, and core.BeginRemote turns that into the
// no-op handle rather than a nil one.
func TestRunTaskAsWithNoPipelineGivesTheNoOpHandle(t *testing.T) {
	r := runTaskAs{&Server{}}
	if h := r.BeginRemote(core.Request{}, "remote"); h != nil {
		t.Fatalf("handle = %v, want nil", h)
	}
	if _, nop := core.BeginRemote(r, core.Request{}, "remote").(core.NopAttribution); !nop {
		t.Fatal("core.BeginRemote must fall back to the no-op handle")
	}
}
