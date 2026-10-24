package gpuactivity

// knownerrors.go: a driver fault event read as a KNOWN ERROR (the "supervised log errors" of NVIDIA's AIOps
// product, and NVSentinel's Xid catalog), for `gpu faults`.
//
// THREE SOURCES, EACH SAYS WHICH IT IS. A Linux Xid has NVIDIA's own row: mnemonic, description and the two
// resolution buckets of NVIDIA's Xid catalog (xidcatalog.go, generated, Apache-2.0 via NVSentinel). A Windows
// nvlddmkm event has no NVIDIA table at all, so the three ids this fleet logs are read as the Xid they match,
// and the basis says "observed": id 13's message is the same "Graphics Exception" text Linux prints for Xid 13,
// id 153 is "Error occurred on GPUID", the engine reset after a fault or timeout (TDR), and id 14 the microcode
// reset. A Windows LiveKernelEvent (a live kernel dump Windows Error Reporting files) carries a bug check code
// whose name Microsoft documents for 117, 141 and 193; 1a8 and 1b8 are named from user reports only.
//
// NEVER A VERDICT. The buckets are NVIDIA's guidance for datacenter parts (the catalog's applicability columns
// name A100, H100, B100 and GB200, not GeForce), shown so an operator knows what NVIDIA would do; a wrong class
// costs a mislabelled line, never a changed lease verdict or a drained seat (C-83-B).

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// xidRow is one row of NVIDIA's Xid catalog: the code's mnemonic, NVIDIA's description, and its immediate and
// investigatory resolution buckets.
type xidRow struct {
	Mnemonic, Description, Immediate, Investigatory string
}

// KnownError is what a fault event is, as far as a table can say, and how sure the table is.
type KnownError struct {
	// Class names the event: "Xid 79", "nvlddmkm 153", "LiveKernelEvent 141".
	Class string `json:"class"`
	// Name is the short name: NVIDIA's mnemonic, or the bug check name.
	Name string `json:"name"`
	// Meaning says what happened, in the source's words where it has them.
	Meaning string `json:"meaning"`
	// Domain is where the fault sits: app, card, driver, display, link, system or unknown.
	Domain string `json:"domain"`
	// Severity is info, warn or fault.
	Severity string `json:"severity"`
	// NVIDIAAction and NVIDIAInvestigate are the catalog's immediate and investigatory resolution buckets
	// (NVIDIA's datacenter guidance), and NVIDIAActionMeaning the immediate bucket in NVIDIA's words.
	NVIDIAAction        string `json:"nvidia_action,omitempty"`
	NVIDIAActionMeaning string `json:"nvidia_action_meaning,omitempty"`
	NVIDIAInvestigate   string `json:"nvidia_investigate,omitempty"`
	// XidAnalogue is the Linux Xid a Windows event is read as (0 = none).
	XidAnalogue int `json:"xid_analogue,omitempty"`
	// Basis is how the class was reached: "nvidia-catalog" (the Xid's own row), "observed" (a Windows event read
	// by its message: NVIDIA publishes no Windows table), "microsoft-bugcheck" (a documented bug check name),
	// "user-reports" (a name only forum reports give) or "unknown".
	Basis string `json:"basis"`
}

// ClassifyEvent reads one driver event as a known error.
func ClassifyEvent(e DriverEvent) KnownError {
	switch e.Source {
	case "Xid":
		return xidKnown(e.ID)
	case "nvlddmkm":
		return nvlddmkmKnown(e.ID)
	}
	return KnownError{Class: fmt.Sprintf("%s %d", e.Source, e.ID), Name: "unknown", Meaning: "no table covers this source", Domain: "unknown", Severity: "warn", Basis: "unknown"}
}

// ClassOf is the class string of an event ("nvlddmkm 153"), without the table lookup.
func ClassOf(e DriverEvent) string { return fmt.Sprintf("%s %d", e.Source, e.ID) }

