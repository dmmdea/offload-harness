package fleetnode

// The media of the token-gated lanes (ADR 0072 follow-up, D26's companion).
//
// GET /fleet/media/{name} is tokenless on purpose: a media client (the image lane's callers) reads a
// render by bare name. But two gated lanes put their OUTPUTS in the same directory, and a file there
// was readable by anyone who learned its name:
//
//   - the stt upload door's transcripts: stt-<digits>-<8 hex>.srt|txt|segments.json. The stem is the
//     private upload's random temp name plus a content hash, hard to stumble on and not a secret;
//   - the project door's renders (ADR 0071): composeproj-<16 hex>.<ext>, with the snapshots the
//     renderer names <stem>-snap-<n>.png beside it. The door sets the output stem itself (the vetted
//     compose-video lane keeps compose-<hash8>, which a node cannot tell from a project render's), so
//     a name alone says which lane wrote it, from the first byte, and across a restart.
//
//   - the legacy path-taking stt lane's transcripts (<basename>-<8 hex>.srt|txt|segments.json): that
//     lane is token-gated on a node with a token, so its outputs ride the bearer too (they are never
//     swept: the pipeline's content-keyed cache, shared with local transcription, owns them).
//
// The match fails closed on the spellings a Windows filesystem folds onto one file (gatedMediaName).
//
// On a node WITH a fleet_auth_token those names now need the bearer; a node with no token, and every
// other name, answer as they always did. The transcripts are also removed: when the job record is
// evicted, and once older than fleet_stt_transcript_ttl_min (default 30), swept at fleet-serve start
// and on the job store's janitor tick. The project renders are not swept (a render is the product the
// asker came for, and it is already kept as long as any media output).

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

var (
	// sttOutputRe is what the transcribe pipeline names an upload's outputs: its private temp file is
	// stt-<digits>.<ext> (os.CreateTemp: the random part is a decimal number), and mediaBase appends 8
	// hex of the content identity. The PRODUCER's shape is pinned against this same literal in
	// internal/pipeline (TestMediaBaseOfAnSTTUploadMatchesTheNodesGatedShape): drift on either side
	// would open the bearer gate and starve the sweep without a consumer-side test noticing.
	sttOutputRe = regexp.MustCompile(`^stt-[0-9]+-[0-9a-f]{8}\.(srt|txt|segments\.json)$`)
	// projectOutputRe is the stem buildComposeProject gives a project render: its video and the
	// renderer's snapshots beside it.
	projectOutputRe = regexp.MustCompile(`^composeproj-[0-9a-f]{16}(\.[a-z0-9]{1,8}|-snap-[A-Za-z0-9._-]+)$`)
)

// projectOutputPrefix is the stem every project render's output starts with.
const projectOutputPrefix = "composeproj-"

// legacySTTOutputRe is what the LEGACY path-taking stt lane names its transcripts:
// <sanitized-basename>-<8 hex of the content identity>.srt|txt|segments.json (pipeline's mediaBase).
// That lane is token-gated on a node with a token, so its outputs ride the bearer too (the stem can
// carry the node's own file names). It is deliberately NOT used by the sweep or the eviction removal:
// those files are the pipeline's content-keyed cache, which a local transcription shares.
var legacySTTOutputRe = regexp.MustCompile(`^.+-[0-9a-f]{8}\.(srt|txt|segments\.json)$`)

// gatedMediaName reports whether a file in media_dir is an output of a token-gated lane. It fails
// closed: a Windows filesystem resolves one file under several spellings (another case, trailing
// dots and spaces, an 8.3 short name, a ":stream" suffix, the case folding of non-ASCII letters), so
// the name is matched in its folded form and any name that is not plain ASCII, or that carries a
// "~" or ":", is treated as gated rather than guessed at. Media this node writes is plain lower-case
// ASCII, so no tokenless lane's file is caught by that.
func gatedMediaName(name string) bool {
	for i := 0; i < len(name); i++ {
		if c := name[i]; c < 0x20 || c > 0x7e || c == '~' || c == ':' {
			return true
		}
	}
	n := strings.ToLower(strings.TrimRight(name, ". "))
	return sttOutputRe.MatchString(n) || projectOutputRe.MatchString(n) || legacySTTOutputRe.MatchString(n)
}

