// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
//
// Hand-authored novel command surface (NOT generator output): the model
// visibility family — `models why` and `models list`.
//
// `models why` is the headline. Every other tool answers "ComfyUI can't see my
// model" with one undifferentiated guess. There are FOUR distinct causes and
// they need four distinct fixes:
//
//	visible            the loader lists it — the graph is passing a different string
//	class-unregistered the loader's COMBO is EMPTY: no folder is registered for that
//	                   model class (missing extra_model_paths.yaml key). NOT a missing file
//	not-listed         the folder is registered and lists other files of this kind,
//	                   but not this one
//	no-such-input      nothing in the live schema would ever load a file of this kind
//	                   (the custom node that provides the loader is not installed)
//
// store.ClassifyModelVisibility owns the per-input verdict; this file scans the
// whole live schema and aggregates.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"comfyui-pp-cli/internal/store"
	"github.com/spf13/cobra"
)

// comfyScannedGroups are the input groups that can hold a model list. Hidden
// inputs (PROMPT, EXTRA_PNGINFO, UNIQUE_ID) never do.
var comfyScannedGroups = map[string]bool{"required": true, "optional": true}

// comfyLoaderInput identifies one input on one class.
type comfyLoaderInput struct {
	ClassType   string `json:"class_type"`
	Input       string `json:"input"`
	Requirement string `json:"requirement"`
	Type        string `json:"type,omitempty"`
	ComboShape  string `json:"combo_shape,omitempty"`
	OptionCount int    `json:"option_count"`
}

func (l comfyLoaderInput) String() string { return l.ClassType + "." + l.Input }

// comfyModelHit is one input's verdict for one filename.
type comfyModelHit struct {
	Loader     comfyLoaderInput
	Visibility store.ModelVisibility
	Options    []string
	// SameKind means this input's option list already holds files with the
	// same extension — evidence that this is the right folder and the file
	// simply is not in it.
	SameKind bool
}

// comfyFileExtension returns the lowercased extension including the dot, or "".
func comfyFileExtension(name string) string {
	name = strings.TrimSpace(name)
	// ComfyUI options carry forward-slash subfolder prefixes ("SDXL/foo.safetensors").
	if idx := strings.LastIndexAny(name, "/\\"); idx >= 0 {
		name = name[idx+1:]
	}
	dot := strings.LastIndex(name, ".")
	if dot < 0 || dot == len(name)-1 {
		return ""
	}
	return strings.ToLower(name[dot:])
}

// comfyIsModelFilename reports whether an option value looks like a model file
// rather than an enum value (euler, karras, nearest-exact...).
func comfyIsModelFilename(value string) bool {
	ext := comfyFileExtension(value)
	if ext == "" {
		return false
	}
	for _, candidate := range comfyModelExtensions {
		if ext == candidate {
			return true
		}
	}
	return false
}

// comfySameKindOptions reports whether an option list already holds files of
// the same kind as filename. Extension match is the signal; when the queried
// name has no extension, any model-looking option counts.
func comfySameKindOptions(filename string, options []string) bool {
	want := comfyFileExtension(filename)
	for _, option := range options {
		if want == "" {
			if comfyIsModelFilename(option) {
				return true
			}
			continue
		}
		if comfyFileExtension(option) == want {
			return true
		}
	}
	return false
}

// comfyLooksLikeModelInput decides whether a COMBO is folder-backed. A
// populated list is judged by its contents; an EMPTY list has no contents to
// judge, so the input NAME is the only available signal — and empty lists are
// exactly the case that matters most, so they must not be filtered away.
//
// The name rule is the `<thing>_name` convention every folder_paths-backed
// COMBO follows (ckpt_name, vae_name, lora_name, gligen_name, style_model_name,
// upscale_model_name, and plain `name` on a few loaders). Measured against a
// live 1133-class server, a looser substring rule ("model", "style", ...)
// swept in 74 empty cloud-API dropdowns — ByteDanceSeedNode.model,
// BuildJsonPromptIdeogram.style — which are empty because a remote catalogue
// has not been fetched, NOT because a folder is unregistered. Diagnosing those
// as "add a key to extra_model_paths.yaml" would be wrong advice, so the loose
// rule is not used.
func comfyLooksLikeModelInput(inputName string, options []string) bool {
	for _, option := range options {
		if comfyIsModelFilename(option) {
			return true
		}
	}
	if len(options) > 0 {
		return false
	}
	lowered := strings.ToLower(strings.TrimSpace(inputName))
	return lowered == "name" || strings.HasSuffix(lowered, "_name")
}

