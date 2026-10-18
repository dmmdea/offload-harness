package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// The detached holder's maintain step scopes its drain and unload to the cards of ITS lease, found by listing the leases of the lease
// directory. A listing does not show a record whose rename is installing it (its live name is absent and only the staged file is
// there), so the holder's own held lease read as not live, the scope came back empty, and the drain and unload acted as for the whole
// node: every other resident model off the memory stack went, including seats on cards the lease does not hold. A lease whose record
// cannot be read right now is held, not gone, and the step must fail with that (detachedMaintainError says the lease is still held and
// `gpu release` frees it) instead of widening what it takes down.
func TestDetachedMaintainDoesNotWidenToTheWholeNodeWhileItsLeaseRecordIsMidRename(t *testing.T) {
	f := newMultiSeatSwap("agent-pool", "qwen3-vl-8b", "gemma-4-26b-agent")
	inner := swapWithRoster(f, "agent-pool", "qwen3-vl-8b", "gemma-4-26b-agent")
	defer inner.Close()
	// The rename that was installing the record ends when the holder first talks to llama-swap, which is after it has read its scope.
	var ends func()
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() {
			if ends != nil {
				ends()
			}
		})
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	old := maintenanceClient
	maintenanceClient = srv.Client()
	t.Cleanup(func() { maintenanceClient = old })
	cfgPath := cardLeaseWrapperFixture(t, srv.URL, "agent-pool")
	cfg := loadCfgPath(cfgPath)
	m, err := gpulease.OpenAt("", cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "detached card-2 render", Devices: []string{leaseCard2ID}, Draining: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })

	// The record is mid-rename: its live name is absent and the staged file beside it was just written.
	rec := filepath.Join(l.Dir(), "e", fmt.Sprintf("%d.json", l.Epoch()))
	data, err := os.ReadFile(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(rec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rec+".tmp", data, 0o666); err != nil {
		t.Fatal(err)
	}
	ends = func() {
		if err := os.Rename(rec+".tmp", rec); err != nil {
			t.Errorf("ending the rename: %v", err)
		}
	}

	var merr error
	out := captureStderr(t, func() {
		merr = detachMaintain(m, l.Epoch(), cfg, true, time.Now().Add(10*time.Second), true, true)
	})
	if merr == nil {
		t.Errorf("detachMaintain succeeded while the record of its lease was mid-rename when it read its scope, want a failure that leaves the lease held")
	}
	if got := sortedCalls(f); len(got) != 0 {
		t.Errorf("the detached holder took down %v while its lease record was mid-rename, want nothing: the cards it holds are not known, and it widened to the whole node\n%s", got, out)
	}
	if strings.Contains(out, "act as for the whole node") {
		t.Errorf("the holder widened its drain and unload to the whole node because its own held lease could not be listed:\n%s", out)
	}
}
