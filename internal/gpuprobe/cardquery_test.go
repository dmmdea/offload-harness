package gpuprobe

// F24 (2): the card table is read with the per-device query alone. nvidia-smi's process listing is the
// slow phase under load, and the allocation has no use for it: which process holds a card is the
// foreign-busy reader's question, asked by the verbs that want it and never by the table every
// media call reads. These pins keep a "while we are here, list the processes" out of the shared query.

import (
	"strings"
	"testing"
)

func TestTheCardTableQueriesTheDeviceFieldsAndNeverListsProcesses(t *testing.T) {
	for name, args := range map[string][]string{"full": smiQueryArgs, "without display_attached": smiQueryArgsNoAttached} {
		if len(args) != 2 {
			t.Fatalf("%s: %v, want a --query-gpu argument and a --format argument", name, args)
		}
		if !strings.HasPrefix(args[0], "--query-gpu=") {
			t.Errorf("%s: %q is not a per-device query", name, args[0])
		}
		if !strings.HasPrefix(args[1], "--format=csv") {
			t.Errorf("%s: %q is not the csv format", name, args[1])
		}
		for _, a := range args {
			low := strings.ToLower(a)
			if strings.Contains(low, "compute-apps") || strings.Contains(low, "pmon") || a == "-q" || a == "--query" {
				t.Errorf("%s: %q lists processes", name, a)
			}
		}
	}
	// Both variants ask for the same device fields, the second without display_attached.
	if want := strings.TrimSuffix(smiQueryArgs[0], ",display_attached"); smiQueryArgsNoAttached[0] != want {
		t.Errorf("the fallback query %q is not the full one %q less display_attached", smiQueryArgsNoAttached[0], smiQueryArgs[0])
	}
	// And the columns are the parser's, IN ITS ORDER. ParseSmiMemoryDevices reads them by position: index,
	// uuid, name, memory.total, memory.used, then utilization.gpu, display_active and display_attached. The
	// allocator reads the identity, the memory and the two display columns; utilisation rides along for the
	// views that print it (`gpu cards`, `gpu status`) and the health sampler, which share this reader. It
	// is not dropped for the allocation's sake: a query without it shifts the display columns one place left
	// (display_attached would be read as display_active, and the operator's screen would look free to the
	// allocator), and what it costs was measured at about 20 ms of a 115 ms exec on a quiet box.
	wantFields := "index,uuid,name,memory.total,memory.used,utilization.gpu,display_active,display_attached"
	if got := strings.TrimPrefix(smiQueryArgs[0], "--query-gpu="); got != wantFields {
		t.Errorf("the table query is %q, want the parser's columns in its order %q", got, wantFields)
	}
}
