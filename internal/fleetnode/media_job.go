package fleetnode

// The media-job door (ADR 0072): POST /fleet/media-job takes ONE image, video, animation, audio or
// ComfyUI-graph job from a holder of the fleet token TOGETHER WITH the input files that job reads, so a
// machine with no render lane of its own can have a still animated, a character retargeted onto a
// driver video or a voice cloned from a sample, with the files a node cannot otherwise get.
//
// The tokenless media tasks (/fleet/dispatch) read NODE-LOCAL paths only; they stay exactly as they
// were. This door is the one that writes caller-supplied bytes to the node's disk, so it is closed unless
// the node opted in (fleet_media_inputs), holds a fleet token and has a media task bound
// (config.MediaInputsAdmissible), and the bearer is checked before a byte of the body is read. The bundle
// is a gzip-tar the node checks against its declared sha256, extracts (regular files only, confined
// names, byte caps: internal/composebundle) into a fresh directory under <media_dir>/fleet-inputs, and
// then sniffs by magic bytes per field kind: a file that is not the kind of media its field names is
// refused. The directory lives exactly as long as the job and a startup sweep retries a crash's leftovers.
// The inner task is then built by the SAME builder /fleet/dispatch uses, with each input field rewritten
// to the extracted absolute path, so a media job renders byte-for-byte like the local call it replaces.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/composebundle"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// MediaJobTask is the fleet task type the media-job door admits.
const MediaJobTask = "media-job"

// MediaJobPath is the door's route.
const MediaJobPath = "/fleet/media-job"

// mediaInputsDir is the subdirectory of media_dir the extracted inputs live in. It is a directory, so
// GET /fleet/media (bare file names directly in media_dir) can never serve anything inside it.
const mediaInputsDir = "fleet-inputs"

// MediaJobPayload is the POST /fleet/media-job body.
type MediaJobPayload struct {
	JobID    string `json:"job_id"`
	TaskType string `json:"task_type"`
	// Payload is the inner task's payload exactly as POST /fleet/dispatch takes it.
	Payload json.RawMessage `json:"payload"`
	// Bundle is base64 (standard encoding) of a gzip-compressed tar of the input files; BundleSHA256 is
	// the hex sha256 of the decoded bytes. Inputs maps a payload field to a bare file name in the bundle.
	Bundle       string            `json:"bundle,omitempty"`
	BundleSHA256 string            `json:"bundle_sha256,omitempty"`
	Inputs       map[string]string `json:"inputs,omitempty"`
}

// mediaJobTasks are the inner tasks the door carries, and mediaJobFields the payload fields of each that
// may arrive as a file (every other field of every task is plain data).
var mediaJobTasks = []string{"image-gen", "video-gen", "animate", "audio-gen", "run-graph"}

type mediaKind int

const (
	kindImage mediaKind = iota
	kindVideo
	kindAudio
)

func (k mediaKind) String() string {
	return [...]string{"an image (PNG, JPEG or WebP)", "a video (MP4, MOV, WebM or MKV)", "audio (WAV, FLAC, MP3, OGG or M4A)"}[k]
}

var mediaJobFields = map[string]map[string]mediaKind{
	"video-gen": {"still": kindImage},
	"animate":   {"ref": kindImage, "driver": kindVideo},
	"audio-gen": {"clone": kindAudio},
}

// MediaJobBodyCap is the largest body the door reads: the bundle cap in base64 plus room for the rest of
// the payload.
func MediaJobBodyCap(cfg config.Config) int64 {
	return int64(base64.StdEncoding.EncodedLen(int(cfg.EffectiveMediaInputsMaxBytes()))) + 64<<10
}

const mediaJobClosed = "media-job is not open on this node (it needs fleet_media_inputs, fleet_auth_token and a bound media task)"

// mediaJobWindow is how long a token holder's request may take to arrive and be admitted; the server's
// blanket 30 s timeouts would cut a driver video on an ordinary link (the compose-project door's reason).
const mediaJobWindow = 15 * time.Minute

