package delegate

// C-95: which source supplies the LOCAL seat. On a box that declares layers the
// placement table's seat wins over agent_model (the documented ADR 0039 rule), so
// a config copy that repoints agent_model but keeps the layers still runs on the
// layer's seat; a plain box hands the runner no seat and the pipeline resolves the
// config's agent_model. The CLI/pipeline notes (config.AgentModelShadowNote,
// pipeline roster defer) speak this rule; these two rows pin that it is true.

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	placetable "github.com/dmmdea/offload-harness/internal/placement"
)

func TestLocalSeatSourceLayersBeatAgentModelOnACompositeBox(t *testing.T) {
	cfg := compositeTestCfg(t)
	cfg.AgentModel = "copy-seat" // the copy's own planner key; no layer serves it
	rd := &readings{pairAgent: []placetable.SeatState{coldPair()}}
	var got LocalOptions
	var calls atomic.Int64
	results, sum, err := RunWith(context.Background(), cfg, echoingLocal(&got, &calls),
		[]core.AgentContract{remoteContract()}, "local", nil, &RunOptions{LocalDecider: tableDecider(cfg, rd)})
	if err != nil || len(results) != 1 {
		t.Fatalf("RunWith: %v (%d results, %+v)", err, len(results), sum)
	}
	if got.Seat == "" || got.Seat == "copy-seat" {
		t.Fatalf("on a composite box the placement table supplies the seat, not agent_model; local runner was handed %q", got.Seat)
	}
	if note := cfg.AgentModelShadowNote(); note == "" {
		t.Fatal("the config must report that its layers shadow agent_model, or the CLI cannot say which source won")
	}
}

func TestLocalSeatSourceIsAgentModelOnAPlainBox(t *testing.T) {
	cfg := testCfg(t) // AgentModel "local-seat", no layers
	var got LocalOptions
	var calls atomic.Int64
	results, sum, err := RunWith(context.Background(), cfg, echoingLocal(&got, &calls),
		[]core.AgentContract{remoteContract()}, "local", nil, nil)
	if err != nil || len(results) != 1 {
		t.Fatalf("RunWith: %v (%d results, %+v)", err, len(results), sum)
	}
	if got.Seat != "" || results[0].Seat != "local-seat" {
		t.Fatalf("a plain box hands the runner no seat and runs the config's agent_model; handed %q, ran %q", got.Seat, results[0].Seat)
	}
}
