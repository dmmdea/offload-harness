package main

import (
	"go/ast"
	"go/token"
	"testing"
)

// The `research` verb publishes the same digests as offload_research, and its
// half of the register F4 check (each digest's quotes looked up in the page it
// came from) has no test that reaches it: the verb fetches the live web and
// needs a planner seat. The wiring is pinned at the source, the way
// TestEveryDelegatorSurfaceWiresTheRescue pins the rescue: runResearch must call
// research.AnnotateWire on the wire it built, after delegate.WireResponse and
// before it marshals the body.
func TestTheResearchVerbChecksItsQuotesAgainstThePages(t *testing.T) {
	fset := token.NewFileSet()
	f := parseMainSplit(t, fset)
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "runResearch" && fd.Recv == nil {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("runResearch not found in main.go: the scan is broken")
	}
	var wire, annotate, marshal token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch {
		case x.Name == "delegate" && sel.Sel.Name == "WireResponse":
			wire = call.Pos()
		case x.Name == "research" && sel.Sel.Name == "AnnotateWire":
			annotate = call.Pos()
		case x.Name == "json" && sel.Sel.Name == "MarshalIndent":
			marshal = call.Pos()
		}
		return true
	})
	if wire == token.NoPos || marshal == token.NoPos {
		t.Fatal("runResearch no longer builds its body with delegate.WireResponse and json.MarshalIndent: the scan is broken")
	}
	if annotate == token.NoPos {
		t.Fatal("runResearch does not call research.AnnotateWire: its digests' quotes would reach the caller unchecked against the pages (register F4)")
	}
	if !(wire < annotate && annotate < marshal) {
		t.Fatal("research.AnnotateWire must run on the wire after delegate.WireResponse and before the body is marshalled")
	}
}
