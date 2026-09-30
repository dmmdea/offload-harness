package parser

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrNotOneObject is ExtractOne's refusal of a reply that holds something other than exactly one
// JSON object: a top-level array, a second value after the object, or a repeated key.
var ErrNotOneObject = errors.New("reply is not exactly one JSON object")

// ExtractOne is Extract for a seat whose decoding nothing constrains (config unconstrained_seats).
// A grammar guarantees one object; without one, Extract's "first object wins" would accept an
// answer the model did not commit to, so this refuses it instead and the existing correction
// retry asks again. After fence stripping and trimming:
//
//   - the first JSON value must be an object: a top-level array (even of one object) is refused,
//     and so is a `[` before the first `{`;
//   - nothing after that object may start another value: any `{` or `[` in the remainder is
//     refused (`{..} {..}`), while trailing prose without one ("hope that helps") is still
//     accepted, as is whatever follows a closing code fence (stripFences drops it);
//   - leading prose before the object is still accepted, as Extract does;
//   - no object at any depth may repeat a key (encoding/json keeps the last one silently).
//
// Grammar seats never reach this function; they keep Extract byte for byte.
func ExtractOne(raw string) (json.RawMessage, error) {
	s := stripFences(strings.TrimSpace(raw))
	i := strings.IndexAny(s, "{[")
	if i < 0 {
		return nil, ErrNoJSON
	}
	if s[i] == '[' {
		return nil, fmt.Errorf("%w: a top-level array", ErrNotOneObject)
	}
	span := firstObjectSpan(s)
	if span == "" {
		return nil, ErrNoJSON
	}
	if strings.ContainsAny(s[i+len(span):], "{[") {
		return nil, fmt.Errorf("%w: a second JSON value follows the first object", ErrNotOneObject)
	}
	for _, cand := range []string{span, minorRepair(span)} {
		obj, ok := tryParse(cand)
		if !ok {
			continue
		}
		if err := noDuplicateKeys(cand); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotOneObject, err)
		}
		return obj, nil
	}
	return nil, ErrNoJSON
}

// noDuplicateKeys walks the syntactically valid JSON text s and errors on the first object, at any
// depth, that names a key twice.
func noDuplicateKeys(s string) error {
	dec := json.NewDecoder(strings.NewReader(s))
	if err := walkKeys(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after the object")
	}
	return nil
}

func walkKeys(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			k, _ := kt.(string)
			if seen[k] {
				return fmt.Errorf("duplicate key %q", k)
			}
			seen[k] = true
			if err := walkKeys(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := walkKeys(dec); err != nil {
				return err
			}
		}
	}
	_, err = dec.Token() // the closing delimiter
	return err
}
