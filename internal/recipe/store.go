package recipe

import "errors"

// Store is a directory of recipe files, one per id.
type Store struct{ Dir string }

// Path is the file a recipe id lives in.
func (s Store) Path(id string) (string, error) { return "", errors.New("not implemented") }

// Load reads one recipe by id.
func (s Store) Load(id string) (Recipe, error) { return Recipe{}, errors.New("not implemented") }

// Save writes a recipe.
func (s Store) Save(r Recipe) error { return errors.New("not implemented") }

// List reads every recipe in the store.
func (s Store) List() ([]Recipe, error) { return nil, errors.New("not implemented") }
