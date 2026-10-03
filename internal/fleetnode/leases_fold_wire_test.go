package fleetnode_test

// The folded singular lease block, read by the delegator's real decoder over a real health
// handler. Every reader of the singular fields computes LeaseBusy = Busy && !Overdue, so what a
// node with an abandoned lease on one card and a live long one on another publishes decides
// whether those readers fence it. (The unit-level fold is in leases_fold_test.go.)

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

func foldView(t *testing.T, leases ...gpulease.Info) delegate.NodeView {
	t.Helper()
	cfg := config.Config{FleetMaxConcurrentJobs: 1, FleetMaxQueueDepth: 4}
	jobs := fleetnode.NewJobs(time.Hour, cfg.FleetConcurrencyLimit())
	t.Cleanup(func() { jobs.DrainAndStop(2 * time.Second) })
	devs := []fleetnode.GPUDevice{
		{Index: 0, UUID: "GPU-1111aaaa-2222-3333-4444-555566667777", Name: "synthetic 16 GB", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
		{Index: 1, UUID: "GPU-3333cccc-4444-5555-6666-777788889999", Name: "synthetic 16 GB", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
	}
	info := leases[0]
	info.Leases = leases
	info.Epochs = nil
	for _, l := range leases {
		info.Epochs = append(info.Epochs, l.Epoch)
	}
	srv := fleetnode.New(nopRunner{}, jobs, fleetnode.Options{
		NodeID: "fold-node", Version: "test",
		Snapshot: func() (fleetnode.Snapshot, bool) {
			return fleetnode.Snapshot{TotalGiB: 16, FreeGiB: 15, Devices: devs, At: time.Now()}, true
		},
		Lease: func() gpulease.Info { return info },
		Cfg:   cfg,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	view, err := delegate.FetchNodeView(context.Background(), ts.URL, "")
	if err != nil {
		t.Fatalf("the delegator's health decoder rejected the payload: %v", err)
	}
	return view
}

func mediaLease(epoch uint64, card string, remaining time.Duration) gpulease.Info {
	return gpulease.Info{
		Held: true, Class: gpulease.ClassMedia, Epoch: epoch, Epochs: []uint64{epoch}, PID: 4000 + int(epoch),
		ExpiresAt: time.Now().Add(remaining), Devices: []string{card},
	}
}

const (
	foldCardA = "gpu-1111aaaa-2222-3333-4444-555566667777"
	foldCardC = "gpu-3333cccc-4444-5555-6666-777788889999"
)

// A reader that reads only the singular fields must still fence a node that holds a live long
// render, even when an abandoned lease sits on another card: the abandoned lease must not turn
// the node's reading into "overdue, not busy, ranked last, never fenced".
func TestAnAbandonedLeaseDoesNotHideALiveRenderFromTheSingularReaders(t *testing.T) {
	view := foldView(t, mediaLease(1, foldCardC, -time.Hour), mediaLease(2, foldCardA, 6*time.Hour))
	if !view.LeaseBusy {
		t.Fatalf("LeaseBusy = false (LeaseOverdue %v): a live 6 h render holds card A, and the readers of the singular fields would take the node as free", view.LeaseOverdue)
	}
	if view.LeaseOverdue {
		t.Fatalf("LeaseOverdue = true beside a live long lease: the node is not abandoned")
	}
	// The same lease alone is exactly that reading, so the fold tells the old reader what the
	// live lease alone would.
	alone := foldView(t, mediaLease(2, foldCardA, 6*time.Hour))
	if alone.LeaseBusy != view.LeaseBusy || alone.LeaseOverdue != view.LeaseOverdue {
		t.Fatalf("with the abandoned lease busy=%v overdue=%v, alone busy=%v overdue=%v: the abandoned lease changed what the old reader is told",
			view.LeaseBusy, view.LeaseOverdue, alone.LeaseBusy, alone.LeaseOverdue)
	}
}

// Every lease abandoned: the old reader ranks the node last and never fences it, as for one.
func TestEveryLeaseAbandonedIsOverdueNotBusyForTheSingularReaders(t *testing.T) {
	view := foldView(t, mediaLease(1, foldCardC, -time.Hour), mediaLease(2, foldCardA, -3*time.Hour))
	if view.LeaseBusy || !view.LeaseOverdue {
		t.Fatalf("busy=%v overdue=%v, want overdue and not busy: nothing live holds a card", view.LeaseBusy, view.LeaseOverdue)
	}
}
