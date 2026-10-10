package mediaremote

// Image identity on the delegator's side (ADR 0082): what the family a request names IS on this machine, and whether a
// node's published recipes include it. The comparison is strict: a node either holds a family whose recipe digests
// to this machine's, or it is not a candidate, and the answer says which key differs. Nothing is ever substituted, so
// a caller who accepts another build names that family himself.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// localID is the identity of the family a request resolves to on this machine.
type localID struct {
	// name is what the request named ("" = this machine's default binding).
	name   string
	recipe mediacap.Recipe
	// others is the recipe of every other image family this machine binds, by name: a node that misses the family may
	// match another of them, and the answer then says which to name. It stats each family's weight files, so it is read
	// only when a node misses (once per call), never on the path of a match.
	others func() map[string]mediacap.Recipe
}

// localIdentity resolves family on this machine, the way the pipeline resolves it ("" or the default binding's own
// family name is the DEFAULT binding, so a call that names none is never taken for a node's own default). ok is false,
// with the reason, when there is no recipe to compare: an unknown family, an sd.cpp binding, or a binding whose files
// are not all here.
func localIdentity(cfg config.Config, family string) (localID, bool, string) {
	eff, fi, err := cfg.ResolveImageFamily(family)
	if err != nil {
		return localID{}, false, err.Error()
	}
	r, ok := mediacap.ImageRecipe(eff, fi, mediacap.ModelRoots(eff.ComfyDir), nil)
	if !ok {
		return localID{}, false, "this binding is not a ComfyUI one, which has no recipe to compare"
	}
	if r.Graph == "" || len(r.Files) == 0 {
		return localID{}, false, "this binding names no weight file, so there is nothing to identify it by"
	}
	if missing := r.Missing(); len(missing) > 0 {
		return localID{}, false, "this machine is missing " + strings.Join(missing, ", ")
	}
	var (
		memo map[string]mediacap.Recipe
		done bool
	)
	others := func() map[string]mediacap.Recipe {
		if done {
			return memo
		}
		memo, done = map[string]mediacap.Recipe{}, true
		for _, fi := range cfg.ImageFamilies() {
			o, _, oerr := cfg.ResolveImageFamily(fi.Name)
			if oerr != nil {
				continue
			}
			if or, ok := mediacap.ImageRecipe(o, fi, mediacap.ModelRoots(o.ComfyDir), nil); ok && len(or.Missing()) == 0 {
				memo[fi.Name] = or
			}
		}
		return memo
	}
	return localID{name: strings.TrimSpace(family), recipe: r, others: others}, true, ""
}

// need is what one call asks of a node beyond the recipe.
type need struct {
	// refineOff: the caller sent refine=false, which only a node that carries it may be given.
	refineOff bool
	// steps: the caller sent a per-request step count.
	steps bool
}

func needOf(params map[string]any) need {
	_, steps := posInt(params, "steps")
	return need{refineOff: refineOff(params), steps: steps}
}

// nodeMatch is the verdict of one node's published recipes against this machine's.
type nodeMatch struct {
	// family is the node's own name for the matching recipe: the family the job is sent under.
	family string
	// why and differs say why there is none; state is the cluster row's state for it.
	why     string
	differs []string
	state   string
}

// matchNode compares what node v publishes with this machine's recipe. ok means v holds a family that digests to it,
// that carries what the call asks, and whose route (when the node reports its routes) is configured.
func matchNode(local localID, v delegate.NodeView, n need) (m nodeMatch, ok bool) {
	miss := func(why string, differs ...string) (nodeMatch, bool) {
		return nodeMatch{why: why, differs: differs, state: "not-capable"}, false
	}
	if !contains(v.Tasks, taskImage) {
		return miss("does not serve " + taskImage)
	}
	if len(v.ImageRecipes) == 0 {
		return miss("publishes no image recipe (a harness older than this one, or no ComfyUI family that names a checkpoint), so it cannot be matched")
	}
	if v.HarnessVersion != localVersion {
		return miss(fmt.Sprintf("runs harness %s and this machine %s: a recipe is only compared between the same release (the graph builders ship with the version)", shownVersion(v.HarnessVersion), localVersion))
	}
	if n.refineOff && !v.RefineHonoured {
		return miss("does not honour refine=false (an older harness would refine the prompt anyway); send without it, or render it on a node that does")
	}
	want := local.recipe.Digest()
	var equal []delegate.ImageRecipeView
	for _, row := range v.ImageRecipes {
		if row.Digest == want {
			equal = append(equal, row)
		}
	}
	if len(equal) == 0 {
		return closest(local, v)
	}
	var reasons []string
	for _, row := range equal {
		if why := rowUnusable(row, v, n); why != "" {
			reasons = append(reasons, why)
			continue
		}
		return nodeMatch{family: row.Name}, true
	}
	return miss(strings.Join(reasons, "; "))
}

