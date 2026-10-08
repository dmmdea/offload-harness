package delegate

// A call's remotes list may only narrow the configured fleet (the diagnosis' R2-F08 and the operator's
// standing rule that a box never delegates to the standalone machine).
//
// A model-named list used to REPLACE delegate_remotes after one check, the SHAPE of each URL
// (netguard.TailnetURL), so any tailnet host passed and was dialled with the fleet's bearer on its health
// read; keeping the standalone box out was a habit. CheckRosterRemotes makes it a refusal: every entry
// must be a configured node, and a box with no roster accepts none. The engine applies it when
// RunOptions.RosterOnly says the list came from a model.

import (
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// TestCheckRosterRemotesAllowsOnlyConfiguredNodes is the rule, one row per shape.
func TestCheckRosterRemotesAllowsOnlyConfiguredNodes(t *testing.T) {
	// A node the roster lists by address. Built here, not written out: a literal address of the tailnet
	// CGNAT range in a test is exactly what the push gate's leak scan exists to stop.
	byAddr := "http://" + net.JoinHostPort(net.IPv4(100, 64, 0, 9).String(), "18811")
	roster := config.Config{DelegateRemotes: []string{"http://node-a:18811", "http://node-b:18811", byAddr}}
	for _, tc := range []struct {
		name  string
		cfg   config.Config
		named []string
		want  string // "" = accepted; else a substring of the refusal
	}{
		{"no list: the roster is used", roster, nil, ""},
		{"one configured node", roster, []string{"http://node-b:18811"}, ""},
		{"two, in another order than the roster", roster, []string{"http://node-b:18811", "http://node-a:18811"}, ""},
		{"a trailing slash and upper case are the same node", roster, []string{"http://Node-A:18811/"}, ""},
		{"a node the roster lists by address", roster, []string{byAddr}, ""},
		{"a duplicate is harmless", roster, []string{"http://node-a:18811", "http://node-a:18811"}, ""},
		{"a node outside the roster", roster, []string{"http://node-x:18811"}, `"http://node-x:18811" is not in this box's delegate_remotes (http://node-a:18811, http://node-b:18811, ` + byAddr + `)`},
		{"one configured and one outside: the outsider is named alone", roster, []string{"http://node-a:18811", "http://node-x:18811"}, `"http://node-x:18811" is not in`},
		{"two outsiders", roster, []string{"http://node-x:18811", "http://node-y:18811"}, "are not in this box's delegate_remotes"},
		{"a node the roster lists by address, named by name", roster, []string{"http://node-c:18811"}, "not in this box's delegate_remotes"},
		{"another port is another node", roster, []string{"http://node-a:18812"}, "not in this box's delegate_remotes"},
		{"another scheme is another base", roster, []string{"https://node-a:18811"}, "not in this box's delegate_remotes"},
		{"no roster: a box with none accepts none", config.Config{}, []string{"http://node-a:18811"}, "has no delegate_remotes"},
		{"no roster and no list", config.Config{}, nil, ""},
	} {
		err := CheckRosterRemotes(tc.cfg, tc.named)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && err == nil:
			t.Errorf("%s: accepted, want a refusal containing %q", tc.name, tc.want)
		case tc.want != "" && !strings.Contains(err.Error(), tc.want):
			t.Errorf("%s: refusal = %q, want it to contain %q", tc.name, err.Error(), tc.want)
		}
	}
	// The refusal says what to do about it.
	err := CheckRosterRemotes(roster, []string{"http://node-x:18811"})
	for _, want := range []string{"can only narrow the configured fleet, never add a node", "add it to delegate_remotes and restart the server"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %v, want it to say %q", err, want)
		}
	}
}

// TestRunWithRosterOnlyRefusesANodeOutsideTheRosterBeforeDialling is the engine half: the standalone box
// stands in as a fake node that must never be asked anything, and a configured node is dialled as usual.
func TestRunWithRosterOnlyRefusesANodeOutsideTheRosterBeforeDialling(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	outsider, outsiderURL := acceptingNode(t, "node-standalone", "answer from the box that must not be asked", nil)
	member, memberURL := acceptingNode(t, "node-member", "answer from the roster", nil)
	cfg := testCfg(t)
	cfg.DelegateRemotes = []string{memberURL}

	// The model names the outsider: refused as a config mistake, nothing dialled.
	_, _, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{outsiderURL}, &RunOptions{RosterOnly: true})
	if err == nil || !strings.Contains(err.Error(), "not in this box's delegate_remotes") || !strings.Contains(err.Error(), outsiderURL) {
		t.Fatalf("RunWith with a node outside the roster returned %v, want a refusal naming it", err)
	}
	if outsider.healths.Load() != 0 || outsider.dispatches.Load() != 0 {
		t.Fatalf("the node outside the roster was dialled (%d health read(s), %d dispatch(es)): the refusal must come first", outsider.healths.Load(), outsider.dispatches.Load())
	}

	// A list that narrows the roster to one of its members runs there.
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{memberURL}, &RunOptions{RosterOnly: true})
	if err != nil || sum.Succeeded != 1 || results[0].Node != "node-member" || member.dispatches.Load() != 1 {
		t.Fatalf("a list naming a roster member: err %v summary %+v dispatches %d, want the subtask run there", err, sum, member.dispatches.Load())
	}

	// No list at all uses the roster, whatever RosterOnly says.
	if _, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", nil, &RunOptions{RosterOnly: true}); err != nil || sum.Succeeded != 1 {
		t.Fatalf("no list: err %v summary %+v, want the roster used", err, sum)
	}
}

