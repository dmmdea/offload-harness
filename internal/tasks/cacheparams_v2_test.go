//go:build go1.27

package tasks

import (
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"testing"
)

// v2Form has its own JSON form only through the encoding/json/v2 method MarshalJSONTo,
// which the json v1 API honours on this toolchain. Without the check it would key like
// the plain string it renders as.
type v2Form string

func (v v2Form) MarshalJSONTo(enc *jsontext.Encoder) error {
	return enc.WriteToken(jsontext.String("V2-FORM-" + string(v)))
}

func TestCacheParamsKeyRejectsAV2OwnForm(t *testing.T) {
	// The hazard is real on this toolchain: json.Marshal renders the type its own way.
	b, err := json.Marshal(map[string]any{"x": v2Form("a")})
	if err != nil || string(b) != `{"x":"V2-FORM-a"}` {
		t.Skipf("json.Marshal does not honour MarshalJSONTo here (%s, %v): nothing to guard", b, err)
	}
	if _, err := CacheParamsKey(map[string]any{"x": v2Form("a")}); !errors.Is(err, ErrParamsNotKeyable) {
		t.Fatalf("a type with a v2 MarshalJSONTo was keyed (err %v); it shares the key of the string it renders as", err)
	}
	if _, err := CacheParamsKey(map[string]any{"x": "V2-FORM-a"}); err != nil {
		t.Fatalf("the plain string failed: %v", err)
	}
}
