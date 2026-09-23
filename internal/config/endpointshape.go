package config

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/netguard"
)

// FleetNodePort is the port a fleet node's dispatcher listens on — the port half
// of FleetListen's default, spelled once so the delegate_remotes shape check and
// the operator guide cannot drift from it. It is a STRING because every consumer
// here compares it against url.URL.Port().
const FleetNodePort = "18811"

// deadEndpointPorts are ports a configured HTTP base can never be answered on.
// A base pointed at one of them is not a slow endpoint, it is an unset one, and
// the only way the harness ever learned that was a dial timeout on the first
// real call: the delegation ledger carried 8 dispatches to
// http://127.0.0.1:9/v1/v1/chat/completions, each paying the full timeout before
// anything named the port. Refusing at load turns a timeout class into a config
// error that names the key.
// The set is keyed on the port NUMBER, not the text of it. Keying on the string
// meant ":09" — which url.Parse preserves verbatim and every dialer reads as 9 —
// walked straight past the check that exists for exactly that port.
var deadEndpointPorts = map[int]string{
	0: "port 0 asks the OS for ANY free port, so nothing ever listens on it",
	9: "port 9 is the IANA discard port, the shape of an endpoint whose value was never substituted",
}

// validateBaseURL is the whole refusal for ONE configured HTTP base: it must be
// a URL that can be dialled, and its port must be one something can answer on.
//
// The URL half is not pedantry, it is the headline scenario. A value that was
// never substituted usually is not a URL at all, and the three shapes measured
// against net/url all used to pass in SILENCE on every key that is not one of
// the two tailnet-guarded maps:
//
//	"${NODE_A_HOST}:18811"  parse error — first path segment cannot contain colon
//	"http://node-a:$PORT"   parse error — invalid port
//	"node-a:18811"          PARSES, as scheme "node-a" with an EMPTY host
//
// The third is the one that matters: nothing errors, there is no port for a port
// check to judge, and the dialer resolves nothing. So a parse error, a scheme
// that is not http/https, and an empty host are all hard errors naming the key
// and the value.
//
// An EMPTY value is not a finding: an unset optional key is a machine that does
// not have that thing, which every consumer already handles.
//
// Only the PORT is judged beyond that. A loopback host on an unusual port is
// explicitly fine — INV-10 sanctions a loopback-only bench twin beside the
// production seat on the delegator box, and refusing it would break measurement.
func validateBaseURL(label, raw string) error {
	v := strings.TrimSpace(raw)
	if v == "" {
		return nil
	}
	u, why := inspectBase(v)
	if u == nil {
		return fmt.Errorf("%s: %q is not a usable URL: %s", label, v, why)
	}
	return deadPortErr(label, v, u)
}

// deadPortErr refuses a parsed base whose port cannot answer.
func deadPortErr(label, raw string, u *url.URL) error {
	port := u.Port()
	if port == "" {
		return nil
	}
	n, err := strconv.Atoi(port)
	if err != nil { // unreachable: url.Parse already refuses a non-numeric port
		return nil
	}
	why, dead := deadEndpointPorts[n]
	if !dead {
		return nil
	}
	return fmt.Errorf("%s: %q dials port :%s — %s; set the real port or remove the key", label, raw, port, why)
}

// validateEndpointValue is the ONE per-value gate every configured HTTP base
// passes through, so the never-cloud rule and the dead-port rule cannot drift
// apart across the keys that carry a base URL. tailnet selects the never-cloud
// guard: it is on for the maps that route model traffic off this box
// (seat_endpoints, cascade_remote_lanes) and the caller decides for the rest.
func validateEndpointValue(label, raw string, tailnet bool) error {
	if tailnet {
		if err := netguard.TailnetURL(raw); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}
	return validateBaseURL(label, raw)
}

// validateEndpointList is validateTailnetEndpoints' list twin: same per-value
// gate, indexed label. delegate_remotes is the one fleet URL LIST in the file.
func validateEndpointList(jsonKey string, values []string, tailnet bool) error {
	for i, v := range values {
		if err := validateEndpointValue(fmt.Sprintf("%s[%d]", jsonKey, i), v, tailnet); err != nil {
			return err
		}
	}
	return nil
}

// validateConfiguredBases holds every OTHER configured HTTP base in the file to
// the usable-URL and dead-port rules — the singular endpoint keys plus
// delegate_remotes. The two maps are covered by validateTailnetEndpoints, which
// runs the same per-value gate.
//
// delegate_remotes deliberately does NOT take the tailnet guard here: that would
// be a new refusal for a key that has never had one, and this release's job is
// to name the unusable-base class, not to re-litigate fleet membership. The dial
// gate (netguard.SafeTransport) still vets every remote at connect time.
func validateConfiguredBases(c Config) error {
	for _, kv := range []struct{ key, val string }{
		{"endpoint", c.Endpoint},
		{"fleet_queue_holder", c.FleetQueueHolder},
		{"tts_endpoint", c.TTSEndpoint},
		{"nim_endpoint", c.NIMEndpoint},
		{"hailo_endpoint", c.HailoEndpoint},
		{"coral_endpoint", c.CoralEndpoint},
		{"pair_workloads_endpoint", c.PairWorkloadsEndpoint},
	} {
		if err := validateBaseURL(kv.key, kv.val); err != nil {
			return err
		}
	}
	return validateEndpointList("delegate_remotes", c.DelegateRemotes, false)
}

