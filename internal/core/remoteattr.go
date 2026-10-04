package core

// Remote-call attribution (PAIR routing fixes, D5/D6).
//
// A call the route sends to a fleet node never passes through Pipeline.Run on the asking box, so
// that box wrote no ledger row and no PAIR card for it. The remote lanes (composeremote,
// visionremote, textremote, and later the stt route) report the call through these two interfaces
// instead. They live in core because the lanes receive only a small Runner interface and must not
// import the pipeline (which imports delegate, which they already import) or each other.
//
// The asker's pipeline implements RemoteAttributor; a Runner that does not (a test double, a node's
// own runner) simply leaves the call unattributed, exactly as before.

// RemoteAttribution is one remote call's attribution handle. Every method is safe to call in any
// order and more than once; after Finish or Discard the rest are no-ops.
type RemoteAttribution interface {
	// Dispatched names the node a call is about to be sent to: base is the dispatch URL, nodeID the
	// fleet node id from its health, fleetJobID the id the node will know the job by. It opens the
	// call's one PAIR card (queued) on that node.
	Dispatched(base, nodeID, fleetJobID string)
	// Running reports that the node says the job has started: the card turns running, once.
	Running()
	// Finish ends the call with its result: one asker ledger row, and the card's terminal frame when
	// a node was chosen. A call that never reached a node still gets its row but no card.
	Finish(res Result)
	// Discard ends a call whose remote attempt is not the final answer because the auto route fell
	// back to a local run (the pipeline writes that run's own row). An attempt that never reached a
	// node writes nothing at all; one that did is finished as a deferred call with reason, so its
	// card is closed failed and the node time it took is on the ledger.
	Discard(reason string)
}

// RemoteAttributor is implemented by a Runner that can attribute a remote call. route is the
// normalized route the caller asked for (local, auto or remote).
type RemoteAttributor interface {
	BeginRemote(req Request, route string) RemoteAttribution
}

// NopAttribution is the handle for a call nobody attributes.
type NopAttribution struct{}

func (NopAttribution) Dispatched(string, string, string) {}
func (NopAttribution) Running()                          {}
func (NopAttribution) Finish(Result)                     {}
func (NopAttribution) Discard(string)                    {}

// BeginRemote starts attribution of a remote call on runner, or returns the no-op handle when the
// runner does not implement RemoteAttributor.
func BeginRemote(runner any, req Request, route string) RemoteAttribution {
	if a, ok := runner.(RemoteAttributor); ok {
		if h := a.BeginRemote(req, route); h != nil {
			return h
		}
	}
	return NopAttribution{}
}
