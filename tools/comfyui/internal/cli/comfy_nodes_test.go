// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
//
// Tests for the node-schema and model-visibility surfaces. Everything under
// test is pure: a decoded /object_info fixture in, a verdict or a rendered
// structure out. No live ComfyUI server is required or contacted.

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"comfyui-pp-cli/internal/store"
	"github.com/spf13/cobra"
)

// comfyTestObjectInfo mirrors the two COMBO shapes a real 0.32.0 box serves at
// the same time: CheckpointLoaderSimple uses the LEGACY shape (option list at
// tuple index 0), VAELoader uses the V3 shape (options at index 1 under
// "options"). LatentUpscaleModelLoader is the trap this whole family exists for
// — a registered COMBO with ZERO options, which means the model CLASS has no
// registered folder, NOT that a file is missing.
const comfyTestObjectInfo = `{
  "CheckpointLoaderSimple": {
    "input": {
      "required": {
        "ckpt_name": [["sd_xl_base_1.0.safetensors", "v1-5-pruned.ckpt"], {"tooltip": "The checkpoint to load."}]
      }
    },
    "output": ["MODEL", "CLIP", "VAE"],
    "output_name": ["MODEL", "CLIP", "VAE"],
    "name": "CheckpointLoaderSimple",
    "display_name": "Load Checkpoint",
    "description": "Loads a diffusion model checkpoint.",
    "category": "loaders",
    "python_module": "nodes",
    "output_node": false
  },
  "VAELoader": {
    "input": {
      "required": {
        "vae_name": ["COMBO", {"options": ["vae-ft-mse-840000-ema-pruned.safetensors", "taesd"]}]
      }
    },
    "output": ["VAE"],
    "output_name": ["VAE"],
    "display_name": "Load VAE",
    "description": "Loads a VAE.",
    "category": "loaders",
    "output_node": false
  },
  "LatentUpscaleModelLoader": {
    "input": {
      "required": {
        "upscale_model_name": ["COMBO", {"options": []}]
      }
    },
    "output": ["UPSCALE_MODEL"],
    "output_name": ["UPSCALE_MODEL"],
    "display_name": "Load Latent Upscale Model",
    "description": "Loads a latent upscale model.",
    "category": "loaders",
    "output_node": false
  },
  "KSampler": {
    "input": {
      "required": {
        "seed": ["INT", {"default": 0, "min": 0, "max": 1125899906842624, "tooltip": "The random seed."}],
        "sampler_name": [["euler", "dpmpp_2m"], {"default": "euler"}],
        "denoise": ["FLOAT", {"default": 1.0, "min": 0.0, "max": 1.0, "step": 0.01}]
      },
      "optional": {
        "positive": ["CONDITIONING"]
      },
      "hidden": {
        "prompt": ["PROMPT"]
      }
    },
    "output": ["LATENT"],
    "output_name": ["LATENT"],
    "display_name": "KSampler",
    "description": "Uses the provided model to denoise the input latent.",
    "category": "sampling",
    "output_node": false
  }
}`

func comfyTestSchema(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	var classes map[string]json.RawMessage
	if err := json.Unmarshal([]byte(comfyTestObjectInfo), &classes); err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}
	return classes
}

// comfyTestSchemaWithout returns the fixture minus the named classes, so a case
// can be constructed where no COMBO is empty.
func comfyTestSchemaWithout(t *testing.T, drop ...string) map[string]json.RawMessage {
	t.Helper()
	classes := comfyTestSchema(t)
	for _, name := range drop {
		delete(classes, name)
	}
	return classes
}

func TestComfyOrderedKeys(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "preserves wire order, not sorted order",
			raw:  `{"seed": 1, "abc": 2, "denoise": 3}`,
			want: []string{"seed", "abc", "denoise"},
		},
		{
			name: "skips nested objects and arrays without losing top-level order",
			raw:  `{"a": {"z": [1, 2, {"q": 3}]}, "b": [[], {}], "c": null}`,
			want: []string{"a", "b", "c"},
		},
		{name: "empty object", raw: `{}`, want: nil},
		{name: "array is not an object", raw: `["a", "b"]`, want: nil},
		{name: "invalid json", raw: `{oops`, want: nil},
		{name: "empty input", raw: ``, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := comfyOrderedKeys(json.RawMessage(tc.raw))
			if len(got) != len(tc.want) {
				t.Fatalf("comfyOrderedKeys(%s) = %v, want %v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("comfyOrderedKeys(%s) = %v, want %v", tc.raw, got, tc.want)
				}
			}
		})
	}
}

