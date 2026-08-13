// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
//
// Tests for the commands added in the perfection wave: features (capability
// drift), deps (node-pack resolution), and node-set identity capture.
//
// No live ComfyUI server is required — httptest stands in for the running box
// and fixture schemas stand in for /object_info. That matters beyond
// convenience here: `free`, `history clear/delete` and `upload mask` all
// MUTATE a real server, so a test that needed a live box would be a test
// nobody could safely run.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"comfyui-pp-cli/internal/comfy/slots"
	"comfyui-pp-cli/internal/store"
	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// features
// ---------------------------------------------------------------------------

// comfyFeaturesPayload is the map ComfyUI 0.32.0 serves, taken from the
// server's own _CORE_FEATURE_FLAGS.
const comfyFeaturesPayload = `{
  "supports_preview_metadata": true,
  "supports_model_type_tags": true,
  "max_upload_size": 104857600,
  "extension": {"manager": {"supports_v4": true}},
  "node_replacements": true,
  "assets": false
}`

func TestComfyBuildFeaturesResult(t *testing.T) {
	tests := []struct {
		name        string
		payload     string
		wantVerdict string
		wantAdded   []string
		wantMissing []string
	}{
		{
			name:        "pinned server matches",
			payload:     comfyFeaturesPayload,
			wantVerdict: "MATCH",
		},
		{
			name: "a newer server adding a key is additive, not drift",
			payload: `{"supports_preview_metadata":true,"supports_model_type_tags":true,"max_upload_size":1,
			           "extension":{},"node_replacements":true,"assets":true,"progress_state":true}`,
			wantVerdict: "NEWER",
			wantAdded:   []string{"progress_state"},
		},
		{
			name: "a dropped key is real drift",
			payload: `{"supports_preview_metadata":true,"max_upload_size":1,
			           "extension":{},"node_replacements":true,"assets":true}`,
			wantVerdict: "DRIFT",
			wantMissing: []string{"supports_model_type_tags"},
		},
		{
			// Values are deployment state (max_upload_size follows a CLI arg),
			// so a different value must NOT be reported as API drift.
			name: "different values are not drift",
			payload: `{"supports_preview_metadata":false,"supports_model_type_tags":false,"max_upload_size":999,
			           "extension":{},"node_replacements":false,"assets":true}`,
			wantVerdict: "MATCH",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := comfyBuildFeaturesResult(json.RawMessage(tc.payload))
			if err != nil {
				t.Fatalf("comfyBuildFeaturesResult: %v", err)
			}
			if got.Verdict != tc.wantVerdict {
				t.Errorf("verdict = %q, want %q (added=%v missing=%v)", got.Verdict, tc.wantVerdict, got.Added, got.Missing)
			}
			if !equalStrings(got.Added, tc.wantAdded) {
				t.Errorf("added = %v, want %v", got.Added, tc.wantAdded)
			}
			if !equalStrings(got.Missing, tc.wantMissing) {
				t.Errorf("missing = %v, want %v", got.Missing, tc.wantMissing)
			}
			if got.PinnedAPIVersion != comfyPinnedAPIVersion {
				t.Errorf("pinned_api_version = %q, want %q", got.PinnedAPIVersion, comfyPinnedAPIVersion)
			}
		})
	}
}

func TestComfyFeaturesCommandAgainstFakeServer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/features", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(comfyFeaturesPayload))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	t.Setenv("COMFYUI_BASE_URL", server.URL)

	flags := &rootFlags{asJSON: true, noCache: true, noLearn: true, timeout: 30 * time.Second}
	cmd := newComfyFeaturesCmd(flags)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err != nil {
		t.Fatalf("features: %v", err)
	}
	var got comfyFeaturesResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (out=%s)", err, out.String())
	}
	if got.Verdict != "MATCH" {
		t.Errorf("verdict = %q, want MATCH", got.Verdict)
	}
	if len(got.Features) != 6 {
		t.Errorf("features carried %d keys, want 6: %v", len(got.Features), got.Features)
	}
}

