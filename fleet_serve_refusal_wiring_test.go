package main

import (
	"go/ast"
	"go/token"
	"testing"
)

// runFleetServe is too large to run in a test, so the call that gives the startup refusal the config the node read is
// pinned at the source: fleetServeParams takes the config.Source (the path loadCfgWithSource resolved) as its fifth
// argument, and runFleetServe passes the one it loaded, not an empty one. Without it the refusal names the key but not
// the file, and a node that will not start cannot say which config to fix.
func TestRunFleetServePassesTheConfigItReadToTheStartupRefusal(t *testing.T) {
	fset := token.NewFileSet()
	f := parseMainSplit(t, fset)
	var calls []*ast.CallExpr
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "runFleetServe" {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "fleetServeParams" {
					calls = append(calls, call)
				}
			}
			return true
		})
	}
	if len(calls) != 1 {
		t.Fatalf("runFleetServe calls fleetServeParams %d times, want once", len(calls))
	}
	args := calls[0].Args
	if len(args) != 6 {
		t.Fatalf("fleetServeParams takes (listen, nodeID, trusted, cfg, src, hostname): got %d argument(s)", len(args))
	}
	if id, ok := args[4].(*ast.Ident); !ok || id.Name != "src" {
		t.Errorf("the fifth argument must be src, the config.Source runFleetServe loaded: it is %T %v", args[4], args[4])
	}
}