func TestComfySummarizeInput(t *testing.T) {
	tests := []struct {
		name            string
		spec            string
		wantKind        string
		wantType        string
		wantShape       store.ComboShape
		wantOptionCount int
		wantStatus      string
		wantRemedy      bool
	}{
		{
			name:            "v3 combo reads options at index 1",
			spec:            `["COMBO", {"options": ["a.safetensors", "b.safetensors"]}]`,
			wantKind:        "combo",
			wantType:        "COMBO",
			wantShape:       store.ComboV3,
			wantOptionCount: 2,
			wantStatus:      "ok",
		},
		{
			name:            "legacy combo reads the list at index 0",
			spec:            `[["a.ckpt", "b.ckpt"], {"tooltip": "pick one"}]`,
			wantKind:        "combo",
			wantType:        "COMBO",
			wantShape:       store.ComboLegacy,
			wantOptionCount: 2,
			wantStatus:      "ok",
		},
		{
			name:            "empty v3 combo is class-unregistered, not a missing file",
			spec:            `["COMBO", {"options": []}]`,
			wantKind:        "combo",
			wantType:        "COMBO",
			wantShape:       store.ComboV3,
			wantOptionCount: 0,
			wantStatus:      string(store.ModelClassUnregistered),
			wantRemedy:      true,
		},
		{
			name:            "empty legacy combo is class-unregistered too",
			spec:            `[[], {"default": ""}]`,
			wantKind:        "combo",
			wantType:        "COMBO",
			wantShape:       store.ComboLegacy,
			wantOptionCount: 0,
			wantStatus:      string(store.ModelClassUnregistered),
			wantRemedy:      true,
		},
		{
			name:       "primitive INT keeps its declared type",
			spec:       `["INT", {"default": 20, "min": 1, "max": 100, "step": 1}]`,
			wantKind:   "primitive",
			wantType:   "INT",
			wantShape:  store.ComboNone,
			wantStatus: string(store.ModelNoSuchInput),
		},
		{
			name:       "single-element type tuple is primitive",
			spec:       `["CONDITIONING"]`,
			wantKind:   "primitive",
			wantType:   "CONDITIONING",
			wantShape:  store.ComboNone,
			wantStatus: string(store.ModelNoSuchInput),
		},
		{
			name:       "bare string spec",
			spec:       `"MODEL"`,
			wantKind:   "primitive",
			wantType:   "MODEL",
			wantShape:  store.ComboNone,
			wantStatus: string(store.ModelNoSuchInput),
		},
		{
			name:       "unrecognised spec shape does not panic",
			spec:       `{"weird": true}`,
			wantKind:   "primitive",
			wantType:   "UNKNOWN",
			wantShape:  store.ComboNone,
			wantStatus: string(store.ModelNoSuchInput),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var spec interface{}
			if err := json.Unmarshal([]byte(tc.spec), &spec); err != nil {
				t.Fatalf("bad spec fixture: %v", err)
			}
			got := comfySummarizeInput("some_input", "required", spec)
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if got.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", got.Type, tc.wantType)
			}
			if got.ComboShape != "" && store.ComboShape(got.ComboShape) != tc.wantShape {
				t.Errorf("ComboShape = %q, want %q", got.ComboShape, tc.wantShape)
			}
			if tc.wantShape == store.ComboNone && got.ComboShape != "" {
				t.Errorf("ComboShape = %q, want empty for a primitive", got.ComboShape)
			}
			if got.OptionCount != tc.wantOptionCount {
				t.Errorf("OptionCount = %d, want %d", got.OptionCount, tc.wantOptionCount)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, tc.wantStatus)
			}
			if tc.wantRemedy {
				if got.Remedy == "" {
					t.Fatal("empty COMBO must carry a remedy")
				}
				if !strings.Contains(got.Remedy, "extra_model_paths.yaml") {
					t.Errorf("remedy must name extra_model_paths.yaml, got %q", got.Remedy)
				}
				if !strings.Contains(got.Remedy, "NOT a missing file") {
					t.Errorf("remedy must deny the missing-file reading, got %q", got.Remedy)
				}
			} else if got.Remedy != "" {
				t.Errorf("Remedy = %q, want empty", got.Remedy)
			}
		})
	}
}

func TestComfySummarizeInputMetadata(t *testing.T) {
	var spec interface{}
	if err := json.Unmarshal([]byte(`["INT", {"default": 20, "min": 1, "max": 100, "step": 2, "multiline": true, "tooltip": "steps"}]`), &spec); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	got := comfySummarizeInput("steps", "required", spec)
	if got.Default != float64(20) {
		t.Errorf("Default = %v, want 20", got.Default)
	}
	if got.Min != float64(1) || got.Max != float64(100) || got.Step != float64(2) {
		t.Errorf("min/max/step = %v/%v/%v, want 1/100/2", got.Min, got.Max, got.Step)
	}
	if !got.Multiline {
		t.Error("Multiline = false, want true")
	}
	if got.Tooltip != "steps" {
		t.Errorf("Tooltip = %q, want %q", got.Tooltip, "steps")
	}
}

func TestComfyShapeDetailNamesTheIndex(t *testing.T) {
	tests := []struct {
		shape store.ComboShape
		want  string
	}{
		{store.ComboV3, "index 1"},
		{store.ComboLegacy, "index 0"},
		{store.ComboNone, "not a COMBO"},
	}
	for _, tc := range tests {
		t.Run(string(tc.shape), func(t *testing.T) {
			if got := comfyShapeDetail(tc.shape); !strings.Contains(got, tc.want) {
				t.Errorf("comfyShapeDetail(%q) = %q, want it to mention %q", tc.shape, got, tc.want)
			}
		})
	}
}

func TestComfyOutputTypes(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "list of strings", raw: `["MODEL","CLIP","VAE"]`, want: []string{"MODEL", "CLIP", "VAE"}},
		{name: "bare string", raw: `"LATENT"`, want: []string{"LATENT"}},
		{name: "nested combo output", raw: `[["a","b"],"IMAGE"]`, want: []string{"COMBO[a,b]", "IMAGE"}},
		{name: "empty", raw: ``, want: nil},
		{name: "object is not an output list", raw: `{"a":1}`, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := comfyOutputTypes(json.RawMessage(tc.raw))
			if len(got) != len(tc.want) {
				t.Fatalf("comfyOutputTypes(%s) = %v, want %v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("comfyOutputTypes(%s) = %v, want %v", tc.raw, got, tc.want)
				}
			}
		})
	}
}