// handleMediaJob checks the door and the bearer BEFORE reading the body, then decodes the typed payload
// and joins the shared admission path.
func (s *Server) handleMediaJob(w http.ResponseWriter, r *http.Request) {
	if !s.opts.Cfg.MediaInputsAdmissible() {
		writeError(w, http.StatusForbidden, mediaJobClosed)
		return
	}
	if !bearerOK(r, s.opts.Cfg.FleetAuthToken) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	// Only a token holder gets the longer window; everyone else met the blanket timeouts above.
	s.extendRead(w, mediaJobWindow, "the media-job door")
	s.extendWrite(w, mediaJobWindow, "the media-job door")
	limit := MediaJobBodyCap(s.opts.Cfg)
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("content-type must be application/json (got %q)", ct))
			return
		}
	}
	body, err := readMediaJobBody(r, limit)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body too large (limit %d bytes: fleet_media_inputs_max_mb)", limit))
			return
		}
		writeError(w, http.StatusBadRequest, "reading media-job body: "+err.Error())
		return
	}
	head, err := decodeMediaJobHead(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "malformed media-job body: "+err.Error())
		return
	}
	s.admit(w, r, dispatchEnvelope{JobID: head.JobID, TaskType: MediaJobTask, Payload: body})
}

// readMediaJobBody reads the whole body into ONE buffer of exactly Content-Length bytes when the client sent
// it (io.ReadAll would grow by doubling, and bytes.Buffer.ReadFrom regrows once fewer than 512 bytes are
// free, either of which leaves garbage the size of the body behind it). After the declared length it probes
// one more byte: a body longer than it declared is refused as too large, as one over the cap is.
func readMediaJobBody(r *http.Request, limit int64) ([]byte, error) {
	if r.ContentLength > limit {
		// Let the MaxBytesReader produce the typed error the caller maps to 413.
		_, err := io.Copy(io.Discard, r.Body)
		return nil, err
	}
	if r.ContentLength > 0 {
		buf := make([]byte, r.ContentLength)
		if _, err := io.ReadFull(r.Body, buf); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, errors.New("body shorter than its Content-Length")
			}
			return nil, err
		}
		var probe [1]byte
		n, err := r.Body.Read(probe[:])
		if n > 0 {
			return nil, &http.MaxBytesError{Limit: r.ContentLength}
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		return buf, nil
	}
	return io.ReadAll(r.Body)
}

// discardJSON accepts any JSON value without copying it: encoding/json hands UnmarshalJSON a slice of the
// input, so decoding a field into this type costs nothing however large the value is.
type discardJSON struct{}

func (*discardJSON) UnmarshalJSON([]byte) error { return nil }

// mediaJobHead is the part of a media-job body the door reads before admission: the job id, plus the other
// fields' presence and types. The bundle and the inner payload are decoded, once, by buildMediaJob.
type mediaJobHead struct {
	JobID        string            `json:"job_id"`
	TaskType     string            `json:"task_type"`
	Payload      discardJSON       `json:"payload"`
	Bundle       bundleHead        `json:"bundle"`
	BundleSHA256 string            `json:"bundle_sha256"`
	Inputs       map[string]string `json:"inputs"`
}

// bundleHead checks that the bundle is a JSON string (or null) and keeps none of it.
type bundleHead struct{}

func (*bundleHead) UnmarshalJSON(b []byte) error {
	if s := bytes.TrimSpace(b); string(s) == "null" || (len(s) >= 2 && s[0] == '"') {
		return nil
	}
	return errors.New("json: cannot unmarshal a non-string into the bundle")
}

// decodeMediaJobHead decodes the body strictly (an unknown top-level key is refused, matched without regard
// to case as the decoder matches a field) WITHOUT materialising the bundle: the keys come from a map whose
// values are discarded, the typed fields from a decode whose bundle and payload are discarded too. The old
// shape (json.Decoder over the body into MediaJobPayload) held the bundle two more times at the same moment.
func decodeMediaJobHead(body []byte) (mediaJobHead, error) {
	var keys map[string]discardJSON
	if err := json.Unmarshal(body, &keys); err != nil {
		return mediaJobHead{}, err
	}
	for k := range keys {
		known := false
		for _, f := range []string{"job_id", "task_type", "payload", "bundle", "bundle_sha256", "inputs"} {
			if strings.EqualFold(k, f) {
				known = true
				break
			}
		}
		if !known {
			return mediaJobHead{}, fmt.Errorf("json: unknown field %q", k)
		}
	}
	var head mediaJobHead
	if err := json.Unmarshal(body, &head); err != nil {
		return mediaJobHead{}, err
	}
	return head, nil
}

