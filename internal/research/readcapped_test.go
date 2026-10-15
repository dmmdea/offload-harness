package research

import (
	"strings"
	"testing"
)

// readCapped is the one bounded read the subtitle file goes through.
func TestReadCappedStopsReadingOnePastTheBound(t *testing.T) {
	var consumed int
	r := readerFunc(func(p []byte) (int, error) {
		for i := range p {
			p[i] = 'z'
		}
		consumed += len(p)
		return len(p), nil
	})
	data, cut, err := readCapped(r, 1000)
	if err != nil || !cut || len(data) != 1000 {
		t.Fatalf("got len %d cut %v err %v, want 1000 bytes, cut", len(data), cut, err)
	}
	if consumed > 1000+4096 {
		t.Errorf("consumed %d bytes from an endless reader for a 1000-byte bound", consumed)
	}
	data, cut, err = readCapped(strings.NewReader("short"), 1000)
	if err != nil || cut || string(data) != "short" {
		t.Errorf("short input: %q cut %v err %v", data, cut, err)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
