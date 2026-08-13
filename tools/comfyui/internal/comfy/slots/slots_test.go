package slots

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"comfyui-pp-cli/internal/store"
)

// sampleGraph is a minimal but realistic txt2img API graph: a checkpoint
// loader, two CLIPTextEncode nodes wired to a KSampler's positive/negative
// inputs (polarity is therefore derivable from the WIRING, not from titles),
// a latent, a decode, and a save.
const sampleGraph = `{
  "3": {
    "class_type": "KSampler",
    "inputs": {
      "seed": 1125899906842624,
      "steps": 20,
      "cfg": 8.0,
      "sampler_name": "euler",
      "scheduler": "normal",
      "denoise": 1.0,
      "model": ["4", 0],
      "positive": ["6", 0],
      "negative": ["7", 0],
      "latent_image": ["5", 0]
    }
  },
  "4": {
    "class_type": "CheckpointLoaderSimple",
    "inputs": {"ckpt_name": "sd_xl_base_1.0.safetensors"}
  },
  "5": {
    "class_type": "EmptyLatentImage",
    "inputs": {"width": 1024, "height": 1024, "batch_size": 1}
  },
  "6": {
    "class_type": "CLIPTextEncode",
    "inputs": {"text": "a lighthouse", "clip": ["4", 1]},
    "_meta": {"title": "CLIP Text Encode (Prompt)"}
  },
  "7": {
    "class_type": "CLIPTextEncode",
    "inputs": {"text": "blurry", "clip": ["4", 1]}
  },
  "8": {
    "class_type": "VAEDecode",
    "inputs": {"samples": ["3", 0], "vae": ["4", 2]}
  },
  "9": {
    "class_type": "SaveImage",
    "inputs": {"filename_prefix": "ComfyUI", "images": ["8", 0]}
  },
  "10": {
    "class_type": "LoadImage",
    "inputs": {"image": "reference.png", "upload": "image"}
  }
}`

// objectInfoFixture exercises BOTH COMBO shapes ComfyUI ships simultaneously:
// CheckpointLoaderSimple uses the LEGACY shape (options at index 0) and
// KSampler's sampler_name uses the V3 shape (options at index 1). LatentUpscale
// carries an EMPTY option list, which means the model CLASS is unregistered.
const objectInfoFixture = `{
  "KSampler": {
    "display_name": "KSampler",
    "category": "sampling",
    "input": {
      "required": {
        "model": ["MODEL"],
        "seed": ["INT", {"default": 0, "min": 0}],
        "steps": ["INT", {"default": 20}],
        "cfg": ["FLOAT", {"default": 8.0}],
        "sampler_name": ["COMBO", {"options": ["euler", "dpmpp_2m", "ddim"]}],
        "scheduler": ["COMBO", {"options": ["normal", "karras"]}],
        "positive": ["CONDITIONING"],
        "negative": ["CONDITIONING"],
        "latent_image": ["LATENT"],
        "denoise": ["FLOAT", {"default": 1.0}]
      }
    },
    "output": ["LATENT"]
  },
  "CheckpointLoaderSimple": {
    "input": {
      "required": {
        "ckpt_name": [["sd_xl_base_1.0.safetensors", "flux1-dev.safetensors"], {"tooltip": "the checkpoint"}]
      }
    },
    "output": ["MODEL", "CLIP", "VAE"]
  },
  "EmptyLatentImage": {
    "input": {
      "required": {
        "width": ["INT", {"default": 512}],
        "height": ["INT", {"default": 512}],
        "batch_size": ["INT", {"default": 1}]
      }
    },
    "output": ["LATENT"]
  },
  "CLIPTextEncode": {
    "input": {
      "required": {
        "text": ["STRING", {"multiline": true}],
        "clip": ["CLIP"]
      }
    },
    "output": ["CONDITIONING"]
  },
  "VAEDecode": {
    "input": {"required": {"samples": ["LATENT"], "vae": ["VAE"]}},
    "output": ["IMAGE"]
  },
  "SaveImage": {
    "input": {
      "required": {"images": ["IMAGE"]},
      "optional": {"filename_prefix": ["STRING", {"default": "ComfyUI"}]}
    },
    "output": []
  },
  "LoadImage": {
    "input": {
      "required": {
        "image": [["reference.png", "photo.jpg"], {"image_upload": true}]
      },
      "optional": {"upload": ["STRING", {"default": "image"}]}
    },
    "output": ["IMAGE", "MASK"]
  },
  "LatentUpscaleModelLoader": {
    "input": {
      "required": {
        "model_name": [[], {}]
      }
    },
    "output": ["UPSCALE_MODEL"]
  }
}`

