package mediacap

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// The models a run-graph manifest may name.
//
// A manifest's models[] makes the node download each entry's source_url into the ComfyUI tree at its path before
// ComfyUI starts. The fleet bearer lets a caller run renders, not write files of its choosing: a path that leaves
// models/<class>/ (custom_nodes/, a ".." climb, an absolute path, a drive, a stream name) would drop code where
// ComfyUI loads it, and a source that is not an https address on a listed host would make the node fetch from where
// the caller chose. So an entry is taken only when
//
//   - its path, with backslashes read as separators, is models/<class>/<file> (at least three segments, the first
//     exactly "models"), no segment empty, "." or "..", none beginning or ending with a space or ending with a dot,
//     none holding a control character or one of : * ? " < > |;
//   - its source_url, when it has one, is https://<host>/<rest> with a plain DNS host name (no user name, port,
//     bracket or IP-literal form, nothing but letters, digits, dots and hyphens) that is, or is a subdomain of, a host
//     in render/node-packs.json (model_hosts).
//
// The checks are written to be the same in render/node-packs.mjs checkModelEntry, and one table of paths and sources
// (render/testdata/manifest-model-cases.json) drives both tests. They do not require a sha256: a hash the caller supplies
// pins one build and authenticates nothing against the publisher, so it is only an optional assertion (the node checks a download
// against the digest the publisher publishes, and defers when the two differ); a model whose host publishes no digest is reported
// unverified.

// ManifestModelRefusedError is a manifest's refusal for a models[] entry the node will not provision.
type ManifestModelRefusedError struct {
	// Model is the entry as the manifest named it (its path, else its place in the list).
	Model string
	// Reason says which part of the entry did not pass.
	Reason string
}

func (e *ManifestModelRefusedError) Error() string {
	return fmt.Sprintf("run-graph manifest: model %q is not allowed (%s); a manifest model lands under models/<class>/ and is fetched over https from a listed host only (render/node-packs.json)", clipPack(e.Model), e.Reason)
}

// modelSourceHosts mirrors render/node-packs.json's model_hosts, in order. A source_url host is taken when it is one of
// these or a subdomain of one. TestModelSourceHostsMatchTheRunnersFile holds the two together.
var modelSourceHosts = []string{"huggingface.co", "hf.co", "civitai.com", "github.com", "githubusercontent.com"}

// ModelSourceHosts returns the hosts a manifest model may be fetched from.
func ModelSourceHosts() []string { return append([]string(nil), modelSourceHosts...) }

// modelSourceURL is the whole shape of a taken source: https, a dotted DNS host name, and then nothing or a path that
// holds no control character, space or backslash. The host is the first group.
var modelSourceURL = regexp.MustCompile(`^https://([A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+)(/[^\x00-\x20\x7f\\]*)?$`)

// modelPathProblem returns why path is not models/<class>/<file>, "" when it is.
func modelPathProblem(path string) string {
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return "the path holds a control character"
		}
	}
	segs := strings.Split(strings.ReplaceAll(path, `\`, "/"), "/")
	if len(segs) < 3 || segs[0] != "models" {
		return "the path is not models/<class>/<file>"
	}
	for _, s := range segs {
		switch {
		case s == "" || s == "." || s == "..":
			return "the path has an empty, \".\" or \"..\" segment"
		case strings.HasSuffix(s, ".") || strings.HasPrefix(s, " ") || strings.HasSuffix(s, " "):
			return "a segment of the path begins with a space or ends with a space or a dot"
		case strings.ContainsAny(s, `:*?"<>|`):
			return "a segment of the path holds a character a file name cannot"
		}
	}
	return ""
}

// modelSourceProblem returns why source is not a taken source_url, "" when it is.
func modelSourceProblem(source string) string {
	m := modelSourceURL.FindStringSubmatch(source)
	if m == nil {
		return "the source is not an https address of the form https://host/path"
	}
	host := strings.ToLower(m[1])
	for _, h := range modelSourceHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return ""
		}
	}
	return fmt.Sprintf("the source host %q is not a listed model host", clipPack(host))
}

// CheckManifestModels refuses a run-graph manifest whose models[] names an entry the node will not provision, with a
// *ManifestModelRefusedError naming the first one. A manifest with no models is fine. The keys are read exactly as the
// runner reads them (case-sensitively), so a spelling the runner would ignore cannot carry a value this check read.
func CheckManifestModels(manifest []byte) error {
	if s := strings.TrimSpace(string(manifest)); s == "" || s == "null" {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(manifest, &top); err != nil {
		return &ManifestModelRefusedError{Model: "manifest", Reason: "the manifest is not a JSON object"}
	}
	raw, ok := top["models"]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return &ManifestModelRefusedError{Model: "models", Reason: "models is not a list"}
	}
	for i, er := range entries {
		if err := checkModelEntry(i, er); err != nil {
			return err
		}
	}
	return nil
}

func checkModelEntry(i int, raw json.RawMessage) error {
	slot := fmt.Sprintf("#%d", i)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return &ManifestModelRefusedError{Model: slot, Reason: "the model entry is not an object"}
	}
	var path string
	if v, present := obj["path"]; !present || json.Unmarshal(v, &path) != nil {
		return &ManifestModelRefusedError{Model: slot, Reason: "the model entry has no path, or its path is not a string"}
	}
	if why := modelPathProblem(path); why != "" {
		return &ManifestModelRefusedError{Model: path, Reason: why}
	}
	v, present := obj["source_url"]
	if !present || strings.TrimSpace(string(v)) == "null" {
		return nil
	}
	var source string
	if err := json.Unmarshal(v, &source); err != nil {
		return &ManifestModelRefusedError{Model: path, Reason: "source_url is not a string"}
	}
	if source == "" {
		return nil
	}
	if why := modelSourceProblem(source); why != "" {
		return &ManifestModelRefusedError{Model: path, Reason: why}
	}
	return nil
}