// xidKnown is NVIDIA's catalog row for an Xid code.
func xidKnown(code int) KnownError {
	k := KnownError{Class: fmt.Sprintf("Xid %d", code)}
	row, ok := xidCatalog[code]
	if !ok {
		k.Name, k.Meaning, k.Domain, k.Severity, k.Basis = "unknown", "not in NVIDIA's Xid catalog (or marked unused there)", "unknown", "warn", "unknown"
		return k
	}
	k.Name, k.Meaning, k.Basis = row.Mnemonic, row.Description, "nvidia-catalog"
	k.NVIDIAAction, k.NVIDIAInvestigate = row.Immediate, row.Investigatory
	k.NVIDIAActionMeaning = xidBuckets[row.Immediate]
	k.Domain, k.Severity = bucketReading(row.Immediate)
	return k
}

// bucketReading is where a fault with this immediate bucket sits and how bad it is: the bucket says what NVIDIA
// would restart (the application, the card, the machine), and that is the reading.
func bucketReading(bucket string) (domain, severity string) {
	switch bucket {
	case "IGNORE", "XID_154":
		return "card", "info"
	case "WORKFLOW_XID_45":
		return "app", "info"
	case "RESTART_APP", "CHECK_APP/CUDA", "WORKFLOW_XID_13", "WORKFLOW_XID_31":
		return "app", "warn"
	case "RESTART_VM", "CHECK_UVM":
		return "system", "warn"
	case "UPDATE_SWFW":
		return "driver", "warn"
	case "RESET_GPU", "CHECK_MECHANICALS", "CONTACT_SUPPORT", "WORKFLOW_XID_48":
		return "card", "fault"
	case "RESTART_BM":
		return "system", "fault"
	case "WORKFLOW_NVLINK_ERR", "WORKFLOW_NVLINK5_ERR":
		return "link", "fault"
	}
	return "unknown", "warn"
}

// nvlddmkmKnown reads a Windows nvlddmkm event id. Only the ids this fleet has logged are mapped; the rest are
// "unknown" rather than guessed.
func nvlddmkmKnown(id int) KnownError {
	k := KnownError{Class: fmt.Sprintf("nvlddmkm %d", id), Basis: "observed"}
	switch id {
	case 13:
		x := xidKnown(13)
		k.Name = "Graphics engine exception"
		k.Meaning = "the card's graphics/compute engine raised an exception (\"Graphics Exception\", \"Graphics SM Warp Exception\"): the Windows record of what Linux logs as Xid 13, " + x.Meaning
		k.Domain, k.Severity = x.Domain, x.Severity
		k.NVIDIAAction, k.NVIDIAActionMeaning, k.NVIDIAInvestigate = x.NVIDIAAction, x.NVIDIAActionMeaning, x.NVIDIAInvestigate
		k.XidAnalogue = 13
	case 153:
		k.Name = "Engine reset (TDR)"
		k.Meaning = "\"Error occurred on GPUID\": the driver reset the card's engines after a fault or a timeout (timeout detection and recovery); the processes on the card lose their GPU context"
		k.Domain, k.Severity = "driver", "fault"
	case 14:
		k.Name = "Microcode reset (UCodeReset TDR)"
		k.Meaning = "the driver reset the card's microcode after a fault or a timeout; the processes on the card lose their GPU context"
		k.Domain, k.Severity = "driver", "fault"
	default:
		k.Name, k.Meaning, k.Domain, k.Severity, k.Basis = "unknown", "an nvlddmkm event id with no mapping here", "unknown", "warn", "unknown"
	}
	return k
}

