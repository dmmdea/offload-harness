package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A tier's declared LAYERS are a capability, and on 2026-09-18 the ampere-16 reference box
// gained one — the `fast` digest layer and the 35B seat behind it — that lived only in
// that box's hand-edited config and in two Go test files. profiles.json never carried it,
// so every fresh ampere-16 install lost it and the tier's generated page could not state
// it: the defect class ADR 0048 was written against, one level up from the vLLM seat.
//
// The gates that already exist could not see it, for the reason ADR 0048 gives for the
// seat gates: they validate the layers that are DECLARED, and a layer that is not declared
// is not iterated. A tier that loses a layer falls out of the check instead of failing it.
// This gate asserts the SET, exactly as TestEveryTierCanSeatAModelUnderVLLM does for vLLM
// seats: layerSetTiers is a regression floor recording which layers each composite tier
// declares and the roles each layer serves, and a tier that stops declaring one fails by
// name.
//
// It is exact in both directions on purpose. Losing a layer or one of its seat roles is a
// REGRESSION (removed only on an explicit operator instruction, with the entry edited here
// in the same change so the deletion is visible in review). Gaining one without registering
// it is a failure too: an unregistered layer is one the floor does not protect, and the next
// silent deletion would pass.
//
// The roles are the capability; the models are not. Swapping the model behind a seat is a
// measured decision and never a reason to touch this table (ADR 0048: change what is inside
// the declaration, never delete it).
var layerSetTiers = map[string]map[string][]string{
	"ampere-16": {
		// The tier's planner default: the 27B GSQ seat. It answers every agent contract the
		// node takes.
		"single": {"agent"},
		// The fast DIGEST layer (register A-100): the 35B-A3B seat, reachable by name only.
		"fast": {"agent"},
	},
	"blackwell-3x16": {
		"single":  {"agent", "ocr", "router", "stt"},
		"pair":    {"agent", "long", "vision"},
		"display": {"router"},
		"triple":  {"agent"},
	},
}

// declaredLayers reads tier -> layer -> sorted seat roles straight from the JSON, not
// through the tier parser: a gate that trusts the code it guards goes blind with it.
func declaredLayers(t *testing.T) map[string]map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			Layers []struct {
				Name  string `json:"name"`
				Seats []struct {
					Role string `json:"role"`
				} `json:"seats"`
			} `json:"layers"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	if len(doc.Profiles) == 0 {
		t.Fatal("no profiles parsed — the schema moved and this gate went blind")
	}
	out := map[string]map[string][]string{}
	for tier, p := range doc.Profiles {
		if len(p.Layers) == 0 {
			continue
		}
		out[tier] = map[string][]string{}
		for _, l := range p.Layers {
			roles := []string{}
			for _, s := range l.Seats {
				roles = append(roles, s.Role)
			}
			sort.Strings(roles)
			out[tier][l.Name] = roles
		}
	}
	return out
}

func TestEveryTierKeepsItsDeclaredLayerSet(t *testing.T) {
	declared := declaredLayers(t)
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tiers struct {
		Profiles map[string]json.RawMessage `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &tiers); err != nil {
		t.Fatal(err)
	}

	for _, tier := range sortedKeys(layerSetTiers) {
		want := layerSetTiers[tier]
		if _, present := tiers.Profiles[tier]; !present {
			t.Errorf("layerSetTiers names tier %s, which profiles.json no longer has — a tier is never deleted "+
				"as a side effect; if this is intended the operator says so and the entry goes with it", tier)
			continue
		}
		have := declared[tier]
		if len(have) == 0 {
			t.Errorf("REGRESSION: tier %s declared layers %v and declares none now. A tier never loses its layers as "+
				"a side effect of changing a model: a fresh install of it would seed no composite identity, no layer "+
				"placement and no fast lane, and nothing else would notice. If the removal is intended the operator "+
				"says so and the entry leaves layerSetTiers in the same change.", tier, sortedKeys(want))
			continue
		}
		for _, layer := range sortedKeys(want) {
			roles, present := have[layer]
			if !present {
				t.Errorf("REGRESSION: tier %s declared layer %q (roles %v) and no longer does — its declared layers are now %v. "+
					"Deleting a layer deletes the capability it routes to (and every contract that names it defers); the "+
					"reference box would keep serving it from a hand-edited config while every fresh install loses it. "+
					"If the removal is intended the operator says so and the layer leaves layerSetTiers in the same change.",
					tier, layer, want[layer], sortedKeys(have))
				continue
			}
			if got, exp := strings.Join(roles, ","), strings.Join(want[layer], ","); got != exp {
				missing, extra := diffRoles(want[layer], roles)
				if len(missing) > 0 {
					t.Errorf("REGRESSION: tier %s layer %q lost its %v seat(s) (declares %v, floor %v)", tier, layer, missing, roles, want[layer])
				}
				if len(extra) > 0 {
					t.Errorf("tier %s layer %q gained seat role(s) %v that layerSetTiers does not list — register them so the "+
						"floor protects them", tier, layer, extra)
				}
			}
		}
		for _, layer := range sortedKeys(have) {
			if _, tracked := want[layer]; !tracked {
				t.Errorf("tier %s declares layer %q that layerSetTiers does not list — register it (roles %v) so the floor "+
					"protects it from a silent deletion", tier, layer, have[layer])
			}
		}
	}

	for _, tier := range sortedKeys(declared) {
		if _, tracked := layerSetTiers[tier]; !tracked {
			t.Errorf("tier %s declares layers %v but layerSetTiers does not track it. A composite tier must be registered "+
				"here: an unregistered one is exactly the tier whose layer can be deleted without anything failing.",
				tier, sortedKeys(declared[tier]))
		}
	}
	t.Logf("layer coverage: %d tier(s) declare layers, %d tracked", len(declared), len(layerSetTiers))
}

