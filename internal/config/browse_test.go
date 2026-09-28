package config

import (
	"bytes"
	"strings"
	"testing"
)

func browseBound() Config {
	c := Default()
	c.BrowsePython = "/opt/offload/browse/.venv/bin/python"
	c.BrowseScript = "/opt/offload/browse/runner.py"
	c.BrowseDecisionURL = "http://127.0.0.1:18720/v1/systemone"
	return c
}

func TestBrowseConfiguredNeedsAllThreeKeys(t *testing.T) {
	if !browseBound().BrowseConfigured() {
		t.Fatal("python + script + loopback decision url must configure the lane")
	}
	for name, clear := range map[string]func(*Config){
		"python": func(c *Config) { c.BrowsePython = "" },
		"script": func(c *Config) { c.BrowseScript = "" },
		"url":    func(c *Config) { c.BrowseDecisionURL = "" },
	} {
		c := browseBound()
		clear(&c)
		if c.BrowseConfigured() {
			t.Errorf("missing %s must leave the lane unconfigured", name)
		}
	}
	if Default().BrowseConfigured() {
		t.Error("the default config must never configure the browse lane (opt-in)")
	}
}

func TestBrowseDecisionURLMustBeLoopbackHTTP(t *testing.T) {
	for _, u := range []string{
		"http://127.0.0.1:18720/v1/systemone",
		"http://localhost:9000/v1/systemone",
		"http://[::1]:18720/v1/systemone",
	} {
		if !BrowseDecisionURLAllowed(u) {
			t.Errorf("%s is loopback http and must be allowed", u)
		}
	}
	for _, u := range []string{
		"https://api.typesafe.ai/v1/systemone", // a provider: never
		"https://openrouter.ai/api/alpha/decisions",
		"http://198.51.100.7:18720/v1/systemone", // a documentation-range address: not loopback
		"http://127.0.0.1.example.com/v1/systemone",
		"https://127.0.0.1:18720/v1/systemone", // loopback, but the shim speaks plain http; keep one shape
		"ftp://127.0.0.1/v1/systemone",
		"127.0.0.1:18720/v1/systemone",
		"",
	} {
		if BrowseDecisionURLAllowed(u) {
			t.Errorf("%s must be refused", u)
		}
		c := browseBound()
		c.BrowseDecisionURL = u
		if c.BrowseConfigured() {
			t.Errorf("a non-loopback decision url (%q) must leave the lane unconfigured", u)
		}
	}
}

func TestBrowseDefaultsAndClamp(t *testing.T) {
	c := Default()
	if c.BrowseTimeoutSec != 300 || c.BrowseMaxActions != 30 {
		t.Fatalf("defaults: timeout %d, max actions %d; want 300, 30", c.BrowseTimeoutSec, c.BrowseMaxActions)
	}
	c.BrowseMaxActions = 500
	if got := c.EffectiveBrowseMaxActions(); got != BrowseMaxActionsCeiling {
		t.Errorf("max actions above the ceiling must clamp to %d, got %d", BrowseMaxActionsCeiling, got)
	}
	c.BrowseMaxActions = 0
	if got := c.EffectiveBrowseMaxActions(); got != 30 {
		t.Errorf("0 must mean the default 30, got %d", got)
	}
}

func TestBrowseWarningsNameTheProblem(t *testing.T) {
	var buf bytes.Buffer
	c := browseBound()
	c.BrowseDecisionURL = "https://api.typesafe.ai/v1/systemone"
	warnBrowseBindingsTo(c, &buf)
	if !strings.Contains(buf.String(), "loopback") {
		t.Errorf("a non-loopback decision url must warn about loopback, got %q", buf.String())
	}
	buf.Reset()
	c = browseBound()
	c.BrowseDecisionURL = ""
	warnBrowseBindingsTo(c, &buf)
	if !strings.Contains(buf.String(), "browse_decision_url") {
		t.Errorf("a half-bound lane must name the missing key, got %q", buf.String())
	}
	buf.Reset()
	c = browseBound()
	c.BrowseBrowser = "netscape"
	warnBrowseBindingsTo(c, &buf)
	if !strings.Contains(buf.String(), "browse_browser") {
		t.Errorf("an unknown browser must warn, got %q", buf.String())
	}
	buf.Reset()
	warnBrowseBindingsTo(Default(), &buf)
	if buf.Len() != 0 {
		t.Errorf("the default config must not warn, got %q", buf.String())
	}
}
