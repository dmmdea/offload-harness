// Package slots turns a ComfyUI API-format graph into stable, addressable slots,
// applies typed overrides to them, and preflights the result against a cached
// /object_info schema.
//
// NOT generated — hand-written and preserved across regeneration.
//
// WHY THIS EXISTS. Patching a ComfyUI graph by hand (or with a bespoke per-model
// Python script) is where template revisions go to die. Three defects recur:
//
//  1. A slot is addressed by node id alone. A template revision renumbers or
//     re-purposes that node, the value still type-checks, and the patch lands on
//     the WRONG node — silently. Every address here may therefore carry an
//     expected class (`<node_id>@<ClassType>.<input>`); a class that no longer
//     matches is a REFUSAL, not a warning. Type-checking the value (what
//     comfy-cli does) cannot catch this, because the value is perfectly valid
//     for the node it just corrupted.
//
//  2. COMBO options live at index 1 for v3 specs and index 0 for legacy specs,
//     and ComfyUI ships BOTH simultaneously. Every options read goes through
//     store.ParseComboOptions; an EMPTY option list means the model CLASS is
//     unregistered (a missing extra_model_paths.yaml key), NOT a missing file,
//     and is reported as such.
//
//  3. There is no validate-only endpoint on the server: POST /prompt validates
//     and immediately queues. Client-side validation is the only dry run that
//     exists, so ValidateGraph runs entirely offline against a cached schema.
//
// Everything in this package is a pure function over values. No HTTP, no SQLite,
// no globals — the CLI layer owns I/O.
package slots

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"comfyui-pp-cli/internal/store"
)

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// ErrUIFormat is returned when the file is a ComfyUI *UI* workflow export
// (nodes[]/links[]) rather than an API-format graph. The two are not
// interchangeable and the failure is otherwise reported as "no class_type",
// which sends people hunting the wrong problem.
var ErrUIFormat = errors.New("this is a ComfyUI UI workflow export (nodes[]/links[]), not an API-format graph; re-export with Workflow > Export (API)")

// NodeNotFoundError reports an address naming a node id absent from the graph.
type NodeNotFoundError struct {
	Address string
	NodeID  string
}

func (e *NodeNotFoundError) Error() string {
	return fmt.Sprintf("%s: no node %q in this graph", e.Address, e.NodeID)
}

// ClassMismatchError is THE guard. The address asserted a class; the graph's
// node carries a different one, so the node was renamed, renumbered, or
// re-purposed by a template revision and the patch would land on the wrong node.
type ClassMismatchError struct {
	Address  string
	NodeID   string
	Expected string
	Actual   string
}

func (e *ClassMismatchError) Error() string {
	return fmt.Sprintf("%s: class assertion failed for node %s: address expects %q, graph has %q — the node was renamed or re-purposed; re-derive the address with 'slots' before patching",
		e.Address, e.NodeID, e.Expected, e.Actual)
}

// UnknownInputError reports an address naming an input the node does not carry.
// Refused by default: a mistyped input name is a silent no-op on the wire, and
// ComfyUI passes unrecognised inputs straight through to the node function.
type UnknownInputError struct {
	Address   string
	NodeID    string
	ClassType string
	Input     string
	Known     []string
}

func (e *UnknownInputError) Error() string {
	return fmt.Sprintf("%s: node %s (%s) has no input %q; it has: %s (pass --allow-new-input to add it anyway)",
		e.Address, e.NodeID, e.ClassType, e.Input, strings.Join(e.Known, ", "))
}

// LinkOverwriteError reports an attempt to replace a wired link ([node, slot])
// with a literal value. That severs graph wiring and the server rejects the
// result as a type error at execution time, long after the patch looked fine.
type LinkOverwriteError struct {
	Address string
	NodeID  string
	Input   string
	Link    string
}

func (e *LinkOverwriteError) Error() string {
	return fmt.Sprintf("%s: input %q on node %s is wired to %s, not a literal; overwriting it would sever the graph (pass --allow-relink to override)",
		e.Address, e.Input, e.NodeID, e.Link)
}

// HostPathError reports an absolute host path assigned to an image-ish input.
// LoadImage takes a BARE FILENAME inside ComfyUI's own input directory (a
// relative subfolder is fine); an absolute host path never resolves.
type HostPathError struct {
	Address string
	Input   string
	Value   string
}

func (e *HostPathError) Error() string {
	return fmt.Sprintf("%s: %q is an absolute host path; ComfyUI resolves %q against its own input directory and takes a bare filename (or a relative subfolder) (pass --allow-host-path to override)",
		e.Address, e.Value, e.Input)
}

