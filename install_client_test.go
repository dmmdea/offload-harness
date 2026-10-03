package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/acceptance"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

func installClientInto(t *testing.T, home string, extra ...string) (string, error) {
	t.Helper()
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("sekrit-fleet-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"--home", home, "--remotes", "http://render-a:18811, http://render-b:18811/", "--token-file", tok}, extra...)
	// Capture stdout: the token must never be printed.
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := runInstallClient(args)
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), err
}

func TestInstallClientRendersAConfigWithNoLocalLane(t *testing.T) {
	home := t.TempDir()
	out, err := installClientInto(t, home)
	if err != nil {
		t.Fatalf("install client: %v", err)
	}
	if strings.Contains(out, "sekrit") {
		t.Fatalf("the token must never be printed: %s", out)
	}
	path := filepath.Join(home, "etc", "config.json")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the rendered config must load: %v", err)
	}
	if cfg.FleetAuthToken != "sekrit-fleet-token" || !cfg.AgentDelegationEnabled {
		t.Errorf("token or delegation flag not set: delegation=%v", cfg.AgentDelegationEnabled)
	}
	if want := []string{"http://render-a:18811", "http://render-b:18811"}; !reflect.DeepEqual(cfg.DelegateRemotes, want) {
		t.Errorf("delegate_remotes = %v, want %v (trimmed, in order)", cfg.DelegateRemotes, want)
	}
	if hasLocalModel(cfg) {
		t.Error("a delegation client must name no local model")
	}
	if cfg.ComposeRouteConfigured() {
		t.Error("a delegation client has no composition lane (compose renders go to the fleet)")
	}
	for _, r := range mediacap.Routes(cfg) {
		if r.State == mediacap.BoundButMissing {
			t.Errorf("route %s claims a binding this box does not have: %s", r.Name, r.Detail)
		}
	}
	def := config.Default()
	v, ty := reflect.ValueOf(cfg), reflect.TypeOf(cfg)
	dv := reflect.ValueOf(def)
	for i := 0; i < ty.NumField(); i++ {
		tag := strings.SplitN(ty.Field(i).Tag.Get("json"), ",", 2)[0]
		if strings.HasSuffix(tag, "_script") && dv.Field(i).Kind() == reflect.String && dv.Field(i).String() != "" && v.Field(i).String() != "" {
			t.Errorf("%s keeps its default binding %q on a client", tag, v.Field(i).String())
		}
	}
	for _, d := range []string{"media", "state"} {
		if fi, err := os.Stat(filepath.Join(home, d)); err != nil || !fi.IsDir() {
			t.Errorf("%s dir not created under the home", d)
		}
	}
	if !strings.HasPrefix(filepath.ToSlash(cfg.LedgerPath), filepath.ToSlash(home)) || !strings.HasPrefix(filepath.ToSlash(cfg.CachePath), filepath.ToSlash(home)) {
		t.Errorf("ledger and cache live under the client's home, got %q and %q", cfg.LedgerPath, cfg.CachePath)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Errorf("the config holds the token and must be 0600, is %v", fi.Mode().Perm())
		}
	}
	chk := aliasCheck2(context.Background(), cfg)
	if chk.Status != acceptance.Skip {
		t.Errorf("acceptance's alias check must SKIP on a client (no roster to fetch), got %s: %s", chk.Status, chk.Detail)
	}
}

