package main

import (
	"context"
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
	if err := runInstallClient([]string{"--home", t.TempDir(), "--token-file", "x"}); err == nil {
		t.Error("no remotes accepted")
	}
	empty := filepath.Join(t.TempDir(), "empty")
	_ = os.WriteFile(empty, []byte("  \n"), 0o600)
	if err := runInstallClient([]string{"--home", t.TempDir(), "--remotes", "http://render-a:18811", "--token-file", empty}); err == nil {
		t.Error("an empty token accepted")
	}
	if err := runInstallClient([]string{"--remotes", "http://render-a:18811", "--token-file", empty}); err == nil {
		t.Error("no home accepted")
	}
}

func TestHasLocalModel(t *testing.T) {
	if !hasLocalModel(config.Default()) {
		t.Error("the default config names a local model")
	}
}
