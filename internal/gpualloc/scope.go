package gpualloc

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// LeaseScope says which seats sit on the cards a lease holds (plan P5, register C-86): a drain
// and an unload under a lease clear THOSE cards, not the node. A whole-node lease (no ids)
// reaches every seat. A model is placed by what the box declares: its `layers` seat pin, else the
// device pin of its entry in the serving config (internal/servingcfg, the same reader the load
// fence places a model with, Config.ModelPins). A model that neither places, a pin
// the card table cannot place, or a card table that cannot be read leaves the placement UNKNOWN,
// and the direction of every doubt is the whole-node behaviour: an unknown model is on the leased
// cards. What a positive placement buys (register F15) is that a model placed on ANOTHER card
// stays where it is.
type LeaseScope struct {
	cfg     config.Config
	ids     []string
	cards   []gpuprobe.Card
	cardsOK bool
	// cardsWhy says why the table is unusable: the read's error, or that it listed no card.
	cardsWhy string
	// holdsAll: the lease names every card the table lists, so it holds the whole node however it is
	// spelled (a one-card box's only card, a lease on all three).
	holdsAll bool
	// serving is the serving config, read lazily and once, only for a model layers do not declare.
	serving *servingState
}

// NewLeaseScope reads the card table once (deps.CardTable, which retries a read that ran out of
// time) for a lease holding ids (lease ids). A table it cannot read is said once on warn (nil =
// silent), and every model is then treated as sitting on the leased cards.
func NewLeaseScope(ctx context.Context, cfg config.Config, ids []string, deps Deps, warn io.Writer) LeaseScope {
	s := LeaseScope{cfg: cfg, ids: ids}
	if len(ids) == 0 {
		return s
	}
	s.serving = &servingState{path: cfg.ServingConfigPath}
	cards, _, err := deps.CardTable(ctx, cfg)
	s.cards, s.cardsOK = cards, err == nil && len(cards) > 0
	if !s.cardsOK {
		s.cardsWhy = "no card was listed"
		if err != nil {
			s.cardsWhy = err.Error()
		}
		if warn != nil {
			// The fallback is right (every doubt fences) and silent, until now: the operator would
			// believe a seat on another card was spared.
			fmt.Fprintf(warn, "gpu reserve: the card table could not be read (%s): every seat is treated as sitting on the leased cards, so the drain and unload act as for the whole node\n", s.cardsWhy)
		}
	}
	if s.cardsOK {
		held := map[string]bool{}
		for _, id := range ids {
			held[strings.ToLower(strings.TrimSpace(id))] = true
		}
		s.holdsAll = true
		for _, c := range cards {
			if !held[c.LeaseID()] {
				s.holdsAll = false
				break
			}
		}
	}
	return s
}

// Bounded reports whether the lease holds a set of cards (false = the whole node).
func (s LeaseScope) Bounded() bool { return len(s.ids) > 0 }

// TouchesPins reports whether a seat pinned to pins sits on the leased cards.
func (s LeaseScope) TouchesPins(pins []string) bool {
	if !s.Bounded() || !s.cardsOK {
		return true
	}
	rids, ok := gpulease.ResolvePins(pins, s.cards)
	if !ok {
		return true
	}
	return gpulease.Info{Held: true, Devices: s.ids}.Touches(rids)
}

// What placed a model (Located.By).
const (
	byWholeNode = "a lease that holds every card"
	byLayers    = "layers"
	byServing   = "the serving config pin"
)

// Located says where one model sits. Known is false when nothing places it, and then Why says
// what is missing; On says whether it sits on a leased card, and By what placed it.
type Located struct {
	On    bool
	Known bool
	By    string
	Why   string
}

