// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
//
// Hand-authored novel command surface (NOT generator output): the node-schema
// family. The generated `objectinfo` mirror hands back the raw /object_info
// payload; this family answers the question an operator actually has —
// "what can this loader ACTUALLY load right now?" — by reading the COMBO
// option VALUES out of the live schema.
//
// Two facts drive every line below, both observed on a real ComfyUI 0.32.0 box:
//
//  1. COMBO options ship in TWO shapes at once — v3 puts them at tuple index 1
//     under {"options": [...]}, legacy puts the list itself at index 0. Measured
//     against the live server (1133 classes): 592 v3 COMBOs and 400 legacy ones
//     served simultaneously, so a reader that assumes one shape silently reports
//     "no options" for ~40% of inputs — including CheckpointLoaderSimple, which
//     is legacy. Every read here goes through store.ParseComboOptions; nothing
//     indexes the tuple by hand.
//  2. An EMPTY option list on a recognised COMBO means the model CLASS is
//     unregistered — ComfyUI has no folder registered for it (a missing
//     extra_model_paths.yaml key). It does NOT mean a file is missing. That
//     confusion cost real debugging time when `latent_upscale_models` surfaced
//     only as the opaque validation error "value not in list: ... not in []".

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"comfyui-pp-cli/internal/store"
	"github.com/spf13/cobra"
)

// comfyExitClassUnregistered is the DISTINCT exit code for "this COMBO input is
// registered but offers ZERO options". It is deliberately not 3 (not found):
// nothing is missing on disk, so an agent must be able to branch on this
// outcome without string-matching a message. 12 is unused by the generated
// framework (2 usage, 3 not-found, 4 auth, 5 api, 6 partial, 7 rate-limit,
// 10 config).
// The value lives in exitcodes.go as ExitNodeClassDrift; this alias keeps the
// nodes-family call sites reading in their own vocabulary.
const comfyExitClassUnregistered = ExitNodeClassDrift

// comfyExitModelNotVisible is the exit code for `models why` when the file is
// simply not offered by any loader and no COMBO is empty (not-listed /
// no-such-input). Separate from 12 so "unregistered class" and "file not in a
// registered folder" never collapse into one branch.
// The value lives in exitcodes.go as ExitModelNotVisible.
const comfyExitModelNotVisible = ExitModelNotVisible

func comfyClassUnregisteredErr(err error) error {
	return &cliError{code: comfyExitClassUnregistered, err: err}
}

func comfyModelNotVisibleErr(err error) error {
	return &cliError{code: comfyExitModelNotVisible, err: err}
}

// comfyClassUnregisteredRemedy is the one message this whole family exists to
// deliver. Kept as a constant so `nodes options`, `nodes show`, `models why`,
// and `models list` all say the same thing, and so a test can assert it never
// degrades into "file missing".
const comfyClassUnregisteredRemedy = "the model CLASS is unregistered: ComfyUI has no folder registered for this input, so the loader offers an EMPTY list. Add the missing key to extra_model_paths.yaml (or register the folder in folder_paths) and RESTART ComfyUI. This is NOT a missing file — copying the model into a directory will not fix it."

// comfyModelExtensions are the suffixes that mark a COMBO option as a model
// file rather than an enum value (sampler_name, scheduler, upscale_method...).
// Used only as a heuristic for grouping and for the same-kind evidence in
// `models why`; never to decide whether a file exists.
var comfyModelExtensions = []string{
	".safetensors", ".sft", ".ckpt", ".pt", ".pth", ".bin", ".gguf",
	".onnx", ".engine", ".pkl", ".npz", ".yaml", ".yml", ".vae", ".lora",
}

// ---------------------------------------------------------------------------
// Schema decoding
// ---------------------------------------------------------------------------

// comfyClassRaw is the wire shape of one entry in /object_info. `input` is kept
// as raw JSON per group so declaration order survives (Go maps do not preserve
// it, and widget order is the order an operator sees in the UI).
type comfyClassRaw struct {
	Input        map[string]json.RawMessage `json:"input"`
	Output       json.RawMessage            `json:"output"`
	OutputName   json.RawMessage            `json:"output_name"`
	Name         string                     `json:"name"`
	DisplayName  string                     `json:"display_name"`
	Description  string                     `json:"description"`
	Category     string                     `json:"category"`
	OutputNode   bool                       `json:"output_node"`
	Deprecated   bool                       `json:"deprecated"`
	Experimental bool                       `json:"experimental"`
	PythonModule string                     `json:"python_module"`
}