// TestTierPagesNameTheirLayers ties the gate to the page an operator reads. The pages are
// GENERATED from the table, so TestTierDocsAreCurrent cannot notice a generator that stopped
// rendering the layers: it would regenerate a page without them and call it current.
func TestTierPagesNameTheirLayers(t *testing.T) {
	for _, tier := range sortedKeys(layerSetTiers) {
		page, err := os.ReadFile(filepath.Join("docs", "tiers", tier+".md"))
		if err != nil {
			t.Errorf("tier %s: %v", tier, err)
			continue
		}
		text := string(page)
		for _, layer := range sortedKeys(layerSetTiers[tier]) {
			if !strings.Contains(text, "| `"+layer+"` |") {
				t.Errorf("docs/tiers/%s.md has no row for layer `%s` — the page cannot state a layer the table declares; "+
					"run `go generate ./...` (and if it still does not appear, the generator dropped the layer table)", tier, layer)
			}
		}
	}
}

func diffRoles(want, have []string) (missing, extra []string) {
	in := func(list []string, s string) bool {
		for _, x := range list {
			if x == s {
				return true
			}
		}
		return false
	}
	for _, w := range want {
		if !in(have, w) {
			missing = append(missing, w)
		}
	}
	for _, h := range have {
		if !in(want, h) {
			extra = append(extra, h)
		}
	}
	return missing, extra
}

// extraSeatFloor is the same regression floor for the tier's EXTRA vLLM seats, and it exists
// because of how the layer resolution works: a layer seat is dropped on a box that does not run
// the vLLM seat it NAMES, but only a seat the table still DECLARES can be recognised as a vLLM
// seat. Delete the extra seat's declaration and keep the layer, and the layer's model is just a
// name — seeded on every box, including the ones that cannot serve it. So the declaration is
// pinned here, and each pinned seat must still be named by a layer.
var extraSeatFloor = map[string][]string{
	"ampere-16": {"qwen36-35b-a3b-gsq-vllm"},
}

func TestEveryTierKeepsItsExtraVLLMSeatsAndTheLayersThatNameThem(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			Extra []struct {
				ID      string   `json:"id"`
				Aliases []string `json:"aliases"`
			} `json:"extra_vllm_seats"`
			Layers []struct {
				Seats []struct {
					Model string `json:"model"`
				} `json:"seats"`
			} `json:"layers"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, tier := range sortedKeys(extraSeatFloor) {
		p, present := doc.Profiles[tier]
		if !present {
			t.Errorf("extraSeatFloor names tier %s, which profiles.json no longer has", tier)
			continue
		}
		named := map[string]bool{}
		for _, l := range p.Layers {
			for _, s := range l.Seats {
				named[s.Model] = true
			}
		}
		// A layer names its seat by id or by alias, so a seat counts as routed-to when either does.
		declared, routed := map[string]bool{}, map[string]bool{}
		for _, e := range p.Extra {
			declared[e.ID] = true
			routed[e.ID] = named[e.ID]
			for _, a := range e.Aliases {
				routed[e.ID] = routed[e.ID] || named[a]
			}
		}
		for _, id := range extraSeatFloor[tier] {
			if !declared[id] {
				t.Errorf("REGRESSION: tier %s declared extra vLLM seat %s and no longer does. Its layer would then be seeded "+
					"on boxes that cannot serve it (a name nothing recognises as a vLLM seat is never dropped), and every "+
					"fresh install loses the seat. If the removal is intended the operator says so and the entry leaves "+
					"extraSeatFloor in the same change.", tier, id)
				continue
			}
			if !routed[id] {
				t.Errorf("tier %s declares extra vLLM seat %s but no layer names it (by id or alias): a seat nothing routes "+
					"to is a capability that exists only on paper", tier, id)
			}
		}
		for _, e := range p.Extra {
			tracked := false
			for _, id := range extraSeatFloor[tier] {
				tracked = tracked || id == e.ID
			}
			if !tracked {
				t.Errorf("tier %s declares extra vLLM seat %s that extraSeatFloor does not list — register it so the floor "+
					"protects it", tier, e.ID)
			}
		}
	}
	for tier, p := range doc.Profiles {
		if len(p.Extra) > 0 {
			if _, tracked := extraSeatFloor[tier]; !tracked {
				t.Errorf("tier %s declares extra vLLM seats but extraSeatFloor does not track it", tier)
			}
		}
	}
}