// Locate places the model serving name: by layers when they declare it, else by its serving config
// pin. Unknown is not an error; it is what the callers read as "on the leased cards".
func (s LeaseScope) Locate(model string) Located {
	if !s.Bounded() || s.holdsAll {
		return Located{On: true, Known: true, By: byWholeNode}
	}
	if !s.cardsOK {
		return Located{Why: "the card table could not be read (" + s.cardsWhy + ")"}
	}
	leased := gpulease.Info{Held: true, Devices: s.ids}
	if pins, ok := s.cfg.LayerPins(model); ok {
		rids, rok := gpulease.ResolvePins(pins, s.cards)
		if !rok {
			return Located{Why: "layers pin it to a card the card table cannot place"}
		}
		return Located{On: leased.Touches(rids), Known: true, By: byLayers}
	}
	st := s.serving
	st.load()
	if st.problem != "" {
		return Located{Why: st.problem}
	}
	pin, ok := st.snap.Pin(model)
	if !ok {
		return Located{Why: "layers do not declare it and the serving config does not list it"}
	}
	if len(pin.Pins) == 0 {
		return Located{Why: pin.Why}
	}
	rids, rok := gpulease.ResolvePins(pin.Pins, s.cards)
	if !rok {
		return Located{Why: "its serving config pin names a card the card table cannot place"}
	}
	return Located{On: leased.Touches(rids), Known: true, By: byServing}
}

// Touches reports whether the model serving name sits on the leased cards. A model placed on
// another card does not; one that is not placed does (the drain waits, the fence holds, the unload
// takes it).
func (s LeaseScope) Touches(model string) bool {
	l := s.Locate(model)
	return !l.Known || l.On
}

// TouchesRun reports whether a registered run is on the leased cards: by the pins it recorded,
// else by the seat it runs on.
func (s LeaseScope) TouchesRun(r gpuactivity.Run) bool {
	if len(r.Devices) > 0 {
		return s.TouchesPins(r.Devices)
	}
	return s.Touches(r.Seat)
}

// Verdict is what an unload decides for one model, and why.
type Verdict struct {
	Model  string
	Unload bool
	Why    string
}

// Judge decides, for each model, whether a lease on these cards unloads it. A model placed on a
// leased card goes; a model placed on another card stays (register F15); a model that is not placed
// goes too, because an unknown placement is read as "on the leased cards" and the render needs its
// card. The memory stack is the caller's to exclude.
func (s LeaseScope) Judge(models []string) []Verdict {
	out := make([]Verdict, 0, len(models))
	for _, m := range models {
		l := s.Locate(m)
		switch {
		case !l.Known:
			out = append(out, Verdict{Model: m, Unload: true, Why: "placement unknown (" + l.Why + ")"})
		case l.On:
			out = append(out, Verdict{Model: m, Unload: true, Why: "placed on a leased card by " + l.By})
		default:
			out = append(out, Verdict{Model: m, Why: "placed elsewhere by " + l.By})
		}
	}
	return out
}

// Unloading is the models of the verdicts that unload, in their order.
func Unloading(vs []Verdict) []string {
	var out []string
	for _, v := range vs {
		if v.Unload {
			out = append(out, v.Model)
		}
	}
	return out
}

// Explain turns verdicts into lines that name what was unloaded and what was left, with the
// reason, one line per distinct reason in the order they first appear.
func Explain(vs []Verdict) []string {
	type group struct {
		line   string
		models []string
	}
	var order []string
	groups := map[string]*group{}
	for _, v := range vs {
		verb := "left resident, "
		if v.Unload {
			verb = "unloaded, "
		}
		key := verb + v.Why
		g, ok := groups[key]
		if !ok {
			g = &group{line: key}
			groups[key] = g
			order = append(order, key)
		}
		g.models = append(g.models, v.Model)
	}
	lines := make([]string, 0, len(order))
	for _, k := range order {
		lines = append(lines, groups[k].line+": "+strings.Join(groups[k].models, ", "))
	}
	return lines
}

// EffectiveMemoryStack is the memory stack a lease never unloads: the config's, else the default.
func EffectiveMemoryStack(cfg config.Config) []string {
	if len(cfg.MemoryStack) == 0 {
		return config.Default().MemoryStack
	}
	return cfg.MemoryStack
}

