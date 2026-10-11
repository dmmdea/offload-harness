package mediaremote

// The results file of the fleet batch (P0 plan S3; ADR 0083 decision 13), second fix round: a results path that cannot work is
// refused when the file is opened, before any lane time is spent, and the record of a batch that was killed is not destroyed by
// the --resume run that recovers from it.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ---- the results file ------------------------------------------------------------------------------------------------------------

// A results path that names a folder is refused when the file is opened, before any lane time is spent: the rows are kept in
// <path>.partial, so the folder would otherwise be found out only when the whole batch ends.
func TestOpenResultsRefusesAFolderAtOnce(t *testing.T) {
	dir := t.TempDir()
	rf, err := OpenResults(dir)
	if err == nil {
		rf.Discard()
		t.Fatalf("a folder was taken for a results file")
	}
	if !strings.Contains(err.Error(), "folder") && !strings.Contains(err.Error(), "directory") {
		t.Errorf("err = %v, want it to say the path is a folder", err)
	}
	if _, serr := os.Stat(dir + ".partial"); serr == nil {
		t.Errorf("a partial file was left beside the folder")
	}
}

// Where renaming over a read-only file fails (Windows), a read-only results file is refused at once too: the batch would
// otherwise run for hours and fail at the rename.
func TestOpenResultsRefusesAReadOnlyFileWhereTheRenameWouldFail(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("a read-only file can be renamed over here")
	}
	p := filepath.Join(t.TempDir(), "r.jsonl")
	if err := os.WriteFile(p, []byte("earlier\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	// the premise: a rename over it fails on this system
	probe := p + ".probe"
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(probe, p); err == nil {
		t.Skip("this system renames over a read-only file")
	}
	_ = os.Remove(probe)
	rf, err := OpenResults(p)
	if err == nil {
		rf.Discard()
		t.Fatalf("a read-only results file was accepted")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("err = %v, want it to say the file is read-only", err)
	}
	if _, serr := os.Stat(p + ".partial"); serr == nil {
		t.Errorf("a partial file was left beside the read-only target")
	}
}

// The record of a batch that died is <path>.partial. Running again with --resume reopens that name, so the record is set
// aside as <path>.partial.prev (one is kept) before the new batch truncates the name, not destroyed by it.
func TestOpenResultsKeepsAnEarlierBatchsRecordAside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.jsonl")
	dead := `{"i":0,"out":"a.png","verdict":"rendered","node":"node-a"}` + "\n"
	if err := os.WriteFile(path+".partial", []byte(dead), 0o644); err != nil {
		t.Fatal(err)
	}
	rf, err := OpenResults(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rf.Discard) // an open file cannot be removed with its folder on Windows
	if raw, rerr := os.ReadFile(path + ".partial.prev"); rerr != nil || string(raw) != dead {
		t.Errorf("the dead batch's record = %q (%v), want it set aside whole as .partial.prev", raw, rerr)
	}
	rf.Append(BatchRow{I: 0, Out: "b.png", Verdict: VerdictRendered})
	if raw, _ := os.ReadFile(path + ".partial"); strings.Contains(string(raw), "a.png") {
		t.Errorf("the new partial still holds the dead batch's rows: %q", raw)
	}
}

// ---- the results file, continued -----------------------------------------------------------------------------------------------------

// The record set aside is told to the caller, a refused run puts it back, a second recovery replaces the older one (one is
// kept), and an empty partial (a batch that died before its first row) never replaces a record that holds rows.
func TestResultsFileSaysWhereTheEarlierRecordWentAndPutsItBackWhenRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.jsonl")
	first := `{"i":0,"verdict":"rendered","out":"a.png"}` + "\n"
	second := `{"i":0,"verdict":"rendered","out":"b.png"}` + "\n"
	write := func(p, s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) string {
		b, _ := os.ReadFile(p)
		return string(b)
	}

	// nothing earlier: nothing to say
	rf, err := OpenResults(path)
	if err != nil || rf.KeptAside() != "" {
		t.Fatalf("OpenResults = %v, kept %q, want nothing set aside", err, rf.KeptAside())
	}
	rf.Discard()

	// a record earlier: set aside and said so
	write(path+".partial", first)
	rf, err = OpenResults(path)
	if err != nil || rf.KeptAside() != path+".partial.prev" || read(path+".partial.prev") != first {
		t.Fatalf("err=%v kept=%q prev=%q, want the record set aside as .partial.prev", err, rf.KeptAside(), read(path+".partial.prev"))
	}
	// a run the command then refuses puts it back under its own name, as it was
	rf.Discard()
	if read(path+".partial") != first {
		t.Errorf("after a refused run the earlier record is %q, want it back whole", read(path+".partial"))
	}
	if _, serr := os.Stat(path + ".partial.prev"); serr == nil {
		t.Errorf("the set-aside copy was left beside the record that was put back")
	}

	// a second recovery replaces the older .prev
	rf, err = OpenResults(path)
	if err != nil {
		t.Fatal(err)
	}
	rf.Append(BatchRow{I: 0, Out: "b.png", Verdict: VerdictRendered})
	if err := rf.Close([]BatchRow{{I: 0, Out: "b.png", Verdict: VerdictRendered}}); err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Stat(path + ".partial"); serr == nil {
		t.Errorf("a finished batch left its partial file")
	}
	if read(path+".partial.prev") != first {
		t.Errorf("a finished batch must keep the earlier record: %q", read(path+".partial.prev"))
	}
	write(path+".partial", second)
	rf, err = OpenResults(path)
	if err != nil || read(path+".partial.prev") != second {
		t.Fatalf("err=%v prev=%q, want the newest record kept", err, read(path+".partial.prev"))
	}
	rf.Discard()

	// an empty partial holds no record: it does not replace the one that does
	_ = os.Remove(path + ".partial")
	write(path+".partial.prev", first)
	write(path+".partial", "")
	rf, err = OpenResults(path)
	if err != nil || rf.KeptAside() != "" || read(path+".partial.prev") != first {
		t.Fatalf("err=%v kept=%q prev=%q, want the older record left alone", err, rf.KeptAside(), read(path+".partial.prev"))
	}
	rf.Discard()
}

// A record that cannot be set aside is not destroyed: the command is refused and the record stays where it is.
func TestOpenResultsRefusesRatherThanDestroyARecordItCannotSetAside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.jsonl")
	dead := `{"i":0,"verdict":"rendered"}` + "\n"
	if err := os.WriteFile(path+".partial", []byte(dead), 0o644); err != nil {
		t.Fatal(err)
	}
	// a non-empty folder under the name of the set-aside copy: it can be neither removed nor renamed over
	if err := os.MkdirAll(filepath.Join(path+".partial.prev", "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	rf, err := OpenResults(path)
	if err == nil {
		rf.Discard()
		t.Fatalf("OpenResults went on after it could not keep the earlier record")
	}
	if !strings.Contains(err.Error(), "could not be set aside") {
		t.Errorf("err = %v, want it to say the record could not be set aside", err)
	}
	if raw, rerr := os.ReadFile(path + ".partial"); rerr != nil || string(raw) != dead {
		t.Errorf("the earlier record = %q (%v), want it untouched", raw, rerr)
	}
}