// comfyInputGroup is one of required / optional / hidden, with declaration order
// preserved in Order.
type comfyInputGroup struct {
	Requirement string
	Order       []string
	Specs       map[string]interface{}
}

// comfyClassSchema is the decoded, order-preserving view of one node class.
type comfyClassSchema struct {
	ClassType    string
	DisplayName  string
	Description  string
	Category     string
	PythonModule string
	OutputNode   bool
	Deprecated   bool
	Experimental bool
	Groups       []comfyInputGroup
	OutputTypes  []string
	OutputNames  []string
}

// comfyGroupOrder fixes the order groups are rendered and scanned in. Any group
// ComfyUI adds later is appended after these, sorted, so an unknown group is
// surfaced rather than dropped.
var comfyGroupOrder = []string{"required", "optional", "hidden"}

// comfyOrderedKeys returns the keys of a JSON object in wire order. json.Unmarshal
// into a map loses that order; the node schema's input order is meaningful, so
// the keys are pulled off the token stream instead. Returns nil for anything
// that is not a JSON object.
func comfyOrderedKeys(raw json.RawMessage) []string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil
	}
	var keys []string
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return keys
		}
		key, ok := keyTok.(string)
		if !ok {
			return keys
		}
		keys = append(keys, key)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return keys
		}
	}
	return keys
}

// comfyDecodeInputGroup decodes one input group, preserving declaration order
// when the token stream agrees with the decoded map (it always does for
// well-formed /object_info payloads; the sorted fallback keeps output
// deterministic if it ever does not).
func comfyDecodeInputGroup(requirement string, raw json.RawMessage) (comfyInputGroup, bool) {
	var specs map[string]interface{}
	if err := json.Unmarshal(raw, &specs); err != nil || specs == nil {
		return comfyInputGroup{}, false
	}
	order := comfyOrderedKeys(raw)
	if len(order) != len(specs) {
		order = comfySortedKeys(specs)
	}
	return comfyInputGroup{Requirement: requirement, Order: order, Specs: specs}, true
}