// comfyEachInput walks every input of every class in a deterministic order
// (classes sorted, groups in declaration order, inputs in wire order) and calls
// visit for each one. Options always come from store.ParseComboOptions, never
// from hand-indexing the tuple, so both live shapes are read. Non-COMBO inputs
// are visited too (shape == store.ComboNone) because an explicitly named input
// that turns out NOT to be a COMBO is itself a diagnosis; callers that only
// care about COMBOs filter on shape.
func comfyEachInput(classes map[string]json.RawMessage, visit func(loader comfyLoaderInput, spec interface{}, options []string, shape store.ComboShape)) (classCount, comboCount int) {
	names := make([]string, 0, len(classes))
	for name := range classes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, className := range names {
		schema, err := comfyDecodeClass(className, classes[className])
		if err != nil {
			continue
		}
		classCount++
		for _, group := range schema.Groups {
			if !comfyScannedGroups[group.Requirement] {
				continue
			}
			for _, inputName := range group.Order {
				spec := group.Specs[inputName]
				options, shape := store.ParseComboOptions(spec)
				if shape != store.ComboNone {
					comboCount++
				}
				visit(comfyLoaderInput{
					ClassType:   className,
					Input:       inputName,
					Requirement: group.Requirement,
					Type:        comfyDeclaredType(spec, shape),
					ComboShape:  string(shape),
					OptionCount: len(options),
				}, spec, options, shape)
			}
		}
	}
	return classCount, comboCount
}

// comfyScanModelVisibility classifies filename against every COMBO input in the
// schema (optionally narrowed to one class and/or one input). Only interesting
// hits are kept: the file was found, the COMBO is empty, the COMBO holds files
// of the same kind, or the caller named an input by hand that is not a COMBO at
// all.
func comfyScanModelVisibility(classes map[string]json.RawMessage, filename, classFilter, inputFilter string) (hits []comfyModelHit, classCount, comboCount, consideredCount int) {
	classCount, comboCount = comfyEachInput(classes, func(loader comfyLoaderInput, spec interface{}, options []string, shape store.ComboShape) {
		if classFilter != "" && !strings.EqualFold(loader.ClassType, classFilter) {
			return
		}
		if inputFilter != "" && !strings.EqualFold(loader.Input, inputFilter) {
			return
		}
		// A primitive input is only worth reporting when the caller named it;
		// otherwise every INT and STRING in the schema would answer
		// "no-such-input" and drown the real evidence.
		if shape == store.ComboNone && inputFilter == "" {
			return
		}
		consideredCount++
		visibility, _ := store.ClassifyModelVisibility(spec, filename)
		sameKind := comfySameKindOptions(filename, options)
		if visibility == store.ModelNotListed && !sameKind {
			return
		}
		// An empty COMBO is only class-unregistered EVIDENCE when the input is
		// model-folder-shaped. A real box serves ~150 empty COMBOs, and most are
		// API-node enums that populate at call time (SaveVideo.codec,
		// ClaudeNode.model, ...) — counting those would bury the handful of
		// genuinely unregistered model classes in noise. An input the caller
		// named explicitly is always reported, whatever its shape.
		if visibility == store.ModelClassUnregistered && inputFilter == "" && !comfyLooksLikeModelInput(loader.Input, options) {
			return
		}
		hits = append(hits, comfyModelHit{
			Loader:     loader,
			Visibility: visibility,
			Options:    options,
			SameKind:   sameKind,
		})
	})
	return hits, classCount, comboCount, consideredCount
}

