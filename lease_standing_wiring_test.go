package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The fleet node publishes what its lease is doing only if main hands the node a standing
// reader (fleetnode.Options.LeaseStanding). Serving needs a live node, so the wiring is
// pinned at the source: the Options literal main builds for the fleet node must carry it,
// and what it carries must be fleetLeaseStanding(cfg), the reader built from the node's own
// config (lease directory and orphan grace).
func TestFleetServeWiresTheLeaseStanding(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var wired, literals int
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := cl.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Options" {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "fleetnode" {
			return true
		}
		literals++
		for _, e := range cl.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "LeaseStanding" {
				if call, ok := kv.Value.(*ast.CallExpr); ok {
					if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "fleetLeaseStanding" {
						wired++
					}
				}
			}
		}
		return true
	})
	if literals == 0 {
		t.Fatal("main.go builds no fleetnode.Options literal: this pin has nothing to check")
	}
	if wired != literals {
		t.Fatalf("%d of %d fleetnode.Options literals in main.go wire LeaseStanding: fleetLeaseStanding(cfg)", wired, literals)
	}
}
