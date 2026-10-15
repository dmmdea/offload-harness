package recipe

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// envRuleKeys reads the vocabulary off the engine's own struct, so a key added there is seen here.
func envRuleKeys(t *testing.T) []string {
	t.Helper()
	var keys []string
	typ := reflect.TypeOf(core.AgentEnvRules{})
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			t.Fatalf("field %s has no json key", typ.Field(i).Name)
		}
		keys = append(keys, tag)
	}
	return keys
}

// Every key of the vocabulary says how it is scored. A new key in core.AgentEnvRules fails this test
// until someone decides whether a recorded trace can read it.
func TestEveryRuleKeyDeclaresHowItIsScored(t *testing.T) {
	want := envRuleKeys(t)
	got := Keys()
	sort.Strings(want)
	sorted := append([]string(nil), got...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(sorted, want) {
		t.Fatalf("scored keys %v, vocabulary %v", sorted, want)
	}
	for _, k := range got {
		if _, ok := ClassOf(k); !ok {
			t.Errorf("%s has no class", k)
		}
	}
}

// The amendment's partition: only a rule that removes or caps calls (the branch: which tools may be
// called; the parallelism: how many calls of one tool) can be read against a recorded trace, with
// the stop class of the run as what it moves. Everything that changes arguments or what the seat
// reads needs the seat.
func TestOnlyBranchAndParallelismRulesAreReplayScorable(t *testing.T) {
	replay := map[string]bool{"deny_tools": true, "allow_tools": true, "max_calls_per_tool": true}
	for _, k := range envRuleKeys(t) {
		class, _ := ClassOf(k)
		if replay[k] && class != ClassReplay {
			t.Errorf("%s is %q, want %q", k, class, ClassReplay)
		}
		if !replay[k] && class != ClassLiveAB {
			t.Errorf("%s is %q, want %q: it changes what the seat reads or is asked, so a recorded trace cannot say what it would do next", k, class, ClassLiveAB)
		}
	}
}

func TestLiveABKeysListsWhatARuleSetNeedsTheSeatFor(t *testing.T) {
	r := loadFixture(t, "proposed.json").Rules
	got := LiveABKeys(&r)
	want := []string{"arg_limits", "max_observation_tokens", "observation_strip", "rewrite_error"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("proposed.json needs a live A/B for %v, want %v", got, want)
	}
	branch := core.AgentEnvRules{DenyTools: []string{"web_fetch"}, MaxCallsPerTool: map[string]int{"grep": 3}}
	if got := LiveABKeys(&branch); len(got) != 0 {
		t.Fatalf("a branch-and-parallelism rule set needs no live A/B, got %v", got)
	}
	if got := LiveABKeys(nil); len(got) != 0 {
		t.Fatalf("a nil rule set needs none, got %v", got)
	}
}