// comfyModelWhyReport is the structured answer. Evidence for every bucket is
// always included, whatever the verdict, so the operator can see what the
// verdict was chosen against.
type comfyModelWhyReport struct {
	Filename        string             `json:"filename"`
	Verdict         string             `json:"verdict"`
	Summary         string             `json:"summary"`
	Remedy          string             `json:"remedy"`
	OfferedBy       []comfyLoaderInput `json:"offered_by,omitempty"`
	EmptyCombos     []comfyLoaderInput `json:"empty_combos,omitempty"`
	SameKindLoaders []comfyLoaderInput `json:"same_kind_loaders,omitempty"`
	NotACombo       []comfyLoaderInput `json:"not_a_combo,omitempty"`
	SampleFrom      string             `json:"sample_from,omitempty"`
	Sample          []string           `json:"sample_offered,omitempty"`
	ClassesScanned  int                `json:"classes_scanned"`
	CombosScanned   int                `json:"combo_inputs_scanned"`
	CombosConsidted int                `json:"combo_inputs_considered"`
	Narrowed        bool               `json:"narrowed,omitempty"`
}

// comfyWhySampleSize bounds the "here is what IS offered" evidence list.
const comfyWhySampleSize = 8

// comfyBuildWhyReport aggregates per-input verdicts into exactly one of the
// four causes.
//
// Precedence — visible > class-unregistered > not-listed > no-such-input.
// class-unregistered outranks not-listed deliberately: model extensions are
// shared across every model class (.safetensors is a checkpoint, a LoRA, a VAE
// and a controlnet), so the same-extension signal fires almost always and would
// otherwise bury the empty-COMBO signal — which is the one case a file copy can
// never fix. Both bodies of evidence stay in the report either way.
//
// When the scan was narrowed to exactly one input, that input's own
// classification IS the verdict; aggregation precedence does not apply because
// the caller already named the loader they care about.
func comfyBuildWhyReport(filename string, hits []comfyModelHit, classCount, comboCount, consideredCount int, narrowed bool) comfyModelWhyReport {
	report := comfyModelWhyReport{
		Filename:        filename,
		ClassesScanned:  classCount,
		CombosScanned:   comboCount,
		CombosConsidted: consideredCount,
		Narrowed:        narrowed,
	}
	for _, hit := range hits {
		switch {
		case hit.Visibility == store.ModelVisible:
			report.OfferedBy = append(report.OfferedBy, hit.Loader)
		case hit.Visibility == store.ModelClassUnregistered:
			report.EmptyCombos = append(report.EmptyCombos, hit.Loader)
		case hit.Visibility == store.ModelNoSuchInput:
			report.NotACombo = append(report.NotACombo, hit.Loader)
		case hit.SameKind:
			report.SameKindLoaders = append(report.SameKindLoaders, hit.Loader)
			if report.SampleFrom == "" {
				report.SampleFrom = hit.Loader.String()
				sample := hit.Options
				if len(sample) > comfyWhySampleSize {
					sample = sample[:comfyWhySampleSize]
				}
				report.Sample = append([]string{}, sample...)
			}
		}
	}

	verdict := store.ModelNoSuchInput
	switch {
	case narrowed && len(hits) == 1:
		verdict = hits[0].Visibility
	case len(report.OfferedBy) > 0:
		verdict = store.ModelVisible
	case len(report.EmptyCombos) > 0:
		verdict = store.ModelClassUnregistered
	case len(report.SameKindLoaders) > 0:
		verdict = store.ModelNotListed
	}
	report.Verdict = string(verdict)
	report.Summary, report.Remedy = comfyWhyProse(verdict, report)
	return report
}

// comfyWhySummaryLoaders is how many loader inputs a one-line summary names
// before collapsing the rest into a count. A real box can match dozens; the
// complete list always stays in the structured payload.
const comfyWhySummaryLoaders = 6

// comfyLoaderList renders a capped, human-readable loader list.
func comfyLoaderList(loaders []comfyLoaderInput) string {
	names := make([]string, 0, len(loaders))
	for _, loader := range loaders {
		names = append(names, loader.String())
	}
	if len(names) > comfyWhySummaryLoaders {
		return strings.Join(names[:comfyWhySummaryLoaders], ", ") + fmt.Sprintf(", +%d more", len(names)-comfyWhySummaryLoaders)
	}
	return strings.Join(names, ", ")
}