// liveKernelCodes names the bug check codes a LiveKernelEvent carries that touch the display stack. 117, 141 and
// 193 are in Microsoft's bug check reference; 1a8 and 1b8 are named from user reports only, and say so.
var liveKernelCodes = map[string]struct{ name, meaning, domain, severity, basis string }{
	"117": {"VIDEO_TDR_TIMEOUT_DETECTED", "the display driver did not respond in time and was reset (TDR)", "driver", "fault", "microsoft-bugcheck"},
	"141": {"VIDEO_ENGINE_TIMEOUT_DETECTED", "one of the card's engines did not respond in time and was reset (TDR)", "driver", "fault", "microsoft-bugcheck"},
	"193": {"VIDEO_DXGKRNL_LIVEDUMP", "the graphics kernel (dxgkrnl) took a live dump after a fault it recovered from", "driver", "warn", "microsoft-bugcheck"},
	"1a8": {"VIDEO_DXGKRNL_BLACK_SCREEN_LIVEDUMP", "a live dump for a black screen (name from user reports; Microsoft does not document it)", "display", "warn", "user-reports"},
	"1b8": {"VIDEO_MINIPORT_BLACK_SCREEN_LIVEDUMP", "a live dump for a black screen taken by the display driver (name from user reports; Microsoft does not document it)", "display", "warn", "user-reports"},
}

// ClassifyLiveKernel reads a LiveKernelEvent's bug check code (P1, hex without 0x, any case).
func ClassifyLiveKernel(code string) KnownError {
	code = strings.TrimLeft(strings.ToLower(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(code)), "0x")), "0")
	k := KnownError{Class: "LiveKernelEvent " + code}
	if c, ok := liveKernelCodes[code]; ok {
		k.Name, k.Meaning, k.Domain, k.Severity, k.Basis = c.name, c.meaning, c.domain, c.severity, c.basis
		return k
	}
	k.Name, k.Meaning, k.Domain, k.Severity, k.Basis = "unknown", "a live kernel dump with a bug check code no table here names", "unknown", "warn", "unknown"
	return k
}

// --- NVIDIA's Xid 13 workflow, applied to the units the records name ---

// smUnitRE reads the unit a graphics exception names: "Graphics SM Warp Exception on (GPC 1, TPC 0, SM 1): ...".
var smUnitRE = regexp.MustCompile(`\(GPC (\d+), TPC (\d+), SM (\d+)\)`)

// smUnit is the GPC and TPC a record names (the SM is not part of the rule), ok false when it names none.
func smUnit(text string) (gpc, tpc int, ok bool) {
	m := smUnitRE.FindStringSubmatch(text)
	if m == nil {
		return 0, 0, false
	}
	g, _ := strconv.Atoi(m[1])
	t, _ := strconv.Atoi(m[2])
	return g, t, true
}

// xid13Reading applies NVIDIA's Xid 13 workflow (catalog bucket WORKFLOW_XID_13) to the units a burst's
// graphics exceptions name: the same GPC and TPC again and again is the pattern NVIDIA sends to diagnostics as a
// possible hardware fault; faults that do not repeat on one GPC and TPC it reads as the application's. Fewer than
// two records that name a unit say nothing, and the reading is "".
func xid13Reading(units map[[2]int]int) string {
	records := 0
	for _, n := range units {
		records += n
	}
	if records < 2 {
		return ""
	}
	keys := make([][2]int, 0, len(units))
	for u := range units {
		keys = append(keys, u)
	}
	sort.Slice(keys, func(i, j int) bool {
		if units[keys[i]] != units[keys[j]] {
			return units[keys[i]] > units[keys[j]]
		}
		return keys[i][0] < keys[j][0] || (keys[i][0] == keys[j][0] && keys[i][1] < keys[j][1])
	})
	top := keys[0]
	// One unit, or one unit holding four records in five: a repeat on the same GPC and TPC.
	if len(units) == 1 || (units[top] >= 3 && units[top]*5 >= records*4) {
		return fmt.Sprintf("%d of %d records on one unit (GPC %d, TPC %d): NVIDIA's Xid 13 workflow reads a repeat on the same GPC and TPC as a possible hardware fault (RUN_DCGMEUD, then field diagnostics)", units[top], records, top[0], top[1])
	}
	return fmt.Sprintf("%d records spread over %d GPC/TPC units (most on GPC %d, TPC %d: %d): NVIDIA's Xid 13 workflow reads faults that do not repeat on one GPC and TPC as the application's (CHECK_APP/CUDA)", records, len(units), top[0], top[1], units[top])
}
