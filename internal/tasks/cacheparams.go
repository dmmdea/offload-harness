package tasks

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"unicode/utf8"
)

// ErrParamsNotKeyable is what CacheParamsKey wraps when params have no canonical typed
// form. A caller must then skip the result cache for that call altogether.
var ErrParamsNotKeyable = errors.New("params cannot be keyed for the result cache")

// maxKeyableDepth bounds how deeply a params value may nest. It is a bound on the walk,
// not a feature: a params map that holds itself would otherwise never finish.
const maxKeyableDepth = 64

var (
	jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

// CacheParamsKey is the params ingredient of a RESULT-CACHE key: the params rendered as
// JSON with the keys of every map sorted at every depth, typed, so values that differ
// in type or in structure render differently. {"seed": "5"} and {"seed": 5} differ,
// []any{"a", "b"} and []any{"a b"} differ, a nested map holding "k2:v2" and a map with a
// second entry differ.
//
// It exists because StableParamsKey cannot do this job. That function prints values
// with %v, which is type-erased and space-separated, so a request could shape a nested
// value of its params (an extract schema's "required" list) to render exactly like
// another request's, get the same cache key, and be served that request's cached answer
// before its own schema was ever checked. StableParamsKey stays for output-file stems
// and the exemplar rows, where a changed string would move file names; the result cache
// uses this.
//
// The same logical params render the same string whatever Go type carried them: an int
// 5 and the float64 5 that the MCP door decodes from JSON are both 5, and a []string
// and a []any holding the same strings are the same list. Empty or nil params render "".
//
// It FAILS CLOSED. Values with no faithful typed form return an error wrapping
// ErrParamsNotKeyable, and the caller must then neither read nor write the result cache
// for that call; falling back to %v or StableParamsKey would reopen the collision this
// function closes. Those values are: NaN and infinities (JSON has no form for them),
// functions, channels, complex numbers, structs, byte slices (JSON would render them as
// a base64 string, indistinguishable from a string), any type with its own JSON or text
// form (time.Time and the like), maps whose keys are not strings, strings that are not
// valid UTF-8 (JSON would silently replace the bad bytes, so two different strings
// would render alike), and values nested deeper than a cycle-guard bound. The error
// names the params key and the kind of the offender, never a value.
func CacheParamsKey(p map[string]any) (string, error) {
	if len(p) == 0 {
		return "", nil
	}
	for k, v := range p {
		if err := checkKeyable(reflect.ValueOf(v), 1); err != nil {
			return "", fmt.Errorf("%w: params[%q]: %v", ErrParamsNotKeyable, k, err)
		}
		if !utf8.ValidString(k) {
			return "", fmt.Errorf("%w: a params key is not valid UTF-8", ErrParamsNotKeyable)
		}
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrParamsNotKeyable, err)
	}
	return string(b), nil
}

// checkKeyable walks one params value and returns an error for anything json.Marshal
// would render lossily, or not at all. The messages carry kinds, never values.
func checkKeyable(v reflect.Value, depth int) error {
	if depth > maxKeyableDepth {
		return errors.New("nested too deeply (a cycle?)")
	}
	if !v.IsValid() { // a nil interface: JSON null
		return nil
	}
	t := v.Type()
	if v.Kind() != reflect.Interface {
		if t.Implements(jsonMarshalerType) || t.Implements(textMarshalerType) ||
			reflect.PointerTo(t).Implements(jsonMarshalerType) || reflect.PointerTo(t).Implements(textMarshalerType) {
			return fmt.Errorf("type %s has its own JSON or text form", t)
		}
	}
	switch v.Kind() {
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return nil
	case reflect.Float32, reflect.Float64:
		if f := v.Float(); math.IsNaN(f) || math.IsInf(f, 0) {
			return errors.New("a NaN or infinite number")
		}
		return nil
	case reflect.String:
		if !utf8.ValidString(v.String()) {
			return errors.New("a string that is not valid UTF-8")
		}
		return nil
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return checkKeyable(v.Elem(), depth+1)
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return errors.New("a byte slice")
		}
		fallthrough
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := checkKeyable(v.Index(i), depth+1); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return fmt.Errorf("a map keyed by %s, not by strings", t.Key())
		}
		it := v.MapRange()
		for it.Next() {
			if !utf8.ValidString(it.Key().String()) {
				return errors.New("a map key that is not valid UTF-8")
			}
			if err := checkKeyable(it.Value(), depth+1); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("a value of kind %s", v.Kind())
	}
}
