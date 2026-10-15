package recipe

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadFixture(t *testing.T, name string) Recipe {
	t.Helper()
	r, err := Decode(fixture(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestStoreSavesAndLoadsACanonicalFilePerID(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "recipes")} // the directory does not exist yet
	want := loadFixture(t, "proposed.json")
	if err := s.Save(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	onDisk, err := os.ReadFile(filepath.Join(s.Dir, want.ID+".json"))
	if err != nil {
		t.Fatalf("the recipe is not in a file named by its id: %v", err)
	}
	if !bytes.Equal(onDisk, fixture(t, "proposed.json")) {
		t.Fatalf("the file is not the canonical bytes:\n%s", onDisk)
	}
	got, err := s.Load(want.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.ID != want.ID || got.Status != want.Status || got.SeatTag != want.SeatTag {
		t.Fatalf("loaded %+v, saved %+v", got, want)
	}
	// A second save replaces the first and leaves nothing beside it.
	want.Status = StatusDemoted
	if err := s.Save(want); err != nil {
		t.Fatalf("second save: %v", err)
	}
	if got, _ := s.Load(want.ID); got.Status != StatusDemoted {
		t.Fatalf("the second save did not replace the first: %s", got.Status)
	}
	if ents, _ := os.ReadDir(s.Dir); len(ents) != 1 {
		t.Fatalf("the store holds %d entries after two saves of one recipe, want 1", len(ents))
	}
}

func TestStoreRefusesWhatItCannotTrust(t *testing.T) {
	dir := t.TempDir()
	s := Store{Dir: dir}
	good := fixture(t, "proposed.json")

	// A file whose record names another id than its file does.
	if err := os.WriteFile(filepath.Join(dir, "other-name.json"), good, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("other-name"); err == nil || !strings.Contains(err.Error(), "other-name") {
		t.Errorf("a record in the wrong file loaded: %v", err)
	}
	// A file that is not a recipe.
	if err := os.WriteFile(filepath.Join(dir, "recipe-note-04.json"), fixture(t, "bad-unknown-key.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("recipe-note-04"); err == nil {
		t.Error("a file with an unknown key loaded")
	}
	// An id that is a path.
	for _, id := range []string{"../recipe-cap-listing-01", "a/b", `a\b`, "", "UPPER"} {
		if _, err := s.Load(id); err == nil {
			t.Errorf("Load(%q) was allowed", id)
		}
	}
	// Saving a record that would not decode.
	bad := loadFixture(t, "proposed.json")
	bad.Rules.RewriteError[0].Text = "run `x`"
	if err := s.Save(bad); err == nil {
		t.Error("a record that does not validate was saved")
	}
	// A missing recipe is an error, not an empty record.
	if _, err := s.Load("recipe-missing-09"); err == nil {
		t.Error("a missing recipe loaded")
	}
}

func TestStoreListsEveryRecipeInIDOrderAndFailsOnABadFile(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	for _, name := range []string{"published.json", "proposed.json", "demoted.json"} {
		if err := s.Save(loadFixture(t, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "notes.txt"), []byte("not a recipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if strings.Join(ids, ",") != "recipe-cap-listing-01,recipe-deny-search-02,recipe-trim-obs-03" {
		t.Fatalf("listed %v", ids)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "recipe-note-04.json"), fixture(t, "bad-unknown-key.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(); err == nil || !strings.Contains(err.Error(), "recipe-note-04.json") {
		t.Fatalf("a bad file did not fail the listing by name: %v", err)
	}
}
