// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.
//
// Tests for `validate`'s schema resolution.
//
// The defect these cover, observed in live use 2026-08-13: `validate` failed a
// good graph with combo-value-not-in-options because it read a 5h27m-old CACHED
// /object_info that predated a newly added checkpoint, and --data-source live
// did not override that cache. The live `nodes options` call was right the whole
// time; only validate's schema source was wrong.
//
// No live ComfyUI server is required — httptest stands in for the running box,
// and the "stale cache" is a seeded SQLite store in a sandboxed home.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"comfyui-pp-cli/internal/cliutil"
	"comfyui-pp-cli/internal/cliutil/testenv"
	"comfyui-pp-cli/internal/comfy/slots"
	"comfyui-pp-cli/internal/store"
)

// slotsStaleObjectInfo is the cache as it stood BEFORE the new checkpoint was
// dropped into ComfyUI's models dir: the loader exists, the file does not.
const slotsStaleObjectInfo = `{
  "CheckpointLoaderSimple": {
    "input": {"required": {"ckpt_name": [["v1-5-pruned.ckpt"], {"tooltip": "The checkpoint to load."}]}},
    "output": ["MODEL", "CLIP", "VAE"],
    "name": "CheckpointLoaderSimple",
    "category": "loaders"
  }
}`

// slotsLiveObjectInfo is what the RUNNING server answers now: same loader, one
// more option. The whole point of --data-source live is to see this list.
const slotsLiveObjectInfo = `{
  "CheckpointLoaderSimple": {
    "input": {"required": {"ckpt_name": [["v1-5-pruned.ckpt", "sd_xl_base_1.0.safetensors"], {"tooltip": "The checkpoint to load."}]}},
    "output": ["MODEL", "CLIP", "VAE"],
    "name": "CheckpointLoaderSimple",
    "category": "loaders"
  }
}`

// slotsValidateGraph names the checkpoint the server has and the cache does not.
const slotsValidateGraph = `{
  "4": {"class_type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "sd_xl_base_1.0.safetensors"}}
}`

// slotsSeedStaleCache writes the pre-checkpoint schema into a sandboxed local
// store and backdates its sync_state row, reproducing the exact condition that
// produced the false failure.
func slotsSeedStaleCache(t *testing.T, age time.Duration, recordSyncState bool) {
	t.Helper()
	testenv.Isolate(t, cliutil.DataDir)

	db, err := store.OpenWithContext(context.Background(), defaultDBPath("comfyui-pp-cli"))
	if err != nil {
		t.Fatalf("open sandboxed store: %v", err)
	}
	defer db.Close()

	if _, err := db.DB().Exec(
		`INSERT INTO resources(id, resource_type, data) VALUES (?, ?, ?)`,
		"object_info", "objectinfo", slotsStaleObjectInfo,
	); err != nil {
		t.Fatalf("seed objectinfo row: %v", err)
	}
	if recordSyncState {
		if _, err := db.DB().Exec(
			`INSERT INTO sync_state(resource_type, last_synced_at, total_count) VALUES (?, ?, ?)`,
			"objectinfo", time.Now().Add(-age), 1,
		); err != nil {
			t.Fatalf("seed sync_state: %v", err)
		}
	}
}

// slotsLiveServer stands in for the running ComfyUI box.
func slotsLiveServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/object_info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(slotsLiveObjectInfo))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	t.Setenv("COMFYUI_BASE_URL", server.URL)
	return server
}

func slotsWriteGraph(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "graph.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write graph: %v", err)
	}
	return path
}

// slotsRunValidate executes the real RunE and decodes the JSON report.
func slotsRunValidate(t *testing.T, dataSource string, args ...string) (map[string]any, error) {
	t.Helper()
	flags := &rootFlags{
		asJSON:     true,
		noCache:    true,
		noLearn:    true,
		timeout:    30 * time.Second,
		dataSource: dataSource,
	}
	cmd := newValidateCmd(flags)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()

	payload := map[string]any{}
	if body := strings.TrimSpace(out.String()); body != "" {
		if jsonErr := json.Unmarshal([]byte(body), &payload); jsonErr != nil {
			t.Fatalf("report is not a JSON object: %v\n%s", jsonErr, body)
		}
	}
	return payload, err
}

