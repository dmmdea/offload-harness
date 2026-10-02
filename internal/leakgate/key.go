package leakgate

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// Key handling. The gate key is 32 random bytes written as 64 hex characters.
// Every key input is trimmed of surrounding whitespace first, and an empty
// result means unset: an unset Actions secret is delivered to a step as an
// empty string, so an empty key is "no key", never "malformed".
//
// Nothing here prints or returns key text, a fragment of it or its length.

// Environment variable names the gate reads (the caller reads them; this
// package never touches the environment).
const (
	EnvKey      = "OFFLOAD_LEAK_GATE_KEY"
	EnvKeyPrev  = "OFFLOAD_LEAK_GATE_KEY_PREV"
	EnvKeyFile  = "OFFLOAD_LEAK_GATE_KEY_FILE"
	EnvRequired = "OFFLOAD_LEAK_GATE_REQUIRED"
)

// The messages are part of the contract: the behaviour matrix and the operator
// documentation quote them.
var (
	ErrMalformedKey     = errors.New("malformed gate key")
	ErrMalformedPrevKey = errors.New("malformed previous gate key")
	ErrKeyFile          = errors.New("gate key file")
	ErrKeyMissing       = errors.New("key missing where secrets exist")
	ErrBadRequired      = errors.New("unrecognized " + EnvRequired + " value")
	ErrMACMismatch      = errors.New("digest file does not match this key")
	ErrBlind            = errors.New("gate went blind")
)

// Keys are the resolved keys: Key is the current key and Prev the previous one
// (rotation), each nil when unset.
type Keys struct {
	Key  []byte
	Prev []byte
}

// DecodeKey reads a key: exactly 64 hex characters (either case), 32 bytes.
func DecodeKey(s string) ([]byte, error) {
	if len(s) != 64 {
		return nil, errors.New("not a key")
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, errors.New("not a key")
	}
	return b, nil
}

func keyFileError(reason string) error { return fmt.Errorf("%w: %s", ErrKeyFile, reason) }

// readKeyFile reads and decodes a key file. mustExist is true for a file the
// operator named (absent is fatal) and false for the default location (absent
// is simply unset); the second result reports whether a file was found.
func readKeyFile(readFile func(string) ([]byte, error), path string, mustExist bool) ([]byte, bool, error) {
	raw, err := readFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if mustExist {
				return nil, false, keyFileError("absent")
			}
			return nil, false, nil
		}
		return nil, false, keyFileError("unreadable")
	}
	key, err := DecodeKey(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, false, keyFileError("malformed")
	}
	return key, true, nil
}

// ResolveKey resolves the gate key and the previous key.
//
//  1. OFFLOAD_LEAK_GATE_KEY, when non-empty, must be 64 hex characters, else
//     ErrMalformedKey; the key file is then never read.
//  2. Else OFFLOAD_LEAK_GATE_KEY_FILE, when non-empty: the file must exist, be
//     readable and hold 64 hex characters, else ErrKeyFile (the operator asked
//     for that file).
//  3. Else defaultFile, when given and the file exists (present but unreadable
//     or malformed is ErrKeyFile; absent is unset).
//
// OFFLOAD_LEAK_GATE_KEY_PREV is read only when a key was found: empty is
// ignored, non-empty must be 64 hex characters (else ErrMalformedPrevKey), and
// it never counts as the key by itself. With no key, a previous key (valid or
// not) is ignored: the gate is keyless and the rows for a missing key apply.
func ResolveKey(getenv func(string) string, readFile func(string) ([]byte, error), defaultFile string) (Keys, error) {
	var k Keys
	switch v := strings.TrimSpace(getenv(EnvKey)); {
	case v != "":
		key, err := DecodeKey(v)
		if err != nil {
			return Keys{}, ErrMalformedKey
		}
		k.Key = key
	default:
		if path := strings.TrimSpace(getenv(EnvKeyFile)); path != "" {
			key, _, err := readKeyFile(readFile, path, true)
			if err != nil {
				return Keys{}, err
			}
			k.Key = key
		} else if defaultFile != "" {
			key, found, err := readKeyFile(readFile, defaultFile, false)
			if err != nil {
				return Keys{}, err
			}
			if found {
				k.Key = key
			}
		}
	}
	if k.Key == nil {
		return Keys{}, nil
	}
	if v := strings.TrimSpace(getenv(EnvKeyPrev)); v != "" {
		prev, err := DecodeKey(v)
		if err != nil {
			return Keys{}, ErrMalformedPrevKey
		}
		k.Prev = prev
	}
	return k, nil
}

// ParseRequired reads OFFLOAD_LEAK_GATE_REQUIRED: empty is off, exactly "1" is
// on, anything else is an error (a typo must not silently switch the gate off).
func ParseRequired(v string) (bool, error) {
	switch v {
	case "":
		return false, nil
	case "1":
		return true, nil
	}
	return false, ErrBadRequired
}

// Decision is what the tree gate does with the resolved keys.
type Decision struct {
	Enforce    bool   // run the gate
	SkipReason string // when not enforcing: the visible reason to log
}

// Decide covers the key rows of the behaviour matrix: with a key the gate
// enforces; without one it is fatal where secrets exist (required) and a
// visible skip elsewhere (a fork's pull request, a contributor's machine, a
// source tarball). A previous key alone never counts as the key.
func Decide(k Keys, required bool) (Decision, error) {
	if k.Key != nil {
		return Decision{Enforce: true}, nil
	}
	if required {
		return Decision{}, ErrKeyMissing
	}
	return Decision{SkipReason: "no gate key (a fork's pull request, a contributor machine or a source tarball)"}, nil
}