// comfyWhyProse maps a verdict to its summary and its remedy. Kept pure and
// separate so a test can assert that class-unregistered never degrades into
// "the file is missing".
func comfyWhyProse(verdict store.ModelVisibility, report comfyModelWhyReport) (summary, remedy string) {
	switch verdict {
	case store.ModelVisible:
		loaders := comfyLoaderList(report.OfferedBy)
		if loaders == "" {
			loaders = "the requested loader input"
		}
		return fmt.Sprintf("%s is VISIBLE: offered by %s", report.Filename, loaders),
			"No action needed — the loader lists this file. If a run still fails validation, the graph is passing a DIFFERENT string: the value must match the listed option byte-for-byte, including any subfolder prefix, case, and extension."
	case store.ModelClassUnregistered:
		summary = fmt.Sprintf("%s is not offered anywhere, and %d model-folder input(s) are registered but EMPTY: %s", report.Filename, len(report.EmptyCombos), comfyLoaderList(report.EmptyCombos))
		// Both signals are reported, never just the winning one: an empty
		// model COMBO proves a class is unregistered on this server, but not
		// that it is THIS file's class. Say so instead of implying certainty.
		if len(report.SameKindLoaders) > 0 {
			summary += fmt.Sprintf(". Competing signal: %d loader input(s) already list other files of this kind — if your model belongs to one of those, read this as not-listed instead (see same_kind_loaders)", len(report.SameKindLoaders))
			// The base remedy closes on an ABSOLUTE denial ("copying the model
			// into a directory will not fix it"). That sentence is only true when
			// the empty COMBO really is this file's class. On a real box the
			// unscoped scan finds populated same-kind loaders for almost any
			// common extension, and leaving the denial unqualified tells the
			// operator the one fix that would have worked is impossible. Qualify
			// it and hand them the command that settles the question.
			return summary, comfyClassUnregisteredRemedy +
				fmt.Sprintf(" CAVEAT: that denial holds only if the empty input above is THIS file's class, and it is not established here — %d loader input(s) on this server already list other files of the same kind, which would instead mean the file simply is not in an already-registered folder. Settle it by naming the loader you actually use: 'comfyui-pp-cli models why %s --class <ClassType> --input <input>' (see same_kind_loaders for candidates), which classifies that one input directly instead of aggregating.",
					len(report.SameKindLoaders), report.Filename)
		}
		return summary, comfyClassUnregisteredRemedy
	case store.ModelNotListed:
		return fmt.Sprintf("%s is NOT LISTED: %d loader input(s) already list other files of this kind (%s) but not this one", report.Filename, len(report.SameKindLoaders), comfyLoaderList(report.SameKindLoaders)),
			"The folder for this model class IS registered and populated — the file just is not in it. Put the file inside the registered folder (a subfolder is only picked up if ComfyUI scans it), confirm the extension matches the listed files exactly, and restart or refresh ComfyUI so the folder is re-scanned. Compare the name against the sample of what IS offered."
	default:
		if len(report.NotACombo) > 0 {
			named := report.NotACombo[0]
			return fmt.Sprintf("%s.%s is not a COMBO at all (declared type %s): that input never holds a model list", named.ClassType, named.Input, named.Type),
				"The input you named takes a value, not a file from a registered folder. Run 'comfyui-pp-cli nodes show " + named.ClassType + "' to see which of its inputs are COMBOs, then re-run without --input to scan every loader."
		}
		return fmt.Sprintf("%s matches NO COMBO input: no loader in the live schema lists a file of this kind, and none is empty", report.Filename),
			"Nothing in the running schema would ever load this file. The node class that provides the loader is probably not installed (missing custom node pack), or the extension is not one any registered loader accepts. Run 'comfyui-pp-cli nodes search <keyword>' to check whether the loader class exists at all."
	}
}

// ---------------------------------------------------------------------------
// models list
// ---------------------------------------------------------------------------

// comfyFolderGroup groups loader inputs that share a name — ComfyUI does not
// expose folder_paths keys over /object_info, so the loader INPUT NAME
// (ckpt_name, lora_name, vae_name...) is the honest grouping key rather than an
// invented folder mapping.
type comfyFolderGroup struct {
	Key         string   `json:"key"`
	Classes     []string `json:"classes"`
	OptionCount int      `json:"option_count"`
	Status      string   `json:"status"`
	Remedy      string   `json:"remedy,omitempty"`
	Files       []string `json:"files,omitempty"`
}

