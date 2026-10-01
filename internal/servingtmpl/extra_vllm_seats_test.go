package servingtmpl

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// extraVLLMSpec is a tier's second vLLM seat: an on-demand digest seat on the same
// card as the lane seat (vllmSpec), never the agent lane.
func extraVLLMSpec() *vllmseat.Spec {
	return &vllmseat.Spec{
		ID:                   "qwen36-35b-a3b-gsq-vllm",
		Aliases:              []string{"a2-pool-35b", "qwen36-35b-gsq"},
		Unit:                 "vllm-35b-seat",
		Port:                 18797,
		Device:               "0",
		ModelRepo:            "hub/models--ISTA-DASLab--Qwen3.6-35B-A3B-2Bit-GSQ",
		MaxModelLen:          32768,
		GPUMemoryUtilization: 0.85,
		MaxNumSeqs:           8,
		MaxBatchedTokens:     4096,
		KVCacheDtype:         "fp8_e5m2",
		ToolCallParser:       "qwen3_coder",
		ReasoningParser:      "qwen3",
	}
}

// renderedSeatDoc is the slice of a rendered config the seat tests read.
type renderedSeatDoc struct {
	HealthCheckTimeout int `yaml:"healthCheckTimeout"`
	Models             map[string]struct {
		Cmd              string   `yaml:"cmd"`
		CmdStop          string   `yaml:"cmdStop"`
		Proxy            string   `yaml:"proxy"`
		UseModelName     string   `yaml:"useModelName"`
		ConcurrencyLimit int      `yaml:"concurrencyLimit"`
		Aliases          []string `yaml:"aliases"`
		TTL              int      `yaml:"ttl"`
	} `yaml:"models"`
	Matrix struct {
		Vars map[string]string `yaml:"vars"`
		Sets map[string]string `yaml:"sets"`
	} `yaml:"matrix"`
}

func renderSeats(t *testing.T, p Params) (string, renderedSeatDoc) {
	t.Helper()
	p.VLLMRuntime = vllmRuntime()
	out, err := Render(linuxCUDA(t), p)
	if err != nil {
		t.Fatal(err)
	}
	var doc renderedSeatDoc
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("the render is not parseable YAML: %v", err)
	}
	return out, doc
}

func varOf(t *testing.T, doc renderedSeatDoc, model string) string {
	t.Helper()
	for k, v := range doc.Matrix.Vars {
		if v == model {
			return k
		}
	}
	t.Fatalf("no matrix var names %s (vars: %v)", model, doc.Matrix.Vars)
	return ""
}

// Two vLLM seats on one 16 GB card cannot both be loaded: 13.9 + 11.7 GiB against a
// 15.3 GiB card, and each is sized with util 0.85-0.87 of it. If both joined the residents
// set as co-resident members (`& a & b`) the matrix would call the pair a valid
// combination and llama-swap would load the second beside the first — an OOM at the
// engine's first allocation. The seats must be ALTERNATIVES of each other inside the
// residents set: still resident-class (an ordinary chat request never evicts the agent
// lane) but one heavy seat at a time.
func TestExtraVLLMSeatsRenderAsAlternativesOfTheLaneSeat(t *testing.T) {
	p := params()
	p.VLLMSeat = vllmSpec()
	p.ExtraVLLMSeats = []*vllmseat.Spec{extraVLLMSpec()}
	_, doc := renderSeats(t, p)

	lane, extra := "qwen3.5-4b-vllm", "qwen36-35b-a3b-gsq-vllm"
	for _, id := range []string{lane, extra} {
		if _, ok := doc.Models[id]; !ok {
			t.Fatalf("model %s is not in the rendered config (got %v)", id, modelKeys(doc))
		}
	}
	laneVar, extraVar := varOf(t, doc, lane), varOf(t, doc, extra)
	if laneVar == extraVar {
		t.Fatalf("both seats share matrix var %q", laneVar)
	}
	want := "(" + laneVar + " | " + extraVar + ")"
	residents := doc.Matrix.Sets["residents"]
	if !strings.Contains(residents, want) {
		t.Fatalf("residents = %q, want the two heavy seats as alternatives %q inside the residents set", residents, want)
	}
	// The shape that would demand both loaded at once must not appear anywhere.
	for name, expr := range doc.Matrix.Sets {
		if strings.Contains(expr, laneVar+" & "+extraVar) || strings.Contains(expr, extraVar+" & "+laneVar) {
			t.Errorf("set %q joins both vLLM seats as CO-RESIDENT members (%q): the card cannot hold them together", name, expr)
		}
		if name != "residents" && (strings.Contains(expr, "| "+extraVar) || strings.Contains(expr, "| "+laneVar)) {
			t.Errorf("set %q joins a vLLM seat as an alternative of the cascade (%q): an ordinary request could evict it", name, expr)
		}
	}
	// A cascade request beside either heavy seat is the cascade seat guard's business,
	// exactly as with one seat; the matrix keeps the memory stack resident with both.
	if !strings.HasPrefix(residents, "emb & rer") {
		t.Errorf("the memory stack left the residents set: %q", residents)
	}
}

