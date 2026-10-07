package displaystate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

func cfgIn(t *testing.T) config.Config {
	t.Helper()
	return config.Config{StateDir: t.TempDir(), DisplayWatchSec: 10}
}

func beat(t *testing.T, cfg config.Config, st State) {
	t.Helper()
	path, err := StatePath(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(path, st); err != nil {
		t.Fatal(err)
	}
}

func TestAliveFromAFreshHeartbeat(t *testing.T) {
	cfg := cfgIn(t)
	now := time.Now()
	beat(t, cfg, State{CheckedAt: now.Add(-8 * time.Second), IntervalSec: 10})
	alive, why := Alive(cfg, now)
	if !alive || !strings.Contains(why, "heartbeat") {
		t.Fatalf("a fresh heartbeat is a live watcher: %v %q", alive, why)
	}
}

func TestNotAliveWithoutAHeartbeatOrWithAStaleOne(t *testing.T) {
	cfg := cfgIn(t)
	now := time.Now()
	if alive, why := Alive(cfg, now); alive || !strings.Contains(why, "no heartbeat") {
		t.Fatalf("no state file is no watcher: %v %q", alive, why)
	}
	beat(t, cfg, State{CheckedAt: now.Add(-10 * time.Minute), IntervalSec: 10})
	if alive, why := Alive(cfg, now); alive || !strings.Contains(why, "10m0s old") {
		t.Fatalf("a heartbeat past StaleAfter is a dead watcher, with its age: %v %q", alive, why)
	}
	// The edge: just inside the bound is alive, just past it is not.
	beat(t, cfg, State{CheckedAt: now.Add(-StaleAfter(10*time.Second) + time.Second), IntervalSec: 10})
	if alive, _ := Alive(cfg, now); !alive {
		t.Fatal("a heartbeat just inside StaleAfter is alive")
	}
	beat(t, cfg, State{CheckedAt: now.Add(-StaleAfter(10*time.Second) - time.Second), IntervalSec: 10})
	if alive, _ := Alive(cfg, now); alive {
		t.Fatal("a heartbeat just past StaleAfter is not")
	}
}

// A watcher that is up but cannot read /running sees no twin to unload: not watching, however fresh.
func TestNotAliveWhileTheWatcherIsBlind(t *testing.T) {
	cfg := cfgIn(t)
	now := time.Now()
	since := now.Add(-time.Minute)
	beat(t, cfg, State{CheckedAt: now.Add(-2 * time.Second), IntervalSec: 10, ReadErr: "connection refused", BlindSince: &since})
	alive, why := Alive(cfg, now)
	if alive || !strings.Contains(why, "connection refused") || !strings.Contains(why, "/running") {
		t.Fatalf("a blind watcher is not alive, and says why: %v %q", alive, why)
	}
}

// display_watch_sec negative switches the check off; a layer that opens only when something watches it
// stays closed, and the reason is the switch, not a missing file.
func TestNotAliveWhenTheCheckIsSwitchedOff(t *testing.T) {
	cfg := cfgIn(t)
	cfg.DisplayWatchSec = -1
	beat(t, cfg, State{CheckedAt: time.Now(), IntervalSec: 10})
	alive, why := Alive(cfg, time.Now())
	if alive || !strings.Contains(why, "display_watch_sec") {
		t.Fatalf("the switch closes the layer: %v %q", alive, why)
	}
}

func TestAnUnparseableStateFileIsNoHeartbeat(t *testing.T) {
	cfg := cfgIn(t)
	path, _ := StatePath(cfg)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if alive, _ := Alive(cfg, time.Now()); alive {
		t.Fatal("garbage is not a heartbeat")
	}
}

func TestBlindFieldsRoundTripAndClearWhenOmitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFileName)
	since := time.Unix(1_700_000_000, 0).UTC()
	if err := Write(path, State{CheckedAt: time.Unix(1_700_000_100, 0), IntervalSec: 10, ReadErr: "boom", BlindSince: &since}); err != nil {
		t.Fatal(err)
	}
	st, ok := Read(path)
	if !ok || st.ReadErr != "boom" || st.BlindSince == nil || !st.BlindSince.Equal(since) {
		t.Fatalf("round trip: %+v %v", st, ok)
	}
	if err := Write(path, State{CheckedAt: time.Unix(1_700_000_200, 0), IntervalSec: 10}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "read_err") || strings.Contains(string(raw), "blind_since") {
		t.Fatalf("a watcher that can see again leaves no blind fields: %s", raw)
	}
}
