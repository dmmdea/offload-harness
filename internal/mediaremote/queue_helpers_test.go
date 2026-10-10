package mediaremote

// Helpers of the ticket queue's tests. The queue is a per-process singleton, so a test gets a fresh one and ends every
// goroutine of it, and the scan rhythm runs in milliseconds, not seconds.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
)

func init() {
	scanEvery = 40 * time.Millisecond
	attachPoll = 10 * time.Millisecond
	firstScanBound = 10 * time.Second
}

// freshQueue swaps in an empty queue for the test and stops it (and the goroutine of every ticket) when the test ends.
func freshQueue(t *testing.T) *queueRT {
	t.Helper()
	old := defaultQ
	q := newQueueRT(selfOwner())
	defaultQ = q
	t.Cleanup(func() {
		q.stop()
		defaultQ = old
	})
	return q
}

// freshRoster makes every roster read a real read: the memo of the shared cache would hide a node that changed.
func freshRoster(t *testing.T) {
	t.Helper()
	prev := probeRoster
	probeRoster = func(ctx context.Context, remotes []string, token string, timeout time.Duration) []rosterprobe.Reading {
		rosterprobe.Default.Reset()
		return prev(ctx, remotes, token, timeout)
	}
	t.Cleanup(func() { probeRoster = prev })
}

// callWindow is how long a test call blocks for its ticket when it does not mean to wait for the job.
const callWindow = 3 * time.Second

// runDoor is a call through an MCP door: it answers inside its window and the ticket stays with the process.
func runDoor(cfg config.Config, runner Runner, req core.Request, window time.Duration) core.Result {
	return RunWith(context.Background(), cfg, runner, req, "auto", nil, Wait{Window: window, Detach: true})
}

// settle re-sends the same request through a door, the way a caller does that obeys the answer (a repeat attaches to the
// ticket) or loops regardless, until accept takes the answer. It fails the test if that takes too long.
func settle(t *testing.T, cfg config.Config, runner Runner, req core.Request, accept func(core.Result) bool) core.Result {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		res := runDoor(cfg, runner, req, AtOnce)
		if accept(res) {
			return res
		}
		if time.Now().After(deadline) {
			t.Fatalf("the answer never became what the test waits for: %+v | %s", res, res.Data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// rowIs accepts an answer whose cluster has a row for the node in the state.
func rowIs(node, state string) func(core.Result) bool {
	return func(res core.Result) bool {
		var d struct {
			Cluster []ClusterRow `json:"cluster"`
		}
		if json.Unmarshal(res.Data, &d) != nil {
			return false
		}
		for _, r := range d.Cluster {
			if strings.Contains(r.Node, node) && r.State == state {
				return true
			}
		}
		return false
	}
}

// waitFor polls cond until it holds or the time is up.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// keeperRunner is a laneRunner that also keeps places in a line, the way the pipeline does through the lease directory.
type keeperRunner struct {
	*laneRunner
	km     sync.Mutex
	seq    int
	places map[string]core.MediaPlace
	leaves []leaveCall
	keeps  map[string]int
	drops  []string
	// noPlace, when set, is what LeaveMediaPlace answers (a host that keeps none); asked counts every ask.
	noPlace error
	asked   int
}

type leaveCall struct {
	req   core.Request
	since time.Time
	token string
}

func newKeeperRunner(v core.LaneVerdict) *keeperRunner {
	return &keeperRunner{laneRunner: &laneRunner{verdict: v}, places: map[string]core.MediaPlace{}, keeps: map[string]int{}}
}

// placeFor makes a place that exists already (a caller left it earlier).
func (k *keeperRunner) placeFor(token string, since time.Time) {
	k.km.Lock()
	defer k.km.Unlock()
	k.places[token] = core.MediaPlace{Token: token, Since: since}
}

func (k *keeperRunner) LeaveMediaPlace(_ context.Context, req core.Request, since time.Time) (core.MediaPlace, error) {
	k.km.Lock()
	defer k.km.Unlock()
	k.asked++
	if k.noPlace != nil {
		return core.MediaPlace{}, k.noPlace
	}
	if tok, _ := req.Params["waiter_token"].(string); tok != "" {
		if p, ok := k.places[tok]; ok {
			k.leaves = append(k.leaves, leaveCall{req, since, p.Token})
			return p, nil
		}
	}
	k.seq++
	if since.IsZero() {
		since = time.Now()
	}
	p := core.MediaPlace{Token: fmt.Sprintf("tk-fake%04d", k.seq), Since: since}
	k.places[p.Token] = p
	k.leaves = append(k.leaves, leaveCall{req, since, p.Token})
	return p, nil
}

func (k *keeperRunner) KeepMediaPlace(token string) bool {
	k.km.Lock()
	defer k.km.Unlock()
	if _, ok := k.places[token]; !ok {
		return false
	}
	k.keeps[token]++
	return true
}

func (k *keeperRunner) DropMediaPlace(token string) {
	k.km.Lock()
	defer k.km.Unlock()
	delete(k.places, token)
	k.drops = append(k.drops, token)
}

func (k *keeperRunner) leaveCalls() []leaveCall {
	k.km.Lock()
	defer k.km.Unlock()
	return append([]leaveCall(nil), k.leaves...)
}

func (k *keeperRunner) keptCount(token string) int {
	k.km.Lock()
	defer k.km.Unlock()
	return k.keeps[token]
}

func (k *keeperRunner) dropped(token string) int {
	k.km.Lock()
	defer k.km.Unlock()
	n := 0
	for _, d := range k.drops {
		if d == token {
			n++
		}
	}
	return n
}

func (k *keeperRunner) lapse(token string) {
	k.km.Lock()
	defer k.km.Unlock()
	delete(k.places, token)
}

var _ core.PlaceKeeper = (*keeperRunner)(nil)

// runBounded is the CLI's call (it blocks until the job is done) with a bound, so a regression that would keep a ticket waiting
// for ever ends the test in seconds and not at the package's timeout.
func runBounded(cfg config.Config, runner Runner, req core.Request, route string, remotes []string) core.Result {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return Run(ctx, cfg, runner, req, route, remotes)
}
