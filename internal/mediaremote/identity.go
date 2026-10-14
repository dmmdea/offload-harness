package mediaremote

// Image identity on the delegator's side (ADR 0082): what the family a request names IS on this machine, and whether a
// node's published recipes include it. The comparison is strict: a node either holds a family whose recipe digests
// to this machine's, or it is not a candidate, and the answer says which key differs. Nothing is ever substituted, so
// a caller who accepts another build names that family himself.
//
// A recipe is compared on CONTENT as well as name and size (F59): each present weight file carries a fingerprint
// (mediacap/fingerprint.go). A machine or a node that has not got one for every present file has no identity yet, and
// that is a named miss (never a match by size, and never two "pending" recipes taken for one).

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// localFingerprints is how this machine's own recipe reads the content of its weight files: through the process-wide
// cache, waiting for a file it has not hashed, for as long as the call's context and the fingerprinter's bound allow (a
// file still unread then is "pending" and the family has no identity for this call). The delegator computes its
// fingerprints the way a node does. A var so a test can hold or fail a read.
var localFingerprints func(ctx context.Context, path string, size int64) string = mediacap.SharedFingerprints().AwaitContext

// localPeek is how the hint of this machine's OTHER families reads a weight file: from the cache only, never waiting (a
// family still being hashed is left out of the hint, and its hash is started for the next call).
var localPeek mediacap.FingerprintFunc = mediacap.SharedFingerprints().Peek

// localID is the identity of the family a request resolves to on this machine.
type localID struct {
	// name is what the request named ("" = this machine's default binding).
	name   string
	recipe mediacap.Recipe
	// others is the recipe of every other image family this machine binds, by name: a node that misses the family may
	// match another of them, and the answer then says which to name. It stats each family's weight files and takes their
	// fingerprints from the cache without waiting (localPeek), so it is read only when a node misses (once per call), never
	// on the path of a match, and never holds the ticket scan up on a hash: a family not hashed yet is not in it, and the
	// map is built again at the next miss until every family is (a ticket lives for hours; a hash lands in seconds).
	others func() map[string]mediacap.Recipe
}

// localIdentity resolves family on this machine, the way the pipeline resolves it ("" or the default binding's own
// family name is the DEFAULT binding, so a call that names none is never taken for a node's own default). ok is false,
// with the reason, when there is no recipe to compare: an unknown family, an sd.cpp binding, or a binding whose files
// are not all here or one of them cannot be fingerprinted.
func localIdentity(ctx context.Context, cfg config.Config, family string) (localID, bool, string) {
	eff, fi, err := cfg.ResolveImageFamily(family)
	if err != nil {
		return localID{}, false, err.Error()
	}
	await := func(path string, size int64) string { return localFingerprints(ctx, path, size) }
	r, ok := mediacap.ImageRecipeWith(eff, fi, mediacap.ModelRoots(eff.ComfyDir), nil, await)
	if !ok {
		return localID{}, false, "this binding is not a ComfyUI one, which has no recipe to compare"
	}
	if r.Graph == "" || len(r.Files) == 0 {
		return localID{}, false, "this binding names no weight file, so there is nothing to identify it by"
	}
	if missing := r.Missing(); len(missing) > 0 {
		return localID{}, false, "this machine is missing " + strings.Join(missing, ", ")
	}
	if gaps := r.Unfingerprinted(); len(gaps) > 0 {
		return localID{}, false, "this machine could not fingerprint " + strings.Join(gaps, ", ")
	}
	var (
		memo map[string]mediacap.Recipe
		done bool
	)
	others := func() map[string]mediacap.Recipe {
		if done {
			return memo
		}
		memo = map[string]mediacap.Recipe{}
		complete := true
		for _, fi := range cfg.ImageFamilies() {
			o, _, oerr := cfg.ResolveImageFamily(fi.Name)
			if oerr != nil {
				continue
			}
			or, ok := mediacap.ImageRecipeWith(o, fi, mediacap.ModelRoots(o.ComfyDir), nil, localPeek)
			if !ok || len(or.Missing()) > 0 {
				continue // not a family with a recipe here, or a file missing: nothing a later look would change
			}
			if len(or.Unfingerprinted()) > 0 {
				complete = false // being hashed (Peek started it) or unreadable: look again at the next miss
				continue
			}
			memo[fi.Name] = or
		}
		done = complete
		return memo
	}
	return localID{name: strings.TrimSpace(family), recipe: r, others: others}, true, ""
}

