package fleetnode

// The generative-edit lane on the fleet node (Qwen-Image-Edit through render/comfy-edit.mjs): its published recipe and the
// image-edit task, the edit twin of image_recipe.go and buildImageGen.
//
// An edit always has an input image, and only the media-job door carries files, so image-edit is carried ONLY by /fleet/media-job
// (media_job.go) and is advertised exactly when that door is open. A payload sent straight to /fleet/dispatch is refused: its
// `image` would be a path on this node. The door ships the target as the file `image` and the references as `images_1`..`images_9`;
// foldEditImages turns the latter into the one `images` list the builder (and the pipeline) read.
//
// What the node publishes (/fleet/health edit_recipes) is its CAPABILITY for the lane (mediacap/edit_recipe.go): graph, UNet, text
// encoder, VAE and the set of LoRA files it holds, each with a content fingerprint. What one edit asks for (preset, steps, cfg,
// megapixels, placement, seed, negative) is in the request. The request names the digest its asker matched, and buildImageEdit
// recomputes it from the files on disk and refuses a different one with 412 before anything is queued, leased or rendered.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// ImageEditTask is the fleet task type of a generative edit. It is advertised only when the node has an edit binding and
// fleet_media_inputs is on, and it is carried only through the media-job door.
const ImageEditTask = "image-edit"

// editInputFields are the payload fields of an image-edit that arrive as files in a media-job bundle: the target and up to
// mediacap.EditMaxImages-1 references.
var editInputFields = func() map[string]mediaKind {
	m := map[string]mediaKind{"image": kindImage}
	for i := 1; i < mediacap.EditMaxImages; i++ {
		m["images_"+strconv.Itoa(i)] = kindImage
	}
	return m
}()

// mediaJobMaxFiles is the most files one media-job bundle may hold for task: a generative edit takes its target and nine
// references, every other task up to eight.
func mediaJobMaxFiles(task string) int {
	if task == ImageEditTask {
		return mediacap.EditMaxImages
	}
	return 8
}

var editRefField = regexp.MustCompile(`^images_([0-9]+)$`)

// editImagesKey matches every spelling a payload could use for the reference list: `images` and `images_<digits>` in any
// case, INCLUDING spellings the bundle never ships (images_01, images_10, IMAGES_2). The door's exact-name guard only knows
// the shipped field names, so this is the second gate: a key here that the bundle did not ship is a node-local path read
// dressed as a reference, and it is refused before the builder sees it.
var editImagesKey = regexp.MustCompile(`(?i)^images(_[0-9]+)?$`)

// foldEditImages folds the reference files the bundle shipped (inner["images_1"].., whose values are the node-side paths
// inputFile returned, never the caller's text) into inner["images"], in numeric order. Only a key named in shipped (the
// media-job inputs) folds; every other key matching editImagesKey is refused when it carries anything, and dropped when it
// is absent, empty or an empty list. A payload `images` of its own is refused the same way: this door carries bytes, never
// paths.
func foldEditImages(inner map[string]json.RawMessage, shipped map[string]string) error {
	type ref struct {
		n    int
		path json.RawMessage
	}
	var refs []ref
	for key, val := range inner {
		if !editImagesKey.MatchString(key) {
			continue
		}
		if _, ok := shipped[key]; ok {
			if m := editRefField.FindStringSubmatch(key); m != nil {
				n, _ := strconv.Atoi(m[1])
				refs = append(refs, ref{n, val})
				delete(inner, key)
				continue
			}
		}
		if !isAbsentJSON(val) && string(val) != `""` && string(val) != `[]` {
			return fmt.Errorf("media-job payload: payload.%s names paths on this node; ship the files in the bundle (inputs images_1..images_9) instead", key)
		}
		delete(inner, key)
	}
	if len(refs) == 0 {
		return nil
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].n < refs[j].n })
	paths := make([]json.RawMessage, 0, len(refs))
	for _, r := range refs {
		paths = append(paths, r.path)
	}
	enc, err := json.Marshal(paths)
	if err != nil {
		return err
	}
	inner["images"] = enc
	return nil
}

// EditRecipes lists the recipes of this node's generative-edit bindings, the default first, then every named edit family in name
// order (config.EditFamilies' order), in the row shape of image_recipes (the row's lane is "edit"). A binding is listed only when
// it names the edit runner and a UNet. Missing files are listed with bytes -1 and such a row never matches; a row whose files are
// not all fingerprinted yet has no digest. It never waits for a hash.
func EditRecipes(cfg config.Config) []ImageRecipeHealth {
	return EditRecipesWith(cfg, recipeFingerprints.Peek)
}

