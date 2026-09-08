package delegate

import (
	"context"
	"reflect"
	"testing"
)

// A node's advertised accelerators reach the delegator's view (Coral Phase B):
// accelremote picks the node that carries a device by this field. Absent =
// none, never an error — every pre-0.114.0 node lacks it.
func TestFetchNodeViewDecodesAccelerators(t *testing.T) {
	srv := healthServer(t, `{"node_id":"lenovo-ampere16","accelerators":["coral-edgetpu"],"queue_depth":0}`, nil)
	got, err := FetchNodeView(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Accelerators, []string{"coral-edgetpu"}) {
		t.Fatalf("Accelerators = %v", got.Accelerators)
	}
	srv2 := healthServer(t, `{"node_id":"aorus-ampere8","queue_depth":0}`, nil)
	got2, err := FetchNodeView(context.Background(), srv2.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got2.Accelerators) != 0 {
		t.Fatalf("absent field decoded as %v", got2.Accelerators)
	}
}
