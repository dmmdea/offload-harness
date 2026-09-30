package config

import (
	"bytes"
	"os"
	"path/filepath"
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

// browse_cdp_url pins the lane to a dedicated browser (its own profile and debugging port)
// instead of discovering the operator's main one. Loopback only, like the decision endpoint:
// the lane must never attach to a browser on another machine.
func TestBrowseCDPURLMustBeLoopback(t *testing.T) {
	for _, u := range []string{
		"http://127.0.0.1:9333",
		"http://localhost:9333/",
		"ws://127.0.0.1:9333/devtools/browser/abc",
		"http://[::1]:9333",
	} {
		if !BrowseCDPURLAllowed(u) {
			t.Errorf("%s is a loopback CDP endpoint and must be allowed", u)
		}
		c := browseBound()
		c.BrowseCDPURL = u
		if !c.BrowseConfigured() {
			t.Errorf("a loopback browse_cdp_url (%q) must keep the lane configured", u)
		}
	}
	for _, u := range []string{
		"http://198.51.100.7:9333",        // another machine
		"ws://browser.example.com:9333/x", // a remote browser
		"https://127.0.0.1:9333",          // one shape per scheme: plain http or ws
		"http://127.0.0.1",                // no port: a CDP endpoint always names one
		"http://user@127.0.0.1:9333",      // no credentials in the URL
		"file:///tmp/DevToolsActivePort",
		"127.0.0.1:9333",
		"http://127.0.0.1:9333/foo?x=1",     // http is the endpoint root; no path, no query
		"http://127.0.0.1:0",                // not a real port
		"ws://127.0.0.1:9333",               // a ws endpoint names a /devtools/ socket
		"ws://198.51.100.7:9333/devtools/x", // ws to another machine
		"http://[::1%25eth0]:9333",          // zoned IPv6 is not a plain loopback literal
	} {
		if BrowseCDPURLAllowed(u) {
			t.Errorf("%s must be refused", u)
		}
		c := browseBound()
		c.BrowseCDPURL = u
		if c.BrowseConfigured() {
			t.Errorf("a non-loopback browse_cdp_url (%q) must leave the lane unconfigured (fail closed)", u)
		}
	}
	if !browseBound().BrowseConfigured() {
		t.Error("an empty browse_cdp_url must keep the discovery behaviour")
	}
	var buf bytes.Buffer
	c := browseBound()
	c.BrowseCDPURL = "http://198.51.100.7:9333"
	warnBrowseBindingsTo(c, &buf)
	if !strings.Contains(buf.String(), "browse_cdp_url") {
		t.Errorf("a refused browse_cdp_url must warn by name, got %q", buf.String())
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

// browse_activate_tab brings the lane's own tab to the front of its window. That switches the
// window's active tab, so it is honoured only against a dedicated endpoint (browse_cdp_url):
// the raw key alone must never reach the sidecar, or an operator who set it once would have
// their everyday browser's active tab switched by a lane run.
func TestBrowseActivateTabNeedsADedicatedEndpoint(t *testing.T) {
	if Default().BrowseActivateTab || Default().EffectiveBrowseActivateTab() {
		t.Fatal("browse_activate_tab must default to false (opt-in)")
	}
	on := browseBound()
	on.BrowseActivateTab = true
	if on.EffectiveBrowseActivateTab() {
		t.Error("browse_activate_tab without browse_cdp_url must be ignored: the lane would be driving the everyday browser")
	}
	if !on.BrowseActivateTabIgnored() {
		t.Error("a browse_activate_tab with no browse_cdp_url must be reported as ignored")
	}
	if !on.BrowseConfigured() {
		t.Error("an ignored browse_activate_tab must not fail the lane")
	}
	on.BrowseBrowser = "brave" // a named everyday browser is still not a dedicated endpoint
	if on.EffectiveBrowseActivateTab() {
		t.Error("browse_browser names the everyday browser; browse_activate_tab must stay ignored")
	}
	on.BrowseBrowser = ""
	for _, u := range []string{"http://127.0.0.1:9555", "ws://127.0.0.1:9555/devtools/browser/abc", "http://[::1]:9555"} {
		on.BrowseCDPURL = u
		if !on.EffectiveBrowseActivateTab() || on.BrowseActivateTabIgnored() {
			t.Errorf("browse_activate_tab with a dedicated endpoint (%q) must be effective", u)
		}
	}
	off := browseBound()
	off.BrowseCDPURL = "http://127.0.0.1:9555"
	if off.EffectiveBrowseActivateTab() || off.BrowseActivateTabIgnored() {
		t.Error("a dedicated endpoint alone must not turn activation on, and is not an ignored setting")
	}
	bad := browseBound()
	bad.BrowseActivateTab = true
	bad.BrowseCDPURL = "http://198.51.100.7:9555" // refused: not loopback
	if bad.EffectiveBrowseActivateTab() {
		t.Error("an endpoint the lane refuses must not count as a dedicated endpoint")
	}
}

func TestBrowseActivateTabLoadsFromJSONAndDefaultsOff(t *testing.T) {
	load := func(js string) Config {
		t.Helper()
		p := filepath.Join(t.TempDir(), "cfg.json")
		if err := os.WriteFile(p, []byte(js), 0o644); err != nil {
			t.Fatal(err)
		}
		c, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if load(`{}`).BrowseActivateTab {
		t.Error("a config without the key must load with browse_activate_tab false")
	}
	if !load(`{"browse_activate_tab": true}`).BrowseActivateTab {
		t.Error(`"browse_activate_tab": true must load as true`)
	}
	if load(`{"browse_activate_tab": false}`).BrowseActivateTab {
		t.Error(`"browse_activate_tab": false must load as false`)
	}
}

func TestBrowseActivateTabWarnsOnlyWhenIgnored(t *testing.T) {
	var buf bytes.Buffer
	c := browseBound()
	c.BrowseActivateTab = true
	warnBrowseBindingsTo(c, &buf)
	if !strings.Contains(buf.String(), "browse_activate_tab") || !strings.Contains(buf.String(), "browse_cdp_url") {
		t.Errorf("an ignored browse_activate_tab must warn naming both keys, got %q", buf.String())
	}
	buf.Reset()
	c.BrowseCDPURL = "http://127.0.0.1:9555"
	warnBrowseBindingsTo(c, &buf)
	if buf.Len() != 0 {
		t.Errorf("a browse_activate_tab with a dedicated endpoint must not warn, got %q", buf.String())
	}
	buf.Reset()
	c.BrowseActivateTab = false
	c.BrowseCDPURL = ""
	warnBrowseBindingsTo(c, &buf)
	if buf.Len() != 0 {
		t.Errorf("browse_activate_tab false must not warn, got %q", buf.String())
	}
}