// loopbackBase reports whether a parsed base URL points at this box. It asks
// netguard, which owns the notion, rather than re-deriving it: a portless base
// is judged through a synthetic port so the IPv6 bracket handling stays there
// too.
func loopbackBase(u *url.URL) bool {
	if netguard.LoopbackAddr(u.Host) {
		return true
	}
	return netguard.LoopbackAddr(net.JoinHostPort(u.Hostname(), "0"))
}

// EndpointWarnings returns the non-fatal findings about this box's FLEET and
// CASCADE base URLs — shapes that load, dial, and then fail in a way that names
// neither the key nor the value (register S-38).
//
// They warn rather than refuse for one release, on purpose. A strict validator
// that refuses a working odd config is a worse outage than the dial timeout it
// replaces, so the operator gets a named `doctor` row and a startup line first;
// the refusal can follow once the fleet is known clean.
//
// Every message names the KEY, the INDEX or map key, and the VALUE: a box with
// four remotes must not leave the operator guessing which one is wrong.
func EndpointWarnings(c Config) []string {
	var out []string
	for i, raw := range c.DelegateRemotes {
		if strings.TrimSpace(raw) == "" {
			continue // unset optional slot — validateBaseURL exempts it too; see inspectBase.
		}
		label := fmt.Sprintf("delegate_remotes[%d]", i)
		u, why := inspectBase(raw)
		if u == nil {
			out = append(out, fmt.Sprintf("%s %q is not a usable URL: %s", label, raw, why))
			continue
		}
		if loopbackBase(u) {
			out = append(out, fmt.Sprintf("%s %q is a loopback base — a delegate REMOTE is another box, so this places \"remote\" work back on this one; name the node by its tailnet hostname", label, raw))
		}
		if u.Port() != FleetNodePort {
			out = append(out, fmt.Sprintf("%s %q is not on the fleet node port :%s — delegate_remotes are fleet NODE base URLs (the node's fleet_listen), and the job routes do not exist on any other port", label, raw, FleetNodePort))
		}
		if hasV1Suffix(u) {
			out = append(out, fmt.Sprintf("%s %q carries a /v1 suffix — a fleet node base is a ROOT (its routes hang off /fleet/); /v1 belongs to an OpenAI-compatible seat, not to a node", label, raw))
		}
	}
	ownPort := endpointPort(c)
	for _, key := range sortedLaneKeys(c.CascadeRemoteLanes) {
		raw := c.CascadeRemoteLanes[key]
		if strings.TrimSpace(raw) == "" {
			continue // unset optional slot — validateBaseURL exempts it too; see inspectBase.
		}
		label := fmt.Sprintf("cascade_remote_lanes[%q]", key)
		u, why := inspectBase(raw)
		if u == nil {
			out = append(out, fmt.Sprintf("%s %q is not a usable URL: %s", label, raw, why))
			continue
		}
		// A lane base is one of exactly two things (internal/llamaclient
		// laneProbe): a plain llama-swap serving the same models this box's
		// `endpoint` does, or a fleet node reached over /fleet/chat. Any other
		// port is neither, which is how a lane survived pointed at a serving
		// unit retired weeks earlier.
		if port := u.Port(); port != FleetNodePort && port != ownPort {
			out = append(out, fmt.Sprintf("%s %q is on neither shape a lane can be: a fleet node (:%s) or a llama-swap on this box's own endpoint port (:%s)", label, raw, FleetNodePort, ownPort))
		}
		if hasV1Suffix(u) {
			out = append(out, fmt.Sprintf("%s %q carries a /v1 suffix — a lane base is a ROOT; the client appends the completion path itself, so a /v1 base dials /v1/v1/chat/completions", label, raw))
		}
	}
	return out
}

// sortedLaneKeys visits a base-URL map in SORTED order, for the same reason
// validateTailnetEndpoints does: Go randomizes map iteration, and a doctor row
// order that changes between runs is not diffable.
func sortedLaneKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// inspectBase parses a configured base and says, in one place, what is wrong
// with it. A nil URL comes with a non-empty clause naming the problem, which is
// what makes an unusable value REPORTABLE rather than skipped — the refusal and
// the warning path read the same verdict, so they cannot disagree about what
// counts as a usable base.
//
// u.Hostname() and not u.Host: "http://:18811" has a non-empty Host and no host
// at all, which is precisely the shape a half-substituted template leaves.
func inspectBase(raw string) (*url.URL, string) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return nil, "the value is empty"
	}
	u, err := url.Parse(v)
	if err != nil {
		return nil, err.Error()
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Sprintf("scheme %q is not http or https (a base with no scheme parses as one: %q reads as scheme %q, not as a host)", u.Scheme, v, u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, "it names no host"
	}
	return u, ""
}