func TestInstallClientRefusesBadInputAndKeepsAnExistingConfig(t *testing.T) {
	home := t.TempDir()
	if _, err := installClientInto(t, home); err != nil {
		t.Fatal(err)
	}
	if _, err := installClientInto(t, home); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("an existing config must not be replaced without --force: %v", err)
	}
	if _, err := installClientInto(t, home, "--force"); err != nil {
		t.Errorf("--force replaces it: %v", err)
	}
	// Each refusal is checked by its own reason: the rules run in order, so a case that only asserted
	// "refused" would pass on an earlier rule with its own gone.
	empty := filepath.Join(t.TempDir(), "empty")
	_ = os.WriteFile(empty, []byte("  \n"), 0o600)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no remotes":    {[]string{"--home", t.TempDir(), "--token-file", empty}, "--remotes is required"},
		"blank remotes": {[]string{"--home", t.TempDir(), "--remotes", " , ", "--token-file", empty}, "--remotes is required"},
		"no token file": {[]string{"--home", t.TempDir(), "--remotes", "http://render-a:18811"}, "--token-file is required"},
		"missing token": {[]string{"--home", t.TempDir(), "--remotes", "http://render-a:18811", "--token-file", filepath.Join(t.TempDir(), "absent")}, "reading the token file"},
		"empty token":   {[]string{"--home", t.TempDir(), "--remotes", "http://render-a:18811", "--token-file", empty}, "the token file is empty"},
		"no home":       {[]string{"--remotes", "http://render-a:18811", "--token-file", empty}, "--home is required"},
	} {
		if err := runInstallClient(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want a refusal naming %q, got %v", name, tc.want, err)
		}
	}
}

func TestInstallClientNeverPrintsTheTokenInJSON(t *testing.T) {
	out, err := installClientInto(t, t.TempDir(), "--json")
	if err != nil {
		t.Fatalf("install client --json: %v", err)
	}
	if strings.Contains(out, "sekrit") {
		t.Fatalf("--json printed the token: %s", out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil || res["ok"] != true {
		t.Fatalf("--json must print one JSON result with ok true: %v: %s", err, out)
	}
}

func TestInstallClientRefusesRemotesThatAreNotFleetNodes(t *testing.T) {
	for remote, want := range map[string]string{
		// The seat port instead of the fleet node port is the operator's likely slip.
		"http://render-a:11436":    "fleet node port",
		"http://127.0.0.1:18811":   "loopback base",
		"http://render-a:18811/v1": "/v1 suffix",
		"http://render-a:9":        "does not load", // the discard port: the config's own validation refuses it
	} {
		home := t.TempDir()
		_, err := installClientInto(t, home, "--remotes", remote)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want a refusal naming %q, got %v", remote, want, err)
		}
		// No half-valid file holding the token may stay behind.
		if _, serr := os.Stat(filepath.Join(home, "etc", "config.json")); !os.IsNotExist(serr) {
			t.Errorf("%s: the refused config must be removed: %v", remote, serr)
		}
	}
}

// ffmpeg stays bound only where this machine has it: CI runners and bare clients often do not, and a
// client must not claim a binary it lacks (the first CI run of this test failed on exactly that).
func TestInstallClientBindsFFmpegOnlyWhereItIs(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // nothing on PATH
	home := t.TempDir()
	if _, err := installClientInto(t, home); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(home, "etc", "config.json"))
	if err != nil || cfg.FFmpegPath != "" {
		t.Fatalf("with no ffmpeg on PATH, ffmpeg_path must be unbound: %q, %v", cfg.FFmpegPath, err)
	}

	bin := t.TempDir()
	for _, name := range []string{"ffmpeg", "ffprobe", "ffmpeg.exe", "ffprobe.exe"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	home = t.TempDir()
	if _, err := installClientInto(t, home); err != nil {
		t.Fatal(err)
	}
	if cfg, err = config.Load(filepath.Join(home, "etc", "config.json")); err != nil || cfg.FFmpegPath != config.Default().FFmpegPath {
		t.Fatalf("with ffmpeg and ffprobe on PATH, ffmpeg_path keeps its default: %q, %v", cfg.FFmpegPath, err)
	}
	for _, r := range mediacap.Routes(cfg) {
		if r.State == mediacap.BoundButMissing && strings.HasPrefix(r.Name, "media") {
			t.Errorf("route %s: %s", r.Name, r.Detail)
		}
	}
}

func TestHasLocalModel(t *testing.T) {
	if !hasLocalModel(config.Default()) {
		t.Error("the default config names a local model")
	}
}
