package nimclient

import "testing"

// The offload_nim base allowlist (security standard L5, register S-30):
// NVIDIA's hosted API, the configured endpoint and the operator's extra list,
// compared by scheme, host and port with the entry's path as a prefix on a
// segment boundary.
func TestBaseAllowed(t *testing.T) {
	configured := "http://127.0.0.1:8000/v1"
	extra := []string{"http://nim-box.example.internal:9000/v1", "https://192.0.2.20/nim"}
	for _, tc := range []struct {
		base string
		want bool
	}{
		{"https://integrate.api.nvidia.com/v1", true},
		{"https://ai.api.nvidia.com/v1", true},
		{"http://127.0.0.1:8000/v1", true},
		{"http://127.0.0.1:8000/v1/", true},
		{"HTTP://127.0.0.1:8000/v1", true},
		{"http://127.0.0.1:8000/v1/extra", true},
		{"http://nim-box.example.internal:9000/v1", true},
		{"http://NIM-BOX.example.internal.:9000/v1", true},
		{"https://192.0.2.20:443/nim/v1", true},
		{"https://192.0.2.20/nim", true},
		// not allowlisted
		{"http://127.0.0.1:8001/v1", false},           // other port
		{"https://127.0.0.1:8000/v1", false},          // other scheme
		{"http://127.0.0.1:8000/v1x", false},          // not a segment boundary
		{"http://127.0.0.1:8000/other", false},        // other path
		{"https://attacker.example/v1", false},        // a third party
		{"http://user@127.0.0.1:8000/v1", false},      // userinfo is never allowlisted
		{"ftp://127.0.0.1:8000/v1", false},            // not http(s)
		{"https://192.0.2.20/nimx", false},            // prefix without a boundary
		{"http://nim-box.example.internal/v1", false}, // default port 80, entry says 9000
		{"https://api.nvidia.com.attacker.example/v1", false},
	} {
		got, why := BaseAllowed(tc.base, configured, extra)
		if got != tc.want {
			t.Errorf("BaseAllowed(%q) = %v (%s), want %v", tc.base, got, why, tc.want)
		}
		if why == "" {
			t.Errorf("BaseAllowed(%q) gave no reason", tc.base)
		}
	}
}