func comfySortedKeys(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// comfyDecodeClass turns one raw /object_info entry into an order-preserving
// schema view.
func comfyDecodeClass(classType string, raw json.RawMessage) (*comfyClassSchema, error) {
	var wire comfyClassRaw
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("decoding schema for %q: %w", classType, err)
	}
	schema := &comfyClassSchema{
		ClassType:    classType,
		DisplayName:  wire.DisplayName,
		Description:  wire.Description,
		Category:     wire.Category,
		PythonModule: wire.PythonModule,
		OutputNode:   wire.OutputNode,
		Deprecated:   wire.Deprecated,
		Experimental: wire.Experimental,
		OutputTypes:  comfyOutputTypes(wire.Output),
		OutputNames:  comfyOutputTypes(wire.OutputName),
	}
	seen := map[string]bool{}
	for _, name := range comfyGroupOrder {
		seen[name] = true
		if group, ok := comfyDecodeInputGroup(name, wire.Input[name]); ok {
			schema.Groups = append(schema.Groups, group)
		}
	}
	extra := make([]string, 0, len(wire.Input))
	for name := range wire.Input {
		if !seen[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		if group, ok := comfyDecodeInputGroup(name, wire.Input[name]); ok {
			schema.Groups = append(schema.Groups, group)
		}
	}
	return schema, nil
}

// comfyOutputTypes renders the `output` / `output_name` arrays as display
// strings. Either can be a bare string, a list of strings, or a list that
// contains a nested literal list (an output typed as a COMBO).
func comfyOutputTypes(raw json.RawMessage) []string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var arr []interface{}
	if err := json.Unmarshal(raw, &arr); err != nil {
		var single string
		if json.Unmarshal(raw, &single) == nil {
			return []string{single}
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		switch typed := item.(type) {
		case string:
			out = append(out, typed)
		case []interface{}:
			parts := make([]string, 0, len(typed))
			for _, element := range typed {
				parts = append(parts, fmt.Sprintf("%v", element))
			}
			out = append(out, "COMBO["+truncate(strings.Join(parts, ","), 60)+"]")
		default:
			out = append(out, fmt.Sprintf("%v", typed))
		}
	}
	return out
}

// comfyFindInput locates one input's spec anywhere in the class (required,
// optional, hidden), returning the group it lives in. An exact match wins; a
// case-insensitive match is the fallback so `VAE_NAME` still resolves.
func comfyFindInput(schema *comfyClassSchema, input string) (interface{}, string, bool) {
	if schema == nil {
		return nil, "", false
	}
	for _, group := range schema.Groups {
		if spec, ok := group.Specs[input]; ok {
			return spec, group.Requirement, true
		}
	}
	lowered := strings.ToLower(input)
	for _, group := range schema.Groups {
		for _, name := range group.Order {
			if strings.ToLower(name) == lowered {
				return group.Specs[name], group.Requirement, true
			}
		}
	}
	return nil, "", false
}

// comfyInputNames lists every input name on a class, in declaration order,
// grouped-prefixed for the "did you mean" hint.
func comfyInputNames(schema *comfyClassSchema) []string {
	if schema == nil {
		return nil
	}
	var out []string
	for _, group := range schema.Groups {
		out = append(out, group.Order...)
	}
	return out
}

// ---------------------------------------------------------------------------
// Input summary — the COMBO-aware view of one input spec
// ---------------------------------------------------------------------------

// comfyInputSummary is the structured answer to "what is this input and what
// may it hold?". Status/Remedy carry the class-unregistered verdict.
type comfyInputSummary struct {
	Name        string   `json:"name"`
	Requirement string   `json:"requirement"`
	Kind        string   `json:"kind"`
	Type        string   `json:"type,omitempty"`
	ComboShape  string   `json:"combo_shape,omitempty"`
	ShapeDetail string   `json:"combo_shape_detail,omitempty"`
	OptionCount int      `json:"option_count"`
	Options     []string `json:"options,omitempty"`
	Status      string   `json:"status"`
	Remedy      string   `json:"remedy,omitempty"`
	Default     any      `json:"default,omitempty"`
	Min         any      `json:"min,omitempty"`
	Max         any      `json:"max,omitempty"`
	Step        any      `json:"step,omitempty"`
	Multiline   bool     `json:"multiline,omitempty"`
	Tooltip     string   `json:"tooltip,omitempty"`
}

// comfyShapeDetail spells out where the options were found, so a reader never
// has to guess which of the two live shapes a class uses.
func comfyShapeDetail(shape store.ComboShape) string {
	switch shape {
	case store.ComboV3:
		return "v3 spec: options at tuple index 1 under {\"options\": [...]}"
	case store.ComboLegacy:
		return "legacy spec: options are the list at tuple index 0"
	default:
		return "not a COMBO: no option list in either shape"
	}
}

// comfyDeclaredType reads the declared type token out of an input spec. It is
// the string at tuple index 0 when there is one; a COMBO carries a list there
// in the legacy shape, so the shape supplies the name instead.
func comfyDeclaredType(spec interface{}, shape store.ComboShape) string {
	if declared, ok := spec.(string); ok {
		return declared
	}
	if arr, ok := spec.([]interface{}); ok && len(arr) > 0 {
		if declared, ok := arr[0].(string); ok {
			return declared
		}
	}
	if shape != store.ComboNone {
		return "COMBO"
	}
	return "UNKNOWN"
}

// comfySummarizeInput is the single place an input spec is interpreted. It
// never indexes the tuple for options itself — store.ParseComboOptions owns
// that, because both shapes ship simultaneously.
func comfySummarizeInput(name, requirement string, spec interface{}) comfyInputSummary {
	summary := comfyInputSummary{Name: name, Requirement: requirement}
	options, shape := store.ParseComboOptions(spec)
	summary.ShapeDetail = comfyShapeDetail(shape)
	summary.Type = comfyDeclaredType(spec, shape)

	arr, _ := spec.([]interface{})
	meta := map[string]interface{}{}
	if len(arr) > 1 {
		if m, ok := arr[1].(map[string]interface{}); ok {
			meta = m
		}
	}

	if shape == store.ComboNone {
		summary.Kind = "primitive"
		summary.Status = string(store.ModelNoSuchInput)
	} else {
		summary.Kind = "combo"
		summary.ComboShape = string(shape)
		summary.OptionCount = len(options)
		summary.Options = options
		if len(options) == 0 {
			summary.Status = string(store.ModelClassUnregistered)
			summary.Remedy = comfyClassUnregisteredRemedy
		} else {
			summary.Status = "ok"
		}
	}

	if v, ok := meta["default"]; ok {
		summary.Default = v
	}
	if v, ok := meta["min"]; ok {
		summary.Min = v
	}
	if v, ok := meta["max"]; ok {
		summary.Max = v
	}
	if v, ok := meta["step"]; ok {
		summary.Step = v
	}
	if v, ok := meta["multiline"].(bool); ok {
		summary.Multiline = v
	}
	if v, ok := meta["tooltip"].(string); ok {
		summary.Tooltip = v
	}
	return summary
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

// comfyFetchObjectInfo pulls the whole live schema. Read-only GET through the
// generated client, so --timeout, --no-cache, and the configured base_url all
// behave exactly as they do on generated commands.
func comfyFetchObjectInfo(ctx context.Context, flags *rootFlags) (map[string]json.RawMessage, error) {
	c, err := flags.newClient()
	if err != nil {
		return nil, err
	}
	data, err := c.Get(ctx, "/object_info", nil)
	if err != nil {
		return nil, classifyAPIError(err, flags)
	}
	var classes map[string]json.RawMessage
	if err := json.Unmarshal(data, &classes); err != nil {
		return nil, apiErr(fmt.Errorf("/object_info did not return a class map: %w", err))
	}
	if len(classes) == 0 {
		return nil, apiErr(fmt.Errorf("/object_info returned an empty schema; is the ComfyUI server still starting up?"))
	}
	return classes, nil
}

// comfyFetchClass pulls one class from /object_info/<class>. ComfyUI answers an
// unknown class with an empty object rather than a 404, so the empty case is
// translated into the not-found exit code here.
func comfyFetchClass(ctx context.Context, flags *rootFlags, classType string) (*comfyClassSchema, error) {
	c, err := flags.newClient()
	if err != nil {
		return nil, err
	}
	path := replacePathParam("/object_info/{class_type}", "class_type", classType)
	data, err := c.Get(ctx, path, nil)
	if err != nil {
		return nil, classifyAPIError(err, flags)
	}
	var classes map[string]json.RawMessage
	if err := json.Unmarshal(data, &classes); err != nil {
		return nil, apiErr(fmt.Errorf("/object_info/%s did not return a class map: %w", classType, err))
	}
	if raw, ok := classes[classType]; ok {
		return comfyDecodeClass(classType, raw)
	}
	// Case-insensitive fallback: the endpoint echoes the canonical class name.
	lowered := strings.ToLower(classType)
	for name, raw := range classes {
		if strings.ToLower(name) == lowered {
			return comfyDecodeClass(name, raw)
		}
	}
	return nil, notFoundErr(fmt.Errorf("node class %q is not registered on this server; run 'comfyui-pp-cli nodes search %s' to find the real class name (a missing class usually means the custom node pack is not installed)", classType, classType))
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// comfyNodeMatch is one row of `nodes search` output.
type comfyNodeMatch struct {
	ClassType   string `json:"class_type"`
	DisplayName string `json:"display_name,omitempty"`
	Category    string `json:"category,omitempty"`
	Description string `json:"description,omitempty"`
	OutputNode  bool   `json:"output_node,omitempty"`
	Deprecated  bool   `json:"deprecated,omitempty"`
}

// comfySearchTokens lowercases and splits the query. Every token must match
// (AND), which is what makes a two-word query like "load lora" useful.
func comfySearchTokens(args []string) []string {
	var tokens []string
	for _, arg := range args {
		for _, field := range strings.Fields(strings.ToLower(arg)) {
			if field != "" {
				tokens = append(tokens, field)
			}
		}
	}
	return tokens
}

// comfyNodeMatches reports whether every token appears somewhere in the class
// name, display name, category, or description.
func comfyNodeMatches(match comfyNodeMatch, tokens []string) bool {
	if len(tokens) == 0 {
		return true
	}
	haystack := strings.ToLower(strings.Join([]string{
		match.ClassType, match.DisplayName, match.Category, match.Description,
	}, "\n"))
	for _, token := range tokens {
		if !strings.Contains(haystack, token) {
			return false
		}
	}
	return true
}

// comfySearchNodes runs the token-AND search over a decoded /object_info map.
// Deterministic: results come back sorted by class name.
func comfySearchNodes(classes map[string]json.RawMessage, tokens []string) []comfyNodeMatch {
	names := make([]string, 0, len(classes))
	for name := range classes {
		names = append(names, name)
	}
	sort.Strings(names)
	matches := make([]comfyNodeMatch, 0, 16)
	for _, name := range names {
		var wire comfyClassRaw
		if err := json.Unmarshal(classes[name], &wire); err != nil {
			continue
		}
		candidate := comfyNodeMatch{
			ClassType:   name,
			DisplayName: wire.DisplayName,
			Category:    wire.Category,
			Description: wire.Description,
			OutputNode:  wire.OutputNode,
			Deprecated:  wire.Deprecated,
		}
		if comfyNodeMatches(candidate, tokens) {
			matches = append(matches, candidate)
		}
	}
	return matches
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

// newNodesCmd is the node-schema parent. Every leaf under it reads /object_info from
// the running server through the generated client; the schema changes whenever a
// custom node is installed or a model folder is registered, so a cached answer would
// be wrong exactly when it matters.
//
// pp:data-source live
func newNodesCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nodes",
		Short: "Introspect the live node schema — including the COMBO option VALUES a loader actually offers",
		Long: `Read the live node schema from the running ComfyUI server.

The difference from a plain schema dump: these commands surface COMBO option
VALUES, so you can answer "what can this loader actually load?" without
submitting a graph and reading a validation error.

Both COMBO spec shapes are handled (v3 keeps options at tuple index 1 under
{"options": [...]}, legacy keeps them at index 0). ComfyUI ships both at the
same time, so anything that reads only one shape reports a false "no options".`,
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:parent-group":     "true",
			"pp:typed-exit-codes": "0,2,3,12",
		},
		RunE: parentNoSubcommandRunE(flags),
	}
	cmd.AddCommand(newNodesOptionsCmd(flags))
	cmd.AddCommand(newNodesShowCmd(flags))
	cmd.AddCommand(newNodesSearchCmd(flags))
	return cmd
}

// comfyRequiresInput mirrors the generated commands' bare-invocation contract:
// a human gets help at exit 0, a machine caller (--json/--agent) gets a
// structured usage error at exit 2 so an incomplete call is never mistaken for
// success. Positional validation lives here rather than in cobra's Args
// validator because Args runs before RunE and would break --dry-run probes.
func comfyRequiresInput(cmd *cobra.Command, flags *rootFlags) error {
	if flags != nil && flags.asJSON {
		if err := printJSONFiltered(cmd.OutOrStdout(), map[string]any{
			"error": "requires input",
			"usage": cmd.CommandPath() + " --help",
		}, flags); err != nil {
			return err
		}
		return usageErr(fmt.Errorf("%q requires input; run %q for usage", cmd.CommandPath(), cmd.CommandPath()+" --help"))
	}
	return cmd.Help()
}

func newNodesOptionsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "options <ClassType> <input>",
		Short: "Print the COMBO option values one node input actually accepts",
		Long: `Print every value a COMBO input will accept right now, read from the live
schema through the shape-aware parser (v3 options live at tuple index 1, legacy
options at index 0 — this box serves both).

Exit codes:
  0   the input is a COMBO and offers at least one option
  2   usage error, or the input exists but is NOT a COMBO (it has no option list)
  3   the class or the input does not exist on this server
  12  the COMBO is registered but offers ZERO options — the model CLASS is
      unregistered (missing extra_model_paths.yaml key). NOT a missing file.`,
		Example: `  comfyui-pp-cli nodes options VAELoader vae_name
  comfyui-pp-cli nodes options CheckpointLoaderSimple ckpt_name --json
  comfyui-pp-cli nodes options KSampler sampler_name`,
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": "0,2,3,12",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && !flags.dryRun {
				return comfyRequiresInput(cmd, flags)
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "nodes options")
			}
			if len(args) < 2 {
				return usageErr(fmt.Errorf("nodes options needs both a class and an input: %s <ClassType> <input>", cmd.CommandPath()))
			}
			classType, input := args[0], args[1]

			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			schema, err := comfyFetchClass(ctx, flags, classType)
			if err != nil {
				return err
			}
			spec, requirement, found := comfyFindInput(schema, input)
			if !found {
				names := comfyInputNames(schema)
				return notFoundErr(fmt.Errorf("node class %q has no input %q; it declares: %s", schema.ClassType, input, strings.Join(names, ", ")))
			}
			summary := comfySummarizeInput(input, requirement, spec)
			if err := comfyRenderOptions(cmd.OutOrStdout(), flags, schema.ClassType, summary); err != nil {
				return err
			}
			switch summary.Status {
			case string(store.ModelClassUnregistered):
				return comfyClassUnregisteredErr(fmt.Errorf("%s.%s is a %s COMBO with ZERO options: %s", schema.ClassType, summary.Name, summary.ComboShape, comfyClassUnregisteredRemedy))
			case string(store.ModelNoSuchInput):
				return usageErr(fmt.Errorf("%s.%s is not a COMBO (declared type %s); it has no option list. Run 'nodes show %s' for its full schema", schema.ClassType, summary.Name, summary.Type, schema.ClassType))
			}
			return nil
		},
	}
	return cmd
}