// bundleBytes is the bundle field decoded from base64 straight into its bytes: the encoded string is never
// copied (encoding/json passes UnmarshalJSON a slice of the request body), so the node holds the body once
// and the decoded bundle once.
type bundleBytes struct{ raw []byte }

func (b *bundleBytes) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if string(data) == "null" {
		return nil
	}
	if len(data) < 2 || data[0] != '"' || data[len(data)-1] != '"' {
		return errors.New("bundle must be a string")
	}
	enc := data[1 : len(data)-1]
	if bytes.IndexByte(enc, '\\') >= 0 {
		// An escaped string (a client that writes "\/" for "/"): rare, so the one copy is fine.
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return fmt.Errorf("bundle is not base64: %w", err)
		}
		b.raw = raw
		return nil
	}
	raw := make([]byte, base64.StdEncoding.DecodedLen(len(enc)))
	n, err := base64.StdEncoding.Decode(raw, enc)
	if err != nil {
		return fmt.Errorf("bundle is not base64: %w", err)
	}
	b.raw = raw[:n]
	return nil
}

// mediaJobWire is MediaJobPayload as the node decodes it for the build.
type mediaJobWire struct {
	JobID        string            `json:"job_id"`
	TaskType     string            `json:"task_type"`
	Payload      json.RawMessage   `json:"payload"`
	Bundle       bundleBytes       `json:"bundle"`
	BundleSHA256 string            `json:"bundle_sha256"`
	Inputs       map[string]string `json:"inputs"`
}

