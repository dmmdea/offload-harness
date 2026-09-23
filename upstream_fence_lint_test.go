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

// llamaSwapModelRoutes are llama-swap's MODEL-DISPATCHED routes (v251
// internal/server/server.go: modelPostJSONRoutes, modelPostFormRoutes and the
// model-bearing GET routes; the versionless /v/… twins strip to these). Each one
// reads the model from the request and swaps it in, exactly as /upstream does.
// Bare GET /props is omitted on purpose: without ?model= llama-swap refuses it
// (ErrNoModelInContext), so the window probe's bare-root fallback cannot load.
var llamaSwapModelRoutes = []string{
	"/v1/chat/completions", "/v1/responses", "/v1/completions", "/v1/messages",
	"/v1/messages/count_tokens", "/v1/embeddings", "/reranking", "/rerank",
	"/v1/rerank", "/v1/reranking", "/infill", "/completion", "/v1/audio/speech",
	"/v1/audio/voices", "/v1/audio/transcriptions", "/v1/images/generations",
	"/v1/images/edits", "/sdapi/v1/txt2img", "/sdapi/v1/img2img", "/sdapi/v1/loras",
	"/audioapi/v1/tasks/run",
}

// gatedRouteFiles are the files allowed to spell a model-dispatched route
// outside a gated builder call, each with the reason it cannot load a model onto
// a fenced card. requiresAdmit marks the ones whose reason IS "the send takes
// modelaffinity.Admit" — the test checks that call is still there.
var gatedRouteFiles = map[string]struct {
	why           string
	requiresAdmit bool
}{
	"internal/llamaclient/client.go":  {"every send goes through sendWithSeatWait, which takes modelaffinity.Admit / AdmitOffBox first", true},
	"internal/agent/client.go":        {"Chat takes modelaffinity.Admit before its request goes out", true},
	"internal/config/config.go":       {"the CompletionPath default, consumed only by llamaclient (gated above)", false},
	"internal/pipeline/agenttask.go":  {"repackClient hands the path to llamaclient (gated above)", false},
	"internal/sttclient/sttclient.go": {"an /upstream subpath handed to fencedURL, which is modelaffinity.AwaitUpstream", false},
	"internal/ttsclient/ttsclient.go": {"tts_endpoint is a separate OpenAI-shaped speech server that owns its GPU, never llama-swap (ttsclient package doc)", false},
	"cmd/local-agent/serve.go":        {"a route this process SERVES, not a request it sends", false},
}

// TestModelDispatchedRoutesAreBuiltOnlyBehindAGate extends the /upstream rule to
// llama-swap's model-dispatched routes (2026-09-22, the fleet chat lane). A
// string literal that spells one of them — alone or at the end of a URL — must
// be an argument of modelaffinity.AwaitModelRoute / AwaitUpstream, or sit in a
// file of gatedRouteFiles. A literal carrying whitespace is prose (an error or
// log message) and is skipped. The chat lane forwarded /v1/chat/completions with
// no gate at all, and the /upstream-only rule could not see it.
func TestModelDispatchedRoutesAreBuiltOnlyBehindAGate(t *testing.T) {
	fset := token.NewFileSet()
	var offenders []string
	admitSeen := map[string]bool{}
	isRoute := func(v string) bool {
		if strings.ContainsAny(v, " \t\n") {
			return false
		}
		for _, r := range llamaSwapModelRoutes {
			if strings.HasSuffix(v, r) {
				return true
			}
		}
		return false
	}
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
		gatedArgs := map[token.Pos]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "AwaitModelRoute" || sel.Sel.Name == "AwaitUpstream") {
					for _, a := range x.Args {
						gatedArgs[a.Pos()] = true
					}
				}
			case *ast.SelectorExpr:
				// A call OR a reference (llamaclient picks Admit or AdmitOffBox
				// into a variable before calling it).
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "modelaffinity" && (x.Sel.Name == "Admit" || x.Sel.Name == "AdmitOffBox") {
					admitSeen[rel] = true
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, uerr := strconv.Unquote(lit.Value)
			if uerr != nil || !isRoute(v) || gatedArgs[lit.Pos()] {
				return true
			}
			if _, allowed := gatedRouteFiles[rel]; allowed {
				return true
			}
			offenders = append(offenders, fset.Position(lit.Pos()).String()+": spells the model-dispatched llama-swap route "+strconv.Quote(v)+" outside a gate (use modelaffinity.AwaitModelRoute, or take modelaffinity.Admit and list the file with its reason)")
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
	for file, entry := range gatedRouteFiles {
		if entry.requiresAdmit && !admitSeen[file] {
			t.Errorf("%s is allowed to spell a model-dispatched route because %s — and no modelaffinity.Admit call is left in it", file, entry.why)
		}
	}
}
