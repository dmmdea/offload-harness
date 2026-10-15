package recipe

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Store is a directory of recipe files, one per id, named <id>.json. Every file is read through
// Decode, so a store never hands back a record that would not decode, and a file whose record
// names another id than its file name does is refused.
type Store struct{ Dir string }

const fileExt = ".json"

// Path is the file a recipe id lives in. An id that is not a slug is refused, which is what keeps
// an id from naming a path outside the store.
func (s Store) Path(id string) (string, error) {
	if s.Dir == "" {
		return "", errors.New("recipe store: no directory")
	}
	if len(id) > 64 || !idRe.MatchString(id) {
		return "", fmt.Errorf("recipe store: id %q is not a slug of lower-case letters, digits and single dashes", id)
	}
	return filepath.Join(s.Dir, id+fileExt), nil
}

// Load reads one recipe by id.
func (s Store) Load(id string) (Recipe, error) {
	p, err := s.Path(id)
	if err != nil {
		return Recipe{}, err
	}
	return readFile(p, id)
}

func readFile(path, wantID string) (Recipe, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Recipe{}, fmt.Errorf("recipe store: %w", err)
	}
	r, err := Decode(raw)
	if err != nil {
		return Recipe{}, fmt.Errorf("recipe store: %s: %w", filepath.Base(path), err)
	}
	if r.ID != wantID {
		return Recipe{}, fmt.Errorf("recipe store: %s holds the record %q, not %q: a file is named by the id inside it", filepath.Base(path), r.ID, wantID)
	}
	return r, nil
}

// Save writes a recipe to <id>.json, replacing what is there. The record is validated first and the
// file is written to a temporary name beside it and renamed over it, so a reader sees the old
// record or the new one, never half of either.
func (s Store) Save(r Recipe) error {
	p, err := s.Path(r.ID)
	if err != nil {
		return err
	}
	raw, err := r.Encode()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return fmt.Errorf("recipe store: %w", err)
	}
	tmp, err := os.CreateTemp(s.Dir, "."+r.ID+".*.tmp")
	if err != nil {
		return fmt.Errorf("recipe store: %w", err)
	}
	_, werr := tmp.Write(raw)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("recipe store: write %s: %w", filepath.Base(p), werr)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("recipe store: %w", err)
	}
	return nil
}

// List reads every <id>.json in the store, in id order. A file that does not decode fails the
// listing by name: a store with a bad record in it is not a store to read from. Other files
// (notes, temporary files of a save in flight) are not recipes and are passed over.
func (s Store) List() ([]Recipe, error) {
	if s.Dir == "" {
		return nil, errors.New("recipe store: no directory")
	}
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, fmt.Errorf("recipe store: %w", err)
	}
	var out []Recipe
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, fileExt) {
			continue
		}
		r, err := readFile(filepath.Join(s.Dir, name), strings.TrimSuffix(name, fileExt))
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
