package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryAgentBuilderWiresEveryAcceleratorLane pins ADR 0037's "one tool set
// per box" at the CALL SITES: every agent.BuildConfig literal that wires the
// Hailo lane (NPU) must wire the accelerator lanes (Accel) too. Found live on
// 2026-09-08: the fleet-node delegation builder (internal/pipeline/agenttask.go)
// and the prompt-replay builder (main.go) passed NPU only, so a remote contract
// against <node-c> was told offload_classify_image did not exist while the
// same seat run locally had it. A per-package unit test cannot see this — the
// lanes are correct in isolation; the defect is a builder that forgets one.
func TestEveryAgentBuilderWiresEveryAcceleratorLane(t *testing.T) {
	var missing []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "vendor", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isBuildConfigType(lit.Type) {
				return true
			}
			hasNPU, hasAccel := false, false
			for _, e := range lit.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				switch id, _ := kv.Key.(*ast.Ident); id.Name {
				case "NPU":
					hasNPU = true
				case "Accel":
					hasAccel = true
				}
			}
			if hasNPU && !hasAccel {
				missing = append(missing, fset.Position(lit.Pos()).String())
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Fatalf("agent.BuildConfig literals wiring NPU without Accel (ADR 0037: every lane the box lists, on every builder):\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// isBuildConfigType matches `agent.BuildConfig` from outside the package and the
// bare `BuildConfig` from inside internal/agent.
func isBuildConfigType(expr ast.Expr) bool {
	switch x := expr.(type) {
	case *ast.SelectorExpr:
		return x.Sel.Name == "BuildConfig"
	case *ast.Ident:
		return x.Name == "BuildConfig"
	}
	return false
}

// TestEveryAgentBuilderThatGrantsBrowseWiresTheLaneAndItsTimeout pins ADR 0060 at
// the CALL SITES the same way: an agent.BuildConfig literal that sets AllowBrowse
// must also hand Build the lane (Browse) and its per-call cap (BrowseTimeout).
// Without the lane, Build refuses the grant with a note — a door that forgot it
// would silently never browse; without the timeout, the loop's generic 120 s tool
// cap would kill every real run long before the lane's own typed TIMEOUT.
func TestEveryAgentBuilderThatGrantsBrowseWiresTheLaneAndItsTimeout(t *testing.T) {
	var missing []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "vendor", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isBuildConfigType(lit.Type) {
				return true
			}
			has := map[string]bool{}
			for _, e := range lit.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok {
						has[id.Name] = true
					}
				}
			}
			if has["AllowBrowse"] && (!has["Browse"] || !has["BrowseTimeout"]) {
				missing = append(missing, fset.Position(lit.Pos()).String())
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Fatalf("agent.BuildConfig literals granting AllowBrowse without Browse and BrowseTimeout (ADR 0060):\n  %s",
			strings.Join(missing, "\n  "))
	}
}