func TestComfyDecodeClassPreservesInputOrder(t *testing.T) {
	classes := comfyTestSchema(t)
	schema, err := comfyDecodeClass("KSampler", classes["KSampler"])
	if err != nil {
		t.Fatalf("comfyDecodeClass: %v", err)
	}
	if len(schema.Groups) != 3 {
		t.Fatalf("groups = %d, want 3 (required, optional, hidden)", len(schema.Groups))
	}
	wantGroups := []string{"required", "optional", "hidden"}
	for i, want := range wantGroups {
		if schema.Groups[i].Requirement != want {
			t.Errorf("group[%d] = %q, want %q", i, schema.Groups[i].Requirement, want)
		}
	}
	wantOrder := []string{"seed", "sampler_name", "denoise"}
	got := schema.Groups[0].Order
	if len(got) != len(wantOrder) {
		t.Fatalf("required order = %v, want %v", got, wantOrder)
	}
	for i := range wantOrder {
		if got[i] != wantOrder[i] {
			t.Fatalf("required order = %v, want %v (wire order, not sorted)", got, wantOrder)
		}
	}
	if schema.Category != "sampling" || schema.DisplayName != "KSampler" {
		t.Errorf("metadata = %q/%q, want sampling/KSampler", schema.Category, schema.DisplayName)
	}
}

func TestComfyFindInput(t *testing.T) {
	classes := comfyTestSchema(t)
	schema, err := comfyDecodeClass("KSampler", classes["KSampler"])
	if err != nil {
		t.Fatalf("comfyDecodeClass: %v", err)
	}
	tests := []struct {
		name            string
		input           string
		wantFound       bool
		wantRequirement string
	}{
		{name: "required input", input: "seed", wantFound: true, wantRequirement: "required"},
		{name: "optional input", input: "positive", wantFound: true, wantRequirement: "optional"},
		{name: "hidden input", input: "prompt", wantFound: true, wantRequirement: "hidden"},
		{name: "case-insensitive fallback", input: "SEED", wantFound: true, wantRequirement: "required"},
		{name: "missing input", input: "nope", wantFound: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, requirement, found := comfyFindInput(schema, tc.input)
			if found != tc.wantFound {
				t.Fatalf("found = %t, want %t", found, tc.wantFound)
			}
			if found && requirement != tc.wantRequirement {
				t.Errorf("requirement = %q, want %q", requirement, tc.wantRequirement)
			}
		})
	}
}

func TestComfyNodeMatchesTokenAND(t *testing.T) {
	match := comfyNodeMatch{
		ClassType:   "CheckpointLoaderSimple",
		DisplayName: "Load Checkpoint",
		Category:    "loaders",
		Description: "Loads a diffusion model checkpoint.",
	}
	tests := []struct {
		name   string
		query  string
		expect bool
	}{
		{name: "single token in class name", query: "checkpointloader", expect: true},
		{name: "case-insensitive", query: "LOAD", expect: true},
		{name: "token from category", query: "loaders", expect: true},
		{name: "token from description", query: "diffusion", expect: true},
		{name: "all tokens must match (AND)", query: "load diffusion", expect: true},
		{name: "one missing token fails the whole query", query: "load lora", expect: false},
		{name: "empty query matches everything", query: "", expect: true},
		{name: "whitespace-only query matches everything", query: "   ", expect: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tokens := comfySearchTokens([]string{tc.query})
			if got := comfyNodeMatches(match, tokens); got != tc.expect {
				t.Errorf("comfyNodeMatches(%q) = %t, want %t", tc.query, got, tc.expect)
			}
		})
	}
}

func TestComfySearchNodesIsSortedAndFiltered(t *testing.T) {
	classes := comfyTestSchema(t)
	tests := []struct {
		name  string
		query []string
		want  []string
	}{
		{name: "loaders category", query: []string{"loaders"}, want: []string{"CheckpointLoaderSimple", "LatentUpscaleModelLoader", "VAELoader"}},
		{name: "two tokens", query: []string{"load", "vae"}, want: []string{"VAELoader"}},
		{name: "no match", query: []string{"lora"}, want: nil},
		{name: "empty query returns all, sorted", query: nil, want: []string{"CheckpointLoaderSimple", "KSampler", "LatentUpscaleModelLoader", "VAELoader"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := comfySearchNodes(classes, comfySearchTokens(tc.query))
			if len(got) != len(tc.want) {
				t.Fatalf("matches = %d (%v), want %d (%v)", len(got), got, len(tc.want), tc.want)
			}
			for i := range tc.want {
				if got[i].ClassType != tc.want[i] {
					t.Fatalf("match[%d] = %q, want %q (results must be sorted by class name)", i, got[i].ClassType, tc.want[i])
				}
			}
		})
	}
}

func TestComfyFileExtension(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "model.safetensors", want: ".safetensors"},
		{in: "SDXL/Model.SafeTensors", want: ".safetensors"},
		{in: "sub\\dir\\weights.PTH", want: ".pth"},
		{in: "euler", want: ""},
		{in: "trailing.", want: ""},
		{in: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := comfyFileExtension(tc.in); got != tc.want {
				t.Errorf("comfyFileExtension(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestComfySameKindOptions(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		options  []string
		want     bool
	}{
		{name: "same extension present", filename: "mine.safetensors", options: []string{"other.safetensors"}, want: true},
		{name: "different extension only", filename: "mine.pth", options: []string{"other.safetensors"}, want: false},
		{name: "enum options are never the same kind", filename: "mine.safetensors", options: []string{"euler", "karras"}, want: false},
		{name: "no extension falls back to any model file", filename: "mine", options: []string{"other.ckpt"}, want: true},
		{name: "no extension with enum options", filename: "mine", options: []string{"euler"}, want: false},
		{name: "empty option list", filename: "mine.safetensors", options: nil, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := comfySameKindOptions(tc.filename, tc.options); got != tc.want {
				t.Errorf("comfySameKindOptions(%q, %v) = %t, want %t", tc.filename, tc.options, got, tc.want)
			}
		})
	}
}

