package main

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// The startup line says which lanes are open, in the words the operator can act on, on every start. There are two
// states it can describe (fleetServeParams refuses the third, a listener beyond loopback with no token).
func TestFleetServeStartupLineSaysWhichLanesAreOpen(t *testing.T) {
	loopbackNoToken := fleetServeAuthNote("127.0.0.1:18811", config.Config{})
	if !strings.HasPrefix(loopbackNoToken, "loopback only: every fleet lane is tokenless on 127.0.0.1:18811") {
		t.Errorf("loopback with no token: %q", loopbackNoToken)
	}
	tokened := config.Config{FleetAuthToken: "a-token"}
	for _, listen := range []string{"192.0.2.7:18811", "127.0.0.1:18811"} {
		line := fleetServeAuthNote(listen, tokened)
		if !strings.HasPrefix(line, "every lane requires the fleet bearer") {
			t.Errorf("token set, listen %s: %q", listen, line)
		}
		if strings.Contains(line, "a-token") {
			t.Errorf("the startup line carries the token: %q", line)
		}
	}
	if line := fleetServeAuthNote("192.0.2.7:18811", tokened); !strings.Contains(line, "beyond loopback") {
		t.Errorf("a listener beyond loopback is not named as such: %q", line)
	}
	for _, line := range []string{loopbackNoToken, fleetServeAuthNote("192.0.2.7:18811", tokened)} {
		if strings.Contains(line, "UNAUTHENTICATED") {
			t.Errorf("the startup line still calls the endpoints UNAUTHENTICATED: %q", line)
		}
	}
}

// runFleetServe prints that line on every start: the old warning was printed only with --listen-trusted-network and
// said the endpoints were unauthenticated, which stopped being true.
func TestFleetServePrintsTheAuthLineOnEveryStart(t *testing.T) {
	src := mainSplitSource(t)
	i := strings.Index(src, "func runFleetServe(")
	if i < 0 {
		t.Fatal("runFleetServe not found")
	}
	body := src[i:]
	if j := strings.Index(body, "\nfunc "); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "fleetServeAuthNote(listen, cfg)") {
		t.Error("runFleetServe does not print fleetServeAuthNote")
	}
	if strings.Contains(body, "UNAUTHENTICATED") {
		t.Error("runFleetServe still warns that the endpoints are UNAUTHENTICATED")
	}
}