func mustGraph(t *testing.T, raw string) store.APIGraph {
	t.Helper()
	g, err := ParseGraph([]byte(raw))
	if err != nil {
		t.Fatalf("ParseGraph: %v", err)
	}
	return g
}

func mustSchema(t *testing.T) Schema {
	t.Helper()
	s, err := ParseObjectInfo([]byte(objectInfoFixture))
	if err != nil {
		t.Fatalf("ParseObjectInfo: %v", err)
	}
	return s
}

// ---------------------------------------------------------------------------
// Address parsing — with and without @Class
// ---------------------------------------------------------------------------

func TestParseAddress(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		input         string
		wantNode      string
		wantClass     string
		wantInput     string
		wantErr       bool
		wantCanonical string
	}{
		{
			name:  "plain address without class assertion",
			input: "3.steps", wantNode: "3", wantClass: "", wantInput: "steps",
			wantCanonical: "3.steps",
		},
		{
			name:  "guarded address with @Class",
			input: "3@KSampler.steps", wantNode: "3", wantClass: "KSampler", wantInput: "steps",
			wantCanonical: "3@KSampler.steps",
		},
		{
			name:     "class type containing a dot splits on the LAST dot",
			input:    "12@was.Image Blend.blend_factor",
			wantNode: "12", wantClass: "was.Image Blend", wantInput: "blend_factor",
			wantCanonical: "12@was.Image Blend.blend_factor",
		},
		{
			name:  "non-numeric node id",
			input: "node_a@PreviewImage.images", wantNode: "node_a", wantClass: "PreviewImage", wantInput: "images",
			wantCanonical: "node_a@PreviewImage.images",
		},
		{
			name:  "surrounding whitespace trimmed",
			input: "  3@KSampler.cfg  ", wantNode: "3", wantClass: "KSampler", wantInput: "cfg",
			wantCanonical: "3@KSampler.cfg",
		},
		{name: "missing dot", input: "3steps", wantErr: true},
		{name: "empty input name", input: "3.", wantErr: true},
		{name: "empty node id", input: ".steps", wantErr: true},
		{name: "at sign with no class", input: "3@.steps", wantErr: true},
		{name: "empty string", input: "", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseAddress(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseAddress(%q) = %+v, want error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAddress(%q) unexpected error: %v", tc.input, err)
			}
			if got.NodeID != tc.wantNode {
				t.Errorf("NodeID = %q, want %q", got.NodeID, tc.wantNode)
			}
			if got.ExpectedClass != tc.wantClass {
				t.Errorf("ExpectedClass = %q, want %q", got.ExpectedClass, tc.wantClass)
			}
			if got.Input != tc.wantInput {
				t.Errorf("Input = %q, want %q", got.Input, tc.wantInput)
			}
			if got.String() != tc.wantCanonical {
				t.Errorf("String() = %q, want %q", got.String(), tc.wantCanonical)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Value coercion — JSON first, literal string fallback
// ---------------------------------------------------------------------------

func TestParseValue(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		raw         string
		wantJSON    string // re-marshalled form of the parsed value
		wantLiteral bool
		wantType    string
	}{
		{name: "integer parses as JSON number", raw: "30", wantJSON: "30", wantType: "int"},
		{name: "float parses as JSON number", raw: "0.55", wantJSON: "0.55", wantType: "number"},
		{
			name: "64-bit seed keeps full precision",
			raw:  "18446744073709551615", wantJSON: "18446744073709551615", wantType: "int",
		},
		{name: "boolean parses as JSON", raw: "true", wantJSON: "true", wantType: "boolean"},
		{name: "null parses as JSON", raw: "null", wantJSON: "null", wantType: "null"},
		{name: "array parses as JSON", raw: `["a.safetensors",0.8]`, wantJSON: `["a.safetensors",0.8]`, wantType: "array"},
		{name: "object parses as JSON", raw: `{"a":1}`, wantJSON: `{"a":1}`, wantType: "object"},
		{name: "quoted string parses as JSON string", raw: `"euler"`, wantJSON: `"euler"`, wantType: "string"},
		{
			name: "bare prose falls back to a literal string",
			raw:  "a lighthouse at dusk", wantJSON: `"a lighthouse at dusk"`, wantLiteral: true, wantType: "string",
		},
		{
			name: "filename falls back to a literal string",
			raw:  "sd_xl_base_1.0.safetensors", wantJSON: `"sd_xl_base_1.0.safetensors"`, wantLiteral: true, wantType: "string",
		},
		{
			name: "windows path falls back to a literal string",
			raw:  `V:\models\x.safetensors`, wantJSON: `"V:\\models\\x.safetensors"`, wantLiteral: true, wantType: "string",
		},
		{
			name: "unterminated JSON falls back to a literal string",
			raw:  `{"a":`, wantJSON: `"{\"a\":"`, wantLiteral: true, wantType: "string",
		},
		{
			name: "trailing garbage after valid JSON falls back to a literal string",
			raw:  `30 steps`, wantJSON: `"30 steps"`, wantLiteral: true, wantType: "string",
		},
		{
			name: "empty value clears the slot with an empty string",
			raw:  "", wantJSON: `""`, wantLiteral: true, wantType: "string",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			value, literal := ParseValue(tc.raw)
			if literal != tc.wantLiteral {
				t.Errorf("ParseValue(%q) literal = %v, want %v", tc.raw, literal, tc.wantLiteral)
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("marshalling parsed value: %v", err)
			}
			if string(encoded) != tc.wantJSON {
				t.Errorf("ParseValue(%q) = %s, want %s", tc.raw, encoded, tc.wantJSON)
			}
			if got := ValueType(value); got != tc.wantType {
				t.Errorf("ValueType = %q, want %q", got, tc.wantType)
			}
		})
	}
}

func TestParseAssignment(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		input     string
		wantNode  string
		wantClass string
		wantInput string
		wantValue string
		wantErr   bool
	}{
		{
			name: "guarded assignment", input: "3@KSampler.steps=30",
			wantNode: "3", wantClass: "KSampler", wantInput: "steps", wantValue: "30",
		},
		{
			name: "unguarded assignment", input: "3.steps=30",
			wantNode: "3", wantInput: "steps", wantValue: "30",
		},
		{
			name:     "value containing = splits on the FIRST equals only",
			input:    "6@CLIPTextEncode.text=a=b=c",
			wantNode: "6", wantClass: "CLIPTextEncode", wantInput: "text", wantValue: `"a=b=c"`,
		},
		{name: "no equals sign", input: "3.steps", wantErr: true},
		{name: "bad address", input: "3steps=30", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseAssignment(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseAssignment(%q) = %+v, want error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAssignment(%q) unexpected error: %v", tc.input, err)
			}
			if got.Address.NodeID != tc.wantNode || got.Address.ExpectedClass != tc.wantClass || got.Address.Input != tc.wantInput {
				t.Errorf("address = %+v, want node=%q class=%q input=%q", got.Address, tc.wantNode, tc.wantClass, tc.wantInput)
			}
			encoded, _ := json.Marshal(got.Value)
			if string(encoded) != tc.wantValue {
				t.Errorf("value = %s, want %s", encoded, tc.wantValue)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Class assertion — the guard
// ---------------------------------------------------------------------------

func TestResolveClassAssertion(t *testing.T) {
	t.Parallel()

	graph := mustGraph(t, sampleGraph)

	cases := []struct {
		name        string
		assignment  string
		opts        ResolveOptions
		wantErr     bool
		wantErrAs   func(error) bool
		wantChanges int
		wantNewJSON string
	}{
		{
			name:        "matching class assertion applies",
			assignment:  "3@KSampler.steps=30",
			wantChanges: 1, wantNewJSON: "30",
		},
		{
			name:        "no assertion applies unguarded",
			assignment:  "3.steps=30",
			wantChanges: 1, wantNewJSON: "30",
		},
		{
			name:       "class mismatch is REFUSED",
			assignment: "3@KSamplerAdvanced.steps=30",
			wantErr:    true,
			wantErrAs: func(err error) bool {
				var e *ClassMismatchError
				return errors.As(err, &e) && e.Expected == "KSamplerAdvanced" && e.Actual == "KSampler"
			},
		},
		{
			name:       "class mismatch on a re-purposed text node is REFUSED",
			assignment: "6@CLIPTextEncodeSDXL.text=hello",
			wantErr:    true,
			wantErrAs: func(err error) bool {
				var e *ClassMismatchError
				return errors.As(err, &e) && e.NodeID == "6" && e.Actual == "CLIPTextEncode"
			},
		},
		{
			name:       "unknown node id is refused",
			assignment: "999@KSampler.steps=30",
			wantErr:    true,
			wantErrAs: func(err error) bool {
				var e *NodeNotFoundError
				return errors.As(err, &e)
			},
		},
		{
			name:       "unknown input name is refused by default",
			assignment: "3@KSampler.stpes=30",
			wantErr:    true,
			wantErrAs: func(err error) bool {
				var e *UnknownInputError
				return errors.As(err, &e) && e.Input == "stpes"
			},
		},
		{
			name:        "unknown input name is allowed with the escape hatch",
			assignment:  "3@KSampler.custom_knob=1",
			opts:        ResolveOptions{AllowNewInput: true},
			wantChanges: 1, wantNewJSON: "1",
		},
		{
			name:       "overwriting a wired link with a literal is refused",
			assignment: "3@KSampler.positive=hello",
			wantErr:    true,
			wantErrAs: func(err error) bool {
				var e *LinkOverwriteError
				return errors.As(err, &e)
			},
		},
		{
			name:        "re-wiring a link to another link is allowed",
			assignment:  `3@KSampler.positive=["7",0]`,
			wantChanges: 1, wantNewJSON: `["7",0]`,
		},
		{
			name:       "absolute host path in an image input is refused",
			assignment: `10@LoadImage.image=C:\Users\me\ref.png`,
			wantErr:    true,
			wantErrAs: func(err error) bool {
				var e *HostPathError
				return errors.As(err, &e)
			},
		},
		{
			name:        "relative subfolder in an image input is allowed",
			assignment:  "10@LoadImage.image=refs/reference.png",
			wantChanges: 1, wantNewJSON: `"refs/reference.png"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, err := ParseAssignment(tc.assignment)
			if err != nil {
				t.Fatalf("ParseAssignment(%q): %v", tc.assignment, err)
			}
			changes, err := Resolve(graph, []Assignment{a}, tc.opts)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Resolve(%q) = %+v, want error", tc.assignment, changes)
				}
				if tc.wantErrAs != nil && !tc.wantErrAs(err) {
					t.Fatalf("Resolve(%q) returned the wrong error type/detail: %v", tc.assignment, err)
				}
				if len(changes) != 0 {
					t.Fatalf("Resolve(%q) returned %d change(s) alongside the refusal; a refused address must produce none", tc.assignment, len(changes))
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q) unexpected error: %v", tc.assignment, err)
			}
			if len(changes) != tc.wantChanges {
				t.Fatalf("Resolve(%q) = %d change(s), want %d", tc.assignment, len(changes), tc.wantChanges)
			}
			encoded, _ := json.Marshal(changes[0].NewValue)
			if string(encoded) != tc.wantNewJSON {
				t.Errorf("new value = %s, want %s", encoded, tc.wantNewJSON)
			}
		})
	}
}

func TestResolveCollectsEveryProblem(t *testing.T) {
	t.Parallel()

	graph := mustGraph(t, sampleGraph)
	specs := []string{
		"3@KSamplerAdvanced.steps=30", // class mismatch
		"999.steps=1",                 // missing node
		"3@KSampler.cfg=7.5",          // fine
	}
	assignments := make([]Assignment, 0, len(specs))
	for _, spec := range specs {
		a, err := ParseAssignment(spec)
		if err != nil {
			t.Fatalf("ParseAssignment(%q): %v", spec, err)
		}
		assignments = append(assignments, a)
	}
	changes, err := Resolve(graph, assignments, ResolveOptions{})
	if err == nil {
		t.Fatal("Resolve: want an error covering both bad addresses")
	}
	var mismatch *ClassMismatchError
	if !errors.As(err, &mismatch) {
		t.Errorf("joined error does not surface the class mismatch: %v", err)
	}
	var missing *NodeNotFoundError
	if !errors.As(err, &missing) {
		t.Errorf("joined error does not surface the missing node: %v", err)
	}
	if len(changes) != 1 || changes[0].Input != "cfg" {
		t.Errorf("valid change should still be resolved for reporting, got %+v", changes)
	}
}

func TestResolveMarksNoOpAndCarriesTypedAddress(t *testing.T) {
	t.Parallel()

	graph := mustGraph(t, sampleGraph)
	a, err := ParseAssignment("3.steps=20") // already 20 in the fixture
	if err != nil {
		t.Fatalf("ParseAssignment: %v", err)
	}
	changes, err := Resolve(graph, []Assignment{a}, ResolveOptions{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1", len(changes))
	}
	if !changes[0].NoOp {
		t.Error("setting a slot to its current value should be marked NoOp")
	}
	if changes[0].TypedAddress != "3@KSampler.steps" {
		t.Errorf("TypedAddress = %q, want %q", changes[0].TypedAddress, "3@KSampler.steps")
	}
	if changes[0].Role != RoleSteps {
		t.Errorf("Role = %q, want %q", changes[0].Role, RoleSteps)
	}
}

// ---------------------------------------------------------------------------
// Applying a patch
// ---------------------------------------------------------------------------

func TestApplyChangesRewritesOnlyTargetedSlots(t *testing.T) {
	t.Parallel()

	raw := []byte(sampleGraph)
	graph := mustGraph(t, sampleGraph)
	a, err := ParseAssignment("3@KSampler.seed=18446744073709551615")
	if err != nil {
		t.Fatalf("ParseAssignment: %v", err)
	}
	changes, err := Resolve(graph, []Assignment{a}, ResolveOptions{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	patched, err := ApplyChanges(raw, changes)
	if err != nil {
		t.Fatalf("ApplyChanges: %v", err)
	}
	if !strings.Contains(string(patched), "18446744073709551615") {
		t.Fatalf("64-bit seed lost precision in the patched graph:\n%s", patched)
	}

	after := mustGraph(t, string(patched))
	// Everything except the seed must be byte-identical, and the ORIGINAL graph
	// must be untouched so a dry run can never mutate the caller's file.
	if got := ValueString(after["3"].Inputs["steps"]); got != "20" {
		t.Errorf("unrelated slot changed: steps = %q", got)
	}
	if got := ValueString(after["6"].Inputs["text"]); got != "a lighthouse" {
		t.Errorf("unrelated slot changed: 6.text = %q", got)
	}
	if got := ValueString(graph["3"].Inputs["seed"]); got != "1125899906842624" {
		t.Errorf("ApplyChanges mutated the input graph: seed = %q", got)
	}
	if _, ok := after["6"].Meta["title"]; !ok {
		t.Error("_meta.title was dropped by the patch round trip")
	}
	// The exact identity must move; the shape identity must not, because seed is
	// a volatile input and two runs differing only by seed are comparable.
	beforeGraphSHA, _ := store.GraphSHA(graph)
	afterGraphSHA, _ := store.GraphSHA(after)
	if beforeGraphSHA == afterGraphSHA {
		t.Error("GraphSHA did not change after patching the seed")
	}
	beforeShape, _ := store.ShapeSHA(graph)
	afterShape, _ := store.ShapeSHA(after)
	if beforeShape != afterShape {
		t.Error("ShapeSHA changed after a seed-only patch; seed must be stripped from the shape identity")
	}
}

// ---------------------------------------------------------------------------
// Slot extraction and roles
// ---------------------------------------------------------------------------

func TestExtractSlotsRoles(t *testing.T) {
	t.Parallel()

	graph := mustGraph(t, sampleGraph)
	extracted := ExtractSlots(graph)

	byAddress := map[string]Slot{}
	for _, s := range extracted {
		byAddress[s.Address] = s
	}

	cases := []struct {
		address  string
		wantRole Role
		wantType string
		wantLink bool
	}{
		{address: "3.seed", wantRole: RoleSeed, wantType: "int"},
		{address: "3.steps", wantRole: RoleSteps, wantType: "int"},
		{address: "3.cfg", wantRole: RoleCFG, wantType: "number"},
		{address: "3.sampler_name", wantRole: RoleSampler, wantType: "string"},
		{address: "3.scheduler", wantRole: RoleScheduler, wantType: "string"},
		{address: "3.denoise", wantRole: RoleDenoise, wantType: "number"},
		{address: "4.ckpt_name", wantRole: RoleCheckpoint, wantType: "string"},
		{address: "5.width", wantRole: RoleWidth, wantType: "int"},
		{address: "5.height", wantRole: RoleHeight, wantType: "int"},
		{address: "5.batch_size", wantRole: RoleBatch, wantType: "int"},
		{address: "6.text", wantRole: RolePositivePrompt, wantType: "string"},
		{address: "7.text", wantRole: RoleNegativePrompt, wantType: "string"},
		{address: "10.image", wantRole: RoleInputImage, wantType: "string"},
		{address: "3.model", wantRole: "", wantType: "link", wantLink: true},
	}

	for _, tc := range cases {
		t.Run(tc.address, func(t *testing.T) {
			got, ok := byAddress[tc.address]
			if !ok {
				t.Fatalf("slot %q not extracted", tc.address)
			}
			if got.Role != tc.wantRole {
				t.Errorf("role = %q, want %q", got.Role, tc.wantRole)
			}
			if got.Type != tc.wantType {
				t.Errorf("type = %q, want %q", got.Type, tc.wantType)
			}
			if got.Link != tc.wantLink {
				t.Errorf("link = %v, want %v", got.Link, tc.wantLink)
			}
		})
	}

	if got := byAddress["3.steps"].TypedAddress; got != "3@KSampler.steps" {
		t.Errorf("typed address = %q, want %q", got, "3@KSampler.steps")
	}
	if got := byAddress["6.text"].Title; got != "CLIP Text Encode (Prompt)" {
		t.Errorf("title = %q, want the _meta title", got)
	}
	// Deterministic ordering: node ids sort numerically, not lexically, so 10
	// must come after 9 rather than after 1.
	var order []string
	for _, s := range extracted {
		if len(order) == 0 || order[len(order)-1] != s.NodeID {
			order = append(order, s.NodeID)
		}
	}
	want := []string{"3", "4", "5", "6", "7", "8", "9", "10"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("node order = %v, want %v", order, want)
	}
}

func TestPromptPolarityFollowsWiringThroughAChain(t *testing.T) {
	t.Parallel()

	// The negative encoder reaches the sampler through a ConditioningCombine,
	// and its title says nothing useful. Only the wiring gives the answer.
	const chained = `{
      "1": {"class_type": "KSampler", "inputs": {"positive": ["2", 0], "negative": ["3", 0]}},
      "2": {"class_type": "CLIPTextEncode", "inputs": {"text": "a castle"}},
      "3": {"class_type": "ConditioningCombine", "inputs": {"conditioning_1": ["4", 0], "conditioning_2": ["5", 0]}},
      "4": {"class_type": "CLIPTextEncode", "inputs": {"text": "blurry"}, "_meta": {"title": "Prompt B"}},
      "5": {"class_type": "CLIPTextEncode", "inputs": {"text": "watermark"}}
    }`
	graph := mustGraph(t, chained)
	byAddress := map[string]Slot{}
	for _, s := range ExtractSlots(graph) {
		byAddress[s.Address] = s
	}
	for _, addr := range []string{"4.text", "5.text"} {
		if got := byAddress[addr].Role; got != RoleNegativePrompt {
			t.Errorf("%s role = %q, want %q", addr, got, RoleNegativePrompt)
		}
	}
	if got := byAddress["2.text"].Role; got != RolePositivePrompt {
		t.Errorf("2.text role = %q, want %q", got, RolePositivePrompt)
	}
}

func TestParseGraphRejectsUIExport(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		raw     string
		wantErr error
		wantSub string
	}{
		{
			name:    "UI workflow export",
			raw:     `{"last_node_id": 9, "nodes": [{"id": 3, "type": "KSampler"}], "links": []}`,
			wantErr: ErrUIFormat,
		},
		{
			name:    "node without class_type",
			raw:     `{"3": {"inputs": {"steps": 20}}}`,
			wantSub: "no class_type",
		},
		{name: "empty object", raw: `{}`, wantSub: "no nodes"},
		{name: "not an object", raw: `[1,2,3]`, wantSub: "not a JSON object"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseGraph([]byte(tc.raw))
			if err == nil {
				t.Fatal("want an error")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantSub)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Offline validation
// ---------------------------------------------------------------------------

func TestParseObjectInfoBothComboShapes(t *testing.T) {
	t.Parallel()

	schema := mustSchema(t)

	// v3: options at index 1 under {"options": [...]}.
	v3 := schema["KSampler"].Inputs["sampler_name"]
	opts, shape := store.ParseComboOptions(v3.Raw)
	if shape != store.ComboV3 {
		t.Errorf("KSampler.sampler_name shape = %q, want %q", shape, store.ComboV3)
	}
	if len(opts) != 3 {
		t.Errorf("KSampler.sampler_name options = %v, want 3", opts)
	}
	if v3.TypeName != "COMBO" {
		t.Errorf("KSampler.sampler_name type = %q, want COMBO", v3.TypeName)
	}
	if !v3.Required {
		t.Error("KSampler.sampler_name should be required")
	}

	// legacy: options at index 0.
	legacy := schema["CheckpointLoaderSimple"].Inputs["ckpt_name"]
	opts, shape = store.ParseComboOptions(legacy.Raw)
	if shape != store.ComboLegacy {
		t.Errorf("ckpt_name shape = %q, want %q", shape, store.ComboLegacy)
	}
	if len(opts) != 2 {
		t.Errorf("ckpt_name options = %v, want 2", opts)
	}
	if legacy.TypeName != "COMBO" {
		t.Errorf("ckpt_name type = %q, want COMBO", legacy.TypeName)
	}

	// A plain typed input must NOT be mistaken for a COMBO.
	if _, shape := store.ParseComboOptions(schema["CLIPTextEncode"].Inputs["text"].Raw); shape != store.ComboNone {
		t.Errorf("CLIPTextEncode.text shape = %q, want %q", shape, store.ComboNone)
	}

	// optional inputs are parsed and marked as such.
	if in, ok := schema["SaveImage"].Inputs["filename_prefix"]; !ok || in.Required {
		t.Errorf("SaveImage.filename_prefix = %+v, want a non-required input", in)
	}
	if schema["KSampler"].Category != "sampling" {
		t.Errorf("KSampler category = %q, want %q", schema["KSampler"].Category, "sampling")
	}
}

func TestValidateGraph(t *testing.T) {
	t.Parallel()

	schema := mustSchema(t)

	cases := []struct {
		name        string
		graph       string
		wantKinds   []string
		wantErrors  int
		wantWarns   int
		wantMessage string
	}{
		{
			name:  "clean graph produces no findings",
			graph: sampleGraph,
		},
		{
			name: "COMBO value not among the server's options is REJECTED",
			graph: `{
              "1": {"class_type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "does_not_exist.safetensors"}}
            }`,
			wantKinds: []string{KindComboNotInOptions}, wantErrors: 1,
			wantMessage: "is not among the 2 options",
		},
		{
			name: "v3 COMBO value not among the options is REJECTED",
			graph: `{
              "1": {"class_type": "KSampler", "inputs": {
                "model": ["9", 0], "seed": 1, "steps": 20, "cfg": 8.0,
                "sampler_name": "not_a_sampler", "scheduler": "normal",
                "positive": ["9", 0], "negative": ["9", 0], "latent_image": ["9", 0], "denoise": 1.0}},
              "9": {"class_type": "CLIPTextEncode", "inputs": {"text": "x", "clip": ["9", 1]}}
            }`,
			wantKinds: []string{KindComboNotInOptions}, wantErrors: 1,
		},
		{
			name: "EMPTY option list reports an unregistered model CLASS, not a missing file",
			graph: `{
              "1": {"class_type": "LatentUpscaleModelLoader", "inputs": {"model_name": "4x-UltraSharp.pth"}}
            }`,
			wantKinds: []string{KindClassUnregistered}, wantErrors: 1,
			wantMessage: "extra_model_paths.yaml",
		},
		{
			name: "unknown class type",
			graph: `{
              "1": {"class_type": "SomeCustomNodeThatIsNotInstalled", "inputs": {"x": 1}}
            }`,
			wantKinds: []string{KindUnknownClass}, wantErrors: 1,
		},
		{
			name: "missing required input",
			graph: `{
              "1": {"class_type": "EmptyLatentImage", "inputs": {"width": 512}}
            }`,
			wantKinds: []string{KindMissingRequiredInput, KindMissingRequiredInput}, wantErrors: 2,
		},
		{
			name: "input the class does not declare is a warning",
			graph: `{
              "1": {"class_type": "EmptyLatentImage", "inputs": {"width": 512, "height": 512, "batch_size": 1, "hieght": 512}}
            }`,
			wantKinds: []string{KindUnknownInput}, wantWarns: 1,
		},
		{
			name: "dangling link",
			graph: `{
              "1": {"class_type": "VAEDecode", "inputs": {"samples": ["77", 0], "vae": ["77", 2]}}
            }`,
			wantKinds: []string{KindDanglingLink, KindDanglingLink}, wantErrors: 2,
		},
		{
			name: "absolute host path in an image input",
			graph: `{
              "1": {"class_type": "LoadImage", "inputs": {"image": "/home/me/ref.png"}}
            }`,
			// Also not among the LoadImage options, hence two errors.
			wantKinds: []string{KindHostPath, KindComboNotInOptions}, wantErrors: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			graph := mustGraph(t, tc.graph)
			findings := ValidateGraph(graph, schema)
			gotErrors, gotWarns := CountFindings(findings)
			if gotErrors != tc.wantErrors || gotWarns != tc.wantWarns {
				t.Fatalf("findings = %d error(s)/%d warning(s), want %d/%d:\n%s",
					gotErrors, gotWarns, tc.wantErrors, tc.wantWarns, renderFindings(findings))
			}
			for _, kind := range tc.wantKinds {
				if !hasKind(findings, kind) {
					t.Errorf("missing finding of kind %q:\n%s", kind, renderFindings(findings))
				}
			}
			if tc.wantMessage != "" && !strings.Contains(renderFindings(findings), tc.wantMessage) {
				t.Errorf("findings do not mention %q:\n%s", tc.wantMessage, renderFindings(findings))
			}
		})
	}
}

func TestValidateGraphWithoutSchemaStillRunsGraphLocalChecks(t *testing.T) {
	t.Parallel()

	graph := mustGraph(t, `{
      "1": {"class_type": "VAEDecode", "inputs": {"samples": ["77", 0], "vae": ["2", 0]}},
      "2": {"class_type": "VAELoader", "inputs": {"vae_name": "vae.safetensors"}}
    }`)
	findings := ValidateGraph(graph, nil)
	if !hasKind(findings, KindDanglingLink) {
		t.Fatalf("dangling link missed with no schema:\n%s", renderFindings(findings))
	}
	if hasKind(findings, KindUnknownClass) {
		t.Error("an absent schema must not be reported as unknown classes")
	}
}

func TestComboOptionsSurfacedOnRejection(t *testing.T) {
	t.Parallel()

	schema := mustSchema(t)
	graph := mustGraph(t, `{"1": {"class_type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "nope.safetensors"}}}`)
	findings := ValidateGraph(graph, schema)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1:\n%s", len(findings), renderFindings(findings))
	}
	got := findings[0]
	if got.Kind != KindComboNotInOptions {
		t.Fatalf("kind = %q, want %q", got.Kind, KindComboNotInOptions)
	}
	if len(got.Options) != 2 {
		t.Errorf("options = %v, want the 2 the server offers so the operator can pick one", got.Options)
	}
	if got.Address != "1.ckpt_name" {
		t.Errorf("address = %q, want %q", got.Address, "1.ckpt_name")
	}
}

func TestLooksLikeClassEntry(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "class entry with input", raw: `{"input": {"required": {}}}`, want: true},
		{name: "class entry with only output", raw: `{"output": ["IMAGE"]}`, want: true},
		{name: "class entry with display_name", raw: `{"display_name": "KSampler"}`, want: true},
		{name: "arbitrary object", raw: `{"foo": 1}`, want: false},
		{name: "array", raw: `[1,2]`, want: false},
		{name: "string", raw: `"x"`, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var v interface{}
			if err := json.Unmarshal([]byte(tc.raw), &v); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := LooksLikeClassEntry(v); got != tc.want {
				t.Errorf("LooksLikeClassEntry(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func hasKind(findings []Finding, kind string) bool {
	for _, f := range findings {
		if f.Kind == kind {
			return true
		}
	}
	return false
}

func renderFindings(findings []Finding) string {
	if len(findings) == 0 {
		return "    (no findings)"
	}
	var b strings.Builder
	for _, f := range findings {
		b.WriteString("    [")
		b.WriteString(f.Severity)
		b.WriteString("] ")
		b.WriteString(f.Kind)
		b.WriteString(" ")
		b.WriteString(f.Address)
		b.WriteString(": ")
		b.WriteString(f.Message)
		b.WriteString("\n")
	}
	return b.String()
}

// TestValidate_AutogrowGroupSatisfiedByDottedChildren pins the defect that shipped in the
// first build: ComfyUI serialises a COMFY_AUTOGROW_V3 group only as dotted child keys, so an
// exact-key check reported "required input values is absent" for a graph the server accepts,
// and warned "class declares no input values.a" for the children. Same bug class as
// mvanhorn/cli-printing-press#97 in the harness preflight.
func TestValidate_AutogrowGroupSatisfiedByDottedChildren(t *testing.T) {
	if _, ok := autogrowParent("values.a"); !ok {
		t.Fatal("autogrowParent must split a dotted child key")
	}
	if _, ok := autogrowParent("values"); ok {
		t.Fatal("a bare group name is not a dotted child")
	}
	if _, ok := autogrowParent(".a"); ok {
		t.Fatal("a leading dot is not a valid child key")
	}
	if _, ok := autogrowParent("values."); ok {
		t.Fatal("a trailing dot is not a valid child key")
	}

	inputs := map[string]interface{}{"expression": "a/2", "values.a": []interface{}{"9", 0}}
	if !hasAutogrowChildren(inputs, "values") {
		t.Fatal("values.a must satisfy the values group")
	}
	if hasAutogrowChildren(inputs, "other") {
		t.Fatal("an unrelated group must not be satisfied")
	}
}

// TestValidate_ClassUnregisteredOnlyForFolderBackedInputs pins the second shipped defect:
// an empty COMBO on SaveVideo.codec or ResizeImageMaskNode.resize_type is NOT an unregistered
// model class, and advising an extra_model_paths.yaml key for them is wrong.
func TestValidate_ClassUnregisteredOnlyForFolderBackedInputs(t *testing.T) {
	folderBacked := []string{"unet_name", "ckpt_name", "vae_name", "lora_name", "model_name", "name"}
	for _, n := range folderBacked {
		if !isFolderBackedInput(n) {
			t.Fatalf("%q must be treated as folder-backed", n)
		}
	}
	enums := []string{"codec", "resize_type", "format", "scheduler", "precision", "weight_dtype"}
	for _, n := range enums {
		if isFolderBackedInput(n) {
			t.Fatalf("%q is an enum dropdown, not a model folder input", n)
		}
	}

	// KNOWN, BENIGN FALSE POSITIVE. `sampler_name` matches the *_name convention but holds
	// enum values (euler, euler_ancestral), not files. It is unreachable in practice: the
	// narrowing only gates the EMPTY-COMBO diagnosis, and a sampler COMBO is never empty, so
	// ClassifyModelVisibility returns visible/not-listed and never reaches the branch.
	// Asserted explicitly so a future reader knows this was considered, not missed.
	if !isFolderBackedInput("sampler_name") {
		t.Fatal("heuristic changed: sampler_name no longer matches *_name — re-check the note above")
	}
}