// UnloadModels is the model-id list the render lane's freeLlamaSwap unloads under a lease that
// holds leaseIDs: every model of the roster except the memory stack and the models POSITIVELY
// placed on another card (register F15). A model placed on a leased card goes, and so does a model
// whose placement is unknown (not declared by layers, no usable pin in the serving config, a pin
// the card table cannot place, a card table that cannot be read): the render needs its card, and
// that is the direction every doubt took before F15. warn, when not nil, names what was unloaded and
// what was left, each with its reason.
//
// The candidates are the llama-swap roster. When the roster was read and is empty nothing is
// resident, and the candidates are the models the config declares (Config.LayerSeatModels) and the
// serving config lists, said so.
//
// rosterErr is the error of the roster read, nil when it was read. A roster that could NOT be read is
// not an empty roster: nothing says what is resident, so no candidate list built from the config can
// be complete (a resident model neither the layers nor the serving config name would never be
// judged), and on a box that declares neither the list would be empty and nothing would be unloaded.
// ok is then false, whatever the config declares, and the render lane keeps its own rule: every model
// off the memory stack, as before F15. F15's per-card judgement applies to a roster that was read.
//
// ok is false for a whole-node lease (no ids): it holds every card, so nothing narrows and the
// render lane keeps its own rule. A lease that names every card the box has is the whole node too.
// ok is false for a card lease too when the roster could not be read (above). Otherwise a card lease
// always has a list, empty when nothing would be unloaded (exported as the none sentinel by
// UnloadEnv).
func UnloadModels(ctx context.Context, cfg config.Config, roster []string, rosterErr error, leaseIDs []string, deps Deps, warn io.Writer) ([]string, bool) {
	if len(leaseIDs) == 0 {
		return nil, false
	}
	if rosterErr != nil {
		if warn != nil {
			fmt.Fprintf(warn, "unload list: the llama-swap roster could not be read (%v), so nothing says what is resident: no list is made and the render lane keeps its own rule (it unloads every model off the memory stack)\n", rosterErr)
		}
		return nil, false
	}
	candidates, fromConfig := roster, false
	if len(candidates) == 0 {
		candidates = append(cfg.LayerSeatModels(), servingModelIDs(cfg.ServingConfigPath)...)
		fromConfig = true
	}
	keep := map[string]bool{}
	for _, m := range EffectiveMemoryStack(cfg) {
		keep[strings.ToLower(strings.TrimSpace(m))] = true
	}
	var models []string
	seen := map[string]bool{}
	for _, id := range candidates {
		k := strings.ToLower(strings.TrimSpace(id))
		if k == "" || keep[k] || seen[k] {
			continue
		}
		seen[k] = true
		models = append(models, id)
	}
	if fromConfig && warn != nil {
		fmt.Fprintf(warn, "unload list: the llama-swap roster is empty, so the candidates are the models the config declares and the serving config lists\n")
	}
	if len(models) == 0 {
		return nil, true
	}
	scope := NewLeaseScope(ctx, cfg, leaseIDs, deps, nil)
	vs := scope.Judge(models)
	if warn != nil {
		for _, line := range Explain(vs) {
			fmt.Fprintf(warn, "unload list: %s\n", line)
		}
	}
	return Unloading(vs), true
}

// UnloadEnv renders the list as the environment variable the render lane reads
// (GPU_LEASE_UNLOAD_MODELS): a comma list, `-` for "unload nothing", and no variable at all
// when the list is unknown, which leaves the lane on its own rule.
func UnloadEnv(models []string, known bool) string {
	if !known {
		return ""
	}
	if len(models) == 0 {
		return "GPU_LEASE_UNLOAD_MODELS=-"
	}
	return "GPU_LEASE_UNLOAD_MODELS=" + strings.Join(models, ",")
}
