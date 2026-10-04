package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The CLI verbs take the routes their MCP twins take (ADR 0072): `transcribe --route`, and
// `classify` / `extract --route`. These tests run the real verbs end to end against a throwaway
// config (home, ledger and media dir in a temp directory, no delegate_remotes, no model): a route that
// reaches its lane answers in the lane's own words, with no network and no model involved.

// routeCLIConfig writes a config whose every path hangs off a temp home and returns its path.
func routeCLIConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	body, _ := json.Marshal(map[string]any{"home": filepath.Join(dir, "home")})
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func routeCLIFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// cliResult runs verb with args and decodes the --json result it printed.
func cliResult(t *testing.T, verb func([]string) error, args ...string) (map[string]any, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() { err = verb(args) })
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if jerr := json.Unmarshal([]byte(out), &m); jerr != nil {
		t.Fatalf("the verb printed no JSON result: %v\n%s", jerr, out)
	}
	return m, nil
}

func metaOf(m map[string]any) map[string]any {
	meta, _ := m["meta"].(map[string]any)
	return meta
}

func TestTranscribeCLIRouteFlagReachesTheLane(t *testing.T) {
	cfg := routeCLIConfig(t)
	audio := routeCLIFile(t, "a.wav", "RIFF")

	// A route the lane does not know is a contract defer in the lane's words, whether the flag comes
	// before or after the audio path.
	for _, args := range [][]string{
		{"--config", cfg, "--route", "bogus", "--json", audio},
		{audio, "--config", cfg, "--route", "bogus", "--json"},
	} {
		res, err := cliResult(t, runTranscribe, args...)
		if err != nil {
			t.Fatal(err)
		}
		if res["deferred"] != true || res["defer_class"] != "contract" || !strings.Contains(res["reason"].(string), `unrecognized route "bogus"`) {
			t.Errorf("%v: result = %v, want the lane's contract defer", args, res)
		}
	}

	// route remote with no fleet configured never touches the local whisper: a config defer.
	res, err := cliResult(t, runTranscribe, "--config", cfg, "--route", "remote", "--json", audio)
	if err != nil {
		t.Fatal(err)
	}
	if res["deferred"] != true || res["defer_class"] != "config" || metaOf(res)["placement"] != "remote: forced" {
		t.Errorf("route remote: %v, want a config defer placed remote: forced", res)
	}

	// No route is local, exactly as before: the pipeline's own answer and no placement stamp.
	res, err = cliResult(t, runTranscribe, "--config", cfg, "--json", audio)
	if err != nil {
		t.Fatal(err)
	}
	if res["deferred"] != true || !strings.Contains(res["reason"].(string), "no stt model configured") || metaOf(res)["placement"] != nil {
		t.Errorf("default route: %v, want the local pipeline's defer and no placement", res)
	}
}

func TestClassifyAndExtractCLIRouteFlagReachesTheTextLane(t *testing.T) {
	cfg := routeCLIConfig(t)
	text := routeCLIFile(t, "in.txt", "the invoice is overdue")
	schema := routeCLIFile(t, "schema.json", `{"type":"object","properties":{"amount":{"type":"string"}}}`)

	for name, args := range map[string][]string{
		"classify": {"--config", cfg, "--labels", "a,b", "--route", "bogus", "--json", text},
		"extract":  {"--config", cfg, "--schema", schema, "--route", "bogus", "--json", text},
	} {
		res, err := cliResult(t, func(a []string) error { return runTask(name, a) }, args...)
		if err != nil {
			t.Fatal(err)
		}
		if res["deferred"] != true || res["defer_class"] != "contract" || !strings.Contains(res["reason"].(string), `unrecognized route "bogus"`) {
			t.Errorf("%s: result = %v, want the text lane's contract defer", name, res)
		}
		// remote with no fleet: a config defer, never a local run.
		remote := append([]string(nil), args...)
		remote[len(remote)-3] = "remote" // the value after --route
		res, err = cliResult(t, func(a []string) error { return runTask(name, a) }, remote...)
		if err != nil {
			t.Fatal(err)
		}
		if res["deferred"] != true || res["defer_class"] != "config" {
			t.Errorf("%s --route remote: %v, want a config defer", name, res)
		}
	}
}

// summarize and triage have no route on the MCP surface (a fleet node refuses them): a --route on them
// is an error, never a silent local run under a route the caller asked for.
func TestSummarizeAndTriageCLIRefuseARoute(t *testing.T) {
	cfg := routeCLIConfig(t)
	text := routeCLIFile(t, "in.txt", "hello")
	for name, args := range map[string][]string{
		"summarize": {"--config", cfg, "--route", "remote", "--json", text},
		"triage":    {"--config", cfg, "--question", "q", "--route", "auto", "--json", text},
	} {
		_, err := cliResult(t, func(a []string) error { return runTask(name, a) }, args...)
		if err == nil || !strings.Contains(err.Error(), "--route applies to classify and extract") {
			t.Errorf("%s --route: err = %v, want the refusal naming classify and extract", name, err)
		}
	}
}