// rowUnusable says why a node's matching family cannot take this call, "" when it can.
func rowUnusable(row delegate.ImageRecipeView, v delegate.NodeView, n need) string {
	route := "generate_image"
	if !row.Default {
		route = mediacap.ImageFamilyRoute(row.Name)
	}
	if st, known := v.RouteState(route); known && st != delegate.MediaRouteConfigured {
		return fmt.Sprintf("its family %q matches, but its route %s is %s on that node", row.Name, route, st)
	}
	r := recipeOfView(row)
	if n.steps && r.PairsStepsAndCFG() && !r.SetsCFG() {
		return fmt.Sprintf("its family %q matches, but a per-request steps needs imagegen_cfg set in that node's binding (this graph takes steps and cfg together, and the node leaves cfg to the builder)", row.Name)
	}
	return ""
}

// closest words the miss of a node none of whose families digests alike: the family that differs least, in which keys,
// and whether another family of THIS machine would match one of the node's.
func closest(local localID, v delegate.NodeView) (nodeMatch, bool) {
	best, bestDiff := -1, []string(nil)
	for i, row := range v.ImageRecipes {
		d := local.recipe.Diff(recipeOfView(row))
		if best < 0 || len(d) < len(bestDiff) {
			best, bestDiff = i, d
		}
	}
	row := v.ImageRecipes[best]
	why := fmt.Sprintf("no family on that node matches: its %s differs in %s", familyLabel(row), strings.Join(bestDiff, "; "))
	if len(bestDiff) == 0 {
		why = fmt.Sprintf("no family on that node matches: its %s digests differently (a recipe version this machine does not compare)", familyLabel(row))
	}
	if hint := alternative(local, v); hint != "" {
		why += "; " + hint
	}
	return nodeMatch{why: why, differs: bestDiff, state: "not-capable"}, false
}

// alternative names a family of this machine that DOES match one of the node's, so the caller can ask for that build
// knowingly: "send family=<ours> to use its <theirs>".
func alternative(local localID, v delegate.NodeView) string {
	others := local.others()
	names := make([]string, 0, len(others))
	for name := range others {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == local.name {
			continue
		}
		for _, row := range v.ImageRecipes {
			if row.Digest == others[name].Digest() {
				return fmt.Sprintf("send family=%s to use its %s", name, familyLabel(row))
			}
		}
	}
	return ""
}

func familyLabel(row delegate.ImageRecipeView) string {
	if row.Name == "" {
		return "default binding"
	}
	return row.Name
}

func shownVersion(v string) string {
	if v == "" {
		return "(none published)"
	}
	return v
}

// recipeOfView rebuilds the recipe a node published from its row, for comparison and for the rules that read it.
func recipeOfView(v delegate.ImageRecipeView) mediacap.Recipe {
	r := mediacap.Recipe{
		Engine: v.Engine, Graph: v.Graph, Preset: v.Preset, Steps: v.Steps, CFG: v.CFG, Sampler: v.Sampler, Scheduler: v.Scheduler,
		Schedule: v.Schedule, Shift: v.Shift, LoRAStrength: v.LoRAStrength, License: v.License, CommercialUse: v.CommercialUse,
		Explicit: v.Explicit,
	}
	for _, f := range v.Files {
		r.Files = append(r.Files, mediacap.RecipeFile{Role: f.Role, Name: f.Name, Bytes: f.Bytes})
	}
	return r
}

// leaseWords says what holds a node, in the node's own terms.
func leaseWords(v delegate.NodeView) string {
	who := gpulease.Class(v.LeaseClass).LeasePhrase()
	s := "holds " + who
	if v.LeaseReason != "" {
		s += fmt.Sprintf(" (%q)", v.LeaseReason)
	}
	if v.LeaseRemainingSec > 0 {
		s += fmt.Sprintf(", about %ds of its declared term left", v.LeaseRemainingSec)
	}
	return s
}