// ---------------------------------------------------------------------------
// Graph loading
// ---------------------------------------------------------------------------

// decodeJSON unmarshals with UseNumber so a 64-bit seed survives the round trip.
// Decoding into interface{} the ordinary way turns every number into a float64,
// which silently mangles seeds above 2^53 — the values most likely to appear in
// a noise_seed slot.
func decodeJSON(raw []byte, v interface{}) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected trailing content after the JSON value")
	}
	return nil
}

// ParseGraph decodes an API-format graph, rejecting the UI export shape with a
// message that names the actual fix.
func ParseGraph(raw []byte) (store.APIGraph, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("graph is not a JSON object: %w", err)
	}
	if _, hasNodes := probe["nodes"]; hasNodes {
		_, hasLinks := probe["links"]
		_, hasLast := probe["last_node_id"]
		if hasLinks || hasLast {
			return nil, ErrUIFormat
		}
	}
	var g store.APIGraph
	if err := decodeJSON(raw, &g); err != nil {
		return nil, fmt.Errorf("decoding API graph: %w", err)
	}
	if len(g) == 0 {
		return nil, errors.New("graph contains no nodes")
	}
	for _, id := range SortedNodeIDs(g) {
		if strings.TrimSpace(g[id].ClassType) == "" {
			return nil, fmt.Errorf("node %q has no class_type; this is not an API-format graph (re-export with Workflow > Export (API))", id)
		}
	}
	return g, nil
}

// SortedNodeIDs orders node ids numerically where possible so every listing,
// patch report, and finding set is stable across runs (Go map order is not).
func SortedNodeIDs(g store.APIGraph) []string {
	ids := make([]string, 0, len(g))
	for id := range g {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return compareNodeIDs(ids[i], ids[j]) < 0 })
	return ids
}