// SweepSTTTranscripts removes the transcript files of stt upload jobs that are older than the node's
// transcript TTL (fleet_stt_transcript_ttl_min; a negative value keeps them for good). Only names
// the upload door's outputs can have are touched, directly under media_dir. Every failure is
// collected and none stops the rest of the sweep.
func SweepSTTTranscripts(cfg config.Config, now time.Time) (swept int, err error) {
	ttl := cfg.EffectiveSTTTranscriptTTL()
	if ttl <= 0 || strings.TrimSpace(cfg.MediaDir) == "" {
		return 0, nil
	}
	entries, rerr := os.ReadDir(cfg.MediaDir)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return 0, nil
		}
		return 0, fmt.Errorf("sweep stt transcripts: %w", rerr)
	}
	var failures []error
	for _, e := range entries {
		if e.IsDir() || !sttOutputRe.MatchString(e.Name()) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			failures = append(failures, fmt.Errorf("sweep stt transcripts: %s: %w", e.Name(), ierr))
			continue
		}
		if now.Sub(info.ModTime()) < ttl {
			continue
		}
		if rmErr := os.Remove(filepath.Join(cfg.MediaDir, e.Name())); rmErr != nil && !os.IsNotExist(rmErr) {
			failures = append(failures, fmt.Errorf("sweep stt transcripts: %s: %w", e.Name(), rmErr))
			continue
		}
		swept++
	}
	return swept, errors.Join(failures...)
}

// sttOutputNames reads the transcript files a finished stt upload job names in its result
// (srt_path, text_path, json_path) and returns the bare names that are an upload's outputs. A path
// is reduced to its last element, whichever separators its OS spells, and anything that is not an
// upload output's name is dropped: the result is the node's own pipeline's, but nothing here deletes
// a file the shape does not vouch for.
func sttOutputNames(data json.RawMessage) []string {
	var r struct {
		SRT  string `json:"srt_path"`
		Text string `json:"text_path"`
		JSON string `json:"json_path"`
	}
	if json.Unmarshal(data, &r) != nil {
		return nil
	}
	var out []string
	for _, p := range []string{r.SRT, r.Text, r.JSON} {
		if i := strings.LastIndexAny(p, `/\`); i >= 0 {
			p = p[i+1:]
		}
		if sttOutputRe.MatchString(p) {
			out = append(out, p)
		}
	}
	return out
}

// sttOutputs is what one upload job's run learned about its transcript files, shared with the job
// record's eviction hook.
type sttOutputs struct {
	mu    sync.Mutex
	names []string
}

func (o *sttOutputs) set(names []string) {
	o.mu.Lock()
	o.names = names
	o.mu.Unlock()
}

func (o *sttOutputs) get() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.names...)
}

// refreshTranscripts restarts the retention clock of the files a finished upload job returned: the
// pipeline's cache is keyed on the audio's CONTENT, so a second upload of the same recording gets the
// first job's files back, and they must live a full TTL from the job that returned them.
func refreshTranscripts(cfg config.Config, names []string, now time.Time) {
	if strings.TrimSpace(cfg.MediaDir) == "" {
		return
	}
	for _, n := range names {
		_ = os.Chtimes(filepath.Join(cfg.MediaDir, n), now, now)
	}
}

// removeTranscripts deletes an evicted job's transcript files (a no-op when the node keeps them).
func removeTranscripts(cfg config.Config, names []string) {
	if cfg.EffectiveSTTTranscriptTTL() <= 0 || strings.TrimSpace(cfg.MediaDir) == "" {
		return
	}
	for _, n := range names {
		if !sttOutputRe.MatchString(n) {
			continue
		}
		if err := os.Remove(filepath.Join(cfg.MediaDir, n)); err != nil && !os.IsNotExist(err) {
			log.Printf("fleet: removing the transcript %s of an evicted stt job: %v", n, err)
		}
	}
}

// sweepTranscriptsOnTick is the janitor tick's sweep: the same one the start runs.
func sweepTranscriptsOnTick(cfg config.Config) {
	n, err := SweepSTTTranscripts(cfg, time.Now())
	if err != nil {
		log.Printf("fleet: %v", err)
	}
	if n > 0 {
		log.Printf("fleet: removed %d stt transcript file(s) past their %s retention", n, cfg.EffectiveSTTTranscriptTTL())
	}
}