func comfyRenderOptions(w io.Writer, flags *rootFlags, classType string, summary comfyInputSummary) error {
	if !wantsHumanTable(w, flags) {
		payload := map[string]any{
			"class_type":         classType,
			"input":              summary.Name,
			"requirement":        summary.Requirement,
			"kind":               summary.Kind,
			"type":               summary.Type,
			"combo_shape":        summary.ComboShape,
			"combo_shape_detail": summary.ShapeDetail,
			"option_count":       summary.OptionCount,
			"options":            summary.Options,
			"status":             summary.Status,
		}
		if summary.Remedy != "" {
			payload["remedy"] = summary.Remedy
		}
		if summary.Default != nil {
			payload["default"] = summary.Default
		}
		if summary.Options == nil {
			payload["options"] = []string{}
		}
		return printJSONFiltered(w, payload, flags)
	}

	fmt.Fprintf(w, "%-8s %s\n", "CLASS", classType)
	fmt.Fprintf(w, "%-8s %s (%s)\n", "INPUT", summary.Name, summary.Requirement)
	fmt.Fprintf(w, "%-8s %s\n", "TYPE", summary.Type)
	fmt.Fprintf(w, "%-8s %s\n", "SHAPE", summary.ShapeDetail)
	if summary.Kind != "combo" {
		fmt.Fprintf(w, "%-8s %s\n", "STATUS", "not a COMBO — this input has no option list")
		return nil
	}
	fmt.Fprintf(w, "%-8s %d\n", "OPTIONS", summary.OptionCount)
	if summary.OptionCount == 0 {
		fmt.Fprintf(w, "%-8s %s\n", "STATUS", string(store.ModelClassUnregistered))
		fmt.Fprintf(w, "\n%s\n", comfyClassUnregisteredRemedy)
		return nil
	}
	fmt.Fprintln(w)
	for _, option := range summary.Options {
		fmt.Fprintf(w, "  %s\n", option)
	}
	return nil
}

