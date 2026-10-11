package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestEveryRawLedgerReaderFiltersTestTraffic keeps register F48 (job-error class C23) from coming back one
// reader at a time. ledger.ReadAll is the raw reader: it returns the test, bake and measurement rows too. A
// function outside the ledger package that calls it must also drop those rows (productionEntries,
// ledger.ProductionRows, ledger.ReadProduction or ledger.ClassOf in the same function) or be listed here with
// the reason it reads something that is not the production ledger. A new report that sums the raw rows fails
// here until someone decides which it is.
func TestEveryRawLedgerReaderFiltersTestTraffic(t *testing.T) {
	// file:function -> why a raw read of the ledger package's reader is right there.
	allowed := map[string]string{
		"loupe_agreement.go:readAgreement": "reads the correctness-label sidecar (confhead-labels.jsonl), whose rows carry no run class: the sidecar is a different file from the ledger",
	}
	fset := token.NewFileSet()
	var raw []string
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
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			var reads, filters bool
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					if id, ok := fn.X.(*ast.Ident); ok && id.Name == "ledger" {
						switch fn.Sel.Name {
						case "ReadAll":
							reads = true
						case "ProductionRows", "ReadProduction", "ClassOf":
							filters = true
						}
					}
				case *ast.Ident:
					if fn.Name == "productionEntries" {
						filters = true
					}
				}
				return true
			})
			if reads && !filters {
				raw = append(raw, filepath.ToSlash(path)+":"+fd.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(raw)
	seen := map[string]bool{}
	for _, site := range raw {
		seen[site] = true
		if _, ok := allowed[site]; !ok {
			t.Errorf("%s reads the ledger with the raw ledger.ReadAll and drops no test traffic: use productionEntries / ledger.ReadProduction (register F48), or list it in this test with the reason it is not the production ledger", site)
		}
	}
	for site, why := range allowed {
		if !seen[site] {
			t.Errorf("%s is allowed to read raw but no longer does: remove it from the list", site)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("%s: allowlist entry carries no reason", site)
		}
	}
}
