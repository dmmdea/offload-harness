package core

import (
	"bytes"
	"encoding/json"
)

// Acceptance-named fields and the schema's `required` list (register C-74).
//
// An acceptance check that names a field (min_items:<f>:<n>, nonempty:<f>)
// reads it from the structured answer and FAILS CLOSED when it is absent. A
// schema that does not declare that field required lets a seat's direct JSON
// answer omit it, validates, skips the structured re-pack, and then fails
// acceptance at the delegator after a whole run; vLLM's structured_outputs
// likewise lets its grammar leave an optional field out. The fix is to say
// out loud, in the schema the seat is held to, that every field a check reads
// must be there — a missing one then goes to the re-pack instead of being
// delivered as valid.
//
// Only PRESENCE is declared here. Whether a present field is non-empty stays
// the acceptance check's job (delegator-side): a schema `minItems` would force
// a non-empty array out of the seat on a page that has nothing to say, which
// is pressure to invent items.

// AcceptedFields returns the schema fields the acceptance checks read
// (min_items and nonempty), in check order, without repeats. Checks that do not
// parse, and checks that read no field (contains, regex, diff_*), are skipped.
func AcceptedFields(acceptance []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range acceptance {
		chk, err := ParseAcceptanceCheck(a)
		if err != nil {
			continue
		}
		if chk.Kind != AccMinItems && chk.Kind != AccNonempty {
			continue
		}
		if chk.Arg == "" || seen[chk.Arg] {
			continue
		}
		seen[chk.Arg] = true
		out = append(out, chk.Arg)
	}
	return out
}

// RequireAcceptanceFields is RequireFields over the fields the acceptance
// checks name.
func RequireAcceptanceFields(schema json.RawMessage, acceptance []string) json.RawMessage {
	return RequireFields(schema, AcceptedFields(acceptance))
}

// RequireFields returns schema with every listed field added to its `required`
// list, in the order given after whatever the schema already required (the
// gbnf builder reads `required` first for field order, so the caller's own
// order is never disturbed). A field the schema does not declare under
// `properties` is left out: it could never be produced, and requiring it would
// turn an acceptance failure into an unsatisfiable schema. When nothing needs
// adding, or the schema is not an object with a properties map, the input is
// returned byte for byte. The rest of the schema is carried through untouched;
// only the top-level key order of a changed schema is normalized.
func RequireFields(schema json.RawMessage, fields []string) json.RawMessage {
	if len(fields) == 0 {
		return schema
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(schema, &top) != nil {
		return schema
	}
	var props map[string]json.RawMessage
	if raw, ok := top["properties"]; !ok || json.Unmarshal(raw, &props) != nil || len(props) == 0 {
		return schema
	}
	var required []json.RawMessage
	have := map[string]bool{}
	if raw, ok := top["required"]; ok {
		if json.Unmarshal(raw, &required) != nil {
			return schema // a malformed `required` is the seat validator's to report, not ours to rewrite
		}
		for _, r := range required {
			var s string
			if json.Unmarshal(r, &s) == nil {
				have[s] = true
			}
		}
	}
	added := false
	for _, f := range fields {
		if _, declared := props[f]; !declared || have[f] {
			continue
		}
		name, err := marshalPlain(f)
		if err != nil {
			continue
		}
		required = append(required, name)
		have[f] = true
		added = true
	}
	if !added {
		return schema
	}
	rb, err := marshalPlain(required)
	if err != nil {
		return schema
	}
	top["required"] = rb
	out, err := marshalPlain(top)
	if err != nil {
		return schema
	}
	return out
}

// marshalPlain is json.Marshal without HTML escaping, so a schema description
// that carries < > & is not rewritten to < on the way through.
func marshalPlain(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}
