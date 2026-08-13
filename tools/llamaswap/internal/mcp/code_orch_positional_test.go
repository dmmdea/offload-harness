// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
)

// Every templated endpoint must be able to substitute every placeholder it
// declares. This is the invariant the empty Positional slices violated.
func TestEveryTemplatedEndpointCanSubstituteItsPathParams(t *testing.T) {
	templated := 0
	for i := range codeOrchEndpoints {
		ep := &codeOrchEndpoints[i]
		params := codeOrchPathParams(ep.Path)
		if len(params) == 0 {
			continue
		}
		templated++
		have := map[string]bool{}
		for _, p := range ep.Positional {
			have[p] = true
		}
		for _, p := range params {
			if !have[p] {
				t.Errorf("endpoint %q (%s) declares {%s} in its path but does not list it in Positional; "+
					"handleCodeOrchExecute iterates Positional, so this placeholder can never be substituted",
					ep.ID, ep.Path, p)
			}
		}
	}
	if templated == 0 {
		t.Fatal("no templated endpoints found at all; the guard would pass vacuously")
	}
	t.Logf("%d templated endpoints checked", templated)
}

func TestBackfillLeavesGeneratorSuppliedPositionalsAlone(t *testing.T) {
	saved := codeOrchEndpoints
	t.Cleanup(func() { codeOrchEndpoints = saved })
	codeOrchEndpoints = []codeOrchEndpoint{
		{ID: "a", Path: "/a/{one}/{two}"},
		{ID: "b", Path: "/b/{one}", Positional: []string{"explicit"}},
		{ID: "c", Path: "/c"},
		{ID: "d", Path: "/d/{one}/x/{one}"},
	}
	backfillCodeOrchPositionals()
	for _, want := range []struct {
		id  string
		pos []string
	}{
		{"a", []string{"one", "two"}},
		{"b", []string{"explicit"}},
		{"c", nil},
		{"d", []string{"one"}}, // repeated placeholder listed once
	} {
		ep := findCodeOrchEndpoint(want.id)
		if ep == nil {
			t.Fatalf("endpoint %q vanished", want.id)
		}
		if strings.Join(ep.Positional, ",") != strings.Join(want.pos, ",") {
			t.Errorf("%s: Positional = %v, want %v", want.id, ep.Positional, want.pos)
		}
	}
}

// The behavioural proof: executing a {model}-templated endpoint must reach
// /upstream/embeddinggemma/props, NOT /upstream/%7Bmodel%7D/props with the
// model smuggled into the query string.
func TestCodeOrchExecuteSubstitutesThePathParameter(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"build_info":"b10356-0666ad2b2","total_slots":4}`))
	}))
	defer srv.Close()
	t.Setenv("LLAMASWAP_BASE_URL", srv.URL)

	req := mcplib.CallToolRequest{}
	req.Params.Name = "llamaswap_execute"
	req.Params.Arguments = map[string]any{
		"endpoint_id": "upstream.props",
		"params":      map[string]any{"model": "embeddinggemma"},
	}
	res, err := handleCodeOrchExecute(context.Background(), req)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("execute returned an error result: %s", firstTextContent(res))
	}
	if gotPath != "/upstream/embeddinggemma/props" {
		t.Errorf("request path = %q, want /upstream/embeddinggemma/props (an unsubstituted template would appear as /upstream/%%7Bmodel%%7D/props)", gotPath)
	}
	if strings.Contains(gotPath, "{") || strings.Contains(gotPath, "%7B") {
		t.Errorf("the path placeholder was not substituted: %q", gotPath)
	}
	// The path param must be CONSUMED, not also echoed as a query param.
	if strings.Contains(gotQuery, "model=") {
		t.Errorf("query = %q; a path parameter must be removed from params after substitution", gotQuery)
	}
	if !strings.Contains(firstTextContent(res), "b10356") {
		t.Errorf("the upstream body did not come back: %s", firstTextContent(res))
	}
}

// A second templated endpoint on a different shape, so the fix is not
// upstream-specific.
func TestCodeOrchExecuteSubstitutesModelsUnload(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	t.Setenv("LLAMASWAP_BASE_URL", srv.URL)

	ep := findCodeOrchEndpoint("models.unload")
	if ep == nil {
		t.Skip("models.unload is not in this build's endpoint registry")
	}
	if !strings.Contains(ep.Path, "{model}") {
		t.Skipf("models.unload path %q is not templated in this build", ep.Path)
	}
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"endpoint_id": "models.unload",
		"params":      map[string]any{"model": "gemma-4-e2b"},
	}
	if _, err := handleCodeOrchExecute(context.Background(), req); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(gotPath, "{") || strings.Contains(gotPath, "%7B") {
		t.Errorf("placeholder survived into the request path: %q", gotPath)
	}
	if !strings.Contains(gotPath, "gemma-4-e2b") {
		t.Errorf("path = %q, want the model substituted in", gotPath)
	}
}

// llamaswap_get must describe the same endpoint the execute tool can reach,
// so an agent following search -> get -> execute is not handed a template it
// cannot fill.
func TestCodeOrchGetReportsTemplatedPaths(t *testing.T) {
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"endpoint_id": "upstream.props"}
	res, err := handleCodeOrchGet(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("get failed: %s", firstTextContent(res))
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(firstTextContent(res)), &meta); err != nil {
		t.Fatalf("get result is not JSON: %v", err)
	}
	if path, _ := meta["path"].(string); !strings.Contains(path, "{model}") {
		t.Errorf("path = %q, want the {model} template visible so the agent knows to supply it", path)
	}
}
