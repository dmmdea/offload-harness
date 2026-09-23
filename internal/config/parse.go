package config

import (
	"bytes"
	"errors"
)

// utf8BOM is the byte-order mark Windows PowerShell 5.1 writes at the head of every
// UTF-8 file (Set-Content/Out-File -Encoding UTF8). encoding/json refuses it
// ("invalid character 'ï' looking for beginning of value").
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// StripBOM drops one leading UTF-8 byte-order mark. Every reader of an operator-written
// JSON file calls it before decoding: a config saved from PowerShell 5.1 is otherwise
// "not valid JSON" while it looks perfect in every editor (OptiPlex, 2026-09-23: a
// run-graph ran on built-in defaults because of exactly this).
func StripBOM(b []byte) []byte { return bytes.TrimPrefix(b, utf8BOM) }

// ParseError is a config file that was read but could not be DECODED. Unlike a
// validation failure (the file's settings are in effect, one key is refused), nothing
// from the file is in effect: Load returns the built-in defaults. The message says so
// itself, so every surface that prints the load error verbatim (doctor, the MCP
// config_error, fleet-serve's refusal) tells the truth without a special case.
type ParseError struct{ Err error }

func (e *ParseError) Error() string {
	return "not valid JSON, so NOTHING from the file is in effect (running on built-in defaults): " + e.Err.Error()
}

func (e *ParseError) Unwrap() error { return e.Err }

// IsParseError reports whether err is (or wraps) a config ParseError.
func IsParseError(err error) bool {
	var p *ParseError
	return errors.As(err, &p)
}
