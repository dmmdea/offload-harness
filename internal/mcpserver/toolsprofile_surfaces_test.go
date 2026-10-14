package mcpserver

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
)

// gateDevices is every accelerator id the server has a tool table for. TestGateDevicesCoverEveryAcceleratorTable reads the table
// switch in acceltools.go and fails when this list lacks an id the switch has.
var gateDevices = []string{"hailo-8l", "coral-edgetpu", "rknpu"}

// gateSurface is one tools/list the server can emit, for the review gate to read.
type gateSurface struct {
	name   string
	cfg    func(profile string) config.Config
	device string // the one accelerator the surface lists; "" for a combined surface
	fleet  bool   // that device is a fleet accelerator (its tools are forwarders), not a local one
}

// deviceSurfaceCfg is every optional surface on (the delegation pair, the browse lane) plus exactly one accelerator, local or
// fleet. With one device listed nothing shadows its table: every tool of the table is registered under its own description and
// schema, which the combined surfaces cannot show (the shared-name rule gives a name to the first listed device only).
func deviceSurfaceCfg(id string, fleet bool) func(string) config.Config {
	return func(profile string) config.Config {
		c := browseCfg()
		c.AgentDelegationEnabled = true
		if fleet {
			c.FleetAccelerators = []string{id}
		} else {
			c.Accelerators = []string{id}
		}
		c.ToolsSchemaProfile = profile
		return c
	}
}

// gateSurfaces is every tools/list the gate and the liveness checks read: the two combined surfaces (all three devices local, all
// three fleet) and, for each device, the surface that lists only that device as a local accelerator and the one that lists only
// that device as a fleet accelerator.
func gateSurfaces() []gateSurface {
	out := []gateSurface{
		{name: "full surface", cfg: fullSurfaceCfg},
		{name: "fleet accelerators", cfg: fleetSurfaceCfg},
	}
	for _, id := range gateDevices {
		out = append(out,
			gateSurface{name: "local " + id, cfg: deviceSurfaceCfg(id, false), device: id},
			gateSurface{name: "fleet " + id, cfg: deviceSurfaceCfg(id, true), device: id, fleet: true},
		)
	}
	return out
}

// requireSurfaceRegistersItsDevice fails unless a single-device surface advertises every tool of that device's table with the
// table's own description (a fleet surface: the description plus the forwarding note) and the table's own parameter names. A
// surface that registers nothing would otherwise pass the review gate with nothing read.
func requireSurfaceRegistersItsDevice(t *testing.T, s gateSurface, tools []*mcp.Tool) {
	t.Helper()
	byName := map[string]*mcp.Tool{}
	for _, tool := range tools {
		byName[tool.Name] = tool
	}
	table := accelMCPTools(s.device)
	if len(table) == 0 {
		t.Fatalf("%s: %s has no tool table", s.name, s.device)
	}
	for _, row := range table {
		tool, ok := byName[row.name]
		if !ok {
			t.Errorf("%s: %s is in the %s table and not in the advertised list", s.name, row.name, s.device)
			continue
		}
		if s.fleet {
			if !strings.HasPrefix(tool.Description, row.desc+" [FLEET: this box has no "+s.device+" ") || !strings.HasSuffix(tool.Description, "]") {
				t.Errorf("%s: %s is not the table description followed by the forwarding note: %q", s.name, row.name, tool.Description)
			}
		} else if tool.Description != row.desc {
			t.Errorf("%s: %s is not the table description: %q", s.name, row.name, tool.Description)
		}
		var want struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal([]byte(row.schema), &want); err != nil {
			t.Fatalf("%s: the %s schema of %s does not parse: %v", s.name, s.device, row.name, err)
		}
		schema, _ := tool.InputSchema.(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if len(props) != len(want.Properties) {
			t.Errorf("%s: %s advertises %d parameters, the %s table has %d", s.name, row.name, len(props), s.device, len(want.Properties))
		}
		for name := range want.Properties {
			if _, ok := props[name]; !ok {
				t.Errorf("%s: %s lacks the table parameter %q", s.name, row.name, name)
			}
		}
	}
}

// TestGateDevicesCoverEveryAcceleratorTable: a device with a tool table that the gate's device list lacks would have its
// descriptions advertised on a box that lists it and read by no test.
func TestGateDevicesCoverEveryAcceleratorTable(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "acceltools.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	ast.Inspect(f, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "accelMCPTools" {
			return true
		}
		ast.Inspect(fd.Body, func(m ast.Node) bool {
			cc, ok := m.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if id, err := strconv.Unquote(lit.Value); err == nil {
						ids = append(ids, id)
					}
				}
			}
			return true
		})
		return false
	})
	want := append([]string(nil), gateDevices...)
	sort.Strings(ids)
	sort.Strings(want)
	if len(ids) < 3 || !reflect.DeepEqual(ids, want) {
		t.Fatalf("accelMCPTools has tables for %v, the gate reads %v: add the device to gateDevices (and classify its sentences)", ids, want)
	}
}

