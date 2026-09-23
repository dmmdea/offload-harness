package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestUpstreamURLsAreBuiltOnlyBehindTheFence pins the invariant of 2026-09-22:
// while a GPU lease fences the cards (a media render, or a text hold stamped
// exclusive), no harness request may make llama-swap LOAD a model. llama-swap
// starts any model a request under /upstream/<model>/… names, so every such
// request must come from internal/modelaffinity's AwaitUpstream, which refuses
// it unless the model is already resident or the card is free.
//
// The incident this closes: the served-window probe, the seat-pin probe and the
// tokenizer each spelled the route themselves, outside every gate, and an agent
// run admitted just before a video render loaded the 3-card seat onto the
// render's cards. A new probe that spells the route again fails here, not on a
// render.
//
// Two rules, checked on the Go AST of every non-test file (comments and error
// prose are free to NAME the route; only a string literal that BUILDS it counts):
//
//  1. a string literal containing "/upstream/" appears only in
//     internal/modelaffinity/upstream.go;
//  2. modelaffinity.HolderUpstreamURL — the one unfenced builder, for the lease
//     holder's own warm-back — is called only from gpu_drain.go.
//
// tools/ is a separate module (the vendored llama-swap client); its passthrough
// methods refuse a model that is not in /running (requireLoaded), so they cannot
// load one.
func TestUpstreamURLsAreBuiltOnlyBehindTheFence(t *testing.T) {
	const builder = "internal/modelaffinity/upstream.go"
	holderCallers := map[string]bool{"gpu_drain.go": true}
	fset := token.NewFileSet()
	var offenders []string
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "tools", "testdata", "node_modules", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(path)
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind != token.STRING || rel == builder {
					return true
				}
				if v, uerr := strconv.Unquote(x.Value); uerr == nil && strings.Contains(v, "/upstream/") {
					offenders = append(offenders, fset.Position(x.Pos()).String()+": builds an /upstream/ URL outside the fence (use modelaffinity.AwaitUpstream)")
				}
			case *ast.SelectorExpr:
				if x.Sel.Name == "HolderUpstreamURL" && !holderCallers[rel] {
					offenders = append(offenders, fset.Position(x.Pos()).String()+": calls the unfenced HolderUpstreamURL (reserved for the lease holder's warm-back in gpu_drain.go)")
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range offenders {
		t.Error(o)
	}
}