// EditRecipesWith is EditRecipes with the source of the content fingerprints chosen by the caller (a blocking one for a command
// that prints digests). It is the one place the published rows are built.
func EditRecipesWith(cfg config.Config, fp mediacap.FingerprintFunc) []ImageRecipeHealth {
	var out []ImageRecipeHealth
	for _, fi := range cfg.EditFamilies() {
		eff := cfg
		if !fi.Default {
			var err error
			if eff, _, err = cfg.ResolveEditFamily(fi.Name); err != nil {
				continue
			}
		}
		if eff.GenEditScript == "" || eff.GenEditUnet == "" {
			continue
		}
		r, ok := mediacap.EditRecipeWith(eff, fi, mediacap.ModelRoots(eff.ComfyDir), nil, fp)
		if !ok || !namesACheckpoint(r) {
			continue
		}
		row := ImageRecipeHealth{Name: fi.Name, Default: fi.Default, Recipe: r}
		if len(r.Unfingerprinted()) == 0 && len(r.Missing()) == 0 {
			row.Digest = r.Digest()
		}
		out = append(out, row)
	}
	return out
}

// editRecipeMemo is the memoised EditRecipes of a server's config, rebuilt as imageRecipeRows is: after mediaRoutesTTL, or as soon
// as a fingerprint read has settled.
type editRecipeMemo struct {
	mu   sync.Mutex
	rows []ImageRecipeHealth
	at   time.Time
	read bool
	gen  uint64
}

func (s *Server) editRecipeRows() []ImageRecipeHealth {
	m := &s.editRecipes
	m.mu.Lock()
	defer m.mu.Unlock()
	now := mediaClock()
	if gen := recipeFingerprints.Generation(); !m.read || now.Sub(m.at) >= mediaRoutesTTL || gen != m.gen {
		m.rows, m.at, m.read, m.gen = EditRecipes(s.opts.Cfg), now, true, gen
	}
	return m.rows
}

func (e *recipeMismatchError) editError() string {
	name := e.family
	if name == "" {
		name = "(the default binding)"
	}
	return fmt.Sprintf("recipe_digest %s does not match edit family %s on this node: %s; read /fleet/health edit_recipes again and place the job from that",
		shortDigest(e.want), name, e.why)
}

// checkEditRecipe admits an image-edit payload that named recipe_digest want for family only if this node's edit family digests to
// it, computed now from the files on disk (checkRecipe's twin for the edit lane). A family with a file that is missing or that this
// node cannot fingerprint is no identity and is refused with the cause.
func checkEditRecipe(ctx context.Context, cfg config.Config, family, want string) error {
	refuse := func(why, cause string) error {
		return &recipeMismatchError{family: family, want: want, why: why, cause: cause, lane: mediacap.RecipeLaneEdit}
	}
	eff, fi, err := cfg.ResolveEditFamily(family)
	if err != nil {
		return refuse(err.Error(), mediacap.RecipeRefusalChanged)
	}
	if eff.GenEditScript == "" || eff.GenEditUnet == "" {
		return refuse("this node has no generative edit binding for that family", mediacap.RecipeRefusalChanged)
	}
	await := func(path string, size int64) string { return recipeFingerprints.AwaitContext(ctx, path, size) }
	rec, ok := mediacap.EditRecipeWith(eff, fi, mediacap.ModelRoots(eff.ComfyDir), nil, await)
	if !ok {
		return refuse("that binding is not a ComfyUI one, which has no recipe to match", mediacap.RecipeRefusalChanged)
	}
	if gaps := rec.Unfingerprinted(); len(gaps) > 0 {
		return refuse("this node cannot fingerprint "+strings.Join(gaps, ", "), gapCause(rec))
	}
	if have := rec.Digest(); have != strings.TrimSpace(want) {
		why := fmt.Sprintf("this node's edit family digests to %s", shortDigest(have))
		if missing := rec.Missing(); len(missing) > 0 {
			sort.Strings(missing)
			why += " and is missing " + strings.Join(missing, ", ")
		}
		return refuse(why, mediacap.RecipeRefusalChanged)
	}
	return nil
}

