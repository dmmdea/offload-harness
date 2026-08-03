package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediaseat"
	"github.com/dmmdea/offload-harness/internal/servingtmpl"
	"github.com/dmmdea/offload-harness/internal/tierseed"
)

// The harness config and the serving config are two artifacts written by two
// different commands from ONE table, and until this test existed nothing checked
// them against each other. That gap shipped: config.Default() bound
// stt_model="whisper-stt" while not one of the six serving templates defined a
// whisper seat, so a freshly installed node advertised `stt` to the fleet
// dispatcher (internal/fleetnode/tasks.go gates on STTModel != "") and failed its
// own acceptance gate (aliasCheck2) at the same time — out of the box, on every
// tier, with no media seat involved.
//
// The invariant: for every tier, and every OS the tier has a template for, every
// non-empty model alias the config binds must be a seat the rendered serving
// config actually defines. A binding with no seat is a phantom capability, which
// is the exact class internal/mediacap was built to end for FILE-backed routes —
// this is its counterpart for ALIAS-backed ones.

// servedAliases returns every id an llama-swap config answers to: the model keys
// plus each seat's declared aliases. Deliberately a small line scanner rather
// than a YAML dependency — it reads the same shape servingtmpl emits, and a
// parser that drifts from the emitter would defeat the point of the check.
func servedAliases(rendered string) map[string]bool {
	var (
		out     = map[string]bool{}
		modelRe = regexp.MustCompile(`^ {2}"?([A-Za-z0-9._-]+)"?:\s*$`)
		aliasRe = regexp.MustCompile(`^\s+aliases:\s*\[(.*)\]`)
		inModel bool
	)
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(line, "models:") {
			inModel = true
			continue
		}
		// Any other column-0 key ends the models block.
		if inModel && len(line) > 0 && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "#") {
			inModel = false
		}
		if !inModel {
			continue
		}
		if m := modelRe.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
			continue
		}
		if m := aliasRe.FindStringSubmatch(line); m != nil {
			for _, a := range strings.Split(m[1], ",") {
				if a = strings.Trim(strings.TrimSpace(a), `"'`); a != "" {
					out[a] = true
				}
			}
		}
	}
	return out
}

// effectiveConfig reproduces what a node actually runs with: config.Load starts
// from Default() and unmarshals the installed file over it, and `install seed`
// writes that file from the tier's resolved seed. Anything Default() binds and
// the seed does not clear survives onto the box — which is how the phantom got
// there.
func effectiveConfig(t *testing.T, prof tierseed.Profile, id, goos string) config.Config {
	t.Helper()
	cfg := config.Default()
	seed, err := tierseed.Resolve(prof, id, tierseed.Options{Home: "/opt/offload", GOOS: goos})
	if err != nil {
		t.Fatalf("tier %s: resolving the seed: %v", id, err)
	}
	if len(seed) == 0 {
		return cfg
	}
	b, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("tier %s: applying the seed the way config.Load does: %v", id, err)
	}
	return cfg
}

func renderParams(prof servingProfile, goos string) servingtmpl.Params {
	// No ram tier: the closure gate asks whether a BOUND alias is served, and the RAM
	// gate only ever removes the 26B — which no MEDIA binding routes to. The AGENT
	// seat (agent_model) CAN route to the 26B, and that is exactly what this ungated
	// widest-roster render cannot see; the low-RAM guard for it is the dedicated
	// table lint below (TestNo26BAgentSeatOnRAMDroppable26B). Passing "" is the
	// ungated case, i.e. the widest roster a tier can render.
	moe, include26B := moePlacement(prof, "")
	return servingtmpl.Params{
		LlamaBin: "/opt/offload/bin", ModelsDir: "/opt/offload/models",
		Listen: "127.0.0.1:11436", Ctx: prof.CtxSize, KVType: prof.KVType,
		FlashAttn: prof.FlashAttn, MoE26B: moe, Threads: 8,
		Include26B: include26B,
		Seats:      prof.MediaSeats, Home: "/opt/offload", GOOS: goos, GPUEnv: prof.GPUEnv,
	}
}

// TestCommittedTierTableSeatsAreValid is the authoring-time gate: a bad seat dies
// in CI, not on a collaborator's machine where the symptom is a service that will
// not start or a route that quietly defers. It walks the table that actually
// ships (the embedded copy), so a hand edit to profiles.json cannot bypass it.
func TestCommittedTierTableSeatsAreValid(t *testing.T) {
	profiles, err := tierseed.Parse(embeddedProfiles)
	if err != nil {
		t.Fatal(err)
	}
	for id, p := range profiles {
		if err := mediaseat.Validate(p.MediaSeats, id); err != nil {
			t.Error(err)
		}
	}
}

