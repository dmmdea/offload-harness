package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestNoNewBareHTTPClient makes ADR 0042 executable (security framework gate G2,
// register SF-.. S-03): every place that builds its own HTTP client — a bare
// http.Client or http.Transport literal, http.Get/Post/Head/PostForm, or
// http.DefaultClient — is listed in bareClientAllowlist with the reason it may
// exist. A NEW site fails here until someone reviews whether it dials a
// caller-named host (then it must use netguard's pinned transport, ADR 0042) or
// a configured endpoint (then it is listed, with that reason). A site that
// disappears must be removed from the list too, so the list stays the truth.
//
// Keyed by file and enclosing function, counted: a second client added inside
// an already-listed function fails as well. tools/ is a separate module and is
// out of scope, like the fence lints beside this file.
func TestNoNewBareHTTPClient(t *testing.T) {
	got := bareHTTPClientSites(t)
	if os.Getenv("BARE_CLIENT_LINT_PRINT") != "" {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("\t%q: {%d, \"\"},\n", k, got[k])
		}
		t.Skip("printed the current sites")
	}
	for key, n := range got {
		want, ok := bareClientAllowlist[key]
		switch {
		case !ok:
			t.Errorf("%s builds %d bare HTTP client(s) and is not in bareClientAllowlist: if it dials a host a caller names, use netguard's pinned transport (ADR 0042); if it dials a configured endpoint, add it with that reason", key, n)
		case n > want.count:
			t.Errorf("%s builds %d bare HTTP client(s), the allowlist says %d: review the new one (ADR 0042) and update the count with its reason", key, n, want.count)
		case n < want.count:
			t.Errorf("%s builds %d bare HTTP client(s), the allowlist says %d: lower the count so the list stays the truth", key, n, want.count)
		}
	}
	for key := range bareClientAllowlist {
		if _, ok := got[key]; !ok {
			t.Errorf("%s is in bareClientAllowlist but builds no bare HTTP client any more: remove it", key)
		}
	}
	for key, e := range bareClientAllowlist {
		if strings.TrimSpace(e.why) == "" {
			t.Errorf("%s: allowlist entry carries no reason", key)
		}
	}
}

// bareHTTPClientSites walks every non-test Go file outside tools/ and returns
// "file:func" -> number of bare-client constructions in it.
func bareHTTPClientSites(t *testing.T) map[string]int {
	t.Helper()
	fset := token.NewFileSet()
	sites := map[string]int{}
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
		httpName := ""
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "net/http" {
				httpName = "http"
				if imp.Name != nil {
					httpName = imp.Name.Name
				}
			}
		}
		if httpName == "" || httpName == "_" {
			return nil
		}
		isHTTP := func(e ast.Expr, names ...string) bool {
			sel, ok := e.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != httpName {
				return false
			}
			for _, n := range names {
				if sel.Sel.Name == n {
					return true
				}
			}
			return false
		}
		for _, decl := range f.Decls {
			fn := "<file-scope>"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
				if fd.Recv != nil && len(fd.Recv.List) == 1 {
					fn = recvName(fd.Recv.List[0].Type) + "." + fn
				}
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					if isHTTP(x.Type, "Client", "Transport") {
						sites[rel+":"+fn]++
					}
				case *ast.CallExpr:
					if isHTTP(x.Fun, "Get", "Post", "Head", "PostForm") {
						sites[rel+":"+fn]++
					}
				case *ast.SelectorExpr:
					if isHTTP(x, "DefaultClient") {
						sites[rel+":"+fn]++
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sites
}

func recvName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return recvName(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr:
		return recvName(x.X)
	case *ast.IndexListExpr:
		return recvName(x.X)
	}
	return "?"
}
