// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

// Package schemaref reflects a Go result struct into a JSON Schema
// (draft 2020-12) describing the JSON that struct marshals to.
//
// It exists so an MCP tool's advertised outputSchema is DERIVED from the
// struct the handler actually returns, rather than hand-written beside it.
// A hand-written schema is a second source of truth that drifts silently the
// first time a field is renamed; a derived one cannot.
//
// Deliberately dependency-free and deliberately small. It covers exactly what
// this CLI's result envelopes use — structs, pointers, slices, maps with
// string keys, the numeric and string kinds, time.Duration, and json.RawMessage
// — and returns an open `{}` (any) for anything else rather than guessing a
// shape. Output is deterministic (properties are emitted in a stable order and
// marshalled through an ordered writer) so a committed golden file is a real
// drift gate.
package schemaref

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Schema is the subset of JSON Schema this package emits.
type Schema struct {
	Type                 string             `json:"type,omitempty"`
	Description          string             `json:"description,omitempty"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	Required             []string           `json:"required,omitempty"`
	Items                *Schema            `json:"items,omitempty"`
	AdditionalProperties *Schema            `json:"additionalProperties,omitempty"`
	// PropertyOrder is not part of JSON Schema; it is dropped at marshal time
	// and exists only to keep the emitted property object deterministic.
	PropertyOrder []string `json:"-"`
}

// Of reflects T. The returned schema always has type "object" at the root:
// every result envelope in this CLI is an object, and a tool that advertised
// a non-object outputSchema would be rejected by the 2025-06-18 contract.
func Of[T any]() *Schema {
	var zero T
	s := reflectType(reflect.TypeOf(&zero).Elem(), map[reflect.Type]bool{})
	if s.Type != "object" {
		return &Schema{Type: "object"}
	}
	return s
}

// JSON renders the schema with $schema and deterministic property order.
func JSON(s *Schema) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("{\n")
	buf.WriteString("  \"$schema\": \"https://json-schema.org/draft/2020-12/schema\",\n")
	body, err := marshal(s, "")
	if err != nil {
		return nil, err
	}
	// marshal returns a full object; splice its members in after $schema.
	inner := strings.TrimSpace(string(body))
	inner = strings.TrimPrefix(inner, "{")
	inner = strings.TrimSuffix(inner, "}")
	inner = strings.TrimRight(strings.TrimLeft(inner, "\n"), "\n ")
	if inner != "" {
		buf.WriteString(inner)
	}
	buf.WriteString("\n}\n")
	return buf.Bytes(), nil
}

// marshal writes one schema object with properties in PropertyOrder.
func marshal(s *Schema, indent string) ([]byte, error) {
	var buf bytes.Buffer
	inner := indent + "  "
	buf.WriteString("{\n")
	var parts []string
	if s.Type != "" {
		parts = append(parts, fmt.Sprintf("%s%q: %q", inner, "type", s.Type))
	}
	if s.Description != "" {
		d, _ := json.Marshal(s.Description)
		parts = append(parts, fmt.Sprintf("%s%q: %s", inner, "description", d))
	}
	if len(s.Properties) > 0 {
		var pb bytes.Buffer
		pb.WriteString(fmt.Sprintf("%s%q: {\n", inner, "properties"))
		for i, name := range s.PropertyOrder {
			sub, err := marshal(s.Properties[name], inner+"  ")
			if err != nil {
				return nil, err
			}
			nameJSON, _ := json.Marshal(name)
			pb.WriteString(fmt.Sprintf("%s  %s: %s", inner, nameJSON, strings.TrimSpace(string(sub))))
			if i < len(s.PropertyOrder)-1 {
				pb.WriteString(",")
			}
			pb.WriteString("\n")
		}
		pb.WriteString(inner + "}")
		parts = append(parts, pb.String())
	}
	if s.Items != nil {
		sub, err := marshal(s.Items, inner)
		if err != nil {
			return nil, err
		}
		parts = append(parts, fmt.Sprintf("%s%q: %s", inner, "items", strings.TrimSpace(string(sub))))
	}
	if s.AdditionalProperties != nil {
		sub, err := marshal(s.AdditionalProperties, inner)
		if err != nil {
			return nil, err
		}
		parts = append(parts, fmt.Sprintf("%s%q: %s", inner, "additionalProperties", strings.TrimSpace(string(sub))))
	}
	if len(s.Required) > 0 {
		req, _ := json.Marshal(s.Required)
		parts = append(parts, fmt.Sprintf("%s%q: %s", inner, "required", req))
	}
	buf.WriteString(strings.Join(parts, ",\n"))
	buf.WriteString("\n" + indent + "}")
	return buf.Bytes(), nil
}

var rawMessageType = reflect.TypeOf(json.RawMessage(nil))

// reflectType maps a Go type onto a schema. seen breaks recursive types by
// degrading to an open object rather than looping forever.
func reflectType(t reflect.Type, seen map[reflect.Type]bool) *Schema {
	if t == nil {
		return &Schema{}
	}
	if t == rawMessageType {
		// Arbitrary embedded JSON: any shape is legal here, and pinning one
		// would reject valid results.
		return &Schema{}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return reflectType(t.Elem(), seen)
	case reflect.Bool:
		return &Schema{Type: "boolean"}
	case reflect.String:
		return &Schema{Type: "string"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &Schema{Type: "integer"}
	case reflect.Float32, reflect.Float64:
		return &Schema{Type: "number"}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			// []byte marshals to a base64 string.
			return &Schema{Type: "string"}
		}
		return &Schema{Type: "array", Items: reflectType(t.Elem(), seen)}
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return &Schema{Type: "object"}
		}
		return &Schema{Type: "object", AdditionalProperties: reflectType(t.Elem(), seen)}
	case reflect.Interface:
		// `any` fields carry provider-shaped payloads; an open schema is the
		// honest description.
		return &Schema{}
	case reflect.Struct:
		if seen[t] {
			return &Schema{Type: "object"}
		}
		seen[t] = true
		defer delete(seen, t)
		return reflectStruct(t, seen)
	default:
		return &Schema{}
	}
}

func reflectStruct(t reflect.Type, seen map[reflect.Type]bool) *Schema {
	s := &Schema{Type: "object", Properties: map[string]*Schema{}}
	var required []string
	var collect func(reflect.Type)
	collect = func(st reflect.Type) {
		for i := 0; i < st.NumField(); i++ {
			f := st.Field(i)
			if f.PkgPath != "" && !f.Anonymous {
				continue // unexported
			}
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, opts, _ := strings.Cut(tag, ",")
			if f.Anonymous && name == "" {
				// Embedded struct: its fields are promoted into this object.
				ft := f.Type
				if ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				if ft.Kind() == reflect.Struct {
					collect(ft)
					continue
				}
			}
			if name == "" {
				name = f.Name
			}
			if _, exists := s.Properties[name]; exists {
				continue
			}
			s.Properties[name] = reflectType(f.Type, seen)
			s.PropertyOrder = append(s.PropertyOrder, name)
			// A field without omitempty is always present in the marshalled
			// object, so it is genuinely required. omitempty fields are not.
			if !strings.Contains(opts, "omitempty") && f.Type.Kind() != reflect.Pointer {
				required = append(required, name)
			}
		}
	}
	collect(t)
	sort.Strings(required)
	s.Required = required
	return s
}
