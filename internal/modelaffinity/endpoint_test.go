package modelaffinity

import (
	"os"
	"strings"
	"testing"
)

// EndpointHost names another box's engine and nothing else: loopback,
// localhost, this machine's own name and garbage all read as "this box".
func TestEndpointHostNamesOnlyAnotherBox(t *testing.T) {
	hn, _ := os.Hostname()
	short := strings.ToLower(hn)
	if i := strings.IndexByte(short, '.'); i > 0 {
		short = short[:i]
	}
	cases := map[string]string{
		"http://127.0.0.1:11434":         "",
		"http://localhost:11434":         "",
		"http://[::1]:11434":             "",
		"http://0.0.0.0:11434":           "",
		"http://" + hn + ":11434":        "",
		"http://" + short + ".lan:11434": "",
		"":                               "",
		"://bad":                         "",
		"http://node-b:18797":            "node-b",
		"http://NODE-B:18797/v1":         "node-b",
		"http://192.0.2.9:18797":         "192.0.2.9",
		"http://node-c.example:1234":     "node-c.example",
	}
	for in, want := range cases {
		if got := EndpointHost(in); got != want {
			t.Errorf("EndpointHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// DisarmGPULease makes the gate read as not armed after SetGPULease armed it.
func TestDisarmGPULeaseClearsTheArmedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := SetGPULease("", root); err != nil {
		t.Fatal(err)
	}
	if GPULeaseDir() == "" {
		t.Fatal("SetGPULease must arm the gate")
	}
	DisarmGPULease()
	if GPULeaseDir() != "" {
		t.Fatal("DisarmGPULease must clear the armed directory")
	}
}