func TestComfyLooksLikeModelInput(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		options []string
		want    bool
	}{
		{name: "populated with model files", input: "ckpt_name", options: []string{"a.safetensors"}, want: true},
		{name: "populated with enum values", input: "sampler_name", options: []string{"euler", "dpmpp_2m"}, want: false},
		{name: "empty but folder-backed name is kept (the case that matters)", input: "upscale_model_name", options: nil, want: true},
		{name: "empty bare name input is kept", input: "name", options: nil, want: true},
		{name: "empty and unrelated name is dropped", input: "mode", options: nil, want: false},
		// Live-server evidence: empty cloud-API dropdowns are named `model`,
		// `style`, `duration` — they are empty because a remote catalogue was
		// not fetched, not because a folder is unregistered.
		{name: "empty cloud-node model dropdown is dropped", input: "model", options: nil, want: false},
		{name: "empty cloud-node style dropdown is dropped", input: "style", options: nil, want: false},
		{name: "empty codec enum is dropped", input: "codec", options: nil, want: false},
		{name: "model files win over enum siblings", input: "control_net_name", options: []string{"none", "diff.pth"}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := comfyLooksLikeModelInput(tc.input, tc.options); got != tc.want {
				t.Errorf("comfyLooksLikeModelInput(%q, %v) = %t, want %t", tc.input, tc.options, got, tc.want)
			}
		})
	}
}

func TestComfyModelWhyVerdicts(t *testing.T) {
	tests := []struct {
		name           string
		classes        func(*testing.T) map[string]json.RawMessage
		filename       string
		classFilter    string
		inputFilter    string
		wantVerdict    store.ModelVisibility
		wantRemedyHas  string
		wantOfferedBy  string
		wantConsidered int
	}{
		{
			name:           "visible: the loader lists it",
			classes:        comfyTestSchema,
			filename:       "sd_xl_base_1.0.safetensors",
			wantVerdict:    store.ModelVisible,
			wantRemedyHas:  "No action needed",
			wantOfferedBy:  "CheckpointLoaderSimple.ckpt_name",
			wantConsidered: 4,
		},
		{
			name:          "visible through the v3 shape too",
			classes:       comfyTestSchema,
			filename:      "vae-ft-mse-840000-ema-pruned.safetensors",
			wantVerdict:   store.ModelVisible,
			wantOfferedBy: "VAELoader.vae_name",
		},
		{
			name:          "class-unregistered outranks not-listed when a COMBO is empty",
			classes:       comfyTestSchema,
			filename:      "4x-UltraSharp.safetensors",
			wantVerdict:   store.ModelClassUnregistered,
			wantRemedyHas: "extra_model_paths.yaml",
		},
		{
			name: "not-listed: same kind is offered elsewhere, no empty COMBO exists",
			classes: func(t *testing.T) map[string]json.RawMessage {
				return comfyTestSchemaWithout(t, "LatentUpscaleModelLoader")
			},
			filename:      "not-installed.safetensors",
			wantVerdict:   store.ModelNotListed,
			wantRemedyHas: "IS registered and populated",
		},
		{
			name: "no-such-input: nothing would ever load this kind",
			classes: func(t *testing.T) map[string]json.RawMessage {
				return comfyTestSchemaWithout(t, "LatentUpscaleModelLoader")
			},
			filename:      "weights.gguf",
			wantVerdict:   store.ModelNoSuchInput,
			wantRemedyHas: "not installed",
		},
		{
			name:           "narrowed to one input: that input's own classification wins",
			classes:        comfyTestSchema,
			filename:       "sd_xl_base_1.0.safetensors",
			classFilter:    "VAELoader",
			inputFilter:    "vae_name",
			wantVerdict:    store.ModelNotListed,
			wantConsidered: 1,
		},
		{
			name:           "narrowed to an empty COMBO",
			classes:        comfyTestSchema,
			filename:       "anything.safetensors",
			classFilter:    "LatentUpscaleModelLoader",
			inputFilter:    "upscale_model_name",
			wantVerdict:    store.ModelClassUnregistered,
			wantRemedyHas:  "NOT a missing file",
			wantConsidered: 1,
		},
		{
			name:           "narrowed to a primitive input reports no-such-input, not a bare miss",
			classes:        comfyTestSchema,
			filename:       "sd_xl_base_1.0.safetensors",
			classFilter:    "KSampler",
			inputFilter:    "seed",
			wantVerdict:    store.ModelNoSuchInput,
			wantRemedyHas:  "not a file from a registered folder",
			wantConsidered: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			classes := tc.classes(t)
			hits, classCount, comboCount, considered := comfyScanModelVisibility(classes, tc.filename, tc.classFilter, tc.inputFilter)
			narrowed := tc.classFilter != "" && tc.inputFilter != ""
			report := comfyBuildWhyReport(tc.filename, hits, classCount, comboCount, considered, narrowed)
			if report.Verdict != string(tc.wantVerdict) {
				t.Fatalf("verdict = %q, want %q (summary: %s)", report.Verdict, tc.wantVerdict, report.Summary)
			}
			if tc.wantRemedyHas != "" && !strings.Contains(report.Remedy, tc.wantRemedyHas) {
				t.Errorf("remedy = %q, want it to contain %q", report.Remedy, tc.wantRemedyHas)
			}
			if tc.wantOfferedBy != "" {
				found := false
				for _, loader := range report.OfferedBy {
					if loader.String() == tc.wantOfferedBy {
						found = true
					}
				}
				if !found {
					t.Errorf("offered_by = %v, want it to include %q", report.OfferedBy, tc.wantOfferedBy)
				}
			}
			if tc.wantConsidered != 0 && considered != tc.wantConsidered {
				t.Errorf("considered = %d, want %d", considered, tc.wantConsidered)
			}
			if report.ClassesScanned != len(classes) {
				t.Errorf("classes_scanned = %d, want %d", report.ClassesScanned, len(classes))
			}
		})
	}
}

