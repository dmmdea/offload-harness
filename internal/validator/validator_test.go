package validator

import (
	"strings"
	"testing"
)

func TestValidateAgainstSchema(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":   map[string]any{"type": "string"},
			"amount": map[string]any{"type": "number"},
		},
		"required": []any{"name", "amount"},
	}

	tests := []struct {
		name    string
		data    string
		wantErr string // substring; empty means the call must succeed
	}{
		{
			name: "conforming object passes",
			data: `{"name":"acme","amount":12.5}`,
		},
		{
			name:    "missing required field fails",
			data:    `{"name":"acme"}`,
			wantErr: "schema validation failed",
		},
		{
			name:    "wrong type fails",
			data:    `{"name":"acme","amount":"twelve"}`,
			wantErr: "schema validation failed",
		},
		{
			name:    "malformed json fails before schema checks",
			data:    `{"name":`,
			wantErr: "invalid json",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate([]byte(tc.data), schema)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Validate(%s) = %v, want nil", tc.data, err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("Validate(%s) = nil, want error containing %q", tc.data, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("Validate(%s) = %v, want error containing %q", tc.data, err, tc.wantErr)
			}
		})
	}
}

// An absent schema means the grammar already constrained the shape, so Validate
// degrades to a JSON well-formedness check rather than passing everything.
func TestValidateWithoutSchema(t *testing.T) {
	for _, schema := range []map[string]any{nil, {}} {
		if err := Validate([]byte(`{"anything":true}`), schema); err != nil {
			t.Errorf("Validate with empty schema rejected valid json: %v", err)
		}
		err := Validate([]byte(`not json`), schema)
		if err == nil {
			t.Fatal("Validate with empty schema accepted malformed json")
		}
		if !strings.Contains(err.Error(), "invalid json") {
			t.Errorf("got %v, want an invalid-json error", err)
		}
	}
}

// Documents a gap: the doc comment says the no-schema path "confirms it is a
// JSON object", but the check unmarshals into `any`, so a bare scalar or array
// is accepted. Pinned as-is — tightening it is a behaviour change that belongs
// in its own PR.
func TestValidateWithoutSchemaAcceptsNonObjects(t *testing.T) {
	for _, data := range []string{`123`, `"a string"`, `[1,2,3]`, `null`} {
		if err := Validate([]byte(data), nil); err != nil {
			t.Errorf("Validate(%s, nil) = %v; the no-schema path currently accepts any valid JSON", data, err)
		}
	}
}

func TestValidateRejectsUncompilableSchema(t *testing.T) {
	// "type" must be a string or array of strings; a number cannot compile.
	bad := map[string]any{"type": 42}
	err := Validate([]byte(`{}`), bad)
	if err == nil {
		t.Fatal("Validate accepted an uncompilable schema")
	}
	if !strings.Contains(err.Error(), "schema") {
		t.Errorf("got %v, want an error naming the schema", err)
	}
}