// The extra seat's entry names its OWN wrappers: the lane seat's `vllm-seat-cmd.sh`
// has the lane unit baked in, so a second entry pointing at it would start the wrong
// engine when the extra seat was requested.
func TestExtraVLLMSeatEntryUsesItsOwnWrappersAndTheLanesDoNotMove(t *testing.T) {
	p := params()
	p.VLLMSeat = vllmSpec()
	p.ExtraVLLMSeats = []*vllmseat.Spec{extraVLLMSpec()}
	_, doc := renderSeats(t, p)
	lane, extra := doc.Models["qwen3.5-4b-vllm"], doc.Models["qwen36-35b-a3b-gsq-vllm"]
	if lane.Cmd != "/srv/llama-swap/seat/vllm-seat-cmd.sh" || lane.CmdStop != "/srv/llama-swap/seat/vllm-seat-cmdstop.sh" {
		t.Errorf("the lane seat's wrappers moved: %q / %q", lane.Cmd, lane.CmdStop)
	}
	if extra.Cmd != "/srv/llama-swap/seat/vllm-35b-seat-cmd.sh" || extra.CmdStop != "/srv/llama-swap/seat/vllm-35b-seat-cmdstop.sh" {
		t.Errorf("the extra seat must drive ITS unit through its own wrappers, got %q / %q", extra.Cmd, extra.CmdStop)
	}
	if extra.Proxy != "http://192.0.2.10:18797" || extra.UseModelName != "qwen36-35b-a3b-gsq-vllm" || extra.ConcurrencyLimit != 8 || extra.TTL != 300 {
		t.Errorf("extra seat entry = %+v, want the literal proxy, its own served name, concurrencyLimit 8 and ttl 300", extra)
	}
	if !reflect.DeepEqual(extra.Aliases, []string{"a2-pool-35b", "qwen36-35b-gsq"}) {
		t.Errorf("aliases = %v", extra.Aliases)
	}
}

// A box that runs the extra seat but not the lane seat still gets a valid, resident
// entry: the seat is the only heavy member, so it joins the residents set alone.
func TestAnExtraVLLMSeatAloneJoinsTheResidentsSetPlain(t *testing.T) {
	p := params()
	p.ExtraVLLMSeats = []*vllmseat.Spec{extraVLLMSpec()}
	_, doc := renderSeats(t, p)
	id := varOf(t, doc, "qwen36-35b-a3b-gsq-vllm")
	if got := doc.Matrix.Sets["residents"]; got != "emb & rer & "+id {
		t.Errorf("residents = %q, want a plain single member %q", got, "emb & rer & "+id)
	}
}

