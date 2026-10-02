package tierdocs

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// ADR 0049 Amendment 6 (register A-119 / A-129): the ampere-16 lane seat is started with a fixed KV pool, and its
// 35B seat's utilization is 0.855. The page that states a seat's operating point has to state both exactly, or it
// reads as the old declaration: the extra-seat table printed utilization with %.2f, which renders 0.855 as 0.85,
// and neither table said that a pinned pool makes vLLM ignore the utilization beside it.
func TestSeatTablesStateTheKVPoolAndTheExactUtilization(t *testing.T) {
	lane := laneSpec()
	lane.GPUMemoryUtilization = 0.84
	lane.KVCacheMemoryBytes = 1524713390
	extra := extraSpec()
	extra.GPUMemoryUtilization = 0.855
	page := renderTier("t", Profile{CtxSize: 8192, VLLMSeat: lane, ExtraVLLMSeats: []vllmseat.Spec{extra}, Layers: layers()}, nil)

	section := strings.Index(page, "### Extra vLLM seats")
	if section < 0 {
		t.Fatalf("the page lost its extra-seats section:\n%s", page)
	}
	laneTable, extraTable := page[:section], page[section:]

	if !strings.Contains(laneTable, "| kv_cache_memory_bytes | 1524713390 |") {
		t.Errorf("the lane seat's table does not state its pinned pool:\n%s", laneTable)
	}
	for _, row := range strings.Split(laneTable, "\n") {
		if strings.HasPrefix(row, "| kv_cache_memory_bytes |") && !strings.Contains(row, "ignores `gpu_memory_utilization`") {
			t.Errorf("the pool row must say vLLM ignores the utilization while the pool is pinned, or a reader tunes the wrong knob: %s", row)
		}
	}
	if !strings.Contains(laneTable, "| gpu_memory_utilization | 0.84 |") {
		t.Errorf("the lane seat's utilization is not printed as declared:\n%s", laneTable)
	}
	if !strings.Contains(extraTable, "| gpu_memory_utilization | 0.855 | the engine's share of the card |") {
		t.Errorf("the extra seat's utilization 0.855 is not printed exactly (a %%.2f format prints it as 0.85):\n%s", extraTable)
	}
	if strings.Contains(extraTable, "| gpu_memory_utilization | 0.85 |") || strings.Contains(extraTable, "| gpu_memory_utilization | 0.86 |") {
		t.Errorf("the extra seat's utilization was rounded:\n%s", extraTable)
	}
	if strings.Contains(extraTable, "kv_cache_memory_bytes") {
		t.Errorf("an extra seat that pins no pool printed a pool row (a pool is util-sized unless declared):\n%s", extraTable)
	}

	// An extra seat that does pin a pool states it too: a pin hidden from the page is a declaration nobody reads.
	extra.KVCacheMemoryBytes = 2147483648
	page = renderTier("t", Profile{CtxSize: 8192, VLLMSeat: lane, ExtraVLLMSeats: []vllmseat.Spec{extra}, Layers: layers()}, nil)
	if i := strings.Index(page, "### Extra vLLM seats"); i < 0 || !strings.Contains(page[i:], "| kv_cache_memory_bytes | 2147483648 |") {
		t.Errorf("a pinned extra seat's pool is not on the page:\n%s", page)
	}
}

// The two-decimal format stays for every value that has two decimals, so the other tiers' pages (util 0.90, 0.85,
// 0.65) are byte-for-byte what they were; only a value that needs a third digit prints it.
func TestUtilizationKeepsTwoDecimalsWhereTwoSuffice(t *testing.T) {
	for in, want := range map[float64]string{0.9: "0.90", 0.85: "0.85", 0.84: "0.84", 0.65: "0.65", 0.855: "0.855", 0.875: "0.875", 0.92: "0.92"} {
		if got := utilization(in); got != want {
			t.Errorf("utilization(%v) = %q, want %q", in, got, want)
		}
	}
	page := renderTier("t", Profile{CtxSize: 8192, VLLMSeat: laneSpec(), ExtraVLLMSeats: []vllmseat.Spec{extraSpec()}, Layers: layers()}, nil)
	if strings.Count(page, "| gpu_memory_utilization | 0.90 |") != 2 {
		t.Errorf("a util-0.9 seat must still print 0.90 in both tables:\n%s", page)
	}
	if strings.Contains(page, "kv_cache_memory_bytes") {
		t.Errorf("a seat with no pinned pool printed a pool row:\n%s", page)
	}
}
