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

// TestEveryFleetClientRequestSetsTheBearer pins, for code that does not exist yet, what the per-client
// tests (TestEveryRequestCarriesTheFleetBearer in each client package) pin for the code that does: a
// fleet node requires the fleet bearer on every lane, so a function that builds an HTTP request to one
// must set it. A request builder in a fleet client package, or in any file that names a "/fleet/" route,
// has to call that package's auth helper or set the Authorization header itself; a request that
// deliberately goes without (an identity read of a base that may not be a fleet node, a call to a local
// engine) is listed in fleetBearerAllowlist with the reason, and a stale entry fails too.
//
// Keyed by file and enclosing function. tools/ is a separate module and out of scope.
func TestEveryFleetClientRequestSetsTheBearer(t *testing.T) {
	got := fleetRequestSitesWithoutBearer(t)
	if os.Getenv("FLEET_BEARER_LINT_PRINT") != "" {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("\t%q: \"\",\n", k)
		}
		t.Skip("printed the current sites")
	}
	for key := range got {
		if _, ok := fleetBearerAllowlist[key]; !ok {
			t.Errorf("%s builds an HTTP request to a fleet node without setting the fleet bearer: call the package's auth helper, or, if the request deliberately carries none, list it in fleetBearerAllowlist with the reason", key)
		}
	}
	for key, why := range fleetBearerAllowlist {
		if _, ok := got[key]; !ok {
			t.Errorf("%s is in fleetBearerAllowlist but no longer builds a request without the bearer: remove it", key)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("%s: allowlist entry carries no reason", key)
		}
	}
}

// fleetClientDirs are the packages whose HTTP requests go to a fleet node.
var fleetClientDirs = map[string]bool{
	"internal/mediaremote":   true,
	"internal/composeremote": true,
	"internal/sttremote":     true,
	"internal/textremote":    true,
	"internal/visionremote":  true,
	"internal/accelremote":   true,
	"internal/delegate":      true,
	"internal/fleetview":     true,
	"internal/pairworkloads": true,
	"internal/rosterprobe":   true,
	"internal/llamaclient":   true,
}

// fleetBearerAllowlist: requests that go without the bearer, and why.
var fleetBearerAllowlist = map[string]string{
	"internal/fleetnode/chat_lane.go:handleChat":       "the node's own forward to its loopback llama-swap, not a request to a fleet node",
	"internal/fleetnode/kvslots.go:handleKVSlot":       "the node's own forward to its loopback llama-swap, not a request to a fleet node",
	"internal/llamaclient/client.go:Health":            "llama-swap's own /health on the client's endpoint, not a fleet node",
	"internal/llamaclient/lanes.go:probeFleetHealth":   "the identity read of a cascade lane base that may be a plain llama-swap: health is open, and the bearer must never be offered to a base that is not a fleet node",
	"internal/pairworkloads/nodeinfo.go:probeHostUUID": "PAIR's loopback node-info, not a fleet node",
	"internal/pairworkloads/nodeinfo.go:probeIngress":  "PAIR's loopback ingress, not a fleet node",
	"internal/pairworkloads/pairworkloads.go:post":     "a frame to PAIR's own ingress, not a fleet node (the relay to a fleet node is postRelay, which sets the bearer)",
	"internal/pairworkloads/seatwatch.go:get":          "a read of this box's own llama-swap, not a fleet node",
}

func fleetRequestSitesWithoutBearer(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	sites := map[string]string{}
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
		namesFleetRoute := false
		ast.Inspect(f, func(n ast.Node) bool {
			if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING && strings.Contains(bl.Value, "/fleet/") {
				namesFleetRoute = true
			}
			return true
		})
		inClientPkg := fleetClientDirs[filepath.ToSlash(filepath.Dir(path))] || rel == "internal/fleetnode/claimloop.go"
		if !inClientPkg && !namesFleetRoute {
			return nil
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			buildsRequest, setsBearer := false, false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					switch fun := x.Fun.(type) {
					case *ast.SelectorExpr:
						if id, ok := fun.X.(*ast.Ident); ok && id.Name == "http" && (fun.Sel.Name == "NewRequest" || fun.Sel.Name == "NewRequestWithContext") {
							buildsRequest = true
						}
					case *ast.Ident:
						if fun.Name == "auth" {
							setsBearer = true
						}
					}
				case *ast.BasicLit:
					if x.Kind == token.STRING && strings.Contains(x.Value, "Authorization") {
						setsBearer = true
					}
				}
				return true
			})
			if buildsRequest && !setsBearer {
				sites[rel+":"+fd.Name.Name] = ""
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sites
}
