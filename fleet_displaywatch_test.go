package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/displaywatch"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/placement"
)

// The display layer's post-admission guard runs inside fleet-serve. Serving needs a live node, so the
// wiring is pinned at the source: runFleetServe must start it and must stop it on the way out
// (`stop := startDisplayWatch(...)` and `defer stop()`), or a node that never starts the guard looks
// exactly like one that does until a twin outlives the operator's return.
func TestFleetServeStartsAndStopsTheDisplayWatch(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "runFleetServe" {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("main.go has no runFleetServe: this pin has nothing to check")
	}
	stopName := ""
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "startDisplayWatch" {
			if lhs, ok := as.Lhs[0].(*ast.Ident); ok {
				stopName = lhs.Name
			}
		}
		return true
	})
	if stopName == "" {
		t.Fatal("runFleetServe never starts the display watch: `stop := startDisplayWatch(ctx, cfg, sampler)`")
	}
	deferred := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if ds, ok := n.(*ast.DeferStmt); ok {
			if id, ok := ds.Call.Fun.(*ast.Ident); ok && id.Name == stopName {
				deferred = true
			}
		}
		return true
	})
	if !deferred {
		t.Fatalf("runFleetServe starts the display watch but never defers %s(): the node would exit with the check still running", stopName)
	}
}

// countingDeps is a display-watch box whose /running reads are counted and whose desk is "present".
func countingDeps(reads *atomic.Int32) displaywatch.Deps {
	return displaywatch.Deps{
		Running: func(context.Context) ([]displaywatch.Model, error) {
			reads.Add(1)
			return nil, nil
		},
		Unload:   func(context.Context, string) error { return nil },
		Devices:  func() ([]gpuprobe.Device, bool) { return nil, false },
		Presence: func() placement.Presence { return placement.Presence{Mode: "present", Known: true} },
		Logf:     func(string, ...any) {},
	}
}

func displayLayerCfg() config.Config {
	cfg := config.CompositeFixture()
	for i := range cfg.Layers {
		if cfg.Layers[i].Name == "display" {
			cfg.Layers[i].Dormant = false
		}
	}
	return cfg
}

// The node starts the guard, which checks at once, and stops it when it stops: stop returns only after
// the check in flight has finished, so fleet-serve never exits with a check mid-unload.
func TestStartDisplayWatchChecksAtOnceAndStopWaitsForTheCheckInFlight(t *testing.T) {
	var reads atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	deps := countingDeps(&reads)
	deps.Running = func(context.Context) ([]displaywatch.Model, error) {
		reads.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release // the check is mid-flight until the test lets it finish
		return nil, nil
	}
	stop := startDisplayWatchWith(context.Background(), displayLayerCfg(), deps)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the guard must check as soon as it starts, not after the first period")
	}
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop returned while a check was still in flight: the node could exit mid-unload")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop must return once the check in flight has finished")
	}
	after := reads.Load()
	time.Sleep(50 * time.Millisecond)
	if reads.Load() != after {
		t.Fatal("no check may run after stop returned")
	}
}

func TestStartDisplayWatchIsInertWithoutALayerToWatchOrWhenSwitchedOff(t *testing.T) {
	var reads atomic.Int32
	stop := startDisplayWatchWith(context.Background(), config.Config{}, countingDeps(&reads))
	stop()
	off := displayLayerCfg()
	off.DisplayWatchSec = -1
	stop = startDisplayWatchWith(context.Background(), off, countingDeps(&reads))
	stop()
	time.Sleep(30 * time.Millisecond)
	if n := reads.Load(); n != 0 {
		t.Fatalf("a plain box and a switched-off guard read nothing, got %d /running reads", n)
	}
}