// comfyGroupModelFolders unions the option lists of every loader input sharing
// a name. Deterministic: classes are visited in sorted order and first-seen
// file order is preserved within each group (that is ComfyUI's own scan order).
func comfyGroupModelFolders(classes map[string]json.RawMessage, includeAll bool) []comfyFolderGroup {
	type accumulator struct {
		classes []string
		files   []string
		seen    map[string]bool
		modelly bool
	}
	groups := map[string]*accumulator{}
	comfyEachInput(classes, func(loader comfyLoaderInput, spec interface{}, options []string, shape store.ComboShape) {
		if shape == store.ComboNone {
			return
		}
		acc := groups[loader.Input]
		if acc == nil {
			acc = &accumulator{seen: map[string]bool{}}
			groups[loader.Input] = acc
		}
		acc.classes = append(acc.classes, loader.ClassType)
		if comfyLooksLikeModelInput(loader.Input, options) {
			acc.modelly = true
		}
		for _, option := range options {
			if acc.seen[option] {
				continue
			}
			acc.seen[option] = true
			acc.files = append(acc.files, option)
		}
	})

	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]comfyFolderGroup, 0, len(keys))
	for _, key := range keys {
		acc := groups[key]
		if !includeAll && !acc.modelly {
			continue
		}
		group := comfyFolderGroup{
			Key:         key,
			Classes:     acc.classes,
			OptionCount: len(acc.files),
			Files:       acc.files,
			Status:      "ok",
		}
		if len(acc.files) == 0 {
			group.Status = string(store.ModelClassUnregistered)
			group.Remedy = comfyClassUnregisteredRemedy
		}
		out = append(out, group)
	}
	return out
}

// comfyFilterFolderGroups applies --folder (case-insensitive substring on the
// grouping key) and --empty.
func comfyFilterFolderGroups(groups []comfyFolderGroup, folder string, emptyOnly bool) []comfyFolderGroup {
	folder = strings.ToLower(strings.TrimSpace(folder))
	out := make([]comfyFolderGroup, 0, len(groups))
	for _, group := range groups {
		if folder != "" && !strings.Contains(strings.ToLower(group.Key), folder) {
			continue
		}
		if emptyOnly && group.Status != string(store.ModelClassUnregistered) {
			continue
		}
		out = append(out, group)
	}
	return out
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

// newModelsCmd is the model-visibility parent. Every command in this family reads
// the running server's /object_info and nothing else — there is no local mirror of
// the loader lists, because "what can this box load right now" is only true live.
//
// pp:data-source live
func newModelsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "Answer what the loaders can actually load — and why one file is invisible",
		Long: `Model visibility, read from the live node schema.

ComfyUI never tells you WHY a model is invisible; it fails a run with
"value not in list: ... not in []" and leaves you to guess. These commands
separate the four causes that need four different fixes — most importantly the
one that looks like a missing file but is not: an EMPTY option list means the
model CLASS has no registered folder (a missing extra_model_paths.yaml key).`,
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:parent-group":     "true",
			"pp:typed-exit-codes": "0,2,3,12,13",
		},
		RunE: parentNoSubcommandRunE(flags),
	}
	cmd.AddCommand(newModelsWhyCmd(flags))
	cmd.AddCommand(newModelsLsCmd(flags))
	return cmd
}