// Every tier declares no extra seats, and their output must stay byte-identical to a
// build that had no support for them — the same line TestNoVLLMSeatChangesNothing
// holds for the lane seat.
func TestNoExtraVLLMSeatsChangesNothing(t *testing.T) {
	base := params()
	base.VLLMSeat = vllmSpec()
	base.VLLMRuntime = vllmRuntime()
	want, err := Render(linuxCUDA(t), base)
	if err != nil {
		t.Fatal(err)
	}
	for name, extras := range map[string][]*vllmseat.Spec{"nil": nil, "empty": {}, "a nil entry": {nil}} {
		p := base
		p.ExtraVLLMSeats = extras
		got, err := Render(linuxCUDA(t), p)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s ExtraVLLMSeats changed the rendered config", name)
		}
	}
	if strings.Contains(want, "(vagt") || strings.Contains(want, "vagt2") {
		t.Errorf("a lane-seat-only render grew an alternation:\n%s", want)
	}
}

// A seat may not shadow a model the template owns, and every seat's cold load raises the
// global health-check ceiling (llama-swap's is one number for every model).
func TestExtraVLLMSeatRefusesATemplateModelNameAndRaisesTheHealthCeiling(t *testing.T) {
	p := params()
	p.VLLMSeat = vllmSpec()
	e := extraVLLMSpec()
	e.ID = "offload-e4b"
	p.ExtraVLLMSeats = []*vllmseat.Spec{e}
	p.VLLMRuntime = vllmRuntime()
	if _, err := Render(linuxCUDA(t), p); err == nil || !strings.Contains(err.Error(), "already defined") {
		t.Fatalf("an extra seat named after a template model must be refused, got %v", err)
	}
	slow := extraVLLMSpec()
	slow.HealthCheckTimeout = 900
	p.ExtraVLLMSeats = []*vllmseat.Spec{slow}
	_, doc := renderSeats(t, p)
	if doc.HealthCheckTimeout != 900 {
		t.Errorf("healthCheckTimeout = %d, want the slowest seat's 900", doc.HealthCheckTimeout)
	}
}

func modelKeys(doc renderedSeatDoc) []string {
	out := make([]string, 0, len(doc.Models))
	for k := range doc.Models {
		out = append(out, k)
	}
	return out
}

// A seat rendered from an incomplete runtime is a dead entry (`cmd: /vllm-35b-seat-cmd.sh`,
// `proxy: http://:18797`) that every gate reading the text passes, because none of them reads the
// address. Render refuses it by seat name instead: a caller that has a seat has resolved a runtime
// for it, so an incomplete one is that caller's bug.
func TestAVLLMSeatWithAnIncompleteRuntimeIsRefused(t *testing.T) {
	without := func(clear func(*vllmseat.Runtime)) vllmseat.Runtime {
		r := vllmRuntime()
		clear(&r)
		return r
	}
	for setName, set := range map[string]func(*Params){
		"an extra seat alone": func(p *Params) { p.ExtraVLLMSeats = []*vllmseat.Spec{extraVLLMSpec()} },
		"the lane seat":       func(p *Params) { p.VLLMSeat = vllmSpec() },
		"both seats":          func(p *Params) { p.VLLMSeat = vllmSpec(); p.ExtraVLLMSeats = []*vllmseat.Spec{extraVLLMSpec()} },
	} {
		for rtName, tc := range map[string]struct {
			rt   vllmseat.Runtime
			want string
		}{
			"no runtime at all": {vllmseat.Runtime{}, "no user"},
			"no address":        {without(func(r *vllmseat.Runtime) { r.ProxyHost = "" }), "no proxy host"},
			"no seat directory": {without(func(r *vllmseat.Runtime) { r.SeatDir = "" }), "no seat dir"},
		} {
			t.Run(setName+"/"+rtName, func(t *testing.T) {
				p := params()
				set(&p)
				p.VLLMRuntime = tc.rt
				_, err := Render(linuxCUDA(t), p)
				if err == nil || !strings.Contains(err.Error(), "cannot be rendered") || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("a seat with %s must be refused, naming what is missing (%q); got %v", rtName, tc.want, err)
				}
			})
		}
	}
	// The control: the same params with a complete runtime render.
	p := params()
	p.ExtraVLLMSeats = []*vllmseat.Spec{extraVLLMSpec()}
	if _, doc := renderSeats(t, p); doc.Models["qwen36-35b-a3b-gsq-vllm"].Proxy == "" {
		t.Error("the control render carries no proxy address")
	}
}