// TestEveryDeviceTableIsReadByTheGate: for each device, as a local accelerator and as a fleet accelerator, every tool of its
// table is advertised under the table's description and parameters, so the review gate (which reads these lists) sees the
// descriptions the shared-name rule hides on a box that lists several devices, and sees a fleet one with the note
// registerAccelTools appends.
func TestEveryDeviceTableIsReadByTheGate(t *testing.T) {
	seen := 0
	for _, s := range gateSurfaces() {
		if s.device == "" {
			continue
		}
		requireSurfaceRegistersItsDevice(t, s, listTools(t, s.cfg("full")))
		seen += len(accelMCPTools(s.device))
	}
	if seen < 2*(11+4+3) {
		t.Fatalf("only %d device tools were checked: the per-device surfaces are not being read", seen)
	}
}

// TestCompactKeepsEveryToolAndParameterOnEverySurface is TestCompactKeepsEveryToolAndParameter over the per-device surfaces: the
// shadowed tables differ in schema (the Coral's size enum on offload_object_detect, the RKNPU's missing domain), and the
// structure of each must survive compaction.
func TestCompactKeepsEveryToolAndParameterOnEverySurface(t *testing.T) {
	for _, s := range gateSurfaces() {
		full, compact := listTools(t, s.cfg("full")), listTools(t, s.cfg("compact"))
		if len(full) != len(compact) {
			t.Errorf("%s: compact advertises %d tools, full %d", s.name, len(compact), len(full))
			continue
		}
		for i := range full {
			if full[i].Name != compact[i].Name {
				t.Errorf("%s: tool %d is %q in compact and %q in full", s.name, i, compact[i].Name, full[i].Name)
				continue
			}
			if !reflect.DeepEqual(stripProse(full[i].InputSchema), stripProse(compact[i].InputSchema)) {
				t.Errorf("%s: %s: compact changed the schema beyond its prose", s.name, full[i].Name)
			}
			if strings.TrimSpace(compact[i].Description) == "" {
				t.Errorf("%s: %s: compact left no description", s.name, full[i].Name)
			}
		}
	}
}

var fleetNoteDevice = regexp.MustCompile(`\[FLEET: this box has no ([a-z0-9-]+) `)