func slotsFindingKinds(t *testing.T, payload map[string]any) []string {
	t.Helper()
	raw, _ := payload["findings"].([]any)
	kinds := make([]string, 0, len(raw))
	for _, item := range raw {
		f, _ := item.(map[string]any)
		if kind, ok := f["kind"].(string); ok {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// TestValidateDataSourceHonorsLive is the (a) half: --data-source live must
// leave the machine. Before the fix every row here read the cache and the live
// row failed a graph the server accepts.
func TestValidateDataSourceHonorsLive(t *testing.T) {
	cases := []struct {
		name        string
		dataSource  string
		wantVerdict string
		wantSource  string
		wantFail    bool
	}{
		{
			name:        "live reads the running server, not the stale cache",
			dataSource:  "live",
			wantVerdict: slotsVerdictPass,
			wantSource:  "live server",
			wantFail:    false,
		},
		{
			name:        "local stays on the cache",
			dataSource:  "local",
			wantVerdict: slotsVerdictFail,
			wantSource:  "local store",
			wantFail:    true,
		},
		{
			// auto is deliberately NOT live here: validate is the offline
			// preflight and must keep working with no server. Locking that in
			// so a future edit cannot quietly add a network round trip to the
			// default path.
			name:        "auto stays on the cache",
			dataSource:  "auto",
			wantVerdict: slotsVerdictFail,
			wantSource:  "local store",
			wantFail:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slotsSeedStaleCache(t, 5*time.Hour+27*time.Minute, true)
			slotsLiveServer(t)
			graph := slotsWriteGraph(t, slotsValidateGraph)

			payload, err := slotsRunValidate(t, tc.dataSource, graph)

			if tc.wantFail && err == nil {
				t.Fatalf("expected a validation failure, got none: %v", payload)
			}
			if !tc.wantFail && err != nil {
				t.Fatalf("unexpected error: %v\n%v", err, payload)
			}
			if got := payload["verdict"]; got != tc.wantVerdict {
				t.Errorf("verdict = %v, want %v", got, tc.wantVerdict)
			}
			if got := payload["schema_source"]; got != tc.wantSource {
				t.Errorf("schema_source = %v, want %v", got, tc.wantSource)
			}
			kinds := slotsFindingKinds(t, payload)
			hasCombo := false
			for _, k := range kinds {
				if k == slots.KindComboNotInOptions {
					hasCombo = true
				}
			}
			if tc.wantFail != hasCombo {
				t.Errorf("combo-value-not-in-options present = %v, want %v (kinds=%v)", hasCombo, tc.wantFail, kinds)
			}
		})
	}
}

// TestValidateStaleCacheHint is the (b) half: when the cache is what produced
// the miss, the report has to say how old it is and name the way out. A bare
// "not among the options" is indistinguishable from a real rejection.
func TestValidateStaleCacheHint(t *testing.T) {
	cases := []struct {
		name            string
		age             time.Duration
		recordSyncState bool
		wantAgeFragment string
		wantNoAgeText   bool
	}{
		{
			name:            "backdated sync reports the age",
			age:             5*time.Hour + 27*time.Minute,
			recordSyncState: true,
			wantAgeFragment: "5h27m0s old",
		},
		{
			name:            "fresh sync still reports its age",
			age:             90 * time.Second,
			recordSyncState: true,
			wantAgeFragment: "2m0s old",
		},
		{
			// Rows can reach the resources table by write-through from a live
			// read, with no sync_state entry. That is unknown age, never fresh.
			name:            "no recorded sync reports unknown age",
			recordSyncState: false,
			wantAgeFragment: "age unknown",
			wantNoAgeText:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slotsSeedStaleCache(t, tc.age, tc.recordSyncState)
			graph := slotsWriteGraph(t, slotsValidateGraph)

			payload, err := slotsRunValidate(t, "local", graph)
			if err == nil {
				t.Fatalf("expected the stale cache to fail the graph: %v", payload)
			}

			hint, _ := payload["hint"].(string)
			if hint == "" {
				t.Fatalf("no hint on a cached combo miss; report=%v", payload)
			}
			if !strings.Contains(hint, tc.wantAgeFragment) {
				t.Errorf("hint = %q, want it to state the cache age (%q)", hint, tc.wantAgeFragment)
			}
			if !strings.Contains(hint, "sync --resources objectinfo") {
				t.Errorf("hint = %q, want it to suggest 'sync --resources objectinfo'", hint)
			}
			if !strings.Contains(hint, "--data-source live") {
				t.Errorf("hint = %q, want it to name the live escape hatch", hint)
			}

			age, _ := payload["schema_age"].(string)
			if !strings.Contains(age, tc.wantAgeFragment) {
				t.Errorf("schema_age = %q, want %q", age, tc.wantAgeFragment)
			}
			if _, hasTimestamp := payload["schema_synced_at"]; hasTimestamp == tc.wantNoAgeText {
				t.Errorf("schema_synced_at present = %v, want %v", hasTimestamp, !tc.wantNoAgeText)
			}
		})
	}
}

// TestValidateStaleHintOnlyForCacheInventableFindings keeps the hint honest. A
// dangling link is wrong in the graph itself; telling the operator to resync
// would send them chasing a schema that has nothing to do with it. And a live
// schema is the authority — a miss there is real.
func TestValidateStaleHintOnlyForCacheInventableFindings(t *testing.T) {
	const danglingGraph = `{
  "4": {"class_type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "v1-5-pruned.ckpt"}},
  "8": {"class_type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "v1-5-pruned.ckpt", "model": ["99", 0]}}
}`

	t.Run("graph-local finding gets no resync advice", func(t *testing.T) {
		slotsSeedStaleCache(t, time.Hour, true)
		graph := slotsWriteGraph(t, danglingGraph)

		payload, _ := slotsRunValidate(t, "local", graph)
		kinds := slotsFindingKinds(t, payload)
		if len(kinds) == 0 {
			t.Fatalf("fixture stopped producing a graph-local finding; report=%v", payload)
		}
		for _, k := range kinds {
			if slotsStaleSensitiveKinds[k] {
				t.Fatalf("fixture drifted: kind %q is staleness-sensitive, so this case no longer tests what it claims (kinds=%v)", k, kinds)
			}
		}
		if hint, _ := payload["hint"].(string); strings.Contains(hint, "sync --resources objectinfo") {
			t.Errorf("hint = %q, want no resync advice for a graph-local finding", hint)
		}
	})

	t.Run("live miss carries no cache age", func(t *testing.T) {
		slotsSeedStaleCache(t, time.Hour, true)
		slotsLiveServer(t)
		graph := slotsWriteGraph(t, `{"4": {"class_type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "not-on-this-server.safetensors"}}}`)

		payload, err := slotsRunValidate(t, "live", graph)
		if err == nil {
			t.Fatalf("a value absent from the LIVE schema must fail: %v", payload)
		}
		if hint, _ := payload["hint"].(string); hint != "" {
			t.Errorf("hint = %q, want none: the live server is the authority", hint)
		}
		if age, ok := payload["schema_age"]; ok {
			t.Errorf("schema_age = %v, want absent for a live read", age)
		}
	})
}

// TestValidateObjectInfoFileRefusesLive covers the contradictory-flag case the
// way every other read command does: refuse, rather than silently resolve one
// of the two and let the operator believe they checked against the server.
func TestValidateObjectInfoFileRefusesLive(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "object_info.json")
	if err := os.WriteFile(dump, []byte(slotsLiveObjectInfo), 0o600); err != nil {
		t.Fatalf("write dump: %v", err)
	}
	graph := slotsWriteGraph(t, slotsValidateGraph)

	cases := []struct {
		name       string
		dataSource string
		wantErr    bool
	}{
		{name: "explicit dump with live is refused", dataSource: "live", wantErr: true},
		{name: "explicit dump with local is fine", dataSource: "local", wantErr: false},
		{name: "explicit dump with auto is fine", dataSource: "auto", wantErr: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := slotsRunValidate(t, tc.dataSource, graph, "--object-info", dump)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected a usage error, got report %v", payload)
				}
				if got := ExitCode(err); got != 2 {
					t.Errorf("exit = %d, want 2 (usage)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := payload["schema_source"]; got != "file:"+dump {
				t.Errorf("schema_source = %v, want the dump path", got)
			}
			if age, ok := payload["schema_age"]; ok {
				t.Errorf("schema_age = %v, want absent for an explicit dump", age)
			}
		})
	}
}

// TestSlotsCacheAge pins the two age renderings the report and hint share.
func TestSlotsCacheAge(t *testing.T) {
	at := time.Now().Add(-3 * time.Hour)
	cases := []struct {
		name     string
		syncedAt *time.Time
		want     string
	}{
		{name: "recorded sync renders a duration", syncedAt: &at, want: "3h0m0s old"},
		{name: "no recorded sync is unknown, not fresh", syncedAt: nil, want: "age unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := slotsCacheAge(tc.syncedAt); !strings.Contains(got, tc.want) {
				t.Errorf("slotsCacheAge() = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}