// hasV1Suffix reports whether a base carries the trailing /v1 segment that
// swapclient.BaseURL exists to strip. It is the same normalisation rule read as
// a question instead of an answer.
func hasV1Suffix(u *url.URL) bool {
	return strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/v1")
}

// endpointPort is this box's own serving port — the one a remote llama-swap
// lane is expected to mirror. An unparseable or portless endpoint yields "",
// which matches no lane and so warns rather than silently accepting anything.
func endpointPort(c Config) string {
	if u, _ := inspectBase(c.Endpoint); u != nil {
		return u.Port()
	}
	return ""
}

// Findings returns every NON-FATAL configuration finding `local-offload doctor`
// reports as a FAIL row: the endpoint-shape warnings above, plus the two that
// only doctor surfaces — a media-lane GPU wait that has drifted away from the
// vision lane's, and any retired key the file still carries.
//
// It is a method on the loaded value rather than a second read of the file so
// doctor reports on the config the process is actually running under, which is
// the only one that can explain its behaviour.
func (c Config) Findings() []string {
	out := EndpointWarnings(c)
	out = append(out, gpuWaitFindings(c)...)
	// A negative Wan split is not a split: the pipeline passes only a positive value, so
	// the render silently uses the builder's default while the file names another number.
	if c.VideoGenWanVirtualVramGB < 0 {
		out = append(out, fmt.Sprintf("videogen_wan_virtual_vram_gb %g is negative — no split is passed and the Wan graph uses its builder default (%g GiB per expert in RAM) while the file says otherwise; set this card's measured value, or 0 to mean the default",
			c.VideoGenWanVirtualVramGB, Default().VideoGenWanVirtualVramGB))
	}
	for _, k := range c.RetiredKeys {
		out = append(out, fmt.Sprintf("config key %q is retired and ignored — %s; delete it so the file stops describing behaviour the harness no longer has", k, retiredKeys[k]))
	}
	return out
}

// gpuWaitFindings names the two ways this box's GPU waits stop meaning what the
// file says (register C-33/S-42).
//
// A NEGATIVE wait is not an unset one. Both consumers turn it into a
// non-positive duration, so the task gets a single try — the same behaviour as
// an explicit 0, which IS a documented choice — while the file says ten minutes.
// It is reported rather than refused because nothing breaks: this whole class is
// warn-then-fail, and a value that behaves as the documented 0 does not earn a
// refusal ahead of the ones that cannot work at all.
//
// A media wait far past the VISION wait is the second: the two keys are one
// card's queue seen from two lanes, and gpu_wait_ms documents itself as "90000,
// matching VisionGPUWaitSec so the two GPU waiters behave alike". A live config
// carried 600000 against a 90 s vision wait — ten minutes of blocking on every
// image/video/audio/run_graph call, against the key's own stated design. 3x is
// the threshold rather than equality because a longer media wait is a legitimate
// choice: a video render genuinely outlasts a VQA. An order of magnitude is not
// a choice, it is a value nobody revisited.
func gpuWaitFindings(c Config) []string {
	var out []string
	if c.GPUWaitMs < 0 {
		out = append(out, fmt.Sprintf("gpu_wait_ms %d is negative — the wait is built as a duration, so a negative value is a ZERO wait: every image/video/audio/run_graph call gets one try and defers the moment the card is held, while the file reads as a wait. Use 0 to mean that on purpose, or a positive number of milliseconds (default %d)",
			c.GPUWaitMs, Default().GPUWaitMs))
	}
	if c.VisionGPUWaitSec < 0 {
		out = append(out, fmt.Sprintf("vision_gpu_wait_sec %d is negative — same shape: a negative wait is a ZERO wait, so a vqa/ocr/assess_image/video_describe call defers the moment a render holds the card. Use 0 to mean that on purpose, or a positive number of seconds (default %d)",
			c.VisionGPUWaitSec, Default().VisionGPUWaitSec))
	}
	if c.GPUWaitMs > 0 && c.VisionGPUWaitSec > 0 && c.GPUWaitMs > 3*c.VisionGPUWaitSec*1000 {
		out = append(out, fmt.Sprintf("gpu_wait_ms %d ms (%d s) is more than 3x vision_gpu_wait_sec %d s — every image/video/audio/run_graph call blocks that long behind a lease holder while a vision call on the SAME card gives up at %d s; the key's own documented default is %d (90 s)",
			c.GPUWaitMs, c.GPUWaitMs/1000, c.VisionGPUWaitSec, c.VisionGPUWaitSec, Default().GPUWaitMs))
	}
	return out
}
