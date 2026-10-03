package pipeline

import (
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

// SetPairEmitter wires the emitter that carries the PAIR cards of calls this box sends to a fleet node
// (BeginRemote); nil = such calls open no card. The ledger row they write needs no emitter.
func (p *Pipeline) SetPairEmitter(e *pairworkloads.Emitter) { p.pair = e }

// BeginRemote implements core.RemoteAttributor: the remote lanes (composeremote, visionremote,
// textremote) never call Run for a call they send to a node, so the asking box wrote no ledger row
// and showed no PAIR card for it. The handle they get back writes both (pairworkloads.RemoteCall).
func (p *Pipeline) BeginRemote(req core.Request, route string) core.RemoteAttribution {
	if p == nil {
		return nil
	}
	return pairworkloads.NewRemoteCall(p.pair, p.led, req, route)
}

var _ core.RemoteAttributor = (*Pipeline)(nil)
