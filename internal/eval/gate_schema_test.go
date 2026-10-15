package eval

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Every Go field of the input types has a schema property, so the schema cannot describe a
// different document from the one the gate reads.
func TestGateInputSchemaPropertiesMatchGoFields(t *testing.T) {
	var doc struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Defs       map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(GateInputSchema, &doc); err != nil {
		t.Fatal(err)
	}
	check := func(label string, typ reflect.Type, props map[string]json.RawMessage) {
		t.Helper()
		var goNames []string
		for i := 0; i < typ.NumField(); i++ {
			name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			goNames = append(goNames, name)
			if _, ok := props[name]; !ok {
				t.Errorf("%s: Go field %q has no schema property", label, name)
			}
		}
		if len(props) != len(goNames) {
			t.Errorf("%s: schema has %d properties, Go has %d (%v)", label, len(props), len(goNames), goNames)
		}
	}
	check("input", reflect.TypeOf(GateInput{}), doc.Properties)
	check("attempt", reflect.TypeOf(Attempt{}), doc.Defs["attempt"].Properties)
	check("task", reflect.TypeOf(Task{}), doc.Defs["task"].Properties)
	check("arm", reflect.TypeOf(Arm{}), doc.Defs["arm"].Properties)
	check("stamps", reflect.TypeOf(Stamps{}), doc.Defs["stamps"].Properties)
	check("judge", reflect.TypeOf(Judge{}), doc.Defs["judge"].Properties)
	check("receipt", reflect.TypeOf(JudgeReceipt{}), doc.Defs["receipt"].Properties)
}
