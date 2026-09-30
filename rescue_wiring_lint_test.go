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

// every production call of delegate.RunWith / delegate.RunBatched — the
// delegator surfaces agent_delegate, offload_research and the CLI verbs
// delegate and research — hands the engine its RunOptions.Rescue (register C-66,
// PR-4). The rescue is wired at four call sites and the CLI half has no test
// that ever reaches the engine (the verbs need a live planner seat), so the
// wiring is pinned at the source: a call that passes nil options, or a
// RunOptions literal without `Rescue`, fails here.
func TestEveryDelegatorSurfaceWiresTheRescue(t *testing.T) {
	fset := token.NewFileSet()
	var seen int
	var bad []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "runs", "tools", "docs", "testdata", ".bin", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "delegate" || (sel.Sel.Name != "RunWith" && sel.Sel.Name != "RunBatched") {
				return true
			}
			seen++
			last := call.Args[len(call.Args)-1]
			if u, ok := last.(*ast.UnaryExpr); ok {
				last = u.X
			}
			lit, ok := last.(*ast.CompositeLit)
			has := false
			if ok {
				for _, el := range lit.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Rescue" {
							has = true
						}
					}
				}
			}
			if !has {
				bad = append(bad, fset.Position(call.Pos()).String())
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 4 {
		t.Fatalf("found only %d delegate.RunWith/RunBatched call sites, want the four delegator surfaces: the scan is broken", seen)
	}
	if len(bad) > 0 {
		t.Fatalf("delegate engine calls that do not hand it a Rescue (a finished answer whose re-pack failed is lost work there): %v", bad)
	}
}
