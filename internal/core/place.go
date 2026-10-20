package core

import (
	"context"
	"errors"
	"time"
)

// The place a queued media call keeps in its box's own line (P0 plan, S2; ADR 0083).
//
// A media call that cannot take its lane now used to leave a place-keeping token in the lease directory
// (gpulease/tokens.go) only when its own admission waited its whole window, and the token held the place for 30
// seconds after its last poll. A ticket (internal/mediaremote) waits for the first lane that frees ANYWHERE, so it
// waits far longer than a window and must keep its arrival time in the local line the whole while: callers that
// arrive after it queue behind it, and when the local lane frees it is served before them.
//
// The router receives only a small Runner interface and must not import the pipeline, so the three things it needs
// of the line are an optional interface, like RemoteAttributor and LaneProber. They wrap the lease manager's token
// calls (LeaveToken, TouchToken, DropToken) and add no admission logic of their own: the final local run spends the
// place through the waiter_token path every resumed call already takes.

// MediaPlace is a place in the local line.
type MediaPlace struct {
	// Token names the place (a gpulease token id). The local run that finally serves the ticket carries it as its
	// waiter_token.
	Token string
	// Since is when the call first joined the line: the arrival time every give-up and every resume keeps.
	Since time.Time
	// Devices are the cards the call would queue on (empty: the whole node).
	Devices []string
}

// ErrNoMediaPlace says the box keeps no place in line for this call: a host that does not lease cards keeps no
// tokens, a call that runs under a lease it inherited has a card chosen for it, a card table that would not read or
// an allocator that finds nothing to queue on gives it no card to hold a place on. The ticket then waits without
// local seniority and says so in its answer; it is never an error for the call.
var ErrNoMediaPlace = errors.New("this machine keeps no place in its line for the call")

// PlaceKeeper is implemented by a Runner that can keep a call's place in its local line.
type PlaceKeeper interface {
	// LeaveMediaPlace records req's place in the local line and returns it. since is the arrival time to keep (zero:
	// now). A request that carries a waiter_token that can still be resumed keeps that token's id and its arrival
	// time instead, so a caller that already holds a place in line brings it along.
	LeaveMediaPlace(ctx context.Context, req Request, since time.Time) (MediaPlace, error)
	// KeepMediaPlace records that the place token names is still wanted, so it keeps holding against later callers.
	// It refreshes a place that exists and never writes one: false means the place is gone (spent, pruned or never
	// was).
	KeepMediaPlace(token string) bool
	// DropMediaPlace forgets the place (the call was served, or no longer wants it). A token that names nothing is
	// harmless.
	DropMediaPlace(token string)
}

// LeavePlace asks runner to keep req's place in its line. A runner that cannot (a test double, a node's own runner,
// nil) answers ErrNoMediaPlace, so the call goes on without local seniority.
func LeavePlace(ctx context.Context, runner any, req Request, since time.Time) (MediaPlace, error) {
	if k, ok := runner.(PlaceKeeper); ok {
		return k.LeaveMediaPlace(ctx, req, since)
	}
	return MediaPlace{}, ErrNoMediaPlace
}

// KeepPlace refreshes the place token names on runner; false when runner keeps none or the place is gone.
func KeepPlace(runner any, token string) bool {
	if k, ok := runner.(PlaceKeeper); ok && token != "" {
		return k.KeepMediaPlace(token)
	}
	return false
}

// PlaceMarker is implemented by a PlaceKeeper whose places can say what their call waits for (gpulease.Token.WaitingFor).
type PlaceMarker interface {
	// MarkMediaPlace refreshes the place token names, as KeepMediaPlace does, and records whether its call's last reading of
	// the lane found only host RAM in the way. Such a place does not hold its cards against a request that declares no host
	// RAM, as a waiter on host RAM does not.
	MarkMediaPlace(token string, hostRAMOnly bool) bool
}

// MarkPlace records on runner whether the place token names waits only on host RAM; false when runner keeps no marks.
func MarkPlace(runner any, token string, hostRAMOnly bool) bool {
	if k, ok := runner.(PlaceMarker); ok && token != "" {
		return k.MarkMediaPlace(token, hostRAMOnly)
	}
	return false
}

// HostRAMOnly says the verdict found nothing in the way but host RAM: no lease on the cards and nobody ahead in line.
func (v LaneVerdict) HostRAMOnly() bool {
	return !v.Free && v.HostRAM != "" && len(v.Holders) == 0 && v.Ahead == 0
}

// DropPlace forgets the place token names on runner.
func DropPlace(runner any, token string) {
	if k, ok := runner.(PlaceKeeper); ok && token != "" {
		k.DropMediaPlace(token)
	}
}
