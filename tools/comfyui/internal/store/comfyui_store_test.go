// Tests for the ComfyUI domain layer.
//
// NOT generated — hand-written and preserved across regeneration.
//
// Every test here pins a defect that was actually observed against a live ComfyUI 0.32.0,
// not a hypothetical. Where a case encodes a real incident, the comment says which.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
)

func mustGraph(t *testing.T, s string) APIGraph {
	t.Helper()
	var g APIGraph
	if err := json.Unmarshal([]byte(s), &g); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return g
}

func TestGraphSHA_IsOrderStable(t *testing.T) {
	// Same graph, keys written in a different order. A hash that depends on map iteration
	// order would make the submit lease useless — it would never recognise a resubmit.
	a := mustGraph(t, `{
		"1": {"class_type":"KSampler","inputs":{"seed":42,"steps":20,"cfg":8.0}},
		"2": {"class_type":"SaveImage","inputs":{"images":["1",0]}}
	}`)
	b := mustGraph(t, `{
		"2": {"class_type":"SaveImage","inputs":{"images":["1",0]}},
		"1": {"class_type":"KSampler","inputs":{"cfg":8.0,"steps":20,"seed":42}}
	}`)

	ha, err := GraphSHA(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := GraphSHA(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatalf("GraphSHA is order-dependent: %s != %s", ha, hb)
	}
}

func TestGraphSHA_DiffersOnRealChange(t *testing.T) {
	a := mustGraph(t, `{"1":{"class_type":"KSampler","inputs":{"steps":20}}}`)
	b := mustGraph(t, `{"1":{"class_type":"KSampler","inputs":{"steps":30}}}`)
	ha, _ := GraphSHA(a)
	hb, _ := GraphSHA(b)
	if ha == hb {
		t.Fatal("GraphSHA collided across different step counts")
	}
}

func TestShapeSHA_StripsVolatileInputsOnly(t *testing.T) {
	tests := []struct {
		name      string
		a, b      string
		wantEqual bool
	}{
		{
			// Two runs differing only by seed did the SAME work. They must share a shape or
			// the regression guard has a sample size of one forever.
			name:      "seed differs -> same shape",
			a:         `{"1":{"class_type":"KSampler","inputs":{"seed":1,"steps":20}}}`,
			b:         `{"1":{"class_type":"KSampler","inputs":{"seed":999,"steps":20}}}`,
			wantEqual: true,
		},
		{
			name:      "noise_seed differs -> same shape",
			a:         `{"1":{"class_type":"RandomNoise","inputs":{"noise_seed":42}}}`,
			b:         `{"1":{"class_type":"RandomNoise","inputs":{"noise_seed":7}}}`,
			wantEqual: true,
		},
		{
			name:      "filename_prefix differs -> same shape",
			a:         `{"1":{"class_type":"SaveVideo","inputs":{"filename_prefix":"video/a"}}}`,
			b:         `{"1":{"class_type":"SaveVideo","inputs":{"filename_prefix":"video/b"}}}`,
			wantEqual: true,
		},
		{
			// steps changes the work. Grouping these would compare unlike runs and
			// manufacture a fake regression.
			name:      "steps differs -> different shape",
			a:         `{"1":{"class_type":"KSampler","inputs":{"seed":1,"steps":20}}}`,
			b:         `{"1":{"class_type":"KSampler","inputs":{"seed":1,"steps":40}}}`,
			wantEqual: false,
		},
		{
			// The real Wan case: virtual_vram_gb 7 vs 10 decided OOM vs success.
			name:      "virtual_vram_gb differs -> different shape",
			a:         `{"1":{"class_type":"UnetLoaderGGUFDisTorch2MultiGPU","inputs":{"virtual_vram_gb":7.0}}}`,
			b:         `{"1":{"class_type":"UnetLoaderGGUFDisTorch2MultiGPU","inputs":{"virtual_vram_gb":10.0}}}`,
			wantEqual: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ha, err := ShapeSHA(mustGraph(t, tc.a))
			if err != nil {
				t.Fatal(err)
			}
			hb, err := ShapeSHA(mustGraph(t, tc.b))
			if err != nil {
				t.Fatal(err)
			}
			if (ha == hb) != tc.wantEqual {
				t.Fatalf("ShapeSHA equal=%v, want %v (%s vs %s)", ha == hb, tc.wantEqual, ha[:12], hb[:12])
			}
		})
	}
}

