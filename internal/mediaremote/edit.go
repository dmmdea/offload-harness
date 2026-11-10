package mediaremote

// The generative edit lane on the fleet, asker side (the edit twin of the image path in request.go, identity.go and place.go).
//
// An edit is identified by its edit recipe (mediacap.EditRecipeWith: the edit graph, the weights and the SET of LoRA files the
// node holds, each with its content fingerprint). A node is a candidate when it publishes an edit_recipes row whose digest equals
// this machine's, runs this release, and serves both image-edit and media-job (the images travel in the media-job bundle). The
// node compares the digest again at its door and answers 412 when its files are not what it published, so the asker never sends an
// edit to a node it has only matched by name. Everything a single edit chooses (preset, steps, cfg, megapixels, placement, seed,
// negative) travels in the payload and is not part of the digest.

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"crypto/sha256"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/imagegen"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// taskEdit is the fleet task type of a generative edit. It is carried only by /fleet/media-job.
const taskEdit = "image-edit"

// editRoute is the media route name of a node's default edit binding.
const editRoute = "edit_image_generative"

func init() { Budgets[taskEdit] = 2 * time.Hour }

// strList reads a list of strings from its Go shape or the []any a JSON decode produces, dropping empty entries.
func strList(v any) []string {
	var out []string
	switch t := v.(type) {
	case []string:
		for _, s := range t {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	}
	return out
}

// planEdit maps a generative edit to its wire form: the payload the node's buildImageEdit reads and the input files that travel
// (the target as `image`, the references as images_1..images_9).
func planEdit(req core.Request) (planned, error) {
	p := req.Params
	if strings.TrimSpace(req.Input) == "" {
		return planned{}, &contractError{"edit_image_generative: prompt required"}
	}
	image := str(p, "image")
	if image == "" {
		image = strings.TrimSpace(req.Image)
	}
	if image == "" {
		return planned{}, &contractError{"edit_image_generative: image required"}
	}
	refs := strList(p["images"])
	if 1+len(refs) > mediacap.EditMaxImages {
		return planned{}, &contractError{fmt.Sprintf("edit_image_generative: an edit takes at most %d images in all (the target plus %d references); got %d", mediacap.EditMaxImages, mediacap.EditMaxImages-1, 1+len(refs))}
	}
	pl := planned{fleetTask: taskEdit, payload: map[string]any{"prompt": req.Input}}
	out := pl.payload
	for _, k := range []string{"negative", "family", "preset"} {
		setStr(p, k, out)
	}
	for _, k := range []string{"steps", "seed"} {
		setPos(p, k, out)
	}
	if f, ok := num(p, "cfg"); ok && f > 0 {
		out["cfg"] = f
	}
	if _, set := p["megapixels"]; set {
		mp, ok := num(p, "megapixels")
		if !ok || mp < mediacap.EditMegapixelsMin || mp > mediacap.EditMegapixelsMax {
			return planned{}, &contractError{fmt.Sprintf("edit_image_generative: megapixels must be between %g and %g (got %v)", mediacap.EditMegapixelsMin, mediacap.EditMegapixelsMax, p["megapixels"])}
		}
		out["megapixels"] = mp
	}
	if pm := str(p, "placement"); pm != "" {
		if pm != "single" && pm != "multicard" {
			return planned{}, &contractError{fmt.Sprintf("edit_image_generative: placement must be single or multicard (got %q)", pm)}
		}
		out["placement"] = pm
	}
	if flag(p, "transparent") {
		out["transparent"] = true
	}
	pl.inputs = append(pl.inputs, input{"image", image})
	for i, r := range refs {
		pl.inputs = append(pl.inputs, input{fmt.Sprintf("images_%d", i+1), r})
	}
	return pl, nil
}

// ---- identity -----------------------------------------------------------------------------------------------------------

// localEditIdentity resolves family on this machine's generative-edit lane and builds its edit recipe: the way localIdentity
// does for an image family, with the same refusals (a binding with no recipe, a file that is missing or that cannot be
// fingerprinted).
func localEditIdentity(ctx context.Context, cfg config.Config, family string) (localID, bool, string) {
	eff, fi, err := cfg.ResolveEditFamily(family)
	if err != nil {
		return localID{}, false, err.Error()
	}
	if eff.GenEditScript == "" || eff.GenEditUnet == "" {
		return localID{}, false, "this machine has no generative edit binding"
	}
	await := func(path string, size int64) string { return localFingerprints(ctx, path, size) }
	r, ok := mediacap.EditRecipeWith(eff, fi, mediacap.ModelRoots(eff.ComfyDir), nil, await)
	if !ok {
		return localID{}, false, "this edit binding is not a ComfyUI one, which has no recipe to compare"
	}
	if len(r.Files) == 0 {
		return localID{}, false, "this edit binding names no weight file, so there is nothing to identify it by"
	}
	if missing := r.Missing(); len(missing) > 0 {
		return localID{}, false, "this machine is missing " + strings.Join(missing, ", ")
	}
	if gaps := r.Unfingerprinted(); len(gaps) > 0 {
		return localID{}, false, "this machine could not fingerprint " + strings.Join(gaps, ", ")
	}
	return localID{name: strings.TrimSpace(family), recipe: r, others: func() map[string]mediacap.Recipe { return nil }}, true, ""
}

// identityOrLogFor is the identity of the lane req runs on: the edit recipe for a generative edit, the image family's for
// anything else. A call without one goes on without the fleet, and the reason is logged once per call.
func identityOrLogFor(ctx context.Context, cfg config.Config, req core.Request) (localID, bool) {
	if req.Task != core.TaskEditImageGenerative {
		return identityOrLog(ctx, cfg, str(req.Params, "family"))
	}
	local, ok, why := localEditIdentity(ctx, cfg, str(req.Params, "family"))
	if !ok {
		log.Printf("mediaremote: generative edit (family %q) has no fleet overflow and runs on this machine's own line: %s", str(req.Params, "family"), why)
	}
	return local, ok
}

// editRowFile and editRecipeOfView rebuild the recipe a node published for the edit lane from its row.
func editRecipeOfView(v delegate.ImageRecipeView) mediacap.Recipe {
	r := recipeOfView(v)
	r.Lane = mediacap.RecipeLaneEdit
	return r
}

// matchEditNode compares what node v publishes in edit_recipes with this machine's edit recipe. ok means v holds an edit family
// that digests to it, serves the media-job door the images travel in, runs this release, and has the family's route configured
// when it reports its routes.
func matchEditNode(local localID, v delegate.NodeView) (m nodeMatch, ok bool) {
	miss := func(why string, differs ...string) (nodeMatch, bool) {
		return nodeMatch{why: why, differs: differs, state: "not-capable"}, false
	}
	if !contains(v.Tasks, taskEdit) {
		return miss("does not serve " + taskEdit + " (it needs a generative edit binding and fleet_media_inputs on)")
	}
	if !contains(v.Tasks, taskMediaJob) {
		return miss("does not serve " + taskMediaJob + ", which the images of an edit travel through")
	}
	if len(v.EditRecipes) == 0 {
		return miss("publishes no edit recipe (a harness older than this one, or no ComfyUI edit family that names a UNet), so it cannot be matched")
	}
	if v.HarnessVersion != localVersion {
		return miss(fmt.Sprintf("runs harness %s and this machine %s: a recipe is only compared between the same release (the graph builders ship with the version)", shownVersion(v.HarnessVersion), localVersion))
	}
	if gaps := local.recipe.Unfingerprinted(); len(gaps) > 0 {
		return miss("this machine has not fingerprinted " + strings.Join(gaps, ", ") + ", so it has no identity to compare yet")
	}
	want := local.recipe.Digest()
	var reasons []string
	found := false
	for _, row := range v.EditRecipes {
		if row.Digest != want || (row.Lane != "" && row.Lane != mediacap.RecipeLaneEdit) {
			continue
		}
		found = true
		if gaps := editRecipeOfView(row).Unfingerprinted(); len(gaps) > 0 {
			reasons = append(reasons, fmt.Sprintf("its edit family %q publishes a matching digest but has not fingerprinted %s", row.Name, strings.Join(gaps, ", ")))
			continue
		}
		route := editRoute
		if !row.Default {
			route = mediacap.EditFamilyRoute(row.Name)
		}
		if st, known := v.RouteState(route); known && st != delegate.MediaRouteConfigured {
			reasons = append(reasons, fmt.Sprintf("its edit family %q matches, but its route %s is %s on that node", row.Name, route, st))
			continue
		}
		return nodeMatch{family: row.Name}, true
	}
	if found {
		return miss(strings.Join(reasons, "; "))
	}
	// A node that holds this machine's edit weights by name and size but has not fingerprinted all of them is a wait, not a difference.
	shape := local.recipe.WithoutContent().Digest()
	for _, row := range v.EditRecipes {
		r := editRecipeOfView(row)
		if gaps := r.Unfingerprinted(); len(gaps) > 0 && r.WithoutContent().Digest() == shape {
			return miss(fmt.Sprintf("its edit family %s holds this machine's build by name and size but has not fingerprinted %s, and a family is matched on content: it is a candidate once the node has (read its /fleet/health edit_recipes again in a moment)", familyLabel(row), strings.Join(gaps, ", ")), gaps...)
		}
	}
	best, bestDiff := 0, []string(nil)
	for i, row := range v.EditRecipes {
		d := local.recipe.Diff(editRecipeOfView(row))
		if i == 0 || len(d) < len(bestDiff) {
			best, bestDiff = i, d
		}
	}
	row := v.EditRecipes[best]
	why := fmt.Sprintf("no edit family on that node matches: its %s differs in %s", familyLabel(row), strings.Join(bestDiff, "; "))
	if len(bestDiff) == 0 {
		why = fmt.Sprintf("no edit family on that node matches: its %s digests differently (a recipe version this machine does not compare)", familyLabel(row))
	}
	return miss(why, bestDiff...)
}

// ---- sending ------------------------------------------------------------------------------------------------------------

// wireFor is the route and the body of the job that attempt sends to candidate c: a dispatch for the tasks that carry no files, and
// the media-job envelope, with the bundle of the images, for an edit. An edit always carries the node's own name for the family
// and the recipe digest, which the node compares at its door (412 when its files are not what it published).
func wireFor(cfg config.Config, pl planned, c candidate, local localID, jobID string) (route string, body []byte, err error) {
	payload := make(map[string]any, len(pl.payload)+2)
	for k, v := range pl.payload {
		payload[k] = v
	}
	if pl.fleetTask == taskImage || pl.fleetTask == taskEdit {
		// The node renders the family under ITS name for this recipe, never the caller's (a name binds different files on
		// different nodes), and the digest lets it refuse a recipe it no longer holds.
		delete(payload, "family")
		if c.family != "" {
			payload["family"] = c.family
		}
		payload["recipe_digest"] = local.recipe.Digest()
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", nil, &contractError{fmt.Sprintf("the %s request cannot be encoded for the wire: %v", pl.fleetTask, err)}
	}
	if pl.fleetTask == taskEdit {
		bundle, names, berr := buildBundle(cfg, pl.inputs)
		if berr != nil {
			return "", nil, berr
		}
		sum := sha256.Sum256(bundle)
		inputs := map[string]string{}
		for i, in := range pl.inputs {
			inputs[in.field] = names[i]
		}
		body, err = json.Marshal(map[string]any{
			"job_id": jobID, "task_type": pl.fleetTask, "payload": json.RawMessage(raw),
			"bundle": base64.StdEncoding.EncodeToString(bundle), "bundle_sha256": hex.EncodeToString(sum[:]), "inputs": inputs,
		})
		if err != nil {
			return "", nil, &contractError{fmt.Sprintf("the %s request cannot be encoded for the wire: %v", pl.fleetTask, err)}
		}
		return "/fleet/media-job", body, nil
	}
	body, err = json.Marshal(map[string]any{"job_id": jobID, "task_type": pl.fleetTask, "payload": json.RawMessage(raw)})
	if err != nil {
		return "", nil, &contractError{fmt.Sprintf("the %s request cannot be encoded for the wire: %v", pl.fleetTask, err)}
	}
	if len(body) > maxDispatchBody {
		return "", nil, &contractError{fmt.Sprintf("the %s request is %d bytes, over the %d bytes a fleet dispatch carries", pl.fleetTask, len(body), maxDispatchBody)}
	}
	return "/fleet/dispatch", body, nil
}

// callEdit places one generative edit on a fleet node without a ticket (route remote, or auto on a machine with no edit lane): it
// builds this machine's edit identity, matches the roster on it and offers the job to each matching node in turn. A node that
// refuses it at its door (busy, full, a recipe it no longer holds) is passed over; a node's own answer after it accepted the job is
// final.
func callEdit(ctx context.Context, cfg config.Config, req core.Request, pl planned, bases []string, h core.RemoteAttribution, start time.Time) (core.Result, error) {
	if _, has := ctx.Deadline(); !has {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, Budgets[taskEdit])
		defer cancel()
	}
	local, ok, why := localEditIdentity(ctx, cfg, str(req.Params, "family"))
	if !ok {
		return core.Result{}, &placementError{core.DeferClassContract, "a generative edit is placed on a node only by its edit recipe, and this machine has none to compare: " + why + "; use route local, or fix the edit binding here"}
	}
	cands, rows := candidatesFor(ctx, cfg, bases, local, needOf(req))
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].idx < rows[b].idx })
	var parts []string
	busy := false
	for _, r := range rows {
		parts = append(parts, r.row.Node+": "+r.row.Why)
		busy = busy || r.matched
	}
	for _, c := range cands {
		jobID, err := newJobID()
		if err != nil {
			return core.Result{}, &placementError{core.DeferClassInfrastructure, "job id: " + err.Error()}
		}
		a := attempt(ctx, cfg, req, pl, c, local, start, jobID, h)
		switch a.kind {
		case attemptDone:
			return a.res, nil
		case attemptFailed:
			return a.res, a.err
		}
		parts = append(parts, a.row.Node+": "+a.row.Why)
	}
	msg := "no fleet node can take this edit now"
	if len(parts) > 0 {
		msg += " (" + strings.Join(parts, "; ") + ")"
	}
	if len(cands) == 0 && !busy {
		return core.Result{}, &noNodeConfigured{&placementError{core.DeferClassCapacity, msg}}
	}
	return core.Result{}, &placementError{core.DeferClassCapacity, msg}
}