// TestRunWithoutRosterOnlyKeepsNamingAnyTailnetNode is the control: the operator's own CLI verbs (and
// fleet-smoke, which tests a node before it joins the roster) pass no RosterOnly and keep naming any
// node. The engine refuses nothing it did not refuse before.
func TestRunWithoutRosterOnlyKeepsNamingAnyTailnetNode(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	outsider, outsiderURL := acceptingNode(t, "node-new", "answer from a node that has not joined", nil)
	cfg := testCfg(t)
	cfg.DelegateRemotes = []string{"http://node-elsewhere:18811"}
	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{outsiderURL}, nil)
	if err != nil || sum.Succeeded != 1 || results[0].Node != "node-new" || outsider.dispatches.Load() != 1 {
		t.Fatalf("an operator naming a node outside the roster: err %v summary %+v dispatches %d, want it to run there as before", err, sum, outsider.dispatches.Load())
	}
}

// TestCanonicalRosterRemotesRespellsEachEntryAsTheRosterDoes: the check accepts "http://Node-A:18811/" as
// the roster's "http://node-a:18811", and what proceeds must be the roster's spelling, because every
// per-node key after the check (the process in-flight gate, the per-base maps) is the string itself. A node
// named twice, in whatever spellings, is one entry; an empty list stays empty (the roster is used as it is).
func TestCanonicalRosterRemotesRespellsEachEntryAsTheRosterDoes(t *testing.T) {
	roster := config.Config{DelegateRemotes: []string{"http://Node-A:18811/", "http://node-b:18811"}}
	for _, tc := range []struct {
		name  string
		named []string
		want  []string
	}{
		{"no list", nil, nil},
		{"the roster's own spelling", []string{"http://node-b:18811"}, []string{"http://node-b:18811"}},
		{"another case and a trailing slash", []string{"HTTP://NODE-B:18811/"}, []string{"http://node-b:18811"}},
		{"a roster entry that carries a slash and capitals keeps them", []string{"http://node-a:18811"}, []string{"http://Node-A:18811/"}},
		{"the caller's order is kept", []string{"http://node-b:18811", "http://node-a:18811"}, []string{"http://node-b:18811", "http://Node-A:18811/"}},
		{"two spellings of one node are one entry", []string{"http://node-b:18811", "http://node-b:18811/", "HTTP://Node-B:18811"}, []string{"http://node-b:18811"}},
	} {
		got, err := CanonicalRosterRemotes(roster, tc.named)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: CanonicalRosterRemotes(%q) = %q, %v; want %q", tc.name, tc.named, got, err, tc.want)
		}
	}
	if got, err := CanonicalRosterRemotes(roster, []string{"http://node-b:18811", "http://node-x:18811"}); err == nil || got != nil {
		t.Errorf("a list with an outsider = %q, %v, want a refusal and no list", got, err)
	}
}

// TestRunWithRosterOnlyKeysTheNodeUnderTheRostersSpelling is the engine half, at the place the spelling
// matters: the process in-flight gate. A model names the roster's node with a trailing slash; while the
// job is on the node the gate must hold ONE open dispatch under the roster's own key and none under the
// caller's spelling. Before the respelling it held the caller's, a second key for the same node: a call
// spelled differently from the roster (or from a concurrent call) was admitted up to the node's ceiling
// again.
func TestRunWithRosterOnlyKeysTheNodeUnderTheRostersSpelling(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	var (
		mu                          sync.Mutex
		memberURL                   string
		underRoster, underCallerKey int
		seen                        bool
	)
	member, url := acceptingNode(t, "node-member", "answer from the roster", func(f *fakeNode) {
		f.dispatchHook = func(int64) int {
			mu.Lock()
			defer mu.Unlock()
			underRoster, underCallerKey, seen = processGate.load(memberURL), processGate.load(memberURL+"/"), true
			return 0
		}
	})
	mu.Lock()
	memberURL = url
	mu.Unlock()
	cfg := testCfg(t)
	cfg.DelegateRemotes = []string{url}

	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{url + "/"}, &RunOptions{RosterOnly: true})
	if err != nil || sum.Succeeded != 1 || member.dispatches.Load() != 1 {
		t.Fatalf("err %v summary %+v dispatches %d, want the subtask run on the node named with a trailing slash", err, sum, member.dispatches.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if !seen || underRoster != 1 || underCallerKey != 0 {
		t.Fatalf("during the dispatch the gate held %d under the roster's spelling and %d under the caller's (seen %v), want 1 and 0", underRoster, underCallerKey, seen)
	}
	if results[0].ranBase != url {
		t.Fatalf("the result's base = %q, want the roster's spelling %q", results[0].ranBase, url)
	}
}