// A class-unregistered verdict must never be reported as a missing file: that
// conflation is the exact defect this command was built to end.
func TestComfyClassUnregisteredNeverReadsAsMissingFile(t *testing.T) {
	classes := comfyTestSchema(t)
	hits, classCount, comboCount, considered := comfyScanModelVisibility(classes, "4x-UltraSharp.safetensors", "", "")
	report := comfyBuildWhyReport("4x-UltraSharp.safetensors", hits, classCount, comboCount, considered, false)
	if report.Verdict != string(store.ModelClassUnregistered) {
		t.Fatalf("verdict = %q, want %q", report.Verdict, store.ModelClassUnregistered)
	}
	if !strings.Contains(report.Remedy, "NOT a missing file") {
		t.Errorf("remedy must deny the missing-file reading: %q", report.Remedy)
	}
	if !strings.Contains(report.Remedy, "RESTART") {
		t.Errorf("remedy must say a restart is required: %q", report.Remedy)
	}
	if len(report.EmptyCombos) != 1 || report.EmptyCombos[0].String() != "LatentUpscaleModelLoader.upscale_model_name" {
		t.Fatalf("empty_combos = %v, want the one empty COMBO", report.EmptyCombos)
	}
	// The competing same-kind evidence must still be reported, not swallowed.
	if len(report.SameKindLoaders) == 0 {
		t.Error("same_kind_loaders must stay in the report as competing evidence")
	}
	if report.SampleFrom == "" || len(report.Sample) == 0 {
		t.Error("a sample of what IS offered must accompany the verdict")
	}
}

// A real server serves ~150 empty COMBOs, nearly all of them API-node enums
// that fill in at call time. Only model-folder-shaped inputs may drive the
// class-unregistered verdict, or the signal is drowned in noise.
func TestComfyEmptyEnumIsNotUnregisteredEvidence(t *testing.T) {
	const fixture = `{
      "SaveVideo": {
        "input": {"required": {"codec": ["COMBO", {"options": []}]}},
        "display_name": "Save Video", "category": "image"
      },
      "CheckpointLoaderSimple": {
        "input": {"required": {"ckpt_name": [["already-here.safetensors"], {}]}},
        "display_name": "Load Checkpoint", "category": "loaders"
      }
    }`
	var classes map[string]json.RawMessage
	if err := json.Unmarshal([]byte(fixture), &classes); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}

	t.Run("an empty enum does not make the verdict class-unregistered", func(t *testing.T) {
		hits, classCount, comboCount, considered := comfyScanModelVisibility(classes, "mine.safetensors", "", "")
		report := comfyBuildWhyReport("mine.safetensors", hits, classCount, comboCount, considered, false)
		if report.Verdict != string(store.ModelNotListed) {
			t.Fatalf("verdict = %q, want not-listed (SaveVideo.codec is an enum, not a model folder)", report.Verdict)
		}
		if len(report.EmptyCombos) != 0 {
			t.Errorf("empty_combos = %v, want none", report.EmptyCombos)
		}
	})

	t.Run("naming that input explicitly still reports it", func(t *testing.T) {
		hits, classCount, comboCount, considered := comfyScanModelVisibility(classes, "mine.safetensors", "SaveVideo", "codec")
		report := comfyBuildWhyReport("mine.safetensors", hits, classCount, comboCount, considered, true)
		if report.Verdict != string(store.ModelClassUnregistered) {
			t.Fatalf("verdict = %q, want class-unregistered when the caller names the input", report.Verdict)
		}
		if len(report.EmptyCombos) != 1 {
			t.Errorf("empty_combos = %v, want the named input", report.EmptyCombos)
		}
	})
}

func TestComfyWhyProseStaysReadable(t *testing.T) {
	loaders := make([]comfyLoaderInput, 0, 9)
	for i := 0; i < 9; i++ {
		loaders = append(loaders, comfyLoaderInput{ClassType: fmt.Sprintf("Loader%d", i), Input: "ckpt_name"})
	}

	t.Run("summary caps the loader list", func(t *testing.T) {
		summary, _ := comfyWhyProse(store.ModelVisible, comfyModelWhyReport{Filename: "x.safetensors", OfferedBy: loaders})
		if !strings.Contains(summary, "+3 more") {
			t.Errorf("summary = %q, want the tail collapsed into a count", summary)
		}
		if strings.Contains(summary, "Loader8") {
			t.Errorf("summary must not enumerate every loader: %q", summary)
		}
	})

	t.Run("class-unregistered states the competing signal instead of implying certainty", func(t *testing.T) {
		report := comfyModelWhyReport{
			Filename:        "x.safetensors",
			EmptyCombos:     []comfyLoaderInput{{ClassType: "GLIGENLoader", Input: "gligen_name"}},
			SameKindLoaders: []comfyLoaderInput{{ClassType: "CheckpointLoaderSimple", Input: "ckpt_name"}},
		}
		summary, remedy := comfyWhyProse(store.ModelClassUnregistered, report)
		if !strings.Contains(summary, "Competing signal") {
			t.Errorf("summary = %q, want the not-listed possibility stated", summary)
		}
		if !strings.Contains(remedy, "NOT a missing file") {
			t.Errorf("remedy = %q, want the missing-file reading denied", remedy)
		}
	})
}

