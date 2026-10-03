package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/datahome"
)

// TestInstallerTemplateFollowsHome: the config template install.ps1 seeds a fresh
// node from used to spell sixteen data paths as `~/.local-offload/...`. A path
// written in the file is an explicit value, which `home` must not rebase; the
// literals only happened to equal the built-in defaults, so they rebased by luck,
// and stopped doing so the moment $LOCAL_OFFLOAD_HOME was set. A node installed from
// it then kept its media, cache and ledger on the OS drive however `home` read
// (register C-92). The template now says nothing about them and the defaults speak.
func TestInstallerTemplateFollowsHome(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tpl map[string]any
	if err := json.Unmarshal(config.StripBOM(raw), &tpl); err != nil {
		t.Fatalf("the template must be valid JSON: %v", err)
	}
	for k, v := range tpl {
		if s, ok := v.(string); ok && strings.Contains(s, ".local-offload") {
			t.Errorf("template key %s pins %q: a data path spelled out in the file is not rebased by `home`", k, s)
		}
	}

	// And through the loader, in the failing shape: the template as written plus a
	// `home` on another drive, with the env override that broke the literal paths.
	tpl["home"] = "D:/local-offload"
	body, _ := json.Marshal(tpl)
	for _, envHome := range []string{"", `C:\relocated\root`} {
		t.Run("LOCAL_OFFLOAD_HOME="+envHome, func(t *testing.T) {
			profile := `C:\Users\user`
			t.Setenv("USERPROFILE", profile)
			t.Setenv("HOME", profile)
			t.Setenv("LOCAL_OFFLOAD_HOME", envHome)
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, body, 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			for _, l := range datahome.Locations(cfg) {
				if !strings.HasPrefix(strings.ToLower(filepath.ToSlash(l.Path)), "d:/local-offload") {
					t.Errorf("%s resolves to %s, not under home", l.Key, l.Path)
				}
			}
		})
	}
}