func compareNodeIDs(a, b string) int {
	an, aerr := strconv.ParseInt(a, 10, 64)
	bn, berr := strconv.ParseInt(b, 10, 64)
	switch {
	case aerr == nil && berr == nil:
		if an != bn {
			if an < bn {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	case aerr == nil:
		return -1
	case berr == nil:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// LinkTarget reports whether v is a wired link — ComfyUI encodes those as
// [<upstream node id>, <output slot>] — and returns the upstream node id.
func LinkTarget(v interface{}) (nodeID string, slot int, ok bool) {
	arr, isArr := v.([]interface{})
	if !isArr || len(arr) != 2 {
		return "", 0, false
	}
	switch id := arr[0].(type) {
	case string:
		nodeID = id
	case json.Number:
		nodeID = id.String()
	case float64:
		nodeID = strconv.FormatFloat(id, 'f', -1, 64)
	default:
		return "", 0, false
	}
	switch s := arr[1].(type) {
	case json.Number:
		n, err := s.Int64()
		if err != nil {
			return "", 0, false
		}
		slot = int(n)
	case float64:
		slot = int(s)
	default:
		return "", 0, false
	}
	return nodeID, slot, true
}

// IsLink reports whether v is wired rather than a literal widget value.
func IsLink(v interface{}) bool {
	_, _, ok := LinkTarget(v)
	return ok
}

// ---------------------------------------------------------------------------
// Slots
// ---------------------------------------------------------------------------

// Role is a semantic tag for the inputs an operator actually reaches for.
type Role string

const (
	RolePositivePrompt Role = "positive_prompt"
	RoleNegativePrompt Role = "negative_prompt"
	RoleSeed           Role = "seed"
	RoleSteps          Role = "steps"
	RoleCFG            Role = "cfg"
	RoleSampler        Role = "sampler"
	RoleScheduler      Role = "scheduler"
	RoleDenoise        Role = "denoise"
	RoleWidth          Role = "width"
	RoleHeight         Role = "height"
	RoleBatch          Role = "batch"
	RoleCheckpoint     Role = "checkpoint"
	RoleUNet           Role = "unet"
	RoleLoRA           Role = "lora"
	RoleVAE            Role = "vae"
	RoleInputImage     Role = "input_image"
)

// Slot is one addressable input of a graph.
type Slot struct {
	Address   string      `json:"address"`
	NodeID    string      `json:"node_id"`
	ClassType string      `json:"class_type"`
	Input     string      `json:"input"`
	Type      string      `json:"type"`
	Value     interface{} `json:"value"`
	Role      Role        `json:"role,omitempty"`
	Title     string      `json:"title,omitempty"`
	Link      bool        `json:"link"`
	// TypedAddress carries the class assertion — the form to paste into `set`
	// so a later template revision cannot silently redirect the patch.
	TypedAddress string `json:"typed_address"`
}

// roleByInputName maps widget names that carry a role regardless of class.
var roleByInputName = map[string]Role{
	"seed":         RoleSeed,
	"noise_seed":   RoleSeed,
	"steps":        RoleSteps,
	"cfg":          RoleCFG,
	"guidance":     RoleCFG,
	"sampler_name": RoleSampler,
	"scheduler":    RoleScheduler,
	"denoise":      RoleDenoise,
	"width":        RoleWidth,
	"height":       RoleHeight,
	"batch_size":   RoleBatch,
	"ckpt_name":    RoleCheckpoint,
	"unet_name":    RoleUNet,
	"vae_name":     RoleVAE,
}

// textInputNames are the widgets that carry prompt text. Polarity comes from
// the graph wiring, not from these names.
var textInputNames = map[string]bool{
	"text":     true,
	"prompt":   true,
	"string":   true,
	"text_g":   true,
	"text_l":   true,
	"positive": true,
	"negative": true,
}

// ExtractSlots returns every input of every node as a stable address, tagged
// with a semantic role where one is recognisable. Ordering is deterministic.
func ExtractSlots(g store.APIGraph) []Slot {
	polarity := promptPolarity(g)
	out := make([]Slot, 0, len(g)*4)
	for _, id := range SortedNodeIDs(g) {
		node := g[id]
		names := make([]string, 0, len(node.Inputs))
		for name := range node.Inputs {
			names = append(names, name)
		}
		sort.Strings(names)
		title := metaTitle(node)
		for _, name := range names {
			value := node.Inputs[name]
			slot := Slot{
				Address:      id + "." + name,
				TypedAddress: id + "@" + node.ClassType + "." + name,
				NodeID:       id,
				ClassType:    node.ClassType,
				Input:        name,
				Type:         ValueType(value),
				Value:        value,
				Title:        title,
				Link:         IsLink(value),
			}
			if !slot.Link {
				slot.Role = roleFor(node, id, name, value, polarity)
			}
			out = append(out, slot)
		}
	}
	return out
}

func metaTitle(node store.APINode) string {
	if node.Meta == nil {
		return ""
	}
	if t, ok := node.Meta["title"].(string); ok {
		return t
	}
	return ""
}

func roleFor(node store.APINode, nodeID, name string, value interface{}, polarity map[string]Role) Role {
	lower := strings.ToLower(name)
	if r, ok := roleByInputName[lower]; ok {
		return r
	}
	if lower == "lora" || strings.HasPrefix(lower, "lora_name") {
		return RoleLoRA
	}
	if _, isString := value.(string); isString {
		// A wired "image" input is plumbing; a string one is the staged file.
		if lower == "image" || lower == "video" || lower == "audio" {
			return RoleInputImage
		}
		if textInputNames[lower] {
			// An input literally named positive/negative that holds text is
			// self-describing; otherwise polarity comes from the wiring.
			switch lower {
			case "positive":
				return RolePositivePrompt
			case "negative":
				return RoleNegativePrompt
			}
			if r, ok := polarity[nodeID]; ok {
				return r
			}
			return titlePolarity(node)
		}
	}
	return ""
}

func titlePolarity(node store.APINode) Role {
	title := strings.ToLower(metaTitle(node))
	switch {
	case title == "":
		return ""
	case strings.Contains(title, "negative"):
		return RoleNegativePrompt
	case strings.Contains(title, "positive"):
		return RolePositivePrompt
	}
	return ""
}

// promptPolarity walks BACKWARDS from every sampler's positive/negative link to
// find which text nodes feed which side. Reading the title is a guess; reading
// the wiring is the answer, and it survives the ConditioningCombine /
// ControlNet chains that sit between the encoder and the sampler.
func promptPolarity(g store.APIGraph) map[string]Role {
	type seed struct {
		id   string
		role Role
	}
	var seeds []seed
	for _, id := range SortedNodeIDs(g) {
		node := g[id]
		for _, name := range []string{"positive", "negative"} {
			v, ok := node.Inputs[name]
			if !ok {
				continue
			}
			target, _, isLink := LinkTarget(v)
			if !isLink {
				continue
			}
			role := RolePositivePrompt
			if name == "negative" {
				role = RoleNegativePrompt
			}
			seeds = append(seeds, seed{id: target, role: role})
		}
	}
	sort.SliceStable(seeds, func(i, j int) bool { return compareNodeIDs(seeds[i].id, seeds[j].id) < 0 })

	assigned := map[string]Role{}
	conflict := map[string]bool{}
	var walk func(id string, role Role, depth int)
	walk = func(id string, role Role, depth int) {
		if depth > 8 {
			return
		}
		if prev, seen := assigned[id]; seen {
			if prev != role {
				conflict[id] = true
			}
			return
		}
		assigned[id] = role
		node, ok := g[id]
		if !ok {
			return
		}
		upstream := make([]string, 0, len(node.Inputs))
		for _, v := range node.Inputs {
			if target, _, isLink := LinkTarget(v); isLink {
				upstream = append(upstream, target)
			}
		}
		sort.Slice(upstream, func(i, j int) bool { return compareNodeIDs(upstream[i], upstream[j]) < 0 })
		for _, target := range upstream {
			walk(target, role, depth+1)
		}
	}
	for _, s := range seeds {
		walk(s.id, s.role, 0)
	}
	// A node reachable from both sides carries no honest polarity; report none
	// rather than a coin flip.
	for id := range conflict {
		delete(assigned, id)
	}
	return assigned
}

// ValueType names the JSON shape of a slot value for display and filtering.
func ValueType(v interface{}) string {
	if IsLink(v) {
		return "link"
	}
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		// Integer-ness is decided by the LITERAL, not by Int64(), because a
		// 64-bit noise_seed (up to 2^64-1) overflows int64 and would otherwise
		// be mislabelled a float.
		if !strings.ContainsAny(t.String(), ".eE") {
			return "int"
		}
		return "number"
	case float64:
		return "number"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// ValueString renders a value for human-facing output.
func ValueString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Addresses and values
// ---------------------------------------------------------------------------

// Address is a parsed slot address: `<node_id>.<input>`, or the guarded form
// `<node_id>@<ClassType>.<input>` which asserts what the node must still be.
type Address struct {
	Raw           string `json:"raw"`
	NodeID        string `json:"node_id"`
	ExpectedClass string `json:"expected_class,omitempty"`
	Input         string `json:"input"`
}

// String renders the address back to its canonical text form.
func (a Address) String() string {
	if a.ExpectedClass != "" {
		return a.NodeID + "@" + a.ExpectedClass + "." + a.Input
	}
	return a.NodeID + "." + a.Input
}

// ParseAddress parses `<node_id>[@<ClassType>].<input>`.
//
// Split on the LAST dot, not the first: input names never contain a dot, but
// custom-node class types do (e.g. "was.Image Blend"), so a first-dot split
// would truncate the assertion into a different class and defeat the guard.
func ParseAddress(s string) (Address, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Address{}, errors.New("empty address: expected <node_id>[@<ClassType>].<input>")
	}
	dot := strings.LastIndex(raw, ".")
	if dot < 0 {
		return Address{}, fmt.Errorf("invalid address %q: expected <node_id>[@<ClassType>].<input>", raw)
	}
	left, input := raw[:dot], raw[dot+1:]
	if strings.TrimSpace(input) == "" {
		return Address{}, fmt.Errorf("invalid address %q: no input name after the dot", raw)
	}
	addr := Address{Raw: raw, Input: input}
	if at := strings.Index(left, "@"); at >= 0 {
		addr.NodeID = strings.TrimSpace(left[:at])
		addr.ExpectedClass = strings.TrimSpace(left[at+1:])
		if addr.ExpectedClass == "" {
			return Address{}, fmt.Errorf("invalid address %q: '@' with no class type; use <node_id>@<ClassType>.<input> or drop the '@'", raw)
		}
	} else {
		addr.NodeID = strings.TrimSpace(left)
	}
	if addr.NodeID == "" {
		return Address{}, fmt.Errorf("invalid address %q: no node id before the dot", raw)
	}
	return addr, nil
}

// Assignment is one parsed `<address>=<value>` override.
type Assignment struct {
	Raw      string      `json:"raw"`
	Address  Address     `json:"address"`
	RawValue string      `json:"raw_value"`
	Value    interface{} `json:"value"`
	// Literal is true when RawValue was not valid JSON and was therefore taken
	// as a bare string. Surfaced so `set` can show WHY a value was quoted.
	Literal bool `json:"literal"`
}

// ParseAssignment parses `<address>=<value>`. The split is on the FIRST '=' so
// values may contain '=' (base64, query strings, prompt text) unescaped.
func ParseAssignment(s string) (Assignment, error) {
	eq := strings.Index(s, "=")
	if eq < 0 {
		return Assignment{}, fmt.Errorf("invalid override %q: expected <node_id>[@<ClassType>].<input>=<value>", s)
	}
	addr, err := ParseAddress(s[:eq])
	if err != nil {
		return Assignment{}, err
	}
	rawValue := s[eq+1:]
	value, literal := ParseValue(rawValue)
	return Assignment{Raw: s, Address: addr, RawValue: rawValue, Value: value, Literal: literal}, nil
}

// ParseValue coerces a command-line value: JSON first, bare string otherwise.
//
// JSON-first is what makes `steps=30`, `denoise=0.55`, `enabled=true`, and
// `lora=["a.safetensors",0.8]` land as the types ComfyUI expects instead of as
// strings the server rejects at validation time. Anything that is not valid
// JSON — prompt text, a filename, a Windows path — falls back to a literal
// string, which is why `text=a cat` needs no quoting. Numbers decode as
// json.Number so a 64-bit seed re-serialises byte-for-byte.
func ParseValue(raw string) (value interface{}, literal bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		// Deliberate: `5.text=` clears a prompt rather than erroring.
		return "", true
	}
	if !json.Valid([]byte(trimmed)) {
		return raw, true
	}
	var parsed interface{}
	if err := decodeJSON([]byte(trimmed), &parsed); err != nil {
		return raw, true
	}
	return parsed, false
}

// ---------------------------------------------------------------------------
// Resolving and applying a patch set
// ---------------------------------------------------------------------------

// Change is one resolved override: what it touches, and old -> new.
type Change struct {
	Address       string      `json:"address"`
	TypedAddress  string      `json:"typed_address"`
	NodeID        string      `json:"node_id"`
	ClassType     string      `json:"class_type"`
	ExpectedClass string      `json:"expected_class,omitempty"`
	Input         string      `json:"input"`
	OldValue      interface{} `json:"old_value"`
	NewValue      interface{} `json:"new_value"`
	OldExists     bool        `json:"old_exists"`
	Literal       bool        `json:"literal_value"`
	NoOp          bool        `json:"noop"`
	Role          Role        `json:"role,omitempty"`
}

// ResolveOptions are the deliberate escape hatches. Each default is a refusal
// because each corresponds to a way a patch silently corrupts a graph.
type ResolveOptions struct {
	AllowNewInput bool
	AllowRelink   bool
	AllowHostPath bool
}

// Resolve turns assignments into changes against g, enforcing every guard.
// Errors are COLLECTED (joined), not returned at the first failure, so a
// multi-slot patch reports all its problems in one pass instead of one per
// re-run. Class mismatches remain individually detectable with errors.As.
func Resolve(g store.APIGraph, assignments []Assignment, opts ResolveOptions) ([]Change, error) {
	polarity := promptPolarity(g)
	changes := make([]Change, 0, len(assignments))
	var problems []error

	for _, a := range assignments {
		node, ok := g[a.Address.NodeID]
		if !ok {
			problems = append(problems, &NodeNotFoundError{Address: a.Address.Raw, NodeID: a.Address.NodeID})
			continue
		}
		// The guard, before anything else touches the node.
		if a.Address.ExpectedClass != "" && a.Address.ExpectedClass != node.ClassType {
			problems = append(problems, &ClassMismatchError{
				Address:  a.Address.Raw,
				NodeID:   a.Address.NodeID,
				Expected: a.Address.ExpectedClass,
				Actual:   node.ClassType,
			})
			continue
		}
		old, exists := node.Inputs[a.Address.Input]
		if !exists && !opts.AllowNewInput {
			known := make([]string, 0, len(node.Inputs))
			for name := range node.Inputs {
				known = append(known, name)
			}
			sort.Strings(known)
			problems = append(problems, &UnknownInputError{
				Address:   a.Address.Raw,
				NodeID:    a.Address.NodeID,
				ClassType: node.ClassType,
				Input:     a.Address.Input,
				Known:     known,
			})
			continue
		}
		if exists && IsLink(old) && !IsLink(a.Value) && !opts.AllowRelink {
			target, slot, _ := LinkTarget(old)
			problems = append(problems, &LinkOverwriteError{
				Address: a.Address.Raw,
				NodeID:  a.Address.NodeID,
				Input:   a.Address.Input,
				Link:    fmt.Sprintf("node %s output %d", target, slot),
			})
			continue
		}
		if !opts.AllowHostPath {
			if err := CheckInputFilename(a.Address.Raw, a.Address.Input, a.Value); err != nil {
				problems = append(problems, err)
				continue
			}
		}
		changes = append(changes, Change{
			Address:       a.Address.NodeID + "." + a.Address.Input,
			TypedAddress:  a.Address.NodeID + "@" + node.ClassType + "." + a.Address.Input,
			NodeID:        a.Address.NodeID,
			ClassType:     node.ClassType,
			ExpectedClass: a.Address.ExpectedClass,
			Input:         a.Address.Input,
			OldValue:      old,
			NewValue:      a.Value,
			OldExists:     exists,
			Literal:       a.Literal,
			NoOp:          exists && sameJSON(old, a.Value),
			Role:          roleFor(node, a.Address.NodeID, a.Address.Input, a.Value, polarity),
		})
	}
	if len(problems) > 0 {
		return changes, errors.Join(problems...)
	}
	return changes, nil
}

// imageInputNames are the inputs ComfyUI resolves against its OWN input
// directory. An absolute host path in one of these is the single most common
// hand-patching mistake and produces an opaque server-side failure.
var imageInputNames = map[string]bool{
	"image": true, "video": true, "audio": true, "mask": true, "lora_path": true,
}

// CheckInputFilename enforces hard rule 5: LoadImage and friends take a bare
// filename (a relative subfolder is allowed) inside ComfyUI's input dir, never
// an absolute host path.
func CheckInputFilename(address, input string, value interface{}) error {
	if !imageInputNames[strings.ToLower(input)] {
		return nil
	}
	s, ok := value.(string)
	if !ok || s == "" {
		return nil
	}
	if isAbsoluteHostPath(s) {
		return &HostPathError{Address: address, Input: input, Value: s}
	}
	return nil
}

func isAbsoluteHostPath(s string) bool {
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, `\\`) {
		return true
	}
	// Drive-letter form: C:\... or C:/...
	if len(s) >= 3 && s[1] == ':' && (s[2] == '\\' || s[2] == '/') {
		c := s[0]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			return true
		}
	}
	return false
}

func sameJSON(a, b interface{}) bool {
	ab, aerr := json.Marshal(a)
	bb, berr := json.Marshal(b)
	if aerr != nil || berr != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

// ApplyChanges rewrites the ORIGINAL bytes rather than re-marshalling a typed
// graph, so any node field this package does not model survives the patch
// untouched. Returns new bytes; the input slice is never mutated.
func ApplyChanges(raw []byte, changes []Change) ([]byte, error) {
	var doc map[string]interface{}
	if err := decodeJSON(raw, &doc); err != nil {
		return nil, fmt.Errorf("decoding graph for patch: %w", err)
	}
	for _, ch := range changes {
		nodeAny, ok := doc[ch.NodeID]
		if !ok {
			return nil, &NodeNotFoundError{Address: ch.Address, NodeID: ch.NodeID}
		}
		node, ok := nodeAny.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("node %q is not a JSON object", ch.NodeID)
		}
		inputs, ok := node["inputs"].(map[string]interface{})
		if !ok {
			inputs = map[string]interface{}{}
		}
		inputs[ch.Input] = ch.NewValue
		node["inputs"] = inputs
		doc[ch.NodeID] = node
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("re-encoding patched graph: %w", err)
	}
	return append(out, '\n'), nil
}

// ---------------------------------------------------------------------------
// Offline validation against a cached /object_info schema
// ---------------------------------------------------------------------------

// InputSpec is one input declared by /object_info for a class.
type InputSpec struct {
	Name     string      `json:"name"`
	Required bool        `json:"required"`
	TypeName string      `json:"type"`
	Raw      interface{} `json:"-"`
}

// ClassSpec is one node class as declared by /object_info.
type ClassSpec struct {
	ClassType   string               `json:"class_type"`
	DisplayName string               `json:"display_name,omitempty"`
	Category    string               `json:"category,omitempty"`
	Inputs      map[string]InputSpec `json:"inputs"`
}

// Schema is the cached node schema: class type -> spec.
type Schema map[string]ClassSpec

// ParseObjectInfo parses a raw /object_info payload (the whole map, or a single
// class entry keyed by the caller) into a Schema.
func ParseObjectInfo(raw []byte) (Schema, error) {
	var top map[string]interface{}
	if err := decodeJSON(raw, &top); err != nil {
		return nil, fmt.Errorf("decoding object_info: %w", err)
	}
	schema := Schema{}
	for classType, entry := range top {
		spec, ok := ParseClassEntry(classType, entry)
		if !ok {
			continue
		}
		schema[classType] = spec
	}
	if len(schema) == 0 {
		return nil, errors.New("object_info payload declared no node classes")
	}
	return schema, nil
}

// LooksLikeClassEntry reports whether v has the shape of one /object_info class
// entry. Used to tell "one row holding the whole map" from "one row per class"
// when reading whatever the local cache happens to hold.
func LooksLikeClassEntry(v interface{}) bool {
	obj, ok := v.(map[string]interface{})
	if !ok {
		return false
	}
	for _, key := range []string{"input", "input_order", "output", "display_name", "output_name"} {
		if _, ok := obj[key]; ok {
			return true
		}
	}
	return false
}

// ParseClassEntry parses one /object_info class entry.
func ParseClassEntry(classType string, entry interface{}) (ClassSpec, bool) {
	obj, ok := entry.(map[string]interface{})
	if !ok {
		return ClassSpec{}, false
	}
	spec := ClassSpec{ClassType: classType, Inputs: map[string]InputSpec{}}
	if s, ok := obj["display_name"].(string); ok {
		spec.DisplayName = s
	}
	if s, ok := obj["category"].(string); ok {
		spec.Category = s
	}
	input, _ := obj["input"].(map[string]interface{})
	for _, group := range []struct {
		key      string
		required bool
	}{{"required", true}, {"optional", false}} {
		raw, ok := input[group.key].(map[string]interface{})
		if !ok {
			continue
		}
		for name, tuple := range raw {
			spec.Inputs[name] = InputSpec{
				Name:     name,
				Required: group.required,
				TypeName: TypeNameOf(tuple),
				Raw:      tuple,
			}
		}
	}
	if !LooksLikeClassEntry(entry) && len(spec.Inputs) == 0 {
		return ClassSpec{}, false
	}
	return spec, true
}

// TypeNameOf names an input's declared type. A legacy COMBO puts its option
// list at index 0 (so the "type" IS the list); v3 puts the literal string
// "COMBO" there with the options at index 1.
func TypeNameOf(spec interface{}) string {
	arr, ok := spec.([]interface{})
	if !ok || len(arr) == 0 {
		return ""
	}
	if s, ok := arr[0].(string); ok {
		return s
	}
	if _, ok := arr[0].([]interface{}); ok {
		return "COMBO"
	}
	return ""
}

// Finding severities.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// Finding kinds. Stable strings — agents branch on these.
const (
	KindUnknownClass         = "unknown-class"
	KindMissingRequiredInput = "missing-required-input"
	KindComboNotInOptions    = "combo-value-not-in-options"
	KindClassUnregistered    = "model-class-unregistered"
	KindUnknownInput         = "unknown-input"
	KindDanglingLink         = "dangling-link"
	KindHostPath             = "host-path-input"
)

// Finding is one validation result.
type Finding struct {
	Severity  string      `json:"severity"`
	Kind      string      `json:"kind"`
	NodeID    string      `json:"node_id"`
	ClassType string      `json:"class_type"`
	Input     string      `json:"input,omitempty"`
	Address   string      `json:"address,omitempty"`
	Value     interface{} `json:"value,omitempty"`
	Options   []string    `json:"options,omitempty"`
	Message   string      `json:"message"`
}

// ValidateGraph preflights a graph offline. There is no validate-only endpoint
// on the server — POST /prompt validates and immediately queues — so this is
// the only dry run that exists.
//
// With an empty schema only the graph-local checks run (dangling links, host
// paths); those need no server knowledge and still catch real breakage.
func ValidateGraph(g store.APIGraph, schema Schema) []Finding {
	var findings []Finding
	for _, id := range SortedNodeIDs(g) {
		node := g[id]
		names := make([]string, 0, len(node.Inputs))
		for name := range node.Inputs {
			names = append(names, name)
		}
		sort.Strings(names)

		// --- graph-local checks (no schema required) ---
		for _, name := range names {
			value := node.Inputs[name]
			if target, _, ok := LinkTarget(value); ok {
				if _, exists := g[target]; !exists {
					findings = append(findings, Finding{
						Severity: SeverityError, Kind: KindDanglingLink,
						NodeID: id, ClassType: node.ClassType, Input: name,
						Address: id + "." + name, Value: value,
						Message: fmt.Sprintf("input %q is wired to node %q, which is not in this graph", name, target),
					})
				}
				continue
			}
			if err := CheckInputFilename(id+"."+name, name, value); err != nil {
				findings = append(findings, Finding{
					Severity: SeverityError, Kind: KindHostPath,
					NodeID: id, ClassType: node.ClassType, Input: name,
					Address: id + "." + name, Value: value,
					Message: err.Error(),
				})
			}
		}

		if len(schema) == 0 {
			continue
		}

		// --- schema checks ---
		spec, known := schema[node.ClassType]
		if !known {
			findings = append(findings, Finding{
				Severity: SeverityError, Kind: KindUnknownClass,
				NodeID: id, ClassType: node.ClassType,
				Message: fmt.Sprintf("class %q is not registered on this server; the custom node pack is missing or failed to import", node.ClassType),
			})
			continue
		}

		missing := make([]string, 0, 2)
		for name, in := range spec.Inputs {
			if !in.Required {
				continue
			}
			if _, present := node.Inputs[name]; present {
				continue
			}
			// A dynamic autogrow group (COMFY_AUTOGROW_V3) is never wired under its own
			// name. ComfyUI serialises it ONLY as dotted child keys — ComfyMathExpression's
			// `values` arrives as `values.a`, `values.b` — so an exact-key check reports a
			// graph the server happily accepts as missing a required input.
			if hasAutogrowChildren(node.Inputs, name) {
				continue
			}
			missing = append(missing, name)
		}
		sort.Strings(missing)
		for _, name := range missing {
			findings = append(findings, Finding{
				Severity: SeverityError, Kind: KindMissingRequiredInput,
				NodeID: id, ClassType: node.ClassType, Input: name,
				Address: id + "." + name,
				Message: fmt.Sprintf("required input %q (%s) is absent", name, spec.Inputs[name].TypeName),
			})
		}

		for _, name := range names {
			value := node.Inputs[name]
			in, declared := spec.Inputs[name]
			if !declared {
				// `values.a` is a child of the declared autogrow group `values`, not an
				// undeclared input. Warning on it would flag every graph that uses one.
				if parent, ok := autogrowParent(name); ok {
					if _, isGroup := spec.Inputs[parent]; isGroup {
						continue
					}
				}
				findings = append(findings, Finding{
					Severity: SeverityWarning, Kind: KindUnknownInput,
					NodeID: id, ClassType: node.ClassType, Input: name,
					Address: id + "." + name, Value: value,
					Message: fmt.Sprintf("class %s declares no input %q; the graph predates a node revision, or the name is a typo", node.ClassType, name),
				})
				continue
			}
			str, isString := value.(string)
			if !isString {
				continue
			}
			visibility, options := store.ClassifyModelVisibility(in.Raw, str)
			switch visibility {
			case store.ModelVisible, store.ModelNoSuchInput:
				// Either a legitimate member, or not a COMBO at all.
			case store.ModelClassUnregistered:
				// An empty COMBO only implies an unregistered MODEL CLASS when the input is
				// folder-backed. Enum dropdowns such as SaveVideo.codec or
				// ResizeImageMaskNode.resize_type are legitimately empty for unrelated
				// reasons, and telling the operator to add an extra_model_paths.yaml key for
				// them is wrong advice. Same narrowing `models why` already applies.
				if !isFolderBackedInput(name) {
					break
				}
				findings = append(findings, Finding{
					Severity: SeverityError, Kind: KindClassUnregistered,
					NodeID: id, ClassType: node.ClassType, Input: name,
					Address: id + "." + name, Value: str,
					Message: fmt.Sprintf("input %q is a COMBO with ZERO options — the model CLASS is unregistered (a missing extra_model_paths.yaml key for this loader), NOT a missing file; adding the file will not fix it", name),
				})
			case store.ModelNotListed:
				findings = append(findings, Finding{
					Severity: SeverityError, Kind: KindComboNotInOptions,
					NodeID: id, ClassType: node.ClassType, Input: name,
					Address: id + "." + name, Value: str, Options: options,
					Message: fmt.Sprintf("%q is not among the %d options this server offers for %q", str, len(options), name),
				})
			}
		}
	}
	return findings
}

// CountFindings splits a finding set by severity.
func CountFindings(findings []Finding) (errorCount, warningCount int) {
	for _, f := range findings {
		if f.Severity == SeverityError {
			errorCount++
		} else {
			warningCount++
		}
	}
	return errorCount, warningCount
}

// autogrowParent splits a dotted autogrow child key ("values.a") into its group name.
func autogrowParent(name string) (string, bool) {
	i := strings.IndexByte(name, '.')
	if i <= 0 || i == len(name)-1 {
		return "", false
	}
	return name[:i], true
}

// hasAutogrowChildren reports whether any wired input is a dotted child of group.
func hasAutogrowChildren(inputs map[string]interface{}, group string) bool {
	prefix := group + "."
	for k := range inputs {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// isFolderBackedInput reports whether an input names a file in a ComfyUI model folder,
// using the `*_name` convention the loaders follow (unet_name, ckpt_name, vae_name...).
// Only these can meaningfully be diagnosed as an unregistered model class.
func isFolderBackedInput(inputName string) bool {
	lowered := strings.ToLower(strings.TrimSpace(inputName))
	return lowered == "name" || strings.HasSuffix(lowered, "_name")
}
