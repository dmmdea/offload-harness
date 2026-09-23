package config

// Coverage for the composition lane's config helpers (ADR 0059): the one route
// predicate the pipeline and the fleet advertisement share, the worker-count rule,
// the cache-dir default, and the load-time warnings for a compose binding that
// loads cleanly and then defers every call. Each warning case asserts BOTH
// directions: a warning that cannot stay silent proves nothing about the condition.

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func composeWarnOut(c Config) string {
	var buf bytes.Buffer
	warnComposeBindingsTo(c, &buf)
	return buf.String()
}

func boundCompose() Config {
	return Config{ComposeScript: "render/compose-hyperframes.mjs", HyperframesDir: "/opt/hf", HyperframesBrowserPath: "/opt/hf/chs"}
}

func TestComposeRouteConfiguredNeedsAllThreeKeys(t *testing.T) {
	if !boundCompose().ComposeRouteConfigured() {
		t.Fatal("script + install + browser bound must be CONFIGURED")
	}
	for name, mut := range map[string]func(*Config){
		"no script":  func(c *Config) { c.ComposeScript = "" },
		"no install": func(c *Config) { c.HyperframesDir = "" },
		"no browser": func(c *Config) { c.HyperframesBrowserPath = "" },
	} {
		c := boundCompose()
		mut(&c)
		if c.ComposeRouteConfigured() {
			t.Errorf("%s: a half-bound route must not read as configured", name)
		}
	}
	if Default().ComposeRouteConfigured() {
		t.Error("the shipped default must leave the composition route unbound")
	}
}

func TestValidComposeWorkers(t *testing.T) {
	for _, ok := range []string{"", "auto", " auto ", "1", "6", "24"} {
		if !ValidComposeWorkers(ok) {
			t.Errorf("%q must be accepted", ok)
		}
	}
	for _, bad := range []string{"0", "25", "-1", "2.5", "lots", "AUTO"} {
		if ValidComposeWorkers(bad) {
			t.Errorf("%q must be refused (HyperFrames caps --workers at 24)", bad)
		}
	}
}

func TestEffectiveComposeCacheDir(t *testing.T) {
	c := Config{MediaDir: filepath.Join("m", "media")}
	if got, want := c.EffectiveComposeCacheDir(), filepath.Join("m", "media", ".compose-cache"); got != want {
		t.Fatalf("default cache dir = %q, want %q (a dot-dir /fleet/media never serves)", got, want)
	}
	c.ComposeCacheDir = filepath.Join("big", "cache")
	if got := c.EffectiveComposeCacheDir(); got != c.ComposeCacheDir {
		t.Fatalf("compose_cache_dir must win, got %q", got)
	}
}

func TestComposeWarningsFireAndStaySilent(t *testing.T) {
	if out := composeWarnOut(boundCompose()); out != "" {
		t.Fatalf("a fully bound route with defaults must not warn, got %q", out)
	}
	if out := composeWarnOut(Default()); out != "" {
		t.Fatalf("the shipped default must not warn, got %q", out)
	}
	for name, tc := range map[string]struct {
		mut  func(*Config)
		want string
	}{
		"half-bound (no install)": {func(c *Config) { c.HyperframesDir = "" }, "compose_script is set but hyperframes_dir/hyperframes_browser_path is not"},
		"half-bound (no browser)": {func(c *Config) { c.HyperframesBrowserPath = "" }, "compose_script is set but hyperframes_dir/hyperframes_browser_path is not"},
		"bad workers":             {func(c *Config) { c.ComposeWorkers = "99" }, `compose_workers "99"`},
		"bad quality":             {func(c *Config) { c.ComposeQuality = "looks" }, `compose_quality "looks"`},
	} {
		t.Run(name, func(t *testing.T) {
			c := boundCompose()
			tc.mut(&c)
			if out := composeWarnOut(c); !strings.Contains(out, tc.want) {
				t.Fatalf("want a warning containing %q, got %q", tc.want, out)
			}
		})
	}
}
