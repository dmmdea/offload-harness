package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestBuildComposeParams: `compose-video` hands the pipeline the same param shapes the
// MCP door does — html as the page itself (read from the file), variables as an object,
// snapshots as seconds, strict only when turned off.
func TestBuildComposeParams(t *testing.T) {
	dir := t.TempDir()
	page := filepath.Join(dir, "page.html")
	if err := os.WriteFile(page, []byte("<html></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := buildComposeParams(composeFlags{htmlFile: page, format: "webm", fps: 24, workers: 2, snapshots: "1, 2.5", strict: false})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"html": "<html></html>", "format": "webm", "fps": 24, "workers": 2, "snapshots": []float64{1, 2.5}, "strict": false}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("params = %#v, want %#v", got, want)
	}
	got, err = buildComposeParams(composeFlags{template: "lower-third", variables: `{"name":"A","duration":5}`, strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if got["template"] != "lower-third" || got["variables"].(map[string]any)["name"] != "A" {
		t.Fatalf("template params = %#v", got)
	}
	if _, ok := got["strict"]; ok {
		t.Error("strict is only sent when turned off (the pipeline default is true)")
	}
	for name, f := range map[string]composeFlags{
		"variables not an object": {template: "x", variables: `[1]`, strict: true},
		"both variables flags":    {template: "x", variables: `{}`, variablesFile: page, strict: true},
		"bad snapshot":            {template: "x", snapshots: "1,soon", strict: true},
		"missing html file":       {htmlFile: filepath.Join(dir, "nope.html"), strict: true},
	} {
		if _, err := buildComposeParams(f); err == nil || !strings.HasPrefix(err.Error(), "compose-video") {
			t.Errorf("%s: want a compose-video error, got %v", name, err)
		}
	}
}
