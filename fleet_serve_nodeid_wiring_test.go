package main

import (
	"go/ast"
	"go/token"
	"testing"
)

// runFleetServe is too large to run in a test, so the one statement that gives the pipeline the node's resolved id is pinned at
// the source: `cfg = fleetServeConfig(cfg, nodeID)` (nodeID being what fleetServeParams returned) must come after
// fleetServeParams and before openPipeline(cfg) reads the config. Without it a node launched with --node-id names itself
// one way in /fleet/health and another on every result it stamps (the flag never reached the config). The assignment must
// target cfg itself: a call whose value is dropped (`_ = fleetServeConfig(...)`) fixes nothing.
func TestRunFleetServeAppliesItsNodeIDBeforeOpeningThePipeline(t *testing.T) {
	fset := token.NewFileSet()
	f := parseMainSplit(t, fset)
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "runFleetServe" {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("main.go has no runFleetServe: this pin has nothing to check")
	}

	calleeName := func(call *ast.CallExpr) string {
		if id, ok := call.Fun.(*ast.Ident); ok {
			return id.Name
		}
		return ""
	}
	var (
		paramsPos, applyPos, openPos token.Pos
		nodeIDName, openArg          string
		applyLHS                     ast.Expr
		applyCall                    *ast.CallExpr
		opens, applies, paramsCalls  int
	)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		switch calleeName(call) {
		case "fleetServeParams":
			paramsCalls++
			paramsPos = as.Pos()
			if len(as.Lhs) == 3 {
				if id, ok := as.Lhs[1].(*ast.Ident); ok {
					nodeIDName = id.Name
				}
			}
		case "fleetServeConfig":
			applies++
			applyPos, applyLHS, applyCall = as.Pos(), as.Lhs[0], call
		case "openPipeline":
			opens++
			openPos = as.Pos()
			if len(call.Args) == 1 {
				if id, ok := call.Args[0].(*ast.Ident); ok {
					openArg = id.Name
				}
			}
		}
		return true
	})

	if paramsCalls != 1 || nodeIDName == "" {
		t.Fatalf("runFleetServe must resolve its id once with `listen, nodeID, err := fleetServeParams(...)`: found %d call(s), node id variable %q", paramsCalls, nodeIDName)
	}
	if opens != 1 || openArg != "cfg" {
		t.Fatalf("runFleetServe must open its pipeline once with openPipeline(cfg): found %d call(s), argument %q", opens, openArg)
	}
	if applies != 1 {
		t.Fatalf("runFleetServe must apply its node id to the config once with `cfg = fleetServeConfig(cfg, %s)`: found %d assignment(s)", nodeIDName, applies)
	}
	if id, ok := applyLHS.(*ast.Ident); !ok || id.Name != "cfg" {
		t.Errorf("fleetServeConfig's value must be assigned to cfg, the config openPipeline reads: it is assigned to %T %v", applyLHS, applyLHS)
	}
	if len(applyCall.Args) != 2 {
		t.Fatalf("fleetServeConfig takes (cfg, nodeID): got %d argument(s)", len(applyCall.Args))
	}
	if id, ok := applyCall.Args[0].(*ast.Ident); !ok || id.Name != "cfg" {
		t.Errorf("fleetServeConfig's config argument must be cfg: it is %v", applyCall.Args[0])
	}
	if id, ok := applyCall.Args[1].(*ast.Ident); !ok || id.Name != nodeIDName {
		t.Errorf("fleetServeConfig's id argument must be %s, the id fleetServeParams resolved (the one health and the claim use): it is %v", nodeIDName, applyCall.Args[1])
	}
	if applyPos < paramsPos {
		t.Errorf("the node id is applied before fleetServeParams resolves it")
	}
	if applyPos > openPos {
		t.Errorf("the node id is applied after openPipeline(cfg) read the config: the pipeline stamps the id it was opened with")
	}
}
