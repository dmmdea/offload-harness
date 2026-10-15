package rungraph

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The Go side of the run-template steps talks to render/run-template.mjs. These tests run the real script (Node is the harness's own
// runtime; a box without it skips) on a ComfyUI tree built from the catalog fixtures, so the two sides cannot drift apart unseen.

func nodeOrSkip(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	return node
}

// fixtureComfy copies the templates-catalog fixtures into a ComfyUI-shaped tree: the installed package under .venv, and the node's own
// comfy_api_nodes.
func fixtureComfy(t *testing.T) (comfyDir, licenseMap string) {
	t.Helper()
	fx := filepath.Join("..", "..", "render", "testdata", "templates-catalog")
	dir := t.TempDir()
	if err := os.CopyFS(filepath.Join(dir, ".venv", "Lib", "site-packages"), os.DirFS(filepath.Join(fx, "site-packages"))); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(dir, "comfy_api_nodes"), os.DirFS(filepath.Join(fx, "comfy_api_nodes"))); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(filepath.Join(fx, "license-map.fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	return dir, abs
}

func scriptPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "render", "run-template.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestQueryTemplateRunsTheScriptOnAFixtureTree(t *testing.T) {
	node := nodeOrSkip(t)
	dir, lic := fixtureComfy(t)
	ctx := context.Background()

	out, err := QueryTemplate(ctx, node, scriptPath(t), dir, []string{"list", "--license-map", lic}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		OK        bool `json:"ok"`
		Templates []struct {
			Name string `json:"name"`
		} `json:"templates"`
	}
	if err := json.Unmarshal(out, &list); err != nil || !list.OK {
		t.Fatalf("list: %v: %s", err, out)
	}
	names := map[string]bool{}
	for _, tpl := range list.Templates {
		names[tpl.Name] = true
	}
	if !names["utility_interpolation_image_upscale"] || names["utility_bria_increase_resolution"] {
		t.Errorf("list should hold the runnable zero-model template and not the paid one: %v", names)
	}

	out, err = QueryTemplate(ctx, node, scriptPath(t), dir, []string{"resolve", "utility_bria_increase_resolution", "--license-map", lic}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var paid struct {
		OK       bool   `json:"ok"`
		Deferred bool   `json:"deferred"`
		Class    string `json:"class"`
	}
	if err := json.Unmarshal(out, &paid); err != nil || paid.OK || !paid.Deferred || paid.Class != "api_paid" {
		t.Fatalf("a paid-API template: %v: %s", err, out)
	}
}

func TestQueryTemplateReportsAScriptFailureWithItsStderr(t *testing.T) {
	node := nodeOrSkip(t)
	dir, _ := fixtureComfy(t)
	_, err := QueryTemplate(context.Background(), node, scriptPath(t), dir, []string{"frobnicate"}, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "unknown verb") {
		t.Fatalf("want the script's own message in the error, got %v", err)
	}
}

func TestQueryTemplateStopsAtTheDeadline(t *testing.T) {
	node := nodeOrSkip(t)
	dir, _ := fixtureComfy(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := QueryTemplate(ctx, node, scriptPath(t), dir, []string{"list"}, time.Minute); err == nil {
		t.Fatal("a cancelled call must not run")
	}
}

func TestTemplateScriptSitsBesideTheRunGraphRunner(t *testing.T) {
	got, err := TemplateScript(filepath.Join("some", "render", "comfy-run-graph.mjs"))
	if err != nil || got != filepath.Join("some", "render", "run-template.mjs") {
		t.Errorf("TemplateScript = %q, %v", got, err)
	}
	if _, err := TemplateScript(""); err == nil {
		t.Error("no run-graph route configured is an error")
	}
}
