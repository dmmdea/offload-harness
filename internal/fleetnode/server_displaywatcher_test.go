package fleetnode

import (
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/displaystate"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// The display layer's row a node publishes carries the verdict of its own admission, and that includes
// whether this node's post-admission watcher is alive: the delegator cannot read the node's heartbeat
// file, so a row that said admissible while nothing watched the twin would send work onto the operator's
// card.
func TestTheDisplayRowCarriesThisNodesWatcherLiveness(t *testing.T) {
	cfg := compositeNodeCfg("http://127.0.0.1:1")
	cfg.StateDir = t.TempDir()
	cfg.OperatorPresence = "away"
	for i := range cfg.Layers {
		if cfg.Layers[i].Name == "display" {
			cfg.Layers[i].Dormant = false
		}
	}
	s, _ := newTestServer(t, cfg, &fakeRunner{}, authOpts(true))
	snap := Snapshot{Devices: []gpuprobe.Device{
		{Index: 0, UUID: "GPU-aaaa1111", FreeGiB: 14},
		{Index: 1, UUID: "GPU-bbbb2222", FreeGiB: 15, DisplayAttached: true},
		{Index: 2, UUID: "GPU-cccc3333", FreeGiB: 14},
	}}
	display := func() (admissible bool, reason string) {
		for _, r := range s.layerRows(snap) {
			if r.Name == "display" {
				return r.Admissible, r.Reason
			}
		}
		t.Fatal("no display row")
		return
	}

	if ok, reason := display(); ok || !strings.Contains(reason, "watcher") {
		t.Fatalf("no heartbeat: the row is inadmissible and names the watcher: %v %q", ok, reason)
	}
	path, err := displaystate.StatePath(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := displaystate.Write(path, displaystate.State{CheckedAt: time.Now(), IntervalSec: 10}); err != nil {
		t.Fatal(err)
	}
	if ok, reason := display(); !ok {
		t.Fatalf("a fresh heartbeat, the operator away and the floor kept: admissible, got %q", reason)
	}
}