// ---- batches ------------------------------------------------------------------------------------------------------------

// EditJob is the edit part of a batch job (BatchJob.Edit): the source, its references and the per-request knobs of one edit.
type EditJob struct {
	Image      string   `json:"image"`
	Refs       []string `json:"refs,omitempty"`
	Preset     string   `json:"preset,omitempty"`
	CFG        float64  `json:"cfg,omitempty"`
	Megapixels float64  `json:"megapixels,omitempty"`
	Placement  string   `json:"placement,omitempty"`
}

// editBatchRequest is the single edit a job stands for: what the lane question, the roster match and the ticket are about.
// placement is the batch-level --placement; a row's own edit.placement wins over it.
func editBatchRequest(j BatchJob, family, placement string) core.Request {
	e := j.Edit
	params := map[string]any{"image": e.Image}
	if len(e.Refs) > 0 {
		params["images"] = append([]string(nil), e.Refs...)
	}
	if j.Out != "" {
		params["out"] = j.Out
	}
	if j.Negative != "" {
		params["negative"] = j.Negative
	}
	if j.Steps > 0 {
		params["steps"] = j.Steps
	}
	if j.Seed > 0 {
		params["seed"] = j.Seed
	}
	if e.Preset != "" {
		params["preset"] = e.Preset
	}
	if e.CFG > 0 {
		params["cfg"] = e.CFG
	}
	if e.Megapixels > 0 {
		params["megapixels"] = e.Megapixels
	}
	if e.Placement != "" {
		params["placement"] = e.Placement
	} else if placement != "" {
		params["placement"] = placement
	}
	if family != "" {
		params["family"] = family
	}
	return core.Request{Task: core.TaskEditImageGenerative, Door: "cli:edit-image-generative", Input: j.Prompt, Params: params}
}

