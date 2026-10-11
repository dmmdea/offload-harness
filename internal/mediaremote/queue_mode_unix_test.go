//go:build unix

package mediaremote

// The ticket queue is private to the user that runs it. A graph ticket keeps copies of the graph and the manifest (a ComfyUI graph
// carries its prompt text), a finished ticket keeps the call's result (an image's refined prompt among it), and none of that is for
// another user of the machine to read. Every file the queue writes is owner-only (0600), and the queue directory is made owner-only
// (0700) where the queue creates it; the state root above it belongs to the GPU lease and is made as the lease makes it, and a queue
// directory that already exists keeps the mode it has. (A mode means nothing on Windows, where the ACL of the state root decides, so
// this file is for the platforms that have one.)

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// A first ticket on a machine whose state root does not exist yet: the queue makes its own directory owner-only and leaves the root
// as the lease makes it; and every kind of file the queue holds (the lock, a ticket, a node claim, the graph's and the manifest's
// copies) is owner-only.
func TestEveryFileOfTheQueueAndTheQueueDirectoryAreOwnerOnly(t *testing.T) {
	prev := syscall.Umask(0) // no mask: a file has the mode the queue asked for, and not that mode less what a mask removes
	defer syscall.Umask(prev)

	e := newGEnv(t)
	n := e.node("node-a", true, nodeOpts{}, krea2File)
	e.start()
	root := filepath.Join(t.TempDir(), "state") // it does not exist: the queue's first write makes it, and its own directory
	e.cfg.StateDir = root
	q, _ := manual(t)
	mpath := writeFile(t, t.TempDir(), "m.json", []byte(bodyManifestA))
	admitOn(t, q, e.cfg, e.lr, e.req(krea2Graph, map[string]any{"manifest_path": mpath}))

	// A ticket that claims a node writes the claim file with its own.
	st, _ := storeFor(e.cfg)
	tk := ticketFile{ID: "mq-0mode0000001", ReqKey: "k", Created: nowMs(), Owner: selfOwner(), Beat: nowMs(), State: stWaiting}
	if ok, _, err := st.claimNode(&tk, n.srv.URL, "node-a", stPlaced); err != nil || !ok {
		t.Fatalf("the ticket claims the node: %v %v", ok, err)
	}

	if got := modeOf(t, st.dir); got != 0o700 {
		t.Errorf("the queue directory is made owner-only: %04o", got)
	}
	if got := modeOf(t, root); got&0o005 != 0o005 {
		t.Errorf("the state root above it is the lease's and is made as the lease makes it, not stamped private: %04o", got)
	}
	ents, err := os.ReadDir(st.dir)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, ent := range ents {
		name := ent.Name()
		switch {
		case name == lockName:
			kinds["lock"] = true
		case strings.HasPrefix(name, "mq-"):
			kinds["ticket"] = true
		case strings.HasPrefix(name, "use-"):
			kinds["claim"] = true
		case strings.HasPrefix(name, bodyPrefix) && strings.HasSuffix(name, "-graph.json"):
			kinds["graph copy"] = true
		case strings.HasPrefix(name, bodyPrefix) && strings.HasSuffix(name, "-manifest.json"):
			kinds["manifest copy"] = true
		}
		if got := modeOf(t, filepath.Join(st.dir, name)); got != 0o600 {
			t.Errorf("%s is owner-only: %04o", name, got)
		}
	}
	for _, want := range []string{"lock", "ticket", "claim", "graph copy", "manifest copy"} {
		if !kinds[want] {
			t.Errorf("control: the queue holds a %s to be checked (found %v among %d files)", want, kinds, len(ents))
		}
	}
}

// A finished ticket's file holds the call's result, and an image's result carries the prompt the refiner wrote: that file is the
// other one that holds prompt text, and it is owner-only like the copies.
func TestAFinishedTicketsFileHoldsItsResultAndIsOwnerOnly(t *testing.T) {
	prev := syscall.Umask(0)
	defer syscall.Umask(prev)

	e := newGEnv(t)
	e.node("node-a", true, nodeOpts{}, krea2File)
	e.start()
	q, _ := manual(t)
	lt := admitOn(t, q, e.cfg, e.lr, e.req(krea2Graph, nil))
	st, _ := storeFor(e.cfg)
	const prompt = "a red door at dusk, rain on the glass"
	lt.finish(core.Result{OK: true, Data: json.RawMessage(`{"refined_prompt":"` + prompt + `"}`)}, false)

	path := st.path(lt.snapshot().ID)
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), prompt) {
		t.Fatalf("control: a finished ticket's file holds the call's result, the prompt among it: %q %v", b, err)
	}
	if got := modeOf(t, path); got != 0o600 {
		t.Errorf("the finished ticket's file is owner-only: %04o", got)
	}
}

// A queue directory that exists already keeps the mode it has: an older build made it 0755, other processes of the machine may
// rely on that, and the queue chmods nothing it did not create. What the queue writes into it is owner-only all the same, which is
// what protects the copies there.
func TestAQueueDirectoryThatExistsKeepsItsMode(t *testing.T) {
	prev := syscall.Umask(0)
	defer syscall.Umask(prev)

	e := newGEnv(t)
	e.node("node-a", true, nodeOpts{}, krea2File)
	e.start()
	root := t.TempDir()
	e.cfg.StateDir = root
	dir := filepath.Join(root, queueDirName)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	q, _ := manual(t)
	lt := admitOn(t, q, e.cfg, e.lr, e.req(krea2Graph, nil))
	st, _ := storeFor(e.cfg)

	if got := modeOf(t, dir); got != 0o755 {
		t.Errorf("a directory the queue did not create keeps its mode: %04o", got)
	}
	for _, p := range []string{st.path(lt.snapshot().ID), st.bodyPath(lt.snapshot().ID, "graph"), filepath.Join(dir, lockName)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("control: %v", err)
		}
	}
	for _, p := range []string{st.path(lt.snapshot().ID), st.bodyPath(lt.snapshot().ID, "graph")} {
		if got := modeOf(t, p); got != 0o600 {
			t.Errorf("%s is owner-only whatever the directory's mode: %04o", filepath.Base(p), got)
		}
	}
}