func TestComfyGroupModelFolders(t *testing.T) {
	classes := comfyTestSchema(t)

	modelOnly := comfyGroupModelFolders(classes, false)
	gotKeys := make([]string, 0, len(modelOnly))
	for _, group := range modelOnly {
		gotKeys = append(gotKeys, group.Key)
	}
	wantKeys := []string{"ckpt_name", "upscale_model_name", "vae_name"}
	if len(gotKeys) != len(wantKeys) {
		t.Fatalf("model-only groups = %v, want %v (sampler_name is an enum, not a model folder)", gotKeys, wantKeys)
	}
	for i := range wantKeys {
		if gotKeys[i] != wantKeys[i] {
			t.Fatalf("group[%d] = %q, want %q (groups must be sorted by key)", i, gotKeys[i], wantKeys[i])
		}
	}

	all := comfyGroupModelFolders(classes, true)
	if len(all) != 4 {
		t.Fatalf("--all groups = %d, want 4 (sampler_name included)", len(all))
	}

	byKey := map[string]comfyFolderGroup{}
	for _, group := range modelOnly {
		byKey[group.Key] = group
	}
	if got := byKey["ckpt_name"]; got.OptionCount != 2 || got.Status != "ok" {
		t.Errorf("ckpt_name = %d files / %q, want 2 / ok", got.OptionCount, got.Status)
	}
	empty := byKey["upscale_model_name"]
	if empty.Status != string(store.ModelClassUnregistered) {
		t.Errorf("upscale_model_name status = %q, want %q", empty.Status, store.ModelClassUnregistered)
	}
	if !strings.Contains(empty.Remedy, "extra_model_paths.yaml") {
		t.Errorf("empty group remedy = %q, want it to name extra_model_paths.yaml", empty.Remedy)
	}
	if len(empty.Files) != 0 {
		t.Errorf("empty group files = %v, want none", empty.Files)
	}
}

func TestComfyFilterFolderGroups(t *testing.T) {
	groups := comfyGroupModelFolders(comfyTestSchema(t), false)
	tests := []struct {
		name      string
		folder    string
		emptyOnly bool
		want      []string
	}{
		{name: "no filter", want: []string{"ckpt_name", "upscale_model_name", "vae_name"}},
		{name: "substring match", folder: "ckpt", want: []string{"ckpt_name"}},
		{name: "case-insensitive substring", folder: "VAE", want: []string{"vae_name"}},
		{name: "empty only", emptyOnly: true, want: []string{"upscale_model_name"}},
		{name: "no match", folder: "lora", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := comfyFilterFolderGroups(groups, tc.folder, tc.emptyOnly)
			if len(got) != len(tc.want) {
				t.Fatalf("groups = %d, want %d", len(got), len(tc.want))
			}
			for i := range tc.want {
				if got[i].Key != tc.want[i] {
					t.Fatalf("group[%d] = %q, want %q", i, got[i].Key, tc.want[i])
				}
			}
		})
	}
}

func TestComfyTypedExitCodesAreDistinct(t *testing.T) {
	if got := ExitCode(comfyClassUnregisteredErr(errNodesTest)); got != comfyExitClassUnregistered {
		t.Errorf("class-unregistered exit = %d, want %d", got, comfyExitClassUnregistered)
	}
	if got := ExitCode(comfyModelNotVisibleErr(errNodesTest)); got != comfyExitModelNotVisible {
		t.Errorf("model-not-visible exit = %d, want %d", got, comfyExitModelNotVisible)
	}
	// The whole point of a distinct code: an agent must be able to tell
	// "unregistered class" from "not found", "usage", and each other.
	distinct := map[int]string{
		comfyExitClassUnregistered:          "class-unregistered",
		comfyExitModelNotVisible:            "model-not-visible",
		ExitCode(usageErr(errNodesTest)):    "usage",
		ExitCode(notFoundErr(errNodesTest)): "not-found",
		ExitCode(apiErr(errNodesTest)):      "api",
	}
	if len(distinct) != 5 {
		t.Fatalf("exit codes collide: %v", distinct)
	}
}

var errNodesTest = errors.New("comfy nodes test sentinel")

// ---------------------------------------------------------------------------
// End-to-end: the real RunE bodies against a stand-in ComfyUI server.
// Still no live server — httptest serves the fixture — but this exercises flag
// handling, the fetch path, rendering, and the typed exit codes together.
// ---------------------------------------------------------------------------