// buildMediaJob turns an admitted payload into the inner task's request: the bundle is checked against
// its declared sha256, extracted into a fresh directory, every input is sniffed, each input field of the
// inner payload is rewritten to the extracted path, and the SAME builder /fleet/dispatch uses builds the
// request. The returned cleanup runs the inner cleanup and removes the directory; the server runs it when
// the job ends, and on every refusal before that.
func buildMediaJob(ctx context.Context, v *mediaView, loopbackListener bool, payload json.RawMessage) (core.Request, func(), error) {
	cfg := v.cfg
	noop := func() {}
	if !cfg.MediaInputsAdmissible() {
		return core.Request{}, noop, errors.New(mediaJobClosed)
	}
	// The bundle is decoded from base64 straight out of the payload into its bytes (bundleBytes): the
	// encoded string is never copied, so this is the only full-size copy the build makes.
	var in mediaJobWire
	if err := json.Unmarshal(payload, &in); err != nil {
		return core.Request{}, noop, fmt.Errorf("media-job payload: %w", err)
	}
	task := strings.TrimSpace(in.TaskType)
	if !containsString(mediaJobTasks, task) {
		return core.Request{}, noop, fmt.Errorf("media-job payload: task_type %q is not one of %s", in.TaskType, strings.Join(mediaJobTasks, ", "))
	}
	if !taskConfiguredIn(v, task, loopbackListener) {
		if nre := mediaRouteNotReady(v, task); nre != nil {
			return core.Request{}, noop, nre
		}
		return core.Request{}, noop, fmt.Errorf("media-job payload: %s is not configured on this node (supported: %s)", task, strings.Join(supportedTasksIn(v, loopbackListener), ", "))
	}
	inner := map[string]json.RawMessage{}
	if len(in.Payload) > 0 && string(in.Payload) != "null" {
		if err := json.Unmarshal(in.Payload, &inner); err != nil {
			return core.Request{}, noop, fmt.Errorf("media-job payload: payload must be a JSON object: %w", err)
		}
	}
	fields := mediaJobFields[task]
	for name, file := range in.Inputs {
		if _, ok := fields[name]; !ok {
			return core.Request{}, noop, fmt.Errorf("media-job payload: %q is not an input file of %s (allowed: %s)", name, task, allowedFieldList(fields))
		}
		if err := checkInputName(file); err != nil {
			return core.Request{}, noop, fmt.Errorf("media-job payload: inputs[%q]: %w", name, err)
		}
	}
	// A file field the payload fills in by itself would be a node-local path read: this door carries
	// bytes, never paths, so a field is either shipped in the bundle or left out. The inner builders
	// decode field names without regard to case ("Still" fills still), so the guard matches the same
	// way: a key that names a file field in ANY casing is refused when the field is not shipped, and
	// dropped (the shipped file replaces it) when it is, so no spelling reaches the builder as a path.
	for key, val := range inner {
		for name := range fields {
			if !strings.EqualFold(key, name) {
				continue
			}
			if _, shipped := in.Inputs[name]; shipped {
				delete(inner, key)
			} else if !isAbsentJSON(val) && string(val) != `""` {
				return core.Request{}, noop, fmt.Errorf("media-job payload: payload.%s names a path on this node; ship the file in the bundle (inputs) or use /fleet/dispatch", key)
			}
			break
		}
	}
	if len(in.Inputs) == 0 && (len(in.Bundle.raw) > 0 || in.BundleSHA256 != "") {
		return core.Request{}, noop, errors.New("media-job payload: a bundle needs inputs naming the files the payload reads")
	}
	if len(in.Inputs) > 0 && len(in.Bundle.raw) == 0 {
		return core.Request{}, noop, errors.New("media-job payload: inputs name files but the payload carries no bundle")
	}

	cleanup := noop
	if len(in.Inputs) > 0 {
		raw := in.Bundle.raw
		max := cfg.EffectiveMediaInputsMaxBytes()
		if int64(len(raw)) > max {
			return core.Request{}, noop, fmt.Errorf("media-job payload: the bundle is %d bytes, over fleet_media_inputs_max_mb", len(raw))
		}
		sum := sha256.Sum256(raw)
		if !strings.EqualFold(in.BundleSHA256, hex.EncodeToString(sum[:])) {
			return core.Request{}, noop, errors.New("media-job payload: bundle_sha256 does not match the bundle")
		}
		base := filepath.Join(cfg.MediaDir, mediaInputsDir)
		if err := os.MkdirAll(base, 0o755); err != nil {
			return core.Request{}, noop, nodeSideError{fmt.Errorf("media-job: %w", err)}
		}
		dir, err := os.MkdirTemp(base, "in-")
		if err != nil {
			return core.Request{}, noop, nodeSideError{fmt.Errorf("media-job: %w", err)}
		}
		removeDir := func() {
			if rerr := os.RemoveAll(dir); rerr != nil {
				log.Printf("fleet: media-job: removing %s: %v (the startup sweep retries it)", dir, rerr)
			}
		}
		lim := composebundle.Limits{MaxFiles: 8, MaxFileBytes: max, MaxTotal: max}
		if err := composebundle.Extract(raw, dir, lim); err != nil {
			removeDir()
			if errors.Is(err, composebundle.ErrIO) {
				return core.Request{}, noop, nodeSideError{fmt.Errorf("media-job: unpacking the bundle on this node: %w", err)}
			}
			return core.Request{}, noop, fmt.Errorf("media-job bundle refused: %w", err)
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			removeDir()
			return core.Request{}, noop, nodeSideError{fmt.Errorf("media-job: %w", err)}
		}
		names := make([]string, 0, len(in.Inputs))
		for name := range in.Inputs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			path, err := inputFile(abs, in.Inputs[name])
			if err == nil {
				err = sniffMedia(path, fields[name])
			}
			if err != nil {
				removeDir()
				return core.Request{}, noop, fmt.Errorf("media-job bundle refused: inputs[%q] (%s): %w", name, in.Inputs[name], err)
			}
			enc, _ := json.Marshal(path)
			inner[name] = enc
		}
		cleanup = removeDir
	}

	rewritten, err := json.Marshal(inner)
	if err != nil {
		cleanup()
		return core.Request{}, noop, fmt.Errorf("media-job payload: %w", err)
	}
	req, innerCleanup, err := buildRequestIn(ctx, v, loopbackListener, task, rewritten)
	if err != nil {
		innerCleanup()
		cleanup()
		return core.Request{}, noop, err
	}
	return req, func() {
		innerCleanup()
		cleanup()
	}, nil
}

