// Audio calls: speech transcription (runTranscribe) and audio/music generation (runGenerateAudio).

package pipeline

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/audioio"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/hostneed"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/sttclient"
	"github.com/dmmdea/offload-harness/internal/tasks"
)

// runTranscribe converts req.Audio to a 16kHz mono WAV (ffmpeg), transcribes it
// on the whisper upstream, writes .srt/.txt/.segments.json to MediaDir, and
// returns {gist, segments[](capped), language, duration_sec, num_segments,
// *_path}. Any failure (no model / convert / model call / empty) defers to Opus.
// Zero-always-warm unless disabled: the last call out of a burst frees the upstream
// (UnloadIfIdle, every model the burst warmed, once), and a call that finishes while
// another is in line unloads nothing. params: language (string), hq (bool -> the
// large-v3 upstream).
func (p *Pipeline) runTranscribe(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	if p.cfg.STTModel == "" {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Audio), "no stt model configured")
		return core.Deferf("no stt model configured", "", meta)
	}
	model, useOAI := sttRoute(p.cfg, paramBool(req.Params, "hq"))
	meta.Model = model

	lang := p.cfg.STTLanguage
	if l := paramStr(req.Params, "language"); l != "" {
		lang = l
	}
	if strings.EqualFold(lang, "auto") {
		lang = ""
	}

	// IDENTIFY BEFORE CONSUMING. The digest is taken FIRST, ahead of the ffmpeg
	// convert, so the identity belongs to (as nearly as possible) the same bytes
	// the conversion then reads. Hashing afterwards opened a TOCTOU window that
	// content-addressing makes WORSE rather than better: a producer rotating a
	// file between the convert and the hash would cache take A's transcript under
	// sha256(take B) — and that poisoned entry is then reachable from ANY path
	// holding B's bytes, not just the original one.
	audioID, adErr := p.digestMedia(req.Audio)

	// Convert (cheap, deterministic). A bad/missing file defers here.
	wav, cleanup, cerr := audioio.ConvertToWav16k(req.Audio, p.cfg.FFmpegPath)
	if cerr != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		if errors.Is(cerr, audioio.ErrNoAudioStream) {
			// The file has nothing to transcribe: say so in the caller's words, not ffmpeg's, and class it as a
			// bad input (the contract class, so it is not read as a broken stack): no node and no retry changes it
			// (F21).
			meta.ErrClass = core.ErrClassBadInput
			p.recordDefer(req.Task, meta, len(req.Audio), audioio.ErrNoAudioStream.Error())
			res := core.Deferf(audioio.ErrNoAudioStream.Error(), "", meta)
			res.DeferClass = core.DeferClassContract
			return res
		}
		if errors.Is(cerr, audioio.ErrFFmpegUnavailable) {
			// ffmpeg never ran: the box has none or the configured path is wrong. That is the stack's, and the
			// ledger must not file it as the caller's file (bad_input) with the ffmpeg exit codes beside it.
			meta.ErrClass = core.ErrClassFFmpegUnavailable
		}
		p.recordDefer(req.Task, meta, len(req.Audio), "audio convert: "+cerr.Error())
		return core.Deferf("audio convert: "+cerr.Error(), "", meta)
	}
	defer cleanup()

	// Identity = source file CONTENT (sha256 of its bytes) + model + lang. The same fields
	// feed BOTH the cache key (transcribeCacheKey, each one a separate key part) AND the
	// on-disk media filename (the joined `ident` string below) so they agree and never
	// collide across distinct sources that share a basename (recording.m4a is common in
	// field audio) or across model/lang variants of the same source.
	// The protocol is part of the result's identity: the same alias re-bound across
	// protocols (whisper <-> mtmd) produces differently-shaped results (timestamped
	// segments vs one full-span segment), so a protocol flip must never serve the other
	// protocol's cached entry or on-disk media files. On the OAI path the language knob
	// does not apply, so it is excluded — otherwise each distinct language value would
	// re-transcribe and re-cache identical output (review findings, 2026-07-22).
	identLang := lang
	proto := "whisper"
	if useOAI {
		identLang = ""
		proto = "oai"
	}
	// Keyed on CONTENT, not on the path: an identical file at a second path must
	// hit, and a different file at the same path must miss.
	//
	// NO IDENTITY, NO CACHE. When the digest failed we do not know which bytes
	// this result describes, so the work is done and returned but nothing is
	// stored or looked up. Synthesising a key from the path + error text (an
	// earlier design) made a transient read failure write a durable PATH-keyed
	// entry — reintroducing, on the error path, precisely the false hit this
	// change exists to remove. `identifiable` also gates the on-disk media stem,
	// so two recordings that hit the same transient error cannot overwrite each
	// other's .srt/.txt.
	// VERIFY AFTER CONSUMING. Hashing before the convert did not close the TOCTOU
	// window, it transposed it — hash-then-read stores the NEW bytes' transcript
	// under the OLD digest instead of the reverse. One stat afterwards detects
	// both directions, and also the case no re-ordering can touch: a file still
	// being appended to, where the digest covers a prefix and ffmpeg read more.
	identifiable := adErr == nil && p.mediaStillMatches(audioID, req.Audio)
	// Only report a BYPASS where there was a cache to bypass. On a node with no
	// cache configured nothing was ever cacheable, so a cache_bypass row there is
	// noise — and core.Meta.CacheBypass's own doc excludes that case.
	if !identifiable && p.cache != nil {
		// Must be OBSERVABLE. Without this the call is byte-identical in telemetry
		// to an ordinary cold miss, so an input on a flaky mount re-runs whisper at
		// full cost on every call forever while the ledger shows a healthy run of
		// misses.
		why := "source changed during the call"
		if adErr != nil {
			why = adErr.Error()
		}
		meta.CacheBypass = "media identity: " + why
		log.Printf("transcribe: no stable content identity for %q (%s) — cache bypassed; result computed but not stored", req.Audio, why)
	}
	// ident names the on-disk stem (mediaBase) and nothing else, so no output file is
	// renamed by the key fix.
	ident := fmt.Sprintf("%s|model=%s|lang=%s|proto=%s", audioID.Digest, model, identLang, proto)
	// The params ingredient is the typed canonical form; params that cannot be keyed make
	// the call uncacheable, like an unknown file identity does (resultCacheParamsKey says why).
	paramsKey, keyable := p.resultCacheParamsKey(req.Task, req.Params, &meta)
	ck := transcribeCacheKey(audioID.Digest, model, identLang, proto, paramsKey)
	storeable := identifiable && keyable
	if p.cache != nil && storeable {
		if raw, ok := p.cache.Get(ck); ok {
			var cv cacheVal
			// A hit names transcript files on disk: one whose files are gone (a fleet node removes an
			// upload job's transcripts after their retention) would answer with paths that point at
			// nothing, so it is a miss and the transcription is redone.
			if json.Unmarshal(raw, &cv) == nil && len(cv.Data) > 0 && transcriptFilesPresent(cv.Data) {
				meta.CacheHit = true
				meta.LatencyMs = time.Since(start).Milliseconds()
				p.record(req.Task, meta, len(req.Audio))
				return core.Result{OK: true, Data: cv.Data, Meta: meta}
			}
		}
	}

	var tr sttclient.Result
	var terr error
	if useOAI {
		// The OAI path takes no whisper decode knobs (language is model-detected and
		// returned in the transcript prefix; no VAD/beam controls exist there).
		tr, terr = p.stt.TranscribeOAI(ctx, model, wav)
	} else {
		prm := sttclient.DefaultParams()
		prm.Language = lang
		if !p.cfg.STTVAD {
			prm.VAD = false
		}
		tr, terr = p.stt.Transcribe(ctx, model, wav, prm)
	}
	// zero-always-warm: free the upstream's VRAM once the last call of a burst is done
	// (best-effort, short timeout). UnloadIfIdle, not Unload (register C-91): with several
	// transcriptions in line an unload after EACH one landed on the next call's inference,
	// which llama-swap answers "matrix: model unloaded". It sends nothing while another call
	// is waiting or running, so the last call out is the one that unloads — on either
	// protocol, whether that last call succeeded or failed, and every model the burst warmed
	// (a burst can mix stt_model and stt_model_hq, and the last call out knows only its own).
	// Best-effort means the transcription is never failed for it, not that it is silent: a
	// model that stays loaded because its unload failed is logged, since the ttl is then the
	// only thing that will free it.
	if p.cfg.STTUnloadAfter {
		uctx, ucancel := context.WithTimeout(context.Background(), 10*time.Second)
		if uerr := p.stt.UnloadIfIdle(uctx); uerr != nil {
			log.Printf("transcribe: zero-always-warm unload after a %s call failed (what it could not free stays loaded until its ttl): %v", model, uerr)
		}
		ucancel()
	}
	if terr != nil {
		// A held card is congestion, not a failure (register C-89): the request waited its
		// gpu_wait_ms behind a render or an exclusive hold and the fence refused it. Filed
		// as capacity — the class a delegator re-places and a caller can retry — with the
		// holder named, and with the error class the ledger groups on. No other door files
		// a refusal exactly this way; this is the union of two precedents. The agent doors
		// (runAgentTask, agent_run, the review lane) file reason "gpu busy: ..." and defer
		// class capacity, and set no error class. The vision tier files error class gpu_busy
		// and "gpu busy: generation job holds the lock (...)", with no defer class, whenever
		// the lease is still held when its wait ends (runVisionGen): any live holder, a text
		// reservation included, whatever the reason text says; a lease refusal on its model
		// call comes back from the affinity gate and is filed through classifyErr as
		// "timeout", also with no defer class. Through classifyErr this refusal read
		// "timeout" too (the lease error carries the word), which nothing can act on.
		if modelaffinity.IsLeaseRefusal(terr) {
			meta.LatencyMs = time.Since(start).Milliseconds()
			meta.ErrClass = "gpu_busy"
			reason := "gpu busy: " + terr.Error()
			p.recordDefer(req.Task, meta, len(req.Audio), reason)
			res := core.Deferf(reason, "", meta)
			res.DeferClass = core.DeferClassCapacity
			return res
		}
		// ErrUpstreamNoSpeech: the audio had no speech (register C-91). Either the
		// upstream ANSWERED, successfully, with an empty transcript, or — whisper
		// protocol only — it crashed on the audio (an empty-body 5xx, the F-35 crash of
		// 2026-09-23) in a call that ran alone, with no unload sent by this process. Any
		// other empty-body 5xx can be a model unloaded from under the call, which looks
		// the same and used to report audio with speech as silent, so sttclient types it
		// ErrUpstreamVanished instead. The call went through and found nothing to
		// transcribe, so it is a calm defer rather than the "transcribe call failed"
		// wording, which reads as an infrastructure failure it is not, and there is
		// nothing to retry. An upstream that vanished mid-request falls through to the
		// failure below.
		if errors.Is(terr, sttclient.ErrUpstreamNoSpeech) {
			meta.LatencyMs = time.Since(start).Milliseconds()
			p.recordDefer(req.Task, meta, len(req.Audio), "empty transcript (no speech detected)")
			return core.Deferf("empty transcript (no speech detected)", "", meta)
		}
		meta.LatencyMs = time.Since(start).Milliseconds()
		meta.ErrClass = classifyErr(terr)
		p.recordDefer(req.Task, meta, len(req.Audio), "transcribe call failed: "+terr.Error())
		return core.Deferf("transcribe call failed: "+terr.Error(), "", meta)
	}
	full := strings.TrimSpace(tr.Text)
	if full == "" && len(tr.Segments) == 0 {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Audio), "empty transcript")
		return core.Deferf("empty transcript", "", meta)
	}
	// The word times of a drifted transcript (issue #169) are named where an operator reads: the
	// verdict is also published in the payload below, but a run that nobody reads the payload of
	// would otherwise cut audio at the wrong words without a trace.
	if tr.WordsReliable != nil && !*tr.WordsReliable {
		log.Printf("transcribe: word times of %q drift up to %.1f s from their segments (issue #169): words_reliable=false", filepath.Base(req.Audio), tr.WordsDriftSec)
	}

	// Write the full payload to disk (the pointer pattern) — best-effort: a write
	// failure does not fail the result (the inline data still carries the answer).
	// When the input is unidentifiable, salt the on-disk stem with the start time
	// so two recordings that hit the same transient error at the same path cannot
	// overwrite each other's .srt/.txt — the returned srt_path would otherwise
	// point at a different recording's transcript.
	stemIdent := ident
	if !identifiable {
		// A random nonce, NOT a timestamp. Transcribe is reachable concurrently
		// (the fleet node serves each request in its own goroutine), so two
		// parallel calls on different unidentifiable files whose start lands in the
		// same wall-clock tick would produce the same stem — and, sharing a
		// basename as field audio routinely does, overwrite each other's
		// .srt/.txt while each returned a path to the other's transcript. That is
		// exactly the collision the salt exists to prevent, reproduced through the
		// salt itself.
		// crypto/rand.Read cannot fail on this toolchain — since Go 1.24 it always
		// fills the buffer and crashes the process irrecoverably if the OS source
		// errors. An earlier version wrapped it in an err check with a timestamp
		// fallback and a warning log; that branch was unreachable, so the commit
		// advertised a mitigation the code did not have. Ignoring the return is the
		// honest expression of the actual guarantee.
		var nonce [8]byte
		_, _ = crand.Read(nonce[:])
		stemIdent = fmt.Sprintf("%s|unidentified=%x", ident, nonce)
	}
	base := mediaBase(p.cfg.MediaDir, req.Audio, stemIdent)
	srtPath, txtPath, jsonPath := base+".srt", base+".txt", base+".segments.json"
	_ = os.MkdirAll(filepath.Dir(base), 0o755)
	_ = os.WriteFile(srtPath, []byte(sttclient.SRT(tr.Segments)), 0o644)
	_ = os.WriteFile(txtPath, []byte(full), 0o644)
	if sj, e := json.MarshalIndent(tr.Segments, "", "  "); e == nil {
		_ = os.WriteFile(jsonPath, sj, 0o644)
	}

	// Inline a capped set of segments; the rest live in jsonPath.
	segs := tr.Segments
	truncated := false
	if p.cfg.STTMaxInlineSegments > 0 && len(segs) > p.cfg.STTMaxInlineSegments {
		segs = segs[:p.cfg.STTMaxInlineSegments]
		truncated = true
	}

	out := transcribeResult{
		Language:          tr.Language,
		DurationSec:       tr.Duration,
		NumSegments:       len(tr.Segments),
		Gist:              preview(full, 400),
		Segments:          segs,
		SegmentsTruncated: truncated,
		SRTPath:           srtPath,
		TextPath:          txtPath,
		JSONPath:          jsonPath,
		WordsReliable:     tr.WordsReliable,
		WordsDriftSec:     tr.WordsDriftSec,
	}
	data, _ := json.Marshal(out)
	// Storing under a non-content key is what turns a transient read failure into
	// a permanent wrong answer — so an unidentifiable input is computed, returned,
	// and deliberately not persisted.
	if p.cache != nil && storeable {
		if b, e := json.Marshal(cacheVal{Data: data, Model: model, InLoop: p.tierCache}); e == nil {
			_ = p.cache.Put(ck, b)
		}
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	p.record(req.Task, meta, len(req.Audio))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// transcriptFilesPresent reports whether every transcript file a cached result names still exists.
// A result that names none (an older entry, or a payload this build cannot read) counts as present:
// the check only refuses a hit it can prove dangles.
func transcriptFilesPresent(data json.RawMessage) bool {
	var r transcribeResult
	if json.Unmarshal(data, &r) != nil {
		return true
	}
	for _, p := range []string{r.SRTPath, r.TextPath, r.JSONPath} {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}
	return true
}

// transcribeResult is the offload_transcribe payload (the {gist, segments[]}
// citation pattern + on-disk pointers).
type transcribeResult struct {
	Language          string              `json:"language"`
	DurationSec       float64             `json:"duration_sec"`
	NumSegments       int                 `json:"num_segments"`
	Gist              string              `json:"gist"`
	Segments          []sttclient.Segment `json:"segments"`
	SegmentsTruncated bool                `json:"segments_truncated"`
	SRTPath           string              `json:"srt_path"`
	TextPath          string              `json:"text_path"`
	JSONPath          string              `json:"json_path"`
	// WordsReliable and WordsDriftSec are sttclient's word-time verdict (issue #169): false when the
	// first word of some segment starts 0.5 s or more away from that segment's start, with the largest
	// such gap in seconds (later words are not checked); absent when the transcript carries no word times, or when an entry cached by a build
	// older than the check is served (no verdict is not a verdict of true). The segment file stays a
	// bare array, so a reader of json_path alone does not see them.
	WordsReliable *bool   `json:"words_reliable,omitempty"`
	WordsDriftSec float64 `json:"words_drift_sec,omitempty"`
}

// mediaBase returns <MediaDir>/<sanitized-basename>-<8hex of ident> as the
// output stem. The ident hash disambiguates distinct sources that share a
// basename (e.g. two different recording.m4a) or model/lang variants of one
// source, so the returned .srt/.txt/.segments.json pointers never reference a
// different audio's transcript. ident carries the SAME identity fields the cache
// key is built from (transcribeCacheKey passes them as separate parts, this stem
// hashes the joined string), so on-disk files and cache entries agree.
func mediaBase(mediaDir, audioPath, ident string) string {
	name := filepath.Base(audioPath)
	if ext := filepath.Ext(name); ext != "" {
		name = name[:len(name)-len(ext)]
	}
	name = sanitizeStem(name)
	if name == "" || name == "." {
		name = "transcript"
	}
	return filepath.Join(mediaDir, name+"-"+sha256hex(ident)[:8])
}

// runGenerateAudio synthesizes audio on the LOCAL GPU. It reads params["kind"]
// (voice|music, default voice) and dispatches to VoiceGenScript (Chatterbox TTS, no
// ComfyUI) or MusicGenScript (ACE-Step via ComfyUI). An empty target script — or an
// unknown kind — defers cleanly (music defaults empty until B3). Shells out via
// internal/gpugen so the python/ComfyUI worker is process-tree-killed on timeout
// (invariant 3). params: kind (voice|music), clone/lang (voice), seconds (music),
// out (string), seed (int), reserve_vram (float, music only).
func (p *Pipeline) runGenerateAudio(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	kind := paramStr(req.Params, "kind")
	if kind == "" {
		kind = "voice"
	}
	var script string
	switch kind {
	case "voice":
		script = p.cfg.VoiceGenScript
	case "music":
		script = p.cfg.MusicGenScript
	default:
		return p.deferGen(req, meta, start, len(req.Input), "unknown audio kind "+kind)
	}
	// tts_endpoint lane (ttsendpoint.go, 0.113.25): an OpenAI-compatible speech
	// server serves voice when asked for by name (voice=endpoint) or by default
	// on a box with no local voice script. Decided before the script check so
	// a script-less box with an endpoint renders instead of deferring.
	if kind == "voice" && useTTSEndpoint(p.cfg, paramStr(req.Params, "voice")) {
		return p.runVoiceEndpoint(ctx, req, meta, start)
	}
	// CT-49: audio.cpp (voicegen_engine / musicgen_engine "audiocpp") serves voice and
	// music on a Vulkan iGPU; the python/ComfyUI worker below is not involved.
	if audiocppServes(p.cfg, kind, paramStr(req.Params, "voice")) {
		return p.runGenerateAudioAudiocpp(ctx, req, meta, start, kind)
	}
	if script == "" {
		return p.deferGen(req, meta, start, len(req.Input), "no audio-gen route configured for kind "+kind)
	}
	text := strings.TrimSpace(req.Input)
	if text == "" {
		return p.deferGen(req, meta, start, len(req.Input), "empty audio prompt")
	}
	// LO-2: resolve a relative script path against the exe dir (an MCP host spawns
	// us with no meaningful cwd) and defer with a distinct reason when missing.
	script, serr := gpugen.ResolveScript(script)
	if serr != nil {
		return p.deferGen(req, meta, start, len(req.Input), serr.Error())
	}
	meta.Model = "chatterbox-tts"
	if kind == "music" {
		meta.Model = "comfyui-acestep"
	}

	seed := paramIntOr(req.Params, "seed", 0)
	if seed <= 0 {
		seed = mintSeed()
		if req.Params == nil {
			req.Params = map[string]any{}
		}
		req.Params["seed"] = seed
	}

	ext := ".wav"
	if kind == "music" {
		ext = ".flac"
	}
	out := paramStr(req.Params, "out")
	if out == "" {
		_ = os.MkdirAll(p.cfg.MediaDir, 0o755)
		out = filepath.Join(p.cfg.MediaDir, kind+"-"+sha256hex(text + tasks.StableParamsKey(req.Params))[:8]+ext)
	}

	// CLI: tts.mjs <out> "<text>" [--clone ref] [--lang es]
	//      music worker <out> "<style>" --seed N [--seconds N] [--lyrics ..] [--reserve-vram ..]
	args := []string{out, text}
	if kind == "voice" {
		switch voice := paramStr(req.Params, "voice"); voice {
		case "", "generalist":
			// stock multilingual path: a request clone wins; else the machine's default es-MX ref.
			ref := paramStr(req.Params, "clone")
			if ref == "" {
				ref = p.cfg.VoiceGenRef
			}
			if ref != "" {
				args = append(args, "--clone", ref)
			}
			if lang := paramStr(req.Params, "lang"); lang != "" {
				args = append(args, "--lang", lang)
			}
		case "finetuned":
			// per-machine fine-tuned voice; requires a model + base dir, else defer (never cloud).
			if p.cfg.VoiceGenFTModel == "" || p.cfg.VoiceGenFTBaseDir == "" {
				return p.deferGen(req, meta, start, len(req.Input), "no fine-tuned voice configured")
			}
			meta.Model = "chatterbox-tts-ft"
			args = append(args, "--engine", "finetuned",
				"--model", p.cfg.VoiceGenFTModel, "--base-dir", p.cfg.VoiceGenFTBaseDir)
			ref := paramStr(req.Params, "clone")
			if ref == "" {
				ref = p.cfg.VoiceGenFTRef
			}
			if ref != "" {
				args = append(args, "--clone", ref)
			}
			lang := p.cfg.VoiceGenFTLang
			if l := paramStr(req.Params, "lang"); l != "" {
				lang = l
			}
			if lang != "" {
				args = append(args, "--lang", lang)
			}
			args = appendVoiceRecipe(args, p.cfg)
		default:
			return p.deferGen(req, meta, start, len(req.Input), "unknown voice "+voice)
		}
		// voice path is unchanged re: seed — Chatterbox takes no seed, so no --seed flag.
	} else { // music
		// ACE-Step IS seed-reproducible, so pass the minted/echoed seed (fixes the B1
		// gap: the audio path minted a seed but never threaded it to the music worker).
		args = append(args, "--seed", strconv.Itoa(seed))
		if s := paramIntOr(req.Params, "seconds", 0); s > 0 {
			args = append(args, "--seconds", strconv.Itoa(s))
		}
		if l := paramStr(req.Params, "lyrics"); l != "" {
			args = append(args, "--lyrics", l)
		}
		if rv := paramStr(req.Params, "reserve_vram"); rv != "" {
			args = append(args, "--reserve-vram", rv)
		}
	}

	timeout := time.Duration(p.cfg.AudioGenTimeoutSec) * time.Second
	// voice (Chatterbox TTS) has no ComfyUI instance to bind to a card: the whole node. music drives
	// ComfyUI on a single card.
	need := wholeNeed(paramStr(req.Params, "waiter_token")).resumableBy(req)
	if kind == "music" {
		need = singleCardNeed(p.cfg, paramStr(req.Params, "waiter_token")).declaresRAM(routeRAM(hostneed.RouteMusic, p.cfg)).resumableBy(req)
	}
	grant, lerr := p.acquireMediaLease(ctx, "audio-gen ("+kind+")", timeout, p.gpuWait(), need)
	if lerr != nil {
		return p.deferForLease(lerr, req.Task, meta, len(req.Input), start)
	}
	defer grant.Release()
	// voice never starts ComfyUI → skip the post-run ComfyUI /free (still tree-kills
	// the python worker on timeout). music drives ComfyUI → keep the /free, and take the
	// launch profile (a single-card route: the device pin applies, or the instance bound to the card).
	audioGenEnv := p.genEnv()
	if kind == "music" {
		audioGenEnv = p.comfyGenEnvFor(grant, true)
	}
	spec := gpugen.Spec{
		Exe:           p.cfg.NodePath,
		Script:        script,
		Args:          args,
		Env:           append(audioGenEnv, grant.Env...),
		Out:           out,
		Timeout:       timeout,
		SkipFreeComfy: kind == "voice",
	}
	// Passive fleet footprint: acestep for the ComfyUI music worker, chatterbox
	// for the TTS voice paths (incl. finetuned — same engine family).
	audioFamily := "chatterbox"
	if kind == "music" {
		audioFamily = "acestep"
	}
	p.footprintSampling(audioFamily, "", "audio-gen").ApplyTo(&spec)
	outPath, gerr := gpugen.Generate(ctx, spec)
	if gerr != nil {
		meta.ErrClass = gpugen.ClassifyErr(gerr)
		return p.deferGen(req, meta, start, len(req.Input), "audio generation failed: "+gerr.Error())
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	data, _ := json.Marshal(map[string]any{"audio_path": outPath, "kind": kind, "seed": seed})
	p.record(req.Task, meta, len(text))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// appendVoiceRecipe appends the per-machine fine-tuned generate() recipe knobs as CLI
// flags, omitting any that are zero (the worker then uses its own default). The worker
// binds them as kwargs — never positionally — because the English and multilingual
// Chatterbox generate() signatures order their params differently.
func appendVoiceRecipe(args []string, cfg config.Config) []string {
	add := func(flag string, v float64) {
		if v > 0 {
			args = append(args, flag, strconv.FormatFloat(v, 'g', -1, 64))
		}
	}
	add("--temperature", cfg.VoiceGenFTTemperature)
	add("--cfg-weight", cfg.VoiceGenFTCFGWeight)
	add("--exaggeration", cfg.VoiceGenFTExaggeration)
	add("--repetition-penalty", cfg.VoiceGenFTRepetitionPenalty)
	return args
}

// sttRoute picks the STT model and protocol for a transcribe request. Pure — the
// selection logic is the feature's actual switch, so it is unit-tested directly
// (the pipeline holds a concrete client, so routing is not stubbable above it).
// The HQ upstream may speak the OpenAI /v1/audio/transcriptions protocol instead of
// whisper-server's /inference (llama-server mtmd STT, e.g. Qwen3-ASR): binding such a
// model without stt_hq_api="openai" 404'd the whisper endpoint (live finding 2026-07-21).
func sttRoute(cfg config.Config, hq bool) (model string, useOAI bool) {
	model = cfg.STTModel
	if hq && cfg.STTModelHQ != "" {
		model = cfg.STTModelHQ
		useOAI = strings.EqualFold(cfg.STTHQAPI, "openai")
	}
	return model, useOAI
}