// EditRequest is the request of one edit row, for a caller that runs it on this machine (the CLI's warm call).
func EditRequest(j BatchJob, family, placement string) core.Request {
	return editBatchRequest(j, family, placement)
}

// batchVerifies is --resume's proof of a job's file: a PNG that decodes whole and has the size the job names (an image job), or,
// for an edit, a PNG that decodes whole and has the size the edit canvas rule gives for this source and megapixels
// (mediacap.EditOutputSize: the rule render/comfy-edit.mjs applies, so the size is never a number written down here). A family
// whose output size follows no rule this machine can compute (qwen-image-2.1) is accepted on decoding alone.
func batchVerifies(cfg config.Config, family string, j BatchJob) bool {
	if j.Edit == nil {
		return pngVerifies(j.Out, j.Width, j.Height)
	}
	if !pngVerifies(j.Out, 0, 0) {
		return false
	}
	eff, _, err := cfg.ResolveEditFamily(family)
	if err != nil {
		return true
	}
	req := editBatchRequest(j, family, "")
	eff, err = mediacap.ApplyEditRequestKnobs(eff, req.Params)
	if err != nil || eff.GenEditFamily == config.FamilyQwenImage21 {
		return true
	}
	sw, sh := imagegen.OutputSize(j.Edit.Image)
	w, h, ok := mediacap.EditOutputSize(eff.GenEditMegapixels, sw, sh)
	if !ok {
		return true
	}
	return pngVerifies(j.Out, w, h)
}

// editFamily resolves the batch's family on the edit lane.
func editBatchFamily(cfg config.Config, family string) (config.FamilyInfo, error) {
	_, fam, err := cfg.ResolveEditFamily(family)
	return fam, err
}

// editLaneConfigured says whether this machine's own generative-edit route for family is configured, by the same derivation the
// node's advertisement uses (the script AND the unet AND the files behind them).
func editLaneConfigured(cfg config.Config, routes map[string]bool, family string) bool {
	if family == "" {
		return routes[editRoute]
	}
	if _, _, err := cfg.ResolveEditFamily(family); err != nil {
		return false
	}
	for _, fi := range cfg.EditFamilies() {
		if fi.Name == family && fi.Default {
			return routes[editRoute]
		}
	}
	return routes[mediacap.EditFamilyRoute(family)]
}

// bundleFileLimit is the most files one bundle carries: 8 for every task, and the edit's target plus nine references for an edit.
func bundleFileLimit(inputs []input) int {
	for _, in := range inputs {
		if strings.HasPrefix(in.field, "images_") {
			return mediacap.EditMaxImages
		}
	}
	return 8
}