func newModelsWhyCmd(flags *rootFlags) *cobra.Command {
	var classFilter, inputFilter string
	cmd := &cobra.Command{
		Use:   "why <filename>",
		Short: "Explain exactly why ComfyUI can or cannot see one model file",
		Long: `Scan every COMBO input on every registered class for <filename> and return one
of four verdicts, each with its own remedy:

  visible             a loader lists it — pass the string EXACTLY as listed
  class-unregistered  a COMBO is registered but EMPTY: no folder is registered
                      for that model class (missing extra_model_paths.yaml key).
                      This is NOT a missing file; copying the model will not fix it
  not-listed          the folder is registered and holds other files of this kind,
                      but not this one
  no-such-input       nothing in the live schema would load a file of this kind —
                      the loader's custom node pack is probably not installed

<filename> is the bare name as the loader would list it (LoadImage-style bare
filenames, optionally with ComfyUI's subfolder prefix), never a host path.

Use --class/--input to ask about ONE loader input; that input's own
classification is then the verdict.

Exit codes:
  0   visible
  3   the --class/--input filter matched no COMBO input
  12  class-unregistered
  13  not-listed or no-such-input`,
		Example: `  comfyui-pp-cli models why sd_xl_base_1.0.safetensors
  comfyui-pp-cli models why 4x-UltraSharp.pth --json
  comfyui-pp-cli models why my.safetensors --class CheckpointLoaderSimple --input ckpt_name`,
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": "0,2,3,12,13",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && !flags.dryRun {
				return comfyRequiresInput(cmd, flags)
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "models why")
			}
			filename := strings.TrimSpace(args[0])
			if filename == "" {
				return usageErr(fmt.Errorf("models why needs a filename"))
			}
			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			classes, err := comfyFetchObjectInfo(ctx, flags)
			if err != nil {
				return err
			}
			hits, classCount, comboCount, considered := comfyScanModelVisibility(classes, filename, classFilter, inputFilter)
			narrowed := classFilter != "" && inputFilter != ""
			if (classFilter != "" || inputFilter != "") && considered == 0 {
				return notFoundErr(fmt.Errorf("no COMBO input matches --class %q --input %q; run 'comfyui-pp-cli models list' to see the loader inputs that exist", classFilter, inputFilter))
			}
			report := comfyBuildWhyReport(filename, hits, classCount, comboCount, considered, narrowed)
			if err := comfyRenderWhy(cmd.OutOrStdout(), flags, report); err != nil {
				return err
			}
			switch report.Verdict {
			case string(store.ModelVisible):
				return nil
			case string(store.ModelClassUnregistered):
				return comfyClassUnregisteredErr(fmt.Errorf("%s: %s", report.Summary, report.Remedy))
			default:
				return comfyModelNotVisibleErr(fmt.Errorf("%s: %s", report.Summary, report.Remedy))
			}
		},
	}
	cmd.Flags().StringVar(&classFilter, "class", "", "Restrict the scan to one node class")
	cmd.Flags().StringVar(&inputFilter, "input", "", "Restrict the scan to one input name (e.g. ckpt_name)")
	return cmd
}

func comfyRenderWhy(w io.Writer, flags *rootFlags, report comfyModelWhyReport) error {
	if !wantsHumanTable(w, flags) {
		return printJSONFiltered(w, report, flags)
	}
	fmt.Fprintf(w, "%-10s %s\n", "FILE", report.Filename)
	fmt.Fprintf(w, "%-10s %s\n", "VERDICT", report.Verdict)
	fmt.Fprintf(w, "%-10s %s\n", "SUMMARY", report.Summary)
	fmt.Fprintf(w, "%-10s %d classes, %d COMBO inputs scanned\n", "SCANNED", report.ClassesScanned, report.CombosScanned)

	renderLoaders := func(label string, loaders []comfyLoaderInput) {
		if len(loaders) == 0 {
			return
		}
		fmt.Fprintf(w, "\n%s\n", label)
		for _, loader := range loaders {
			fmt.Fprintf(w, "  %-52s %s, %d option(s)\n", loader.String(), loader.ComboShape, loader.OptionCount)
		}
	}
	renderLoaders("OFFERED BY", report.OfferedBy)
	renderLoaders("EMPTY COMBOS (class unregistered)", report.EmptyCombos)
	renderLoaders("LOADERS HOLDING FILES OF THE SAME KIND", report.SameKindLoaders)
	renderLoaders("NAMED INPUTS THAT ARE NOT COMBOS", report.NotACombo)
	if len(report.Sample) > 0 {
		fmt.Fprintf(w, "\nSAMPLE OF WHAT %s DOES OFFER\n", report.SampleFrom)
		for _, file := range report.Sample {
			fmt.Fprintf(w, "  %s\n", file)
		}
	}
	fmt.Fprintf(w, "\nREMEDY\n  %s\n", report.Remedy)
	return nil
}