func newNodesShowCmd(flags *rootFlags) *cobra.Command {
	var withOptions bool
	cmd := &cobra.Command{
		Use:   "show <ClassType>",
		Short: "Full input/output schema for one node class",
		Long: `Print the complete schema for one node class: every required, optional, and
hidden input with its type, constraints, and COMBO option count, plus the
declared outputs.

COMBO inputs that offer ZERO options are flagged inline as class-unregistered —
that is a missing extra_model_paths.yaml key, not a missing file. Use
'nodes options <ClassType> <input>' to dump one input's full option list, or
--options to inline every list here.`,
		Example: `  comfyui-pp-cli nodes show KSampler
  comfyui-pp-cli nodes show CheckpointLoaderSimple --options --json`,
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": "0,2,3",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && !flags.dryRun {
				return comfyRequiresInput(cmd, flags)
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "nodes show")
			}
			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			schema, err := comfyFetchClass(ctx, flags, args[0])
			if err != nil {
				return err
			}
			return comfyRenderClass(cmd.OutOrStdout(), flags, schema, withOptions)
		},
	}
	cmd.Flags().BoolVar(&withOptions, "options", false, "Inline every COMBO's full option list instead of a sample")
	return cmd
}

// comfySampleOptions keeps `nodes show` readable: the first n options plus the
// count, since a checkpoint folder can hold hundreds.
const comfyShowOptionSample = 8

