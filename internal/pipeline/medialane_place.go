package pipeline

// The place a queued image call keeps in this box's line (core.PlaceKeeper; P0 plan S2, ADR 0083). A whole-node graph's place is
// leaveGraphPlace (medialane_graph.go); the keep and the drop below serve both.
//
// A ticket (internal/mediaremote) waits for the first lane that frees anywhere in the cluster, so it can wait for hours
// and must stay senior to every call that arrives after it the whole while. The local line already has the thing for
// that, a place-keeping token (gpulease/tokens.go): a record of the arrival time and the cards the call wants, that
// holds the cards against later callers for as long as somebody keeps touching it, and that the call spends by
// resuming it. Nothing here admits anything: these three methods only leave, refresh and drop that token, on the cards
// the real admission would have queued the call on (the same plan, the same allocator reading), and the final local
// run takes the lane through the waiter_token path every resumed call takes. A host that does not lease cards keeps no
// tokens, so on it a ticket waits without local seniority and its answer says so.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

var _ core.PlaceKeeper = (*Pipeline)(nil)

// noPlace wraps the typed "this box keeps no place" error with the reason.
func noPlace(why string) error { return fmt.Errorf("%w (%s)", core.ErrNoMediaPlace, why) }

// LeaveMediaPlace implements core.PlaceKeeper.
func (p *Pipeline) LeaveMediaPlace(ctx context.Context, req core.Request, since time.Time) (core.MediaPlace, error) {
	if req.Task == core.TaskRunGraph {
		return p.leaveGraphPlace(req, since) // a whole-node graph's place (medialane_graph.go)
	}
	ln, why, _ := p.resolveLane(ctx, req)
	if why != "" {
		return core.MediaPlace{}, noPlace(why)
	}
	m := ln.m
	if !m.CardScoped() {
		return core.MediaPlace{}, noPlace("this host does not lease cards, so it keeps no places in line")
	}
	var ids []string
	switch {
	case ln.plan.whole:
		// The call would hold the whole node: its place is the whole-node one, as the real admission leaves it.
	case ln.plan.auto:
		r := &laneReading{p: p, m: m.ReadOnly(), cards: ln.table, token: ln.token}
		r.since = r.placeSince()
		r.askRAM = p.declaredRAM(ctx, ln.need, ln.table, nil)
		picked, _, err := r.pickAuto(ctx)
		if err != nil {
			// The cards the call may wait for are the ones that would qualify but for what is short (the allocator
			// lists them as Waitable); with none there is nothing to hold a place on, as in the real admission.
			var none *gpulease.NoCardsError
			if !errors.As(err, &none) || len(none.Waitable) == 0 {
				return core.MediaPlace{}, noPlace("no card could take the call however long it waited: " + err.Error())
			}
			picked = none.Waitable
		}
		ids = picked
	default:
		ids = ln.plan.ids
	}
	resume := ""
	if tok, ok := m.ResumeToken(ln.token); ok {
		// A caller that already holds a place brings it along: its id and the arrival it left with.
		resume, since = tok.ID, tok.Since()
	}
	if since.IsZero() {
		since = time.Now()
	}
	tok, err := m.LeaveToken(gpulease.ClassMedia, gpulease.Options{Reason: "image-gen", Origin: "pipeline", Devices: ids, ResumeToken: resume}, since)
	if err != nil {
		return core.MediaPlace{}, noPlace(err.Error())
	}
	return core.MediaPlace{Token: tok.ID, Since: tok.Since(), Devices: tok.Devices}, nil
}

// KeepMediaPlace implements core.PlaceKeeper: it refreshes a place that exists and never writes one.
func (p *Pipeline) KeepMediaPlace(token string) bool {
	m, err := p.scopedManager()
	return err == nil && m.TouchToken(token)
}

var _ core.PlaceMarker = (*Pipeline)(nil)

// MarkMediaPlace implements core.PlaceMarker: it refreshes a place that exists, never writes one, and records whether its
// call waits only on host RAM.
func (p *Pipeline) MarkMediaPlace(token string, hostRAMOnly bool) bool {
	m, err := p.scopedManager()
	if err != nil {
		return false
	}
	waitingFor := ""
	if hostRAMOnly {
		waitingFor = gpulease.WaitHostRAM
	}
	return m.MarkToken(token, waitingFor)
}

// DropMediaPlace implements core.PlaceKeeper.
func (p *Pipeline) DropMediaPlace(token string) {
	if m, err := p.scopedManager(); err == nil {
		m.DropToken(token)
	}
}