func comfyTestHTTP(t *testing.T) *httptest.Server {
	t.Helper()
	classes := comfyTestSchema(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/object_info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(comfyTestObjectInfo))
	})
	mux.HandleFunc("/object_info/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/object_info/")
		w.Header().Set("Content-Type", "application/json")
		if raw, ok := classes[name]; ok {
			payload, _ := json.Marshal(map[string]json.RawMessage{name: raw})
			_, _ = w.Write(payload)
			return
		}
		// ComfyUI answers an unknown class with an empty object, not a 404.
		_, _ = w.Write([]byte(`{}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	t.Setenv("COMFYUI_BASE_URL", server.URL)
	return server
}

// comfyRunCmd executes one command against the stand-in server and returns its
// stdout plus the error (whose ExitCode carries the typed verdict).
func comfyRunCmd(t *testing.T, build func(*rootFlags) *cobra.Command, asJSON bool, args ...string) (string, error) {
	t.Helper()
	comfyTestHTTP(t)
	flags := &rootFlags{asJSON: asJSON, noCache: true, noLearn: true, timeout: 30 * time.Second}
	cmd := build(flags)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if args == nil {
		// Never hand cobra a nil arg slice: it falls back to os.Args[1:],
		// which under `go test` is the test binary's own flags.
		args = []string{}
	}
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	return out.String(), err
}

func comfyDecodeOut(t *testing.T, out string) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("output is not a JSON object: %v\n%s", err, out)
	}
	return payload
}

func TestComfyNodesOptionsEndToEnd(t *testing.T) {
	t.Run("v3 combo returns options and exits 0", func(t *testing.T) {
		out, err := comfyRunCmd(t, newNodesOptionsCmd, true, "VAELoader", "vae_name")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		payload := comfyDecodeOut(t, out)
		if payload["combo_shape"] != "v3" {
			t.Errorf("combo_shape = %v, want v3", payload["combo_shape"])
		}
		options, _ := payload["options"].([]any)
		if len(options) != 2 || options[0] != "vae-ft-mse-840000-ema-pruned.safetensors" {
			t.Errorf("options = %v, want the 2 VAE entries", payload["options"])
		}
		if payload["status"] != "ok" {
			t.Errorf("status = %v, want ok", payload["status"])
		}
	})

	t.Run("legacy combo is read at index 0", func(t *testing.T) {
		out, err := comfyRunCmd(t, newNodesOptionsCmd, true, "CheckpointLoaderSimple", "ckpt_name")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		payload := comfyDecodeOut(t, out)
		if payload["combo_shape"] != "legacy" {
			t.Errorf("combo_shape = %v, want legacy", payload["combo_shape"])
		}
		if payload["option_count"] != float64(2) {
			t.Errorf("option_count = %v, want 2", payload["option_count"])
		}
	})

	t.Run("empty combo exits with the distinct class-unregistered code", func(t *testing.T) {
		out, err := comfyRunCmd(t, newNodesOptionsCmd, true, "LatentUpscaleModelLoader", "upscale_model_name")
		if err == nil {
			t.Fatal("an empty COMBO must not exit 0")
		}
		if got := ExitCode(err); got != comfyExitClassUnregistered {
			t.Fatalf("exit = %d, want %d", got, comfyExitClassUnregistered)
		}
		if !strings.Contains(err.Error(), "extra_model_paths.yaml") {
			t.Errorf("error must name the real cause: %v", err)
		}
		if strings.Contains(strings.ToLower(err.Error()), "file missing") {
			t.Errorf("error must not read as a missing file: %v", err)
		}
		payload := comfyDecodeOut(t, out)
		if payload["status"] != string(store.ModelClassUnregistered) {
			t.Errorf("status = %v, want %v", payload["status"], store.ModelClassUnregistered)
		}
	})

	t.Run("non-COMBO input is a usage error", func(t *testing.T) {
		if _, err := comfyRunCmd(t, newNodesOptionsCmd, true, "KSampler", "seed"); ExitCode(err) != 2 {
			t.Fatalf("exit = %d (%v), want 2", ExitCode(err), err)
		}
	})

	t.Run("unknown input exits not-found and lists the real inputs", func(t *testing.T) {
		_, err := comfyRunCmd(t, newNodesOptionsCmd, true, "KSampler", "nope")
		if ExitCode(err) != 3 {
			t.Fatalf("exit = %d (%v), want 3", ExitCode(err), err)
		}
		if !strings.Contains(err.Error(), "sampler_name") {
			t.Errorf("error must list the available inputs: %v", err)
		}
	})

	t.Run("unknown class exits not-found", func(t *testing.T) {
		_, err := comfyRunCmd(t, newNodesOptionsCmd, true, "NoSuchNode", "whatever")
		if ExitCode(err) != 3 {
			t.Fatalf("exit = %d (%v), want 3", ExitCode(err), err)
		}
	})

	t.Run("dry-run short-circuits before any request", func(t *testing.T) {
		flags := &rootFlags{dryRun: true, noLearn: true}
		cmd := newNodesOptionsCmd(flags)
		out := &bytes.Buffer{}
		cmd.SetOut(out)
		// Explicitly empty, never nil: cobra falls back to os.Args[1:] for a
		// nil arg slice, which under `go test` is the test binary's own flags.
		cmd.SetArgs([]string{})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("dry-run must not fail: %v", err)
		}
		if !strings.Contains(out.String(), "dry-run") {
			t.Errorf("dry-run output = %q, want a dry-run report", out.String())
		}
	})

	t.Run("machine caller with no args gets a usage error, not silent success", func(t *testing.T) {
		out, err := comfyRunCmd(t, newNodesOptionsCmd, true)
		if ExitCode(err) != 2 {
			t.Fatalf("exit = %d (%v), want 2", ExitCode(err), err)
		}
		if payload := comfyDecodeOut(t, out); payload["error"] != "requires input" {
			t.Errorf("payload = %v, want a structured usage envelope", payload)
		}
	})
}

func TestComfyNodesShowEndToEnd(t *testing.T) {
	out, err := comfyRunCmd(t, newNodesShowCmd, true, "KSampler")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	payload := comfyDecodeOut(t, out)
	if payload["class_type"] != "KSampler" {
		t.Fatalf("class_type = %v, want KSampler", payload["class_type"])
	}
	inputs, _ := payload["inputs"].([]any)
	if len(inputs) != 5 {
		t.Fatalf("inputs = %d, want 5 (3 required, 1 optional, 1 hidden)", len(inputs))
	}
	first, _ := inputs[0].(map[string]any)
	if first["name"] != "seed" || first["type"] != "INT" {
		t.Errorf("first input = %v, want seed/INT in wire order", first)
	}
	outputs, _ := payload["output_types"].([]any)
	if len(outputs) != 1 || outputs[0] != "LATENT" {
		t.Errorf("output_types = %v, want [LATENT]", payload["output_types"])
	}
}

func TestComfyNodesSearchEndToEnd(t *testing.T) {
	out, err := comfyRunCmd(t, newNodesSearchCmd, true, "load", "vae")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	payload := comfyDecodeOut(t, out)
	matches, _ := payload["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("matches = %v, want exactly VAELoader", payload["matches"])
	}
	match, _ := matches[0].(map[string]any)
	if match["class_type"] != "VAELoader" {
		t.Errorf("match = %v, want VAELoader", match)
	}
	if payload["classes_searched"] != float64(4) {
		t.Errorf("classes_searched = %v, want 4", payload["classes_searched"])
	}
}

func TestComfyModelsWhyEndToEnd(t *testing.T) {
	t.Run("visible file exits 0", func(t *testing.T) {
		out, err := comfyRunCmd(t, newModelsWhyCmd, true, "sd_xl_base_1.0.safetensors")
		if err != nil {
			t.Fatalf("a visible model must exit 0: %v", err)
		}
		if payload := comfyDecodeOut(t, out); payload["verdict"] != string(store.ModelVisible) {
			t.Errorf("verdict = %v, want visible", payload["verdict"])
		}
	})

	t.Run("unregistered class exits 12 with the honest remedy", func(t *testing.T) {
		out, err := comfyRunCmd(t, newModelsWhyCmd, true, "4x-UltraSharp.safetensors")
		if got := ExitCode(err); got != comfyExitClassUnregistered {
			t.Fatalf("exit = %d (%v), want %d", got, err, comfyExitClassUnregistered)
		}
		payload := comfyDecodeOut(t, out)
		if payload["verdict"] != string(store.ModelClassUnregistered) {
			t.Fatalf("verdict = %v, want class-unregistered", payload["verdict"])
		}
		remedy, _ := payload["remedy"].(string)
		if !strings.Contains(remedy, "NOT a missing file") {
			t.Errorf("remedy = %q, want it to deny the missing-file reading", remedy)
		}
	})

	t.Run("narrowing to one loader input exits 13", func(t *testing.T) {
		_, err := comfyRunCmd(t, newModelsWhyCmd, true, "sd_xl_base_1.0.safetensors", "--class", "VAELoader", "--input", "vae_name")
		if got := ExitCode(err); got != comfyExitModelNotVisible {
			t.Fatalf("exit = %d (%v), want %d", got, err, comfyExitModelNotVisible)
		}
	})

	t.Run("a filter that matches nothing exits not-found", func(t *testing.T) {
		_, err := comfyRunCmd(t, newModelsWhyCmd, true, "any.safetensors", "--class", "KSampler", "--input", "nope")
		if got := ExitCode(err); got != 3 {
			t.Fatalf("exit = %d (%v), want 3", got, err)
		}
	})
}

func TestComfyModelsLsEndToEnd(t *testing.T) {
	t.Run("default listing omits file lists but reports counts", func(t *testing.T) {
		out, err := comfyRunCmd(t, newModelsLsCmd, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		payload := comfyDecodeOut(t, out)
		if payload["group_count"] != float64(3) {
			t.Fatalf("group_count = %v, want 3", payload["group_count"])
		}
		if payload["unregistered_group_count"] != float64(1) {
			t.Errorf("unregistered_group_count = %v, want 1", payload["unregistered_group_count"])
		}
		groups, _ := payload["groups"].([]any)
		first, _ := groups[0].(map[string]any)
		if _, hasFiles := first["files"]; hasFiles {
			t.Errorf("default listing must not inline files: %v", first)
		}
	})

	t.Run("--folder narrows and inlines the file list", func(t *testing.T) {
		out, err := comfyRunCmd(t, newModelsLsCmd, true, "--folder", "ckpt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		payload := comfyDecodeOut(t, out)
		groups, _ := payload["groups"].([]any)
		if len(groups) != 1 {
			t.Fatalf("groups = %v, want just ckpt_name", payload["groups"])
		}
		group, _ := groups[0].(map[string]any)
		files, _ := group["files"].([]any)
		if len(files) != 2 {
			t.Errorf("files = %v, want the 2 checkpoints", group["files"])
		}
	})

	t.Run("--empty finds the unregistered class", func(t *testing.T) {
		out, err := comfyRunCmd(t, newModelsLsCmd, true, "--empty")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		payload := comfyDecodeOut(t, out)
		groups, _ := payload["groups"].([]any)
		if len(groups) != 1 {
			t.Fatalf("groups = %v, want the single empty group", payload["groups"])
		}
		group, _ := groups[0].(map[string]any)
		if group["key"] != "upscale_model_name" || group["status"] != string(store.ModelClassUnregistered) {
			t.Errorf("group = %v, want upscale_model_name/class-unregistered", group)
		}
	})

	t.Run("--folder with no match exits not-found", func(t *testing.T) {
		_, err := comfyRunCmd(t, newModelsLsCmd, true, "--folder", "lora")
		if got := ExitCode(err); got != 3 {
			t.Fatalf("exit = %d (%v), want 3", got, err)
		}
	})
}

func TestComfyParentCommandsAreReadOnly(t *testing.T) {
	flags := &rootFlags{}
	for _, cmd := range []*cobra.Command{newNodesCmd(flags), newModelsCmd(flags)} {
		if cmd.Annotations["mcp:read-only"] != "true" {
			t.Errorf("%s must be annotated read-only", cmd.Name())
		}
		if len(cmd.Commands()) == 0 {
			t.Errorf("%s has no subcommands", cmd.Name())
		}
		for _, sub := range cmd.Commands() {
			if sub.Annotations["mcp:read-only"] != "true" {
				t.Errorf("%s %s must be annotated read-only", cmd.Name(), sub.Name())
			}
			// Cobra evaluates Args before RunE, which breaks the harness's
			// --dry-run probes; validation must live inside RunE instead.
			if sub.Args != nil {
				t.Errorf("%s %s must not set an Args validator", cmd.Name(), sub.Name())
			}
		}
	}
}