// TestComfyFeaturesIsReadOnly guards the annotation: features must stay safe
// for an agent to call unprompted, and `free` must never become so.
func TestComfyFeaturesIsReadOnly(t *testing.T) {
	flags := &rootFlags{}
	if got := newComfyFeaturesCmd(flags).Annotations["mcp:read-only"]; got != "true" {
		t.Errorf("features mcp:read-only = %q, want \"true\"", got)
	}
	for name, cmd := range map[string]string{
		"free":           newComfyFreeCmd(flags).Annotations["mcp:read-only"],
		"history clear":  newComfyHistoryClearCmd(flags).Annotations["mcp:read-only"],
		"history delete": newComfyHistoryDeleteCmd(flags).Annotations["mcp:read-only"],
		"upload mask":    newComfyUploadMaskCmd(flags).Annotations["mcp:read-only"],
	} {
		if cmd != "" {
			t.Errorf("%s carries mcp:read-only=%q — it mutates the server and must never be annotated safe", name, cmd)
		}
	}
}

// TestSideEffectingCommandsPrintByDefault is the guard that matters most on a
// shared-GPU box: none of these may touch the server without --execute.
func TestSideEffectingCommandsPrintByDefault(t *testing.T) {
	// A server that fails the test if it is ever contacted.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server was contacted without --execute: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	t.Setenv("COMFYUI_BASE_URL", server.URL)

	cases := []struct {
		name string
		make func(*rootFlags) *cobra.Command
		args []string
	}{
		{"free", func(f *rootFlags) *cobra.Command { return newComfyFreeCmd(f) }, nil},
		{"history clear", func(f *rootFlags) *cobra.Command { return newComfyHistoryClearCmd(f) }, nil},
		{"history delete", func(f *rootFlags) *cobra.Command { return newComfyHistoryDeleteCmd(f) }, []string{"abc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flags := &rootFlags{asJSON: true, noCache: true, noLearn: true, timeout: 5 * time.Second}
			cmd := tc.make(flags)
			out := &bytes.Buffer{}
			cmd.SetOut(out)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tc.args)
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			if err := cmd.Execute(); err != nil {
				t.Fatalf("%s without --execute returned an error: %v", tc.name, err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
				t.Fatalf("decode: %v (out=%s)", err, out.String())
			}
			if executed, _ := decoded["executed"].(bool); executed {
				t.Errorf("%s reported executed=true without --execute", tc.name)
			}
			if action, _ := decoded["action"].(string); !strings.HasPrefix(action, "would-") {
				t.Errorf("%s action = %q, want a would-* action", tc.name, action)
			}
		})
	}
}

// TestUploadMaskRequiresOriginal pins the trap: /upload/mask 500s server-side
// without original_ref, so the CLI must refuse first with a sentence.
func TestUploadMaskRequiresOriginal(t *testing.T) {
	flags := &rootFlags{asJSON: true, noLearn: true}
	cmd := newComfyUploadMaskCmd(flags)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"mask.png"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	if err == nil {
		t.Fatal("upload mask without --original should fail")
	}
	if got := ExitCode(err); got != ExitUsage {
		t.Errorf("exit code = %d, want %d", got, ExitUsage)
	}
	if !strings.Contains(err.Error(), "--original is required") {
		t.Errorf("error should name the missing flag; got %q", err.Error())
	}
}

// ---------------------------------------------------------------------------
// deps
// ---------------------------------------------------------------------------

func depsSchema() slots.Schema {
	return slots.Schema{
		"KSampler":                {ClassType: "KSampler", PythonModule: "nodes"},
		"CheckpointLoaderSimple":  {ClassType: "CheckpointLoaderSimple", PythonModule: "nodes"},
		"SaveAnimatedWEBP":        {ClassType: "SaveAnimatedWEBP", PythonModule: "comfy_extras.nodes_video"},
		"ImpactWildcardProcessor": {ClassType: "ImpactWildcardProcessor", PythonModule: "custom_nodes.ComfyUI-Impact-Pack"},
	}
}

func depsGraph(classes ...string) store.APIGraph {
	g := store.APIGraph{}
	for i, c := range classes {
		g[string(rune('1'+i))] = store.APINode{ClassType: c}
	}
	return g
}