// TestNo26BAgentSeatOnRAMDroppable26B is the low-RAM guard the ungated-roster
// gate below cannot provide: renderParams passes ramTier "" (the widest roster),
// so a 26B whose placement the RAM gate drops on a low/min box (moe_26b cpu_moe
// or n_cpu_moe) still renders as served there. The agent seat is the one config
// binding that routes to the 26B, and low/min RAM has no seed overlay to blank
// it (config_seed_ram_mid_high applies only to mid/high) — so the TABLE itself
// must never pair a derived-or-seeded 26B agent seat with a RAM-droppable 26B.
// Only moe_26b "gpu" survives every ram tier; anything else can vanish from the
// roster while agent_model keeps naming it, and every run then fails loud.
func TestNo26BAgentSeatOnRAMDroppable26B(t *testing.T) {
	seedProfiles, err := tierseed.Parse(embeddedProfiles)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]servingProfile `json:"profiles"`
	}
	if err := json.Unmarshal(embeddedProfiles, &doc); err != nil {
		t.Fatal(err)
	}
	for id, sp := range doc.Profiles {
		for _, goos := range []string{"linux", "windows"} {
			cfg := effectiveConfig(t, seedProfiles[id], id, goos)
			if agent := cfg.AgentPlannerModel(""); agent == "gemma4-26b-a4b" && sp.MoE26B != "gpu" {
				t.Errorf("tier %s (%s): the resolved agent seat is %q but moe_26b is %q — on a low/min-RAM "+
					"box the RAM gate drops the 26B from the served roster while agent_model still names it, "+
					"and low/min RAM has no overlay to blank the seat. Pin the 26B (moe_26b \"gpu\") or seat a "+
					"RAM-independent model.", id, goos, agent, sp.MoE26B)
			}
		}
	}
}

func TestEveryBoundAliasIsServed(t *testing.T) {
	seedProfiles, err := tierseed.Parse(embeddedProfiles)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]servingProfile `json:"profiles"`
	}
	if err := json.Unmarshal(embeddedProfiles, &doc); err != nil {
		t.Fatal(err)
	}

	for id, sp := range doc.Profiles {
		for _, goos := range []string{"linux", "windows"} {
			tmpl, err := templateFor(goos, sp.Backend)
			if err != nil {
				continue // this tier has no template for this OS; not this test's business
			}
			rendered, err := servingtmpl.Render(tmpl, renderParams(sp, goos))
			if err != nil {
				// A tier that declares seats against a template with no seat mapping
				// REFUSES by name — that pair produces no config at all, so there is no
				// closure to check. It is logged, not passed over in silence: it is a
				// real first-class gap, and the refusal is what keeps it from becoming a
				// node that quietly lost a capability.
				if len(sp.MediaSeats) > 0 && strings.Contains(err.Error(), "offload-seats") {
					// The refusal only protects the node if the OTHER half refuses too.
					// If `install seed` still wrote the bindings for this pair, the node
					// would end up with a config naming aliases nothing serves — the very
					// state this gate exists to prevent, arrived at by a different route.
					if placeErr := seatsPlaceable(goos, sp.Backend); placeErr == nil {
						t.Errorf("tier %s (%s/%s): render refuses the seats but install seed would still write "+
							"their bindings — the two halves must refuse together", id, goos, sp.Backend)
					}
					t.Logf("gap (both halves refuse, as they must): tier %s has no seat-capable template for %s/%s",
						id, goos, sp.Backend)
					continue
				}
				t.Errorf("tier %s (%s/%s): rendering: %v", id, goos, sp.Backend, err)
				continue
			}
			served := servedAliases(rendered)
			cfg := effectiveConfig(t, seedProfiles[id], id, goos)
			for _, a := range modelAliases(cfg) {
				if a.Alias == "" {
					continue // an unset binding is an honest "this route defers"
				}
				if !served[a.Alias] {
					t.Errorf("tier %s (%s/%s): config binds %s=%q but the rendered serving config "+
						"defines no such seat — the node would advertise this route to the fleet and "+
						"fail its own acceptance gate. Either the tier must declare a seat for it or "+
						"the binding must be %q.", id, goos, sp.Backend, a.Key, a.Alias, "")
				}
			}
		}
	}
}
