package trajectory

import (
	"os"
	"path/filepath"
	"testing"
)

func appendRaw(path, s string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(s)
	return err
}

func TestEnqueueDrainRoundTrip(t *testing.T) {
	q := filepath.Join(t.TempDir(), "q.jsonl")
	for i := 0; i < 3; i++ {
		if err := Enqueue(q, Item{Schema: SchemaVersion, ID: "g", Goal: "do it", Tools: []string{"list_dir"}, Steps: 2, StopReason: "done"}); err != nil {
			t.Fatal(err)
		}
	}
	items, err := Drain(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[0].Goal != "do it" || items[0].Tools[0] != "list_dir" {
		t.Errorf("round-trip wrong: %+v", items)
	}
	// Drain claimed + removed the queue: a second drain is empty.
	if again, _ := Drain(q); len(again) != 0 {
		t.Errorf("second drain should be empty; got %d", len(again))
	}
}

func TestCaptureSamplingBounds(t *testing.T) {
	q := filepath.Join(t.TempDir(), "q.jsonl")
	// rate 0 => never capture (no file written).
	for i := 0; i < 20; i++ {
		if got, _ := Capture(q, 0, Item{Goal: "x"}); got {
			t.Fatal("rate=0 must never capture")
		}
	}
	if items, _ := Drain(q); len(items) != 0 {
		t.Errorf("rate=0 should have written nothing; got %d", len(items))
	}
	// rate 1 => always capture.
	for i := 0; i < 5; i++ {
		if got, err := Capture(q, 1, Item{Goal: "y"}); !got || err != nil {
			t.Fatalf("rate=1 must always capture; got=%v err=%v", got, err)
		}
	}
	if items, _ := Drain(q); len(items) != 5 {
		t.Errorf("rate=1 should have captured 5; got %d", len(items))
	}
	// empty path => no-op, no error.
	if got, err := Capture("", 1, Item{Goal: "z"}); got || err != nil {
		t.Errorf("empty path must be a no-op; got=%v err=%v", got, err)
	}
}

func TestDrainCorruptLineSkipped(t *testing.T) {
	q := filepath.Join(t.TempDir(), "q.jsonl")
	_ = Enqueue(q, Item{Goal: "good"})
	// append a corrupt line directly
	if err := appendRaw(q, "{not json\n"); err != nil {
		t.Fatal(err)
	}
	_ = Enqueue(q, Item{Goal: "good2"})
	items, err := Drain(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Errorf("a corrupt line must be skipped, valid ones kept; got %d: %+v", len(items), items)
	}
}