func TestParseComboOptions_BothSpecShapes(t *testing.T) {
	// ComfyUI ships BOTH shapes simultaneously. On this box 480 of 880 inputs are v3 and
	// 400 are legacy, so a reader that handles only one silently mis-reads ~42% of inputs —
	// which then surfaces to the user as a false "missing model".
	tests := []struct {
		name      string
		spec      string
		wantOpts  int
		wantShape ComboShape
	}{
		{
			name:      "v3 shape: options at index 1",
			spec:      `[["COMBO"],{"options":["a.safetensors","b.safetensors"]}]`,
			wantOpts:  2,
			wantShape: ComboV3,
		},
		{
			name:      "legacy shape: options at index 0",
			spec:      `[["a.safetensors","b.safetensors","c.safetensors"],{}]`,
			wantOpts:  3,
			wantShape: ComboLegacy,
		},
		{
			name:      "v3 with EMPTY options -> class unregistered, not missing file",
			spec:      `[["COMBO"],{"options":[]}]`,
			wantOpts:  0,
			wantShape: ComboV3,
		},
		{
			name:      "legacy with empty options",
			spec:      `[[],{}]`,
			wantOpts:  0,
			wantShape: ComboLegacy,
		},
		{
			name:      "not a combo at all",
			spec:      `["STRING",{"multiline":true}]`,
			wantOpts:  0,
			wantShape: ComboNone,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var spec interface{}
			if err := json.Unmarshal([]byte(tc.spec), &spec); err != nil {
				t.Fatal(err)
			}
			opts, shape := ParseComboOptions(spec)
			if shape != tc.wantShape {
				t.Fatalf("shape = %q, want %q", shape, tc.wantShape)
			}
			if len(opts) != tc.wantOpts {
				t.Fatalf("options = %d, want %d (%v)", len(opts), tc.wantOpts, opts)
			}
		})
	}
}

