package gpulease

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// settled.go decides when the absence of a lease file is an answer. These tests pin its pieces; the consequence for the reclaim rule is
// in heartbeat_unknown_test.go.

func TestStagingPathIsTheNameEachKindOfLeaseFileIsStagedUnder(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct{ name, path, want string }{
		{"a heartbeat", filepath.Join(dir, "hb.7"), filepath.Join(dir, "hb.7.tmp")},
		{"a card lease record", filepath.Join(dir, "e", "7.json"), filepath.Join(dir, "e", "7.json.tmp")},
		{"the whole-node record, which stages under a suffix of its own", filepath.Join(dir, "meta.json"), filepath.Join(dir, "meta.json.restamp")},
		{"a card claim, which is created once and never replaced", filepath.Join(dir, "cards", "gpu-0.claim"), ""},
		{"anything else", filepath.Join(dir, "gpu", "epoch"), filepath.Join(dir, "gpu", "epoch.tmp")},
	} {
		if got := stagingPath(c.path); got != c.want {
			t.Errorf("stagingPath(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRenewStagesTheHeartbeatUnderTheNameTheReaderLooksFor(t *testing.T) {
	m, _ := scopedManager(t)
	l := mustDevices(t, m, card0)
	defer l.Release()
	hb := m.heartbeatPath(l.Epoch())
	// A directory in the staged name's place makes Renew's write fail, and the failure names the path it tried.
	if err := os.Mkdir(stagingPath(hb), 0o777); err != nil {
		t.Fatal(err)
	}
	err := l.Renew()
	if err == nil {
		t.Fatal("Renew wrote through a directory standing where it stages the heartbeat: it stages somewhere else than stagingPath says")
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Path != stagingPath(hb) {
		t.Errorf("Renew failed with %v, want a failure to write %s", err, stagingPath(hb))
	}
}

func TestAStatOfALeaseFileIsSettledOnlyWhenNoRenameCanBeInstallingIt(t *testing.T) {
	now := time.Now()
	stage := func(t *testing.T, path string, age time.Duration) {
		t.Helper()
		staged := stagingPath(path)
		if err := os.MkdirAll(filepath.Dir(staged), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(staged, []byte("1"), 0o666); err != nil {
			t.Fatal(err)
		}
		at := now.Add(-age)
		if err := os.Chtimes(staged, at, at); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name     string
		file     string
		prepare  func(t *testing.T, path string)
		inFlight bool // want errRenameInFlight
		absent   bool // want a not-exist error
	}{
		{"the file is there", "hb.1", func(t *testing.T, p string) {
			if err := os.WriteFile(p, []byte("1"), 0o666); err != nil {
				t.Fatal(err)
			}
		}, false, false},
		{"absent, nothing beside it", "hb.1", func(t *testing.T, p string) {}, false, true},
		{"absent, a staged file written a moment ago", "hb.1", func(t *testing.T, p string) { stage(t, p, 0) }, true, false},
		{"absent, a staged file just under the freshness bound", "hb.1", func(t *testing.T, p string) { stage(t, p, stagingFresh-time.Second) }, true, false},
		{"absent, a staged file older than the bound (a writer that died)", "hb.1", func(t *testing.T, p string) { stage(t, p, stagingFresh+2*time.Second) }, false, true},
		{"absent, a staged file dated in the future (a clock that stepped back)", "hb.1", func(t *testing.T, p string) { stage(t, p, -time.Hour) }, true, false},
		{"absent, a directory in the staged name's place (a writer stages a file, never a directory)", "hb.1", func(t *testing.T, p string) {
			if err := os.Mkdir(stagingPath(p), 0o777); err != nil {
				t.Fatal(err)
			}
		}, false, true},
		{"the whole-node record with its restamp file beside it", "meta.json", func(t *testing.T, p string) { stage(t, p, 0) }, true, false},
		{"the whole-node record with only a .tmp beside it, which is not its staging name", "meta.json", func(t *testing.T, p string) {
			if err := os.WriteFile(p+".tmp", []byte("1"), 0o666); err != nil {
				t.Fatal(err)
			}
		}, false, true},
		{"a card claim with a .tmp beside it: claims are never replaced, so nothing is in flight", filepath.Join("cards", "gpu-0.claim"), func(t *testing.T, p string) {
			if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p+".tmp", []byte("1"), 0o666); err != nil {
				t.Fatal(err)
			}
		}, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), c.file)
			c.prepare(t, path)
			fi, err := statSettled(path)
			switch {
			case c.inFlight:
				if !errors.Is(err, errRenameInFlight) || errors.Is(err, fs.ErrNotExist) || !isIOFailure(err) {
					t.Errorf("statSettled = %v, want an I/O-class errRenameInFlight that is not a not-exist error", err)
				}
			case c.absent:
				if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, errRenameInFlight) {
					t.Errorf("statSettled = %v, want a plain not-exist error", err)
				}
			default:
				if err != nil || fi == nil {
					t.Errorf("statSettled = %v, %v, want the file", fi, err)
				}
			}
		})
	}
}

// The test seam of statRecord is the seam of statSettled too, and it is consulted first.
func TestStatSettledReportsWhatTheStatSeamInjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hb.1")
	if err := os.WriteFile(path, []byte("1"), 0o666); err != nil {
		t.Fatal(err)
	}
	boom := &fs.PathError{Op: "stat", Path: path, Err: errors.New("injected")}
	restore := UseStatFault(func(p string) error { return boom })
	defer restore()
	if _, err := statSettled(path); !errors.Is(err, boom.Err) {
		t.Errorf("statSettled = %v, want the injected failure", err)
	}
}

// The rename can end between the look at the name and the look at the staged file: the first look finds nothing, the staged file is
// gone by the second, and the name is back. Reading that as "settled absent" is the miss at the tail of a rename, so a second look at
// the name decides, and it finds the file.
func TestAStatThatMeetsTheRenameEndingBetweenTheLooksReturnsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hb.1")
	if err := os.WriteFile(path, []byte("1"), 0o666); err != nil {
		t.Fatal(err)
	}
	looks := 0
	old := statFile
	statFile = func(p string) (os.FileInfo, error) {
		if p == path {
			looks++
			if looks == 1 {
				return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist} // the rename had not landed yet
			}
		}
		return old(p)
	}
	defer func() { statFile = old }()
	fi, err := statSettled(path)
	if err != nil || fi == nil {
		t.Fatalf("statSettled = %v, %v, want the file that appeared between the looks", fi, err)
	}
	if looks != 2 {
		t.Errorf("the name was looked at %d times, want 2 (before and after the look at the staged file)", looks)
	}
}

// A staged file that cannot be examined for any reason but being absent is not evidence that nothing is in flight.
func TestAStagedFileThatCannotBeExaminedCountsAsInFlight(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hb.1")
	old := statFile
	statFile = func(p string) (os.FileInfo, error) {
		if p == stagingPath(path) {
			return nil, &fs.PathError{Op: "stat", Path: p, Err: os.ErrPermission}
		}
		return old(p)
	}
	defer func() { statFile = old }()
	if _, err := statSettled(path); !errors.Is(err, errRenameInFlight) {
		t.Errorf("statSettled = %v, want errRenameInFlight: the staged file could not be examined", err)
	}
}
