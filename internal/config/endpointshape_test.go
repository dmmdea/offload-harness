package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeShapeCfg writes one config file into a temp dir and returns its path.
func writeShapeCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLoadRefusesDeadEndpointPorts (register S-38) pins the REFUSAL half: a
// configured HTTP base whose port can never answer fails the load by NAME.
//
// Port 9 is IANA discard — the shape of an endpoint whose value was never
// substituted — and port 0 means "any free port", which nothing serves on. The
// ledger carried 8 dispatches to http://127.0.0.1:9/v1/v1/chat/completions that
// each cost a dial timeout before anyone could see the port at all. The CLASS,
// not that row, is what is guarded here.
//
// A loopback endpoint on an unusual port is NOT refused: INV-10 sanctions a
// loopback-only bench twin beside the production seat, and refusing it would
// break measurement on the delegator box.
func TestLoadRefusesDeadEndpointPorts(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		wantErr []string // substrings the Load error must contain; empty = must load cleanly
	}{
		{
			name:    "delegate_remotes at the discard port",
			json:    `{"delegate_remotes":["http://node-a:9"]}`,
			wantErr: []string{"delegate_remotes", ":9"},
		},
		{
			name:    "endpoint at the discard port",
			json:    `{"endpoint":"http://127.0.0.1:9"}`,
			wantErr: []string{"endpoint", ":9"},
		},
		{
			name:    "endpoint at port 0",
			json:    `{"endpoint":"http://127.0.0.1:0"}`,
			wantErr: []string{"endpoint", ":0"},
		},
		{
			name:    "cascade lane at the discard port",
			json:    `{"cascade_remote_lanes":{"offload-e4b":"http://node-a:9"}}`,
			wantErr: []string{"cascade_remote_lanes", "offload-e4b", ":9"},
		},
		{
			name:    "seat endpoint at the discard port",
			json:    `{"seat_endpoints":{"offload-e4b":"http://node-a:9"}}`,
			wantErr: []string{"seat_endpoints", "offload-e4b", ":9"},
		},
		{
			name: "a loopback bench twin on 18798 loads (INV-10)",
			json: `{"endpoint":"http://127.0.0.1:18798"}`,
		},
		{
			name: "a fleet remote on the fleet port loads",
			json: `{"delegate_remotes":["http://node-a:18811"]}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeShapeCfg(t, c.json))
			if len(c.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Load must succeed, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load must refuse %s", c.json)
			}
			for _, want := range c.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load error %q must name %q", err, want)
				}
			}
		})
	}
}

// TestEndpointWarningsFleetRemoteShape (register S-38, WARN half) pins the
// shapes that load but are almost certainly wrong. They warn rather than refuse
// for one release: a strict validator that refuses a WORKING odd config is a
// worse failure than a dial timeout, so the operator gets a named doctor row
// first and the refusal can follow once the fleet is clean.
func TestEndpointWarningsFleetRemoteShape(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want []string // substrings at least one warning must contain
		none bool     // no warning at all
	}{
		{
			name: "a fleet remote on a retired port warns and names the base",
			cfg:  Config{DelegateRemotes: []string{"http://node-a:18798"}},
			want: []string{"delegate_remotes", "http://node-a:18798", "18811"},
		},
		{
			name: "a loopback fleet remote warns: a remote cannot be this box",
			cfg:  Config{DelegateRemotes: []string{"http://127.0.0.1:18811"}},
			want: []string{"delegate_remotes", "http://127.0.0.1:18811", "loopback"},
		},
		{
			name: "a /v1-suffixed fleet remote warns",
			cfg:  Config{DelegateRemotes: []string{"http://node-a:18811/v1"}},
			want: []string{"delegate_remotes", "/v1"},
		},
		{
			name: "a /v1-suffixed cascade lane warns",
			cfg:  Config{Endpoint: "http://127.0.0.1:11436", CascadeRemoteLanes: map[string]string{"offload-e4b": "http://node-a:11436/v1"}},
			want: []string{"cascade_remote_lanes", "offload-e4b", "/v1"},
		},
		{
			name: "a cascade lane on neither serving port warns",
			cfg:  Config{Endpoint: "http://127.0.0.1:11436", CascadeRemoteLanes: map[string]string{"offload-e4b": "http://node-a:18798"}},
			want: []string{"cascade_remote_lanes", "http://node-a:18798"},
		},
		{
			name: "a fleet remote on the fleet port is silent",
			cfg:  Config{DelegateRemotes: []string{"http://node-a:18811"}},
			none: true,
		},
		{
			name: "a cascade lane on this box's own serving port is silent",
			cfg:  Config{Endpoint: "http://127.0.0.1:11436", CascadeRemoteLanes: map[string]string{"offload-e4b": "http://node-c:11436"}},
			none: true,
		},
		{
			name: "a cascade lane pointed at a fleet node is silent",
			cfg:  Config{Endpoint: "http://127.0.0.1:11436", CascadeRemoteLanes: map[string]string{"offload-e4b": "http://node-c:18811"}},
			none: true,
		},
		{
			name: "the default config is silent",
			cfg:  Default(),
			none: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EndpointWarnings(c.cfg)
			if c.none {
				if len(got) != 0 {
					t.Fatalf("want no warning, got %q", got)
				}
				return
			}
			joined := strings.Join(got, "\n")
			for _, want := range c.want {
				if !strings.Contains(joined, want) {
					t.Errorf("warnings %q must name %q", joined, want)
				}
			}
		})
	}
}

// TestLoadCarriesWarningsNotErrors (register S-38): the WARN class must not fail
// the load this release, and the warning must be reachable FROM the loaded value
// so doctor reads it rather than re-deriving it.
func TestLoadCarriesWarningsNotErrors(t *testing.T) {
	cfg, err := Load(writeShapeCfg(t, `{"delegate_remotes":["http://node-a:18798"]}`))
	if err != nil {
		t.Fatalf("a non-fleet-port remote must LOAD (warn-then-fail over one release): %v", err)
	}
	joined := strings.Join(cfg.Findings(), "\n")
	if !strings.Contains(joined, "http://node-a:18798") {
		t.Fatalf("Findings() must carry the warning naming the base; got %q", joined)
	}
}

// TestFindingsGPUWaitAgainstVisionWait (register S-42, C-33): the media lane's
// GPU wait and the vision lane's are two knobs for one card. gpu_wait_ms 600000
// against a 90 s vision wait is a 10-minute block on a media call that the key's
// own documentation sizes at 90 s — a live config carried exactly that. doctor
// names it; nothing else in the harness ever did.
func TestFindingsGPUWaitAgainstVisionWait(t *testing.T) {
	cfg := Default()
	cfg.GPUWaitMs = 600000
	cfg.VisionGPUWaitSec = 90
	joined := strings.Join(cfg.Findings(), "\n")
	for _, want := range []string{"gpu_wait_ms", "600000", "vision_gpu_wait_sec", "90"} {
		if !strings.Contains(joined, want) {
			t.Errorf("findings %q must name %q", joined, want)
		}
	}
	if f := Default().Findings(); len(f) != 0 {
		t.Fatalf("the default config must produce no finding; got %q", f)
	}
	edge := Default()
	edge.GPUWaitMs = 3 * 90 * 1000 // exactly 3x is the ceiling, not past it
	if f := edge.Findings(); len(f) != 0 {
		t.Fatalf("gpu_wait_ms at exactly 3x the vision wait must not fire; got %q", f)
	}
}

// TestFindingsRetiredKeysPresent (register S-42): the loader already knows
// videogen_wait_ms/audiogen_wait_ms are retired — it printed one stderr note at
// startup, which nobody is reading when they run doctor to find out why a media
// call blocked for 20 minutes. The list rides the loaded config so doctor can
// print one row per key still in the FILE.
func TestFindingsRetiredKeysPresent(t *testing.T) {
	cfg, err := Load(writeShapeCfg(t, `{"videogen_wait_ms":1200000,"audiogen_wait_ms":120000}`))
	if err != nil {
		t.Fatalf("a retired key must never fail the load: %v", err)
	}
	joined := strings.Join(cfg.Findings(), "\n")
	for _, want := range []string{"audiogen_wait_ms", "videogen_wait_ms", "retired"} {
		if !strings.Contains(joined, want) {
			t.Errorf("findings %q must name %q", joined, want)
		}
	}
	// Sorted, so a two-key file produces the same two rows in the same order
	// every run (Go randomizes map iteration).
	if got := cfg.RetiredKeys; len(got) != 2 || got[0] != "audiogen_wait_ms" || got[1] != "videogen_wait_ms" {
		t.Fatalf("RetiredKeys = %q, want the two keys sorted", got)
	}
}

// TestLoadRefusesAnUnusableBaseURL (review of #361, blocker 1): the headline
// scenario the dead-port check was written for — "an endpoint whose value was
// never substituted" — does not always reach the port check at all, because the
// value is not a URL. Until now `deadPortErr` and `parseBase` both returned nil
// on a url.Parse error, so an unsubstituted shell/template placeholder passed
// SILENTLY on every key that is not one of the two tailnet-guarded maps.
//
// Three shapes, all verified against net/url:
//   - "${NODE_A_HOST}:18811" -> parse error (first path segment cannot contain colon)
//   - "http://node-a:$PORT"  -> parse error (invalid port)
//   - "node-a:18811"         -> PARSES, as scheme "node-a" with an empty host
//
// The third is the dangerous one: nothing errors, nothing has a port, and the
// dialer resolves nothing. All three now fail the load naming the key and value.
func TestLoadRefusesAnUnusableBaseURL(t *testing.T) {
	unusable := []struct{ name, value string }{
		{"an unsubstituted host template", "${NODE_A_HOST}:18811"},
		{"an unsubstituted port template", "http://node-a:$PORT"},
		{"no scheme at all", "node-a:18811"},
		{"a scheme that is not http(s)", "ftp://node-a:18811"},
		{"a port with no host", "http://:18811"},
	}
	for _, u := range unusable {
		t.Run("endpoint/"+u.name, func(t *testing.T) {
			body := `{"endpoint":` + quote(u.value) + `}`
			_, err := Load(writeShapeCfg(t, body))
			if err == nil {
				t.Fatalf("Load must refuse endpoint %q", u.value)
			}
			for _, want := range []string{"endpoint", u.value} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load error %q must name %q", err, want)
				}
			}
		})
		t.Run("delegate_remotes/"+u.name, func(t *testing.T) {
			body := `{"delegate_remotes":[` + quote(u.value) + `]}`
			_, err := Load(writeShapeCfg(t, body))
			if err == nil {
				t.Fatalf("Load must refuse delegate_remotes %q", u.value)
			}
			for _, want := range []string{"delegate_remotes", u.value} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load error %q must name %q", err, want)
				}
			}
		})
	}
	// Control: the same keys with a usable base still load.
	if _, err := Load(writeShapeCfg(t, `{"endpoint":"http://node-a:18811","delegate_remotes":["http://node-a:18811"]}`)); err != nil {
		t.Fatalf("a usable base must load: %v", err)
	}
}

// quote renders a Go string as a JSON string literal for the table above.
func quote(v string) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestEndpointWarningsNameAnUnparseableValue (review of #361, blocker 1): a
// Config built in process — or one doctor is reporting on after a failed load —
// can still carry a value the refusal never saw. EndpointWarnings used to
// `continue` past it, i.e. say nothing about the very value most likely to be
// broken. It now emits a finding naming it.
func TestEndpointWarningsNameAnUnparseableValue(t *testing.T) {
	cfg := Config{
		Endpoint:           "http://127.0.0.1:11436",
		DelegateRemotes:    []string{"${NODE_A_HOST}:18811"},
		CascadeRemoteLanes: map[string]string{"offload-e4b": "node-c:11436"},
	}
	joined := strings.Join(EndpointWarnings(cfg), "\n")
	for _, want := range []string{"delegate_remotes", "${NODE_A_HOST}:18811", "cascade_remote_lanes", "node-c:11436"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings %q must name %q", joined, want)
		}
	}
}

// TestLoadRefusesAZeroPaddedDeadPort (review of #361, medium 3): the dead-port
// set was keyed on the port STRING, so ":09" — which url.Parse keeps verbatim and
// every dialer reads as 9 — walked straight past it. The comparison is numeric.
func TestLoadRefusesAZeroPaddedDeadPort(t *testing.T) {
	for _, v := range []string{"http://node-a:09", "http://node-a:00", "http://node-a:0009"} {
		_, err := Load(writeShapeCfg(t, `{"endpoint":`+quote(v)+`}`))
		if err == nil {
			t.Fatalf("Load must refuse endpoint %q — it dials a dead port", v)
		}
		if !strings.Contains(err.Error(), "endpoint") {
			t.Errorf("Load error %q must name the key", err)
		}
	}
}

// TestFindingsNamesANegativeWait (review of #361, medium 4): gpuWaitFinding read
// a negative value as "unset" and said nothing, but a negative wait is not unset
// — the pipeline turns it into a zero-length wait, so a GPU task gets ONE try and
// the config file says it should get ten minutes. A negative is a finding, not a
// load error: it behaves as the documented 0 rather than breaking anything, and
// this whole class is warn-then-fail.
func TestFindingsNamesANegativeWait(t *testing.T) {
	cfg := Default()
	cfg.GPUWaitMs = -1
	joined := strings.Join(cfg.Findings(), "\n")
	for _, want := range []string{"gpu_wait_ms", "-1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("findings %q must name %q", joined, want)
		}
	}
	vis := Default()
	vis.VisionGPUWaitSec = -5
	joined = strings.Join(vis.Findings(), "\n")
	for _, want := range []string{"vision_gpu_wait_sec", "-5"} {
		if !strings.Contains(joined, want) {
			t.Errorf("findings %q must name %q", joined, want)
		}
	}
	// 0 is a documented choice (a single try) and stays silent.
	zero := Default()
	zero.GPUWaitMs = 0
	zero.VisionGPUWaitSec = 0
	if f := zero.Findings(); len(f) != 0 {
		t.Fatalf("zero is a deliberate single-try setting, not a finding; got %q", f)
	}
}
