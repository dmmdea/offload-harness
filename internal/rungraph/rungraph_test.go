package rungraph

import "testing"

func TestBuildArgs(t *testing.T) {
	got := buildArgs(Params{
		GraphPath: "g.json", ManifestPath: "m.json", OutDir: "out", ResultPath: "r.json", ReserveVram: "1.5",
	})
	want := []string{"--graph", "g.json", "--manifest", "m.json", "--out-dir", "out", "--result", "r.json", "--reserve-vram", "1.5"}
	if len(got) != len(want) {
		t.Fatalf("len: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestBuildArgsOmitsEmptyOptional(t *testing.T) {
	got := buildArgs(Params{GraphPath: "g.json", OutDir: "out", ResultPath: "r.json"})
	for _, a := range got {
		if a == "--manifest" || a == "--reserve-vram" {
			t.Fatalf("expected no empty optional flag, got %v", got)
		}
	}
}