// identityOrLog is localIdentity for a call that goes on without one: the call then runs on this machine's own line and
// never asks the fleet, so the reason is logged, once per call. It is not noise: an image call is minutes of GPU, and a file
// that read back "unreadable" under load is a transient that turns overflow off with nothing else to show for it (a missing
// file or an sd.cpp binding is stable, and the line says so too).
func identityOrLog(ctx context.Context, cfg config.Config, family string) (localID, bool) {
	local, ok, why := localIdentity(ctx, cfg, family)
	if !ok {
		log.Printf("mediaremote: image call (family %q) has no fleet overflow and runs on this machine's own line: %s", family, why)
	}
	return local, ok
}

// need is what one call asks of a node beyond the recipe.
type need struct {
	// refineOff: the caller sent refine=false, which only a node that carries it may be given.
	refineOff bool
	// steps: the caller sent a per-request step count.
	steps bool
	// graph is what a run_graph asks of a node (graph.go); nil for every other task.
	graph *graphNeed
	// vram is the caller's min_vram_gb on a video or animation call (graph.go); nil when the call asked for none.
	vram *vramNeed
}

func needOf(req core.Request) need {
	_, steps := posInt(req.Params, "steps")
	return need{refineOff: refineOff(req.Params), steps: steps, graph: graphNeedOf(req), vram: vramNeedOf(req)}
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
	// Two recipes whose files are all "pending" digest alike, so a recipe without a fingerprint on every present file is
	// never compared: this machine's here, the node's rows below.
	if gaps := local.recipe.Unfingerprinted(); len(gaps) > 0 {
		return miss("this machine has not fingerprinted " + strings.Join(gaps, ", ") + ", so it has no identity to compare yet")
	}
	want := local.recipe.Digest()
	var equal []delegate.ImageRecipeView
	for _, row := range v.ImageRecipes {
		if row.Digest == want {
			equal = append(equal, row)
		}
	}
	if len(equal) == 0 {
		if m, hashing := stillHashing(local, v); hashing {
			return m, false
		}
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
	// A row whose digest equals ours but whose files are not all fingerprinted claims an identity it has not got.
	if gaps := recipeOfView(row).Unfingerprinted(); len(gaps) > 0 {
		return fmt.Sprintf("its family %q publishes a matching digest but has not fingerprinted %s", row.Name, strings.Join(gaps, ", "))
	}
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

// stillHashing words the miss of a node that holds this machine's build by name, size and sampling but has not
// fingerprinted all of it yet (a node that was just started, or whose file just changed, publishes its rows pending and
// with no digest). It is a wait, not a difference: the same read of its health a little later can match.
func stillHashing(local localID, v delegate.NodeView) (nodeMatch, bool) {
	shape := local.recipe.WithoutContent().Digest()
	for _, row := range v.ImageRecipes {
		r := recipeOfView(row)
		gaps := r.Unfingerprinted()
		if len(gaps) == 0 || r.WithoutContent().Digest() != shape {
			continue
		}
		why := fmt.Sprintf("its %s holds this machine's build by name and size but has not fingerprinted %s, and a family is matched on content: it is a candidate once the node has (read its /fleet/health image_recipes again in a moment)", familyLabel(row), strings.Join(gaps, ", "))
		return nodeMatch{why: why, differs: gaps, state: "not-capable"}, true
	}
	return nodeMatch{}, false
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
		r.Files = append(r.Files, mediacap.RecipeFile{Role: f.Role, Name: f.Name, Bytes: f.Bytes, Fingerprint: f.Fingerprint})
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
