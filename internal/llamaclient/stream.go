// stream.go — the streamed completion (ADR 0055, PR-12). A call that passes
// WithProgress asks the server to stream, hears every delta as progress, and
// decodes the stream into the same GenResult a JSON answer produces, so nothing
// after the decode changes. A server that answers JSON anyway (a proxy that
// ignores `stream`) is decoded as before, and a server that REFUSES a streamed
// request is asked again as one JSON answer.
package llamaclient

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// WithProgress streams the completion and reports its progress: fn is called
// with the number of tokens generated so far (every content or reasoning delta
// counts — a token is a token to a liveness rule) after each frame that carried
// generated text. It rides `stream: true` with
// `stream_options.include_usage`, so the final usage frame still gives the exact
// counts.
//
// It exists for the structured re-pack under the agent liveness monitor. A
// non-streamed re-pack is one silent request: the monitor hears nothing between
// the phase start and the reply, so a flat allowance fired at the same second
// whatever the answer size and killed seats that were producing (RC-6: 80 of 80
// killed re-packs carried a finished answer). Streamed, the allowance bounds
// SILENCE, not the whole answer.
//
// The call's context owns the deadline: the client's own Timeout covers the
// whole body read and would cut a long answer mid-stream (WithoutClientTimeout).
// A call that asked for logprobs is never streamed (the decoder does not read
// them), and a caller that passes no WithProgress sends the request it always
// sent.
func WithProgress(fn func(tokensSoFar int)) GenOption {
	return func(o *genOpts) { o.progress = fn }
}

// streamOptions is the OpenAI `stream_options` object; only usage is asked for.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// streamChunk is one `data:` frame of an OpenAI-style completion stream, in the
// union both engines emit: vLLM's `reasoning`, llama.cpp's `reasoning_content`,
// the usage frame, llama.cpp's `timings`, and an `error` object when the engine
// dies mid-stream.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Timings *struct {
		PredictedPerSecond float64 `json:"predicted_per_second"`
	} `json:"timings"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// streamMaxFrame bounds one `data:` line: a 12 KB answer arriving in a single
// frame is ordinary on a proxy that buffers; 16 MiB is generous, not open.
const streamMaxFrame = 16 << 20

// decodeStreamResult reads a streamed chat completion and returns the SAME
// GenResult decodeGenResult builds from a JSON answer. Only `content` is the
// answer, exactly as on the JSON path; reasoning deltas count as progress but
// are not content. progress (nil = none) is called with the running delta count
// after every frame that carried generated text; the usage frame's exact
// completion_tokens replaces the count at the end. It closes resp.Body.
//
// A stream that dies mid-body — a reset, an engine `error` frame, an end with
// neither a finish_reason nor [DONE] — is a *BodyError, the wire's own failure:
// the same class a JSON body that stops mid-read has, so genErrIsTransport
// files it as the seat not being reachable, never as the model getting the
// shape wrong.
func decodeStreamResult(resp *http.Response, start time.Time, progress func(int)) (GenResult, error) {
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), streamMaxFrame)
	var content strings.Builder
	var finish string
	var promptTokens, completionTokens int
	var tokPerSec float64
	tokens, done := 0, false
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // blank separators, `event:` and `:` keep-alive comment lines
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			done = true
			break
		}
		var c streamChunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			return GenResult{}, &BodyError{Err: fmt.Errorf("stream frame: %w", err)}
		}
		if c.Error != nil {
			return GenResult{}, &BodyError{Err: fmt.Errorf("engine error in stream: %s", c.Error.Message)}
		}
		if c.Usage != nil {
			promptTokens, completionTokens = c.Usage.PromptTokens, c.Usage.CompletionTokens
		}
		if c.Timings != nil && c.Timings.PredictedPerSecond > 0 {
			tokPerSec = c.Timings.PredictedPerSecond
		}
		for _, ch := range c.Choices {
			d := ch.Delta
			ticked := false
			if d.Content != "" {
				content.WriteString(d.Content)
				ticked = true
			}
			if d.Reasoning != "" || d.ReasoningContent != "" {
				ticked = true
			}
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
			if ticked {
				tokens++
				if progress != nil {
					progress(tokens)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return GenResult{}, &BodyError{Err: fmt.Errorf("stream read: %w", err)}
	}
	if !done && finish == "" {
		return GenResult{}, &BodyError{Err: errors.New("stream ended without a finish_reason or [DONE]")}
	}
	out := GenResult{
		Content:      content.String(),
		TokensIn:     promptTokens,
		TokensOut:    completionTokens,
		Truncated:    finish == "length",
		FinishReason: finish,
		TokPerSec:    tokPerSec,
	}
	if out.TokensOut == 0 {
		// the engine sent no usage frame: the delta count is the honest estimate
		out.TokensOut = tokens
	}
	if out.TokPerSec == 0 && out.TokensOut > 0 {
		if elapsed := time.Since(start); elapsed > 0 {
			out.TokPerSec = float64(out.TokensOut) / elapsed.Seconds()
		}
	}
	return out, nil
}

// streamRefusal remembers that a seat refused a streamed request and answered
// the same request as one JSON answer, so the next call to it does not pay the
// refused request first. Keyed on base and model, expiring: a seat that was
// fixed streams again.
var streamRefusals sync.Map // "base\x00model" -> time.Time

// streamRefusalTTL is how long a refusal is remembered.
const streamRefusalTTL = 30 * time.Minute

func streamRefusalKey(base, model string) string { return base + "\x00" + model }

// streamWasRefused reports whether a streamed request to this seat was refused
// recently and its JSON retry answered.
func streamWasRefused(base, model string) bool {
	v, ok := streamRefusals.Load(streamRefusalKey(base, model))
	if !ok {
		return false
	}
	if at, _ := v.(time.Time); time.Since(at) < streamRefusalTTL {
		return true
	}
	streamRefusals.Delete(streamRefusalKey(base, model))
	return false
}

// noteStreamRefused records a refusal, logging the first one for the seat.
func noteStreamRefused(base, model string, cause error) {
	if _, loaded := streamRefusals.Swap(streamRefusalKey(base, model), time.Now()); !loaded {
		log.Printf("llamaclient: %s (model %q) refused a streamed request (%v) but answered it as JSON; calls to it go non-streamed for %s", base, model, cause, streamRefusalTTL)
	}
}

// refusedStream reports whether err is the shape of a server refusing the
// STREAM: a 400 or 422 answer to a request that asked for one. It cannot tell a
// refusal of the stream from a request that would be refused however it was
// sent (a context that does not fit is also a 400), so the caller acts on it
// only by asking again as JSON and remembers nothing unless that answers.
func refusedStream(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && (se.StatusCode == http.StatusBadRequest || se.StatusCode == http.StatusUnprocessableEntity)
}
