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

// TestEveryNodeConfigAgentDoorWiresTheEvidenceReducer pins agent_evidence_reducer at the
// CALL SITES, the way the accelerator lanes and the browse grant are pinned: every
// agent.BuildConfig literal that takes the node's agent policy from its config (it sets
// ReadFloor) must also hand Build the reducer key, read from the config's
// AgentEvidenceReducer. A door that forgot it would silently never reduce while the
// operator's key said it should. The prompt-replay builder takes no node policy and is
// not one of them.
func TestEveryNodeConfigAgentDoorWiresTheEvidenceReducer(t *testing.T) {
	var missing, wrong []string
	doors := 0
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
			var hasFloor, hasReducer bool
			for _, e := range lit.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				id, _ := kv.Key.(*ast.Ident)
				if id == nil {
					continue
				}
				switch id.Name {
				case "ReadFloor":
					hasFloor = true
				case "EvidenceReducer":
					hasReducer = true
					if sel, ok := kv.Value.(*ast.SelectorExpr); !ok || sel.Sel.Name != "AgentEvidenceReducer" {
						wrong = append(wrong, fset.Position(kv.Pos()).String())
					}
				}
			}
			if hasFloor {
				doors++
				if !hasReducer {
					missing = append(missing, fset.Position(lit.Pos()).String())
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if doors < 5 {
		t.Fatalf("found %d node-config agent doors, want at least 5 (local-agent x3, the MCP front door, the fleet-node agent task): the scan is broken", doors)
	}
	if len(missing) > 0 {
		t.Fatalf("agent.BuildConfig literals that take the node's agent policy but not agent_evidence_reducer:\n  %s", strings.Join(missing, "\n  "))
	}
	if len(wrong) > 0 {
		t.Fatalf("EvidenceReducer must be read from the config's AgentEvidenceReducer:\n  %s", strings.Join(wrong, "\n  "))
	}
}
