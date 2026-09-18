package vllmseat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSeatLauncherBindsTheMPHTTPFrontendOnLoopback (2026-09-18). LMCache's MP
// server opens an HTTP frontend beside its ZMQ port and defaults it to
// 0.0.0.0:8080. Under WSL2 mirrored networking that is the host's every
// interface, and 8080 is somebody else's port on every box in this fleet; the
// production MP server was found logging `Uvicorn running on http://0.0.0.0:8080`.
// The launcher must bind it to loopback on its own port, refuse a squatted
// port the way it refuses a squatted engine port, and the env template must
// carry the render token so an installed seat gets the value from its spec.
func TestSeatLauncherBindsTheMPHTTPFrontendOnLoopback(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "vllm-seat", "seat_fg.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, must := range []string{
		`MP_HTTP_PORT="${SEAT_MP_HTTP_PORT:-18793}"`,
		`if ss -ltnp 2>/dev/null | grep -q ":$MP_HTTP_PORT "; then`,
		`--http-host 127.0.0.1 --http-port "$MP_HTTP_PORT"`,
	} {
		if !strings.Contains(s, must) {
			t.Fatalf("seat_fg.sh no longer binds the LMCache MP HTTP frontend to loopback on its own port (LMCache defaults it to 0.0.0.0:8080): %q missing", must)
		}
	}
	env, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "vllm-seat", "windows-wsl", "seat.env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "SEAT_MP_HTTP_PORT=__MP_HTTP_PORT__") {
		t.Fatalf("seat.env template lost the SEAT_MP_HTTP_PORT render token")
	}
	// The renderer fills the token from the spec, defaulted beside the engine port
	// (18797 engine / 18796 MP ZMQ / 18793 MP HTTP) and never colliding with the
	// benchmark arm's pairing on the reference box (18798 / 18794 / 18795).
	spec := Spec{ID: "qwen3.8-27b-vllm", Unit: "vllm-agent-seat-27b", Port: 18797, MPPort: 18796}
	if got := spec.mpHTTPPort(); got != 18793 {
		t.Fatalf("mpHTTPPort default = %d, want 18793 (engine port - 4)", got)
	}
	spec.MPHTTPPort = 18795
	if got := spec.mpHTTPPort(); got != 18795 {
		t.Fatalf("mpHTTPPort override = %d, want 18795", got)
	}
	tokens := spec.tokens(Runtime{})
	if tokens["__MP_HTTP_PORT__"] != "18795" {
		t.Fatalf("__MP_HTTP_PORT__ token = %q, want 18795", tokens["__MP_HTTP_PORT__"])
	}
}