func TestComfyBuildDepsReport(t *testing.T) {
	resolved := slotsObjectInfo{schema: depsSchema(), source: "test fixture"}

	t.Run("core only graph is portable", func(t *testing.T) {
		got := comfyBuildDepsReport("g.json", depsGraph("KSampler", "CheckpointLoaderSimple"), resolved, nil)
		if got.Verdict != comfyDepsVerdictCore {
			t.Errorf("verdict = %q, want %q", got.Verdict, comfyDepsVerdictCore)
		}
		if !got.Portable {
			t.Error("a core-only graph must be portable")
		}
		if len(got.CustomPackages) != 0 {
			t.Errorf("custom_packages = %v, want none", got.CustomPackages)
		}
	})

	t.Run("bundled extras do not make a graph unportable", func(t *testing.T) {
		got := comfyBuildDepsReport("g.json", depsGraph("KSampler", "SaveAnimatedWEBP"), resolved, nil)
		if got.Verdict != comfyDepsVerdictCore {
			t.Errorf("verdict = %q, want %q — comfy_extras ships WITH ComfyUI", got.Verdict, comfyDepsVerdictCore)
		}
		if !got.Portable {
			t.Error("a graph using only core + bundled extras must be portable")
		}
	})

	t.Run("custom pack is named as an installable", func(t *testing.T) {
		got := comfyBuildDepsReport("g.json", depsGraph("KSampler", "ImpactWildcardProcessor"), resolved, nil)
		if got.Verdict != comfyDepsVerdictCustom {
			t.Errorf("verdict = %q, want %q", got.Verdict, comfyDepsVerdictCustom)
		}
		if !equalStrings(got.CustomPackages, []string{"ComfyUI-Impact-Pack"}) {
			t.Errorf("custom_packages = %v, want [ComfyUI-Impact-Pack]", got.CustomPackages)
		}
		if got.Portable {
			t.Error("a graph needing a custom pack is not portable")
		}
	})

	t.Run("unknown class is reported missing with its nodes", func(t *testing.T) {
		got := comfyBuildDepsReport("g.json", depsGraph("KSampler", "SomeNodeNobodyHas"), resolved, nil)
		if got.Verdict != comfyDepsVerdictMissing {
			t.Errorf("verdict = %q, want %q", got.Verdict, comfyDepsVerdictMissing)
		}
		if len(got.Missing) != 1 || got.Missing[0].ClassType != "SomeNodeNobodyHas" {
			t.Fatalf("missing = %+v, want the one unknown class", got.Missing)
		}
		if len(got.Missing[0].NodeIDs) != 1 {
			t.Errorf("missing class must point at the nodes using it; got %v", got.Missing[0].NodeIDs)
		}
	})

	t.Run("pack hint from the workflow is attached to a missing class", func(t *testing.T) {
		hints := map[string]comfyPackHint{
			"SomeNodeNobodyHas": {Pack: "comfyui-impact-pack", Source: "workflow.cnr_id"},
		}
		got := comfyBuildDepsReport("g.json", depsGraph("SomeNodeNobodyHas"), resolved, hints)
		if len(got.Missing) != 1 {
			t.Fatalf("missing = %+v", got.Missing)
		}
		if got.Missing[0].PackHint != "comfyui-impact-pack" {
			t.Errorf("pack_hint = %q, want the workflow's cnr_id", got.Missing[0].PackHint)
		}
		if got.Missing[0].HintSource != "workflow.cnr_id" {
			t.Errorf("hint_source = %q, want workflow.cnr_id", got.Missing[0].HintSource)
		}
	})

	t.Run("no schema reports UNVERIFIED rather than everything missing", func(t *testing.T) {
		// The important negative: with no schema, calling every class missing
		// would be a fabricated verdict on a perfectly good graph.
		empty := slotsObjectInfo{schema: slots.Schema{}, source: "none"}
		got := comfyBuildDepsReport("g.json", depsGraph("KSampler"), empty, nil)
		if got.Verdict != comfyDepsVerdictUnverified {
			t.Errorf("verdict = %q, want %q", got.Verdict, comfyDepsVerdictUnverified)
		}
		if len(got.Missing) != 0 {
			t.Errorf("missing = %v, want none when nothing could be checked", got.Missing)
		}
		if got.Hint == "" {
			t.Error("UNVERIFIED must explain how to get a real verdict")
		}
	})
}

func TestComfyClassifyModule(t *testing.T) {
	tests := []struct {
		module   string
		wantPack string
		wantKind string
	}{
		{"nodes", "core", comfyDepKindCore},
		{"comfy_extras.nodes_video", "nodes_video", comfyDepKindExtra},
		{"custom_nodes.ComfyUI-Impact-Pack", "ComfyUI-Impact-Pack", comfyDepKindCustom},
		{"", "unknown", comfyDepKindUnknown},
		{"some_registered_module", "some_registered_module", comfyDepKindCustom},
	}
	for _, tc := range tests {
		t.Run(tc.module, func(t *testing.T) {
			pack, kind := comfyClassifyModule(tc.module)
			if pack != tc.wantPack || kind != tc.wantKind {
				t.Errorf("comfyClassifyModule(%q) = (%q, %q), want (%q, %q)", tc.module, pack, kind, tc.wantPack, tc.wantKind)
			}
		})
	}
}