// buildImageEdit builds the pipeline request of an image-edit. It runs only from the media-job door, after the bundle was
// extracted and sniffed and its files named in `image` / `images`. The recipe is compared FIRST, so a node that is not what its
// asker matched answers 412 before the request is looked at any further, and nothing is queued, leased or rendered on any refusal
// here. It builds the request the local door builds (core.TaskEditImageGenerative, whose run is pipeline.runEditImageGenerative
// and its argv imagegen.editArgs), so a fleet edit and a local one cannot render differently for the same inputs.
func buildImageEdit(ctx context.Context, cfg config.Config, payload json.RawMessage) (core.Request, func(), error) {
	noop := func() {}
	var in struct {
		Prompt       string   `json:"prompt"`
		Negative     string   `json:"negative"`
		Family       string   `json:"family"`
		Preset       string   `json:"preset"`
		Steps        int      `json:"steps"`
		CFG          float64  `json:"cfg"`
		Seed         int      `json:"seed"`
		Transparent  bool     `json:"transparent"`
		Megapixels   *float64 `json:"megapixels"`
		Placement    string   `json:"placement"`
		RecipeDigest string   `json:"recipe_digest"`
		Image        string   `json:"image"`
		Images       []string `json:"images"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return core.Request{}, noop, fmt.Errorf("image-edit payload: %w", err)
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return core.Request{}, noop, fmt.Errorf("image-edit payload: prompt required")
	}
	if strings.TrimSpace(in.RecipeDigest) == "" {
		return core.Request{}, noop, fmt.Errorf("image-edit payload: recipe_digest required (an edit is placed only on a node whose edit recipe is the asker's; read /fleet/health edit_recipes)")
	}
	if err := checkEditRecipe(ctx, cfg, in.Family, in.RecipeDigest); err != nil {
		return core.Request{}, noop, err
	}
	eff, fam, err := cfg.ResolveEditFamily(in.Family)
	if err != nil {
		return core.Request{}, noop, fmt.Errorf("image-edit payload: %w", err)
	}
	if in.Image == "" {
		return core.Request{}, noop, fmt.Errorf("image-edit payload: image required (ship the edit target in the bundle as the file `image`)")
	}
	is21 := eff.GenEditFamily == config.FamilyQwenImage21
	switch {
	case len(in.Images) > 0 && !is21:
		return core.Request{}, noop, fmt.Errorf("image-edit payload: multi-reference edits need a %s edit family; this node's %q takes one image", config.FamilyQwenImage21, fam.Name)
	case 1+len(in.Images) > mediacap.EditMaxImages:
		return core.Request{}, noop, fmt.Errorf("image-edit payload: an edit takes at most %d images in all (the target plus %d references); got %d", mediacap.EditMaxImages, mediacap.EditMaxImages-1, 1+len(in.Images))
	case in.Transparent && !eff.SupportsTransparentEdit():
		return core.Request{}, noop, fmt.Errorf("image-edit payload: transparent output needs a %s edit family; %q renders opaque", config.FamilyQwenImage21, fam.Name)
	case is21 && in.Preset != "":
		return core.Request{}, noop, fmt.Errorf("image-edit payload: preset is a Qwen-Image-Edit 2511 pairing; the %s edit graph has none (pass steps and cfg together instead)", config.FamilyQwenImage21)
	}
	params := map[string]any{"image": in.Image}
	if in.Family != "" {
		params["family"] = in.Family
	}
	if len(in.Images) > 0 {
		params["images"] = in.Images
	}
	if in.Negative != "" {
		params["negative"] = in.Negative
	}
	if in.Preset != "" {
		params["preset"] = in.Preset
	}
	if in.Steps > 0 {
		params["steps"] = in.Steps
	}
	if in.CFG > 0 {
		params["cfg"] = in.CFG
	}
	if in.Seed > 0 {
		params["seed"] = in.Seed
	}
	if in.Transparent {
		params["transparent"] = true
	}
	if in.Megapixels != nil && *in.Megapixels != 0 {
		params["megapixels"] = *in.Megapixels
	}
	if in.Placement != "" {
		params["placement"] = in.Placement
	}
	// The canvas and the placement are judged now, as the pipeline will judge them: a request over the card-only recipe's largest
	// canvas is refused at admission (400), not after it was queued.
	if _, err := mediacap.ApplyEditRequestKnobs(eff, params); err != nil {
		return core.Request{}, noop, fmt.Errorf("image-edit payload: %w", err)
	}
	return core.Request{Task: core.TaskEditImageGenerative, Input: in.Prompt, Params: params}, noop, nil
}