func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func allowedFieldList(fields map[string]mediaKind) string {
	if len(fields) == 0 {
		return "none: this task reads no input file"
	}
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// checkInputName refuses a bundle file name that is not one plain name: no separator, no drive or stream
// colon, no NUL, never "." or "..".
func checkInputName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\:\x00") {
		return fmt.Errorf("%q is not a bare file name", name)
	}
	return nil
}

// inputFile resolves a bare name to a regular file directly inside dir: never a directory, a symlink or
// anything the name could climb out through.
func inputFile(dir, name string) (string, error) {
	if err := checkInputName(name); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if filepath.Dir(path) != dir {
		return "", fmt.Errorf("%q is not directly inside the bundle", name)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", errors.New("the bundle holds no such file")
		}
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	return path, nil
}

// sniffMedia reads the file's first bytes and refuses one whose magic does not match the field's kind.
func sniffMedia(path string, kind mediaKind) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 16)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return err
	}
	if !magicMatches(head[:n], kind) {
		return fmt.Errorf("the file is not %s (its first bytes match no accepted format)", kind)
	}
	return nil
}

func magicMatches(b []byte, kind mediaKind) bool {
	has := func(off int, s string) bool { return len(b) >= off+len(s) && string(b[off:off+len(s)]) == s }
	riff := func(form string) bool { return has(0, "RIFF") && has(8, form) }
	switch kind {
	case kindImage:
		return has(0, "\x89PNG\r\n\x1a\n") || has(0, "\xff\xd8\xff") || riff("WEBP")
	case kindVideo:
		return has(4, "ftyp") || has(0, "\x1a\x45\xdf\xa3")
	case kindAudio:
		return riff("WAVE") || has(0, "fLaC") || has(0, "ID3") || has(0, "OggS") || has(4, "ftyp") ||
			(len(b) >= 2 && b[0] == 0xff && b[1]&0xe0 == 0xe0)
	}
	return false
}

// SweepOrphanedInputDirs removes extracted media inputs no job holds any more: fleet-serve calls it at
// startup, so a crash never leaves them for good. Only directories older than the longest media timeout
// plus an hour go, so a second process sharing the media dir keeps the ones it is rendering from. Every
// failure is collected and none stops the rest of the sweep.
func SweepOrphanedInputDirs(cfg config.Config, now time.Time) (swept int, err error) {
	dir := filepath.Join(cfg.MediaDir, mediaInputsDir)
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return 0, nil
		}
		return 0, fmt.Errorf("sweep %s: %w", mediaInputsDir, rerr)
	}
	maxAge := longestMediaTimeout(cfg) + time.Hour
	var failures []error
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "in-") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			failures = append(failures, fmt.Errorf("sweep %s: %s: %w", mediaInputsDir, e.Name(), ierr))
			continue
		}
		if now.Sub(info.ModTime()) < maxAge {
			continue
		}
		if rmErr := os.RemoveAll(filepath.Join(dir, e.Name())); rmErr != nil {
			failures = append(failures, fmt.Errorf("sweep %s: %s: %w", mediaInputsDir, e.Name(), rmErr))
			continue
		}
		swept++
	}
	return swept, errors.Join(failures...)
}

// longestMediaTimeout is the longest wall any media task of this config may run (the timeout keys the
// runners honor, with their shipped defaults when a key is unset).
func longestMediaTimeout(cfg config.Config) time.Duration {
	longest := 0
	for _, sec := range []int{
		orDefault(cfg.ImageGenTimeoutSec, 720), orDefault(cfg.VideoGenTimeoutSec, 1500),
		orDefault(cfg.AnimateGenTimeoutSec, 1800), orDefault(cfg.AudioGenTimeoutSec, 720),
	} {
		if sec > longest {
			longest = sec
		}
	}
	return time.Duration(longest) * time.Second
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