func TestComfyExtractPackHints(t *testing.T) {
	uiWorkflow := `{"nodes":[
	  {"type":"ImpactWildcardProcessor","properties":{"cnr_id":"comfyui-impact-pack","ver":"1.2.3"}},
	  {"type":"SomeGitNode","properties":{"aux_id":"owner/some-repo"}},
	  {"type":"KSampler","properties":{"Node name for S&R":"KSampler"}}
	]}`
	hints := comfyExtractPackHints([]byte(uiWorkflow))
	if hints["ImpactWildcardProcessor"].Pack != "comfyui-impact-pack" {
		t.Errorf("cnr_id hint = %+v", hints["ImpactWildcardProcessor"])
	}
	if hints["SomeGitNode"].Pack != "owner/some-repo" {
		t.Errorf("aux_id fallback = %+v", hints["SomeGitNode"])
	}
	if _, ok := hints["KSampler"]; ok {
		t.Error("a node with no pack property must yield no hint rather than a guess")
	}

	// An API-format graph carries no such properties. Returning empty is
	// correct; panicking or erroring on the format this CLI actually submits
	// would not be.
	apiGraph := `{"3":{"class_type":"KSampler","inputs":{}}}`
	if got := comfyExtractPackHints([]byte(apiGraph)); len(got) != 0 {
		t.Errorf("API-format graph yielded hints %v, want none", got)
	}
	if got := comfyExtractPackHints([]byte("not json at all")); len(got) != 0 {
		t.Errorf("garbage input yielded hints %v, want none", got)
	}
}

// ---------------------------------------------------------------------------
// node-set identity
// ---------------------------------------------------------------------------

func TestComfyBuildNodeSetIdentity(t *testing.T) {
	base := comfyBuildNodeSetIdentity(depsSchema(), "0.32.0", "cache")

	t.Run("is stable across calls", func(t *testing.T) {
		again := comfyBuildNodeSetIdentity(depsSchema(), "0.32.0", "cache")
		if base.ID != again.ID {
			t.Errorf("id is not stable: %q vs %q", base.ID, again.ID)
		}
		if base.ClassDigest != again.ClassDigest {
			t.Errorf("class digest is not stable: %q vs %q", base.ClassDigest, again.ClassDigest)
		}
	})

	t.Run("counts packs and classes", func(t *testing.T) {
		if base.ClassCount != 4 {
			t.Errorf("class_count = %d, want 4", base.ClassCount)
		}
		// core, nodes_video (extra), ComfyUI-Impact-Pack (custom)
		if base.PackCount != 3 {
			t.Errorf("pack_count = %d, want 3: %+v", base.PackCount, base.Packs)
		}
	})

	t.Run("a changed node set changes the id", func(t *testing.T) {
		// This is the whole point: a custom pack removed between two runs must
		// be visible, because it silently changes what a graph means.
		reduced := depsSchema()
		delete(reduced, "ImpactWildcardProcessor")
		got := comfyBuildNodeSetIdentity(reduced, "0.32.0", "cache")
		if got.ID == base.ID {
			t.Error("removing a custom pack must change the node-set id")
		}
		if got.ClassDigest == base.ClassDigest {
			t.Error("removing a class must change the class digest")
		}
	})

	t.Run("a changed ComfyUI version changes the id", func(t *testing.T) {
		got := comfyBuildNodeSetIdentity(depsSchema(), "0.33.0", "cache")
		if got.ID == base.ID {
			t.Error("the same class list on a different ComfyUI build is a different environment")
		}
		if got.ClassDigest != base.ClassDigest {
			t.Error("the class digest covers classes only and must not move with the version")
		}
	})

	t.Run("an empty schema gets no id", func(t *testing.T) {
		got := comfyBuildNodeSetIdentity(slots.Schema{}, "0.32.0", "none")
		if got.ID != "" {
			t.Errorf("id = %q, want empty — 'we could not look' must not hash to a shared id", got.ID)
		}
	})

	t.Run("source is always recorded", func(t *testing.T) {
		if base.Source == "" {
			t.Error("an unlabelled digest invites mistaking a cached fingerprint for a live one")
		}
	})
}

// equalStrings compares two string slices, treating nil and empty as equal.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