// TestCompactKeepsTheFleetForwardingSentence: a forwarder says it is on-box and free in its opening sentence ("on the LOCAL
// Hailo-8L NPU (free, on-box ...)") and, in the note registerAccelTools appends, that the call is forwarded to another node and
// that the image bytes travel with it. The note is the sentence that makes the first one true to read, so compact keeps it in
// every forwarder, on the combined fleet surface and on each device's own.
func TestCompactKeepsTheFleetForwardingSentence(t *testing.T) {
	checked := 0
	for _, s := range gateSurfaces() {
		if s.device != "" && !s.fleet {
			continue
		}
		if s.device == "" && s.name != "fleet accelerators" {
			continue
		}
		full, compact := listTools(t, s.cfg("full")), listTools(t, s.cfg("compact"))
		cd := map[string]string{}
		for _, tool := range compact {
			cd[tool.Name] = tool.Description
		}
		for _, ft := range full {
			m := fleetNoteDevice.FindStringSubmatch(ft.Description)
			if m == nil {
				continue
			}
			checked++
			note := ft.Description[strings.Index(ft.Description, "[FLEET:"):]
			if !strings.Contains(cd[ft.Name], note) {
				t.Errorf("%s: %s: compact dropped the forwarding note: %q\n compact description: %s", s.name, ft.Name, note, cd[ft.Name])
			}
			for _, word := range []string{"is forwarded to the fleet node", "bytes travel with the job"} {
				if !strings.Contains(note, word) {
					t.Errorf("%s: %s: the forwarding note no longer says %q (reworded?): %s", s.name, ft.Name, word, note)
				}
			}
		}
	}
	if checked < 13+11+4+3 {
		t.Fatalf("only %d forwarders were checked: the fleet surfaces register no forwarders", checked)
	}
}

// TestCompactKeepsTheSharedNameSpaceWarnings: offload_image_embed is a name all three devices share, and the box that lists only
// the Coral or only the RKNPU advertises that device's own description. Each says its vectors are in a space nothing else on the
// fleet shares, and the combined surfaces never show either sentence (the Hailo's table owns the name there).
func TestCompactKeepsTheSharedNameSpaceWarnings(t *testing.T) {
	pins := []struct{ device, tool, sentence string }{
		{"coral-edgetpu", "offload_image_embed", "space=efficientnet-edgetpu-s — NOT a CLIP space: there is no text tower to pair it with (no offload_text_embed / offload_zero_shot on this device)."},
		{"rknpu", "offload_image_embed", "space names the embedding space — NOT the Hailo's tinyclip and NOT the Coral's efficientnet-edgetpu-s, so vectors from different devices are never comparable, even at the same dimension."},
	}
	for _, p := range pins {
		for _, fleet := range []bool{false, true} {
			cfg := deviceSurfaceCfg(p.device, fleet)
			find := func(profile string) string {
				for _, tool := range listTools(t, cfg(profile)) {
					if tool.Name == p.tool {
						return tool.Description
					}
				}
				t.Fatalf("%s (fleet %v): %s is not advertised", p.device, fleet, p.tool)
				return ""
			}
			if !strings.Contains(find("full"), p.sentence) {
				t.Errorf("%s: the pinned sentence is not in the full description any more (reworded?): %q", p.device, p.sentence)
				continue
			}
			if !strings.Contains(find("compact"), p.sentence) {
				t.Errorf("%s (fleet %v): compact dropped %q", p.device, fleet, p.sentence)
			}
		}
	}
}

// TestGateFiresOnAnAppendedSuffix: a sentence appended to a registered description after the table text (the way
// registerAccelTools appends its forwarding note) is read by the gate like any other, and one whose only rule is that the data
// leaves the box (no prohibition word in it) is reported until it is kept or dropped with a reason.
func TestGateFiresOnAnAppendedSuffix(t *testing.T) {
	const name = "synthetic_forwarder_tool"
	const note = "[FLEET: this box has no gadget — the call is forwarded to the fleet node that does; the file is read HERE and its bytes travel with the job]"
	full := &mcp.Tool{Name: name, Description: "Detect things in a file on the local gadget for the caller. Returns {things}. " + note}

	if r := gateTool(full, compactTool(full), nil); len(r.problems) != 1 || !strings.Contains(r.problems[0], note) {
		t.Fatalf("an appended data-leaves-the-box note was not reported exactly once: %v", r.problems)
	}
	if r := gateTool(full, compactTool(full), map[string]string{note: "reviewed"}); len(r.problems) != 0 {
		t.Fatalf("a dropped-with-reason note was still reported: %v", r.problems)
	}
	compactKeepByTool[name] = map[string]bool{note: true}
	t.Cleanup(func() { delete(compactKeepByTool, name) })
	if r := gateTool(full, compactTool(full), nil); len(r.problems) != 0 || r.kept != 1 {
		t.Fatalf("a kept note was reported: %+v", r)
	}
}