func comfyRenderClass(w io.Writer, flags *rootFlags, schema *comfyClassSchema, withOptions bool) error {
	type renderedInput struct {
		comfyInputSummary
		Sample []string `json:"options_sample,omitempty"`
	}
	inputs := make([]renderedInput, 0, 16)
	unregistered := 0
	for _, group := range schema.Groups {
		for _, name := range group.Order {
			summary := comfySummarizeInput(name, group.Requirement, group.Specs[name])
			if summary.Status == string(store.ModelClassUnregistered) {
				unregistered++
			}
			rendered := renderedInput{comfyInputSummary: summary}
			if !withOptions && len(summary.Options) > comfyShowOptionSample {
				rendered.Sample = summary.Options[:comfyShowOptionSample]
				rendered.Options = nil
			} else if !withOptions {
				rendered.Sample = summary.Options
				rendered.Options = nil
			}
			inputs = append(inputs, rendered)
		}
	}

	if !wantsHumanTable(w, flags) {
		payload := map[string]any{
			"class_type":                schema.ClassType,
			"display_name":              schema.DisplayName,
			"category":                  schema.Category,
			"description":               schema.Description,
			"python_module":             schema.PythonModule,
			"output_node":               schema.OutputNode,
			"deprecated":                schema.Deprecated,
			"experimental":              schema.Experimental,
			"inputs":                    inputs,
			"output_types":              schema.OutputTypes,
			"output_names":              schema.OutputNames,
			"unregistered_combo_inputs": unregistered,
		}
		if unregistered > 0 {
			payload["remedy"] = comfyClassUnregisteredRemedy
		}
		return printJSONFiltered(w, payload, flags)
	}

	fmt.Fprintf(w, "%-14s %s\n", "CLASS", schema.ClassType)
	if schema.DisplayName != "" {
		fmt.Fprintf(w, "%-14s %s\n", "DISPLAY", schema.DisplayName)
	}
	if schema.Category != "" {
		fmt.Fprintf(w, "%-14s %s\n", "CATEGORY", schema.Category)
	}
	if schema.PythonModule != "" {
		fmt.Fprintf(w, "%-14s %s\n", "MODULE", schema.PythonModule)
	}
	fmt.Fprintf(w, "%-14s %t\n", "OUTPUT NODE", schema.OutputNode)
	if schema.Deprecated {
		fmt.Fprintf(w, "%-14s %s\n", "STATE", "DEPRECATED")
	}
	if schema.Experimental {
		fmt.Fprintf(w, "%-14s %s\n", "STATE", "EXPERIMENTAL")
	}
	if schema.Description != "" {
		fmt.Fprintf(w, "%-14s %s\n", "DESCRIPTION", schema.Description)
	}

	for _, group := range schema.Groups {
		if len(group.Order) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s INPUTS\n", strings.ToUpper(group.Requirement))
		tw := newTabWriter(w)
		fmt.Fprintln(tw, "  NAME\tTYPE\tSHAPE\tOPTIONS\tDETAIL")
		for _, name := range group.Order {
			summary := comfySummarizeInput(name, group.Requirement, group.Specs[name])
			shape := summary.ComboShape
			if shape == "" {
				shape = "-"
			}
			options := "-"
			if summary.Kind == "combo" {
				options = fmt.Sprintf("%d", summary.OptionCount)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", name, summary.Type, shape, options, comfyInputDetail(summary, withOptions))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if len(schema.OutputTypes) > 0 {
		fmt.Fprintf(w, "\nOUTPUTS\n")
		for i, outputType := range schema.OutputTypes {
			name := ""
			if i < len(schema.OutputNames) {
				name = schema.OutputNames[i]
			}
			if name != "" && name != outputType {
				fmt.Fprintf(w, "  %d  %s (%s)\n", i, outputType, name)
			} else {
				fmt.Fprintf(w, "  %d  %s\n", i, outputType)
			}
		}
	}
	if unregistered > 0 {
		fmt.Fprintf(w, "\n%d COMBO input(s) offer ZERO options: %s\n", unregistered, comfyClassUnregisteredRemedy)
	}
	return nil
}

// comfyInputDetail renders the right-hand detail column: the unregistered
// warning wins over everything else, then a sample of options, then the
// primitive constraints.
func comfyInputDetail(summary comfyInputSummary, withOptions bool) string {
	if summary.Status == string(store.ModelClassUnregistered) {
		return "EMPTY — model class unregistered (missing extra_model_paths.yaml key), NOT a missing file"
	}
	if summary.Kind == "combo" {
		sample := summary.Options
		if !withOptions && len(sample) > comfyShowOptionSample {
			return strings.Join(sample[:comfyShowOptionSample], ", ") + fmt.Sprintf(", ... (+%d more; run 'nodes options')", len(sample)-comfyShowOptionSample)
		}
		return strings.Join(sample, ", ")
	}
	var parts []string
	if summary.Default != nil {
		parts = append(parts, fmt.Sprintf("default=%v", summary.Default))
	}
	if summary.Min != nil {
		parts = append(parts, fmt.Sprintf("min=%v", summary.Min))
	}
	if summary.Max != nil {
		parts = append(parts, fmt.Sprintf("max=%v", summary.Max))
	}
	if summary.Step != nil {
		parts = append(parts, fmt.Sprintf("step=%v", summary.Step))
	}
	if summary.Multiline {
		parts = append(parts, "multiline")
	}
	if summary.Tooltip != "" {
		parts = append(parts, truncate(summary.Tooltip, 80))
	}
	return strings.Join(parts, " ")
}

func newNodesSearchCmd(flags *rootFlags) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Token-AND search over class name, display name, category, and description",
		Long: `Find node classes by capability. Every whitespace-separated token must match
(AND), case-insensitively, somewhere in the class name, display name, category,
or description.

This is how you get from "I need to load a LoRA" to the real class name before
calling 'nodes show' or building a graph.`,
		Example: `  comfyui-pp-cli nodes search lora
  comfyui-pp-cli nodes search load checkpoint
  comfyui-pp-cli nodes search upscale --limit 0 --json`,
		Annotations: map[string]string{
			"mcp:read-only":       "true",
			"pp:typed-exit-codes": "0,2,3",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && !flags.dryRun {
				return comfyRequiresInput(cmd, flags)
			}
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "nodes search")
			}
			tokens := comfySearchTokens(args)
			ctx, cancel := boundCtx(cmd.Context(), flags)
			defer cancel()
			classes, err := comfyFetchObjectInfo(ctx, flags)
			if err != nil {
				return err
			}
			matches := comfySearchNodes(classes, tokens)
			total := len(matches)
			if limit > 0 && len(matches) > limit {
				matches = matches[:limit]
			}
			w := cmd.OutOrStdout()
			if !wantsHumanTable(w, flags) {
				return printJSONFiltered(w, map[string]any{
					"query":            strings.Join(tokens, " "),
					"count":            len(matches),
					"total_matches":    total,
					"classes_searched": len(classes),
					"matches":          matches,
				}, flags)
			}
			if total == 0 {
				fmt.Fprintf(w, "no node class matches %q (searched %d classes)\n", strings.Join(tokens, " "), len(classes))
				return nil
			}
			tw := newTabWriter(w)
			fmt.Fprintln(tw, "CLASS\tDISPLAY\tCATEGORY\tDESCRIPTION")
			for _, match := range matches {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", match.ClassType, match.DisplayName, match.Category, truncate(strings.ReplaceAll(match.Description, "\n", " "), 72))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if total > len(matches) {
				fmt.Fprintf(w, "\nshowing %d of %d matches (--limit 0 for all)\n", len(matches), total)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum matches to print (0 for all)")
	return cmd
}
