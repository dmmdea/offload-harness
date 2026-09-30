package research

import (
	"encoding/json"
	"reflect"
	"testing"
)

// "one check, never one per required array" and "in the order the caller
// wrote them" are both stated rules of nonEmptyChecks (a caller who lists every
// field as required must not be asked for items in all of them). The existing
// test marks a single array, so neither rule is pinned.
func TestBuildAsksForOneItemsCheckInTheCallersOrder(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{` +
		`"a_findings":{"type":"array","items":{"type":"string"}},` +
		`"b_risks":{"type":"array","items":{"type":"string"}},` +
		`"summary":{"type":"string"}},` +
		`"required":["summary","b_risks","a_findings"]}`)
	specs, _ := Build(Request{Goal: "Digest it.", OutputSchema: schema}, pageWith(prosePage))
	if got, want := minItemsChecks(specs[0].Acceptance), []string{"min_items:b_risks:1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("min_items checks = %v, want exactly %v: one check, for the first array the caller listed", got, want)
	}
}