func TestClassifyModelVisibility_SeparatesTheFourCauses(t *testing.T) {
	// The real incident: latent_upscale_models had no extra_model_paths.yaml key, so the
	// loader offered ZERO options and ComfyUI reported only `not in []`. Every other tool
	// reads that as "file missing" and sends you hunting for a download you already have.
	tests := []struct {
		name string
		spec string
		file string
		want ModelVisibility
	}{
		{
			name: "present and offered",
			spec: `[["COMBO"],{"options":["ltx.safetensors","wan.safetensors"]}]`,
			file: "ltx.safetensors",
			want: ModelVisible,
		},
		{
			name: "EMPTY options -> model CLASS unregistered",
			spec: `[["COMBO"],{"options":[]}]`,
			file: "ltx-2.5-latent-spatial-upscaler.safetensors",
			want: ModelClassUnregistered,
		},
		{
			name: "options exist but this file is not among them",
			spec: `[["COMBO"],{"options":["wan.safetensors"]}]`,
			file: "ltx.safetensors",
			want: ModelNotListed,
		},
		{
			name: "input is not a COMBO",
			spec: `["INT",{"default":20}]`,
			file: "anything",
			want: ModelNoSuchInput,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var spec interface{}
			if err := json.Unmarshal([]byte(tc.spec), &spec); err != nil {
				t.Fatal(err)
			}
			got, _ := ClassifyModelVisibility(spec, tc.file)
			if got != tc.want {
				t.Fatalf("visibility = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNormaliseEpochMS(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want int64
	}{
		{"epoch seconds", 1786556884, 1786556884000},
		{"epoch milliseconds", 1786556884894, 1786556884894},
		{"fractional seconds", 1786556884.5, 1786556884500},
		{"zero", 0, 0},
		{"negative is rejected", -5, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormaliseEpochMS(tc.in); got != tc.want {
				t.Fatalf("NormaliseEpochMS(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// openTestDB gives each test its own migrated database.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Skipf("sqlite driver unavailable in this build: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := MigrateComfyUI(context.Background(), db); err != nil {
		t.Fatalf("MigrateComfyUI: %v", err)
	}
	return db
}

func TestMigrateComfyUI_IsIdempotent(t *testing.T) {
	db := openTestDB(t)
	// Running it again must not error — it runs on every open.
	if err := MigrateComfyUI(context.Background(), db); err != nil {
		t.Fatalf("second MigrateComfyUI: %v", err)
	}
}

func TestSetRunTiming_RefusesInvertedTimestamps(t *testing.T) {
	// A silently wrong duration is worse than a missing one: that is exactly how a false
	// "+49% regression" got reported. Refuse rather than store a negative.
	db := openTestDB(t)
	ctx := context.Background()
	if err := InsertRun(ctx, db, RunRow{PromptID: "p1", GraphSHA: "g1", ShapeSHA: "s1"}); err != nil {
		t.Fatal(err)
	}
	if err := SetRunTiming(ctx, db, "p1", 2000, 1000); err == nil {
		t.Fatal("expected refusal when success precedes start")
	}
	if err := SetRunTiming(ctx, db, "p1", 1000, 2000); err != nil {
		t.Fatalf("valid timing rejected: %v", err)
	}
	var d sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT duration_ms FROM run WHERE prompt_id='p1'`).Scan(&d); err != nil {
		t.Fatal(err)
	}
	if !d.Valid || d.Int64 != 1000 {
		t.Fatalf("duration_ms = %v, want 1000", d)
	}
}

func TestFindActiveRunByGraphSHA_BacksTheIdempotentLease(t *testing.T) {
	// This is the structural fix for the resubmit that burned ~30 GPU-minutes.
	db := openTestDB(t)
	ctx := context.Background()

	if _, found, err := FindActiveRunByGraphSHA(ctx, db, "deadbeef"); err != nil || found {
		t.Fatalf("empty store should report no active run (found=%v err=%v)", found, err)
	}

	if err := InsertRun(ctx, db, RunRow{PromptID: "p1", GraphSHA: "deadbeef", State: "running"}); err != nil {
		t.Fatal(err)
	}
	id, found, err := FindActiveRunByGraphSHA(ctx, db, "deadbeef")
	if err != nil || !found || id != "p1" {
		t.Fatalf("expected to attach to p1, got id=%q found=%v err=%v", id, found, err)
	}

	// Once terminal, a resubmit is a legitimately new render and must NOT attach.
	if err := SetRunState(ctx, db, "p1", "completed", "ok"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := FindActiveRunByGraphSHA(ctx, db, "deadbeef"); found {
		t.Fatal("a completed run must not be attached to")
	}
}

func TestShapeStatsFor_UsesSampleStdDev(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// Three completed runs of one shape: 1000, 2000, 3000 ms. Mean 2000, sample sd 1000.
	for i, d := range []int64{1000, 2000, 3000} {
		id := string(rune('a' + i))
		if err := InsertRun(ctx, db, RunRow{PromptID: id, GraphSHA: "g", ShapeSHA: "shape1"}); err != nil {
			t.Fatal(err)
		}
		if err := SetRunTiming(ctx, db, id, 0+1, d+1); err != nil {
			t.Fatal(err)
		}
		if err := SetRunState(ctx, db, id, "completed", "ok"); err != nil {
			t.Fatal(err)
		}
	}
	st, err := ShapeStatsFor(ctx, db, "shape1")
	if err != nil {
		t.Fatal(err)
	}
	if st.N != 3 {
		t.Fatalf("N = %d, want 3", st.N)
	}
	if st.MeanMS != 2000 {
		t.Fatalf("MeanMS = %v, want 2000", st.MeanMS)
	}
	if st.StdDevMS < 999 || st.StdDevMS > 1001 {
		t.Fatalf("StdDevMS = %v, want ~1000 (sample sd, n-1)", st.StdDevMS)
	}
	if st.MinMS != 1000 || st.MaxMS != 3000 {
		t.Fatalf("min/max = %d/%d, want 1000/3000", st.MinMS, st.MaxMS)
	}
}

func TestUpsertGraph_IsContentAddressedAndIdempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	g := mustGraph(t, `{"1":{"class_type":"KSampler","inputs":{"seed":1,"steps":20}}}`)

	sha1, err := UpsertGraph(ctx, db, g, "tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	sha2, err := UpsertGraph(ctx, db, g, "tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sha1 != sha2 {
		t.Fatalf("same graph hashed differently: %s vs %s", sha1, sha2)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM graph`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("graph rows = %d, want 1 (upsert must not duplicate)", n)
	}
}

func TestClassHistogramAndSortedClassTypes(t *testing.T) {
	g := mustGraph(t, `{
		"1":{"class_type":"KSampler","inputs":{}},
		"2":{"class_type":"KSampler","inputs":{}},
		"3":{"class_type":"SaveImage","inputs":{}}
	}`)
	h := ClassHistogram(g)
	if h["KSampler"] != 2 || h["SaveImage"] != 1 {
		t.Fatalf("histogram = %v", h)
	}
	got := SortedClassTypes(g)
	if len(got) != 2 || got[0] != "KSampler" || got[1] != "SaveImage" {
		t.Fatalf("SortedClassTypes = %v, want [KSampler SaveImage]", got)
	}
}