// newModelsLsCmd groups every model-file COMBO by the loader input that offers it.
//
// Named `list` for cross-CLI consistency; `ls` stays as an alias.
func newModelsLsCmd(flags *rootFlags) *cobra.Command {
	var folder string
	var emptyOnly bool
	var includeAll bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List what the loaders currently offer, grouped by loader input",
		Long: `List the model files the running server currently offers, grouped by the
loader input that offers them.

The grouping key is the loader INPUT NAME (ckpt_name, lora_name, vae_name...).
ComfyUI does not expose folder_paths keys over /object_info, so an input name is
the honest key; --folder matches it as a case-insensitive substring.

Groups with ZERO files are reported as class-unregistered — a missing
extra_model_paths.yaml key, not a missing file. 'list --empty' is the fastest way
to find every model class the server cannot see at all.`,
		Example: `  comfyui-pp-cli models list
  comfyui-pp-cli models list --folder ckpt
  comfyui-pp-cli models list --empty
  comfyui-pp-cli models list --all --json`,
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": "0,2,3",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usageErr(fmt.Errorf("models list takes no positional arguments; use --folder to narrow"))
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "models list")
			}
			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			classes, err := comfyFetchObjectInfo(ctx, flags)
			if err != nil {
				return err
			}
			groups := comfyGroupModelFolders(classes, includeAll)
			filtered := comfyFilterFolderGroups(groups, folder, emptyOnly)
			if len(filtered) == 0 && folder != "" {
				return notFoundErr(fmt.Errorf("no loader input matches --folder %q; run 'comfyui-pp-cli models list' for the full list of grouping keys", folder))
			}
			// Files are inlined when the caller narrowed to a folder; a full
			// listing would otherwise dump every file on the box.
			withFiles := folder != ""
			return comfyRenderFolders(cmd.OutOrStdout(), flags, filtered, withFiles)
		},
	}
	cmd.Flags().StringVar(&folder, "folder", "", "Only groups whose loader input name contains this substring (also inlines their file lists)")
	cmd.Flags().BoolVar(&emptyOnly, "empty", false, "Only groups that offer zero files (unregistered model classes)")
	cmd.Flags().BoolVar(&includeAll, "all", false, "Include every COMBO input, not just the model-file ones")
	return cmd
}

func comfyRenderFolders(w io.Writer, flags *rootFlags, groups []comfyFolderGroup, withFiles bool) error {
	total := 0
	unregistered := 0
	for _, group := range groups {
		total += group.OptionCount
		if group.Status == string(store.ModelClassUnregistered) {
			unregistered++
		}
	}

	if !wantsHumanTable(w, flags) {
		payload := groups
		if !withFiles {
			payload = make([]comfyFolderGroup, len(groups))
			copy(payload, groups)
			for i := range payload {
				payload[i].Files = nil
			}
		}
		return printJSONFiltered(w, map[string]any{
			"groups":                   payload,
			"group_count":              len(groups),
			"file_count":               total,
			"unregistered_group_count": unregistered,
			"files_included":           withFiles,
		}, flags)
	}

	if len(groups) == 0 {
		fmt.Fprintln(w, "no loader inputs matched")
		return nil
	}
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "INPUT\tFILES\tSTATUS\tOFFERED BY")
	for _, group := range groups {
		classes := group.Classes
		shown := strings.Join(classes, ", ")
		if len(classes) > 4 {
			shown = strings.Join(classes[:4], ", ") + fmt.Sprintf(", +%d more", len(classes)-4)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", group.Key, group.OptionCount, group.Status, shown)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if withFiles {
		for _, group := range groups {
			fmt.Fprintf(w, "\n%s (%d)\n", group.Key, group.OptionCount)
			if len(group.Files) == 0 {
				fmt.Fprintf(w, "  (empty) %s\n", comfyClassUnregisteredRemedy)
				continue
			}
			for _, file := range group.Files {
				fmt.Fprintf(w, "  %s\n", file)
			}
		}
	}
	fmt.Fprintf(w, "\n%d group(s), %d file(s)", len(groups), total)
	if unregistered > 0 {
		fmt.Fprintf(w, ", %d unregistered model class(es)", unregistered)
		// Only point at --empty when the current listing is not already it.
		if unregistered != len(groups) {
			fmt.Fprintf(w, " — run 'models list --empty' for just those")
		}
	}
	fmt.Fprintln(w)
	return nil
}
