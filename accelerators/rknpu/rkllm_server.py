#!/usr/bin/env python3
r"""RKLLM OpenAI-compatible server: one NPU seat (RK3588, librkllmrt.so 1.3.x) behind llama-swap.

llama-swap starts one process per `rkllm` seat (rkllm-serve.sh), polls GET /health until it answers 200 and stops
the seat with SIGTERM when its ttl runs out, so there is no idle watchdog here. Wire contract:

    GET  /health               -> 503 {"status":"loading"} until rkllm_init returns, then 200 {"status":"ok"}
    GET  /v1/models            -> OpenAI list: the served name, max_model_len = the served context window
    POST /v1/chat/completions  -> OpenAI chat completion, stream false or true (SSE chunks, then "data: [DONE]")
    anything else              -> 404 {"error": {...}}

Honoured: messages (system first, user, assistant; text parts and image_url data URIs), max_tokens or
max_completion_tokens, temperature, top_p, top_k, repeat_penalty or repetition_penalty, presence_penalty,
frequency_penalty, stop, stream_options.include_usage, chat_template_kwargs.enable_thinking (default OFF).
Accepted and ignored, never a 400: grammar, response_format, logprobs, top_logprobs, cache_prompt, seed, n and every
other chat_template_kwargs key. Refused with 400: tools, tool roles, http(s) image URLs, images on a seat started
without --vision-encoder. Answers carry `usage` and llama.cpp-style `timings` built from RKLLMPerfStat.

Prompt path (measured on the reference board, runtime 1.3.1, Qwen3.5-0.8B .rkllm):
  * With nothing set, rkllm_run takes ONE user turn (RKLLMInput.role "user"; "tool" wants a JSON string, else rc -1)
    and wraps it in the model's own template: ...<|im_start|>assistant\n<think>\n\n</think>\n\n with thinking off ("hi"
    prefills 13 tokens), ...assistant\n<think>\n with enable_thinking. No system prompt, no earlier turns.
  * rkllm_set_chat_template(h, system, prefix, postfix) replaces that, and the runtime says so: it "will disable the
    internal automatic chat template parsing, including enable_thinking". With three empty strings the prompt is
    tokenised exactly as given: a raw ChatML string with a system message obeyed it (an "answer in French" system
    prompt was followed) and a two-turn history was recalled. A prefix/postfix WITHOUT the empty-think stub let the
    model open <think> by itself although the flag was off, so the stub belongs in the prompt.
  So the server installs the empty template once after rkllm_init and renders Qwen3.5 ChatML itself, as the model's
  chat_template.jinja does (system first, |trim on every content, earlier assistant turns without their reasoning,
  generation prompt ending in the empty think block, or in "<think>\n" when enable_thinking is true). With thinking on
  the output starts INSIDE the think block, so the text up to </think> comes back as `reasoning_content` and the rest
  as `content` (<think> is not a special token, so skip_special_token keeps it; EOS is one, so it never shows).

Runtime behaviours relied on (measured; callbacks run on the thread that called rkllm_run):
  * rkllm_abort from inside the callback returns 0 at once, the run then ends with one FINISH event and the next run
    is clean. Stop strings, client disconnects and SIGTERM all end a run this way.
  * A prompt longer than the window makes rkllm_run return -1 with no events (HTTP 400). A generation that outgrows
    the window does NOT stop: the runtime shifts its window (900 tokens came out of a 1024 window). So max_tokens,
    capped at --ctx-size, is what bounds a runaway answer, and finish_reason "length" means that cap was reached.
  * RKLLMPerfStat arrives only on the FINISH event, and generate_tokens is the tokens after the first (n - 1). So
    timings.predicted_n / predicted_ms is the decode rate, while usage.completion_tokens counts every sampled token,
    the stop token included, like llama.cpp.
  * RUN_WAITING events (a token that is half a UTF-8 character) carry no text; the character arrives whole in the
    next RUN_NORMAL event.

The NPU is one device: a lock admits one generation (and its image encodes) at a time; every other request waits
its turn and drops out if its client hangs up while queued. Loopback only (a non-loopback --host exits 2). The
runtime pins its workers to --cpu-mask, and the server pins its own threads (HTTP, image decode) to the same CPUs, so
a seat confined to the A55 cluster never spills onto the cores the host keeps for itself. SIGTERM aborts a running
generation (its request answers 503, never a truncated "stop"), then rkllm_destroy, then exits. `--print-layout`
prints the sizeof/offsetof table that rkllm_layout_check.cpp prints from the vendor headers (see the note above the
structs).
"""
from __future__ import annotations

import argparse
import base64
import ctypes
import io
import ipaddress
import json
import os
import select
import signal
import socket
import sys
import threading
import time
import uuid
from collections.abc import Callable
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import NamedTuple
from urllib.parse import urlparse

DEFAULT_CTX = 4096
DEFAULT_TOP_K = 40  # the sampling defaults below are llama.cpp's, so an unset knob behaves like it does on the GPU seats
DEFAULT_TOP_P = 0.95
DEFAULT_TEMPERATURE = 0.8
BASE_DOMAIN_ID = 1  # the vendor multimodal demo's value for RK3588 (0 is for rk3562/rv1126b); text-only ran fine on 0 too
MAX_BODY = 48 << 20  # request bytes; a 12 MP photo is ~5 MB as a base64 data URI
MAX_IMAGE_PIXELS = 50_000_000
THINK_OFF = "<think>\n\n</think>\n\n"  # what Qwen3.5's own template ends the generation prompt with, thinking off
THINK_ON = "<think>\n"
IMAGE_PLACEHOLDER = "<image>"  # the runtime expands each one into image_start + n_image_tokens x image_content + image_end
# Qwen3.5's image tags, the vendor demo's defaults; another family (Gemma-4: "<|image>" ...) needs its own.
IMAGE_TAGS = (b"<|vision_start|>", b"<|vision_end|>", b"<|image_pad|>")
IMAGE_MIMES = ("image/png", "image/jpeg", "image/jpg", "image/webp")
IMAGE_FORMATS = ["PNG", "JPEG", "WEBP"]
# rknn_api.h enums the encoder path uses
RKNN_QUERY_IN_OUT_NUM, RKNN_QUERY_INPUT_ATTR, RKNN_QUERY_OUTPUT_ATTR = 0, 1, 2
RKNN_TENSOR_NCHW, RKNN_TENSOR_NHWC = 0, 1
RKNN_TENSOR_UINT8 = 3
RKNN_NPU_CORE_0_1_2 = 7

# ---------------------------------------------------------------- RKLLM C API (librkllmrt.so 1.3.1, rkllm.h)
#
# The runtime reads these structs by raw offset: one shifted field is a crash or a silently wrong
# sampler, never an error message. Every class below mirrors rkllm.h field for field, in header order,
# with the header's exact widths (bool, int8_t, uint8_t, int32_t, size_t, float). `--print-layout`
# prints sizeof/offsetof for all of them in the exact line format of rkllm_layout_check.cpp, which
# prints the same numbers from the vendor headers, so `diff` between the two is the check.

RKLLM_RUN_NORMAL, RKLLM_RUN_WAITING, RKLLM_RUN_FINISH, RKLLM_RUN_ERROR = 0, 1, 2, 3
RKLLM_INPUT_PROMPT, RKLLM_INPUT_TOKEN, RKLLM_INPUT_EMBED, RKLLM_INPUT_MULTIMODAL = 0, 1, 2, 3
RKLLM_INFER_GENERATE = 0


class RKLLMExtendParam(ctypes.Structure):
    _fields_ = [
        ("base_domain_id", ctypes.c_int32),
        ("embed_flash", ctypes.c_int8),
        ("enabled_cpus_num", ctypes.c_int8),
        ("enabled_cpus_mask", ctypes.c_uint32),
        ("n_batch", ctypes.c_uint8),
        ("use_cross_attn", ctypes.c_int8),
        ("reserved", ctypes.c_uint8 * 104),
    ]


class RKLLMParam(ctypes.Structure):
    _fields_ = [
        ("model_path", ctypes.c_char_p),
        ("max_context_len", ctypes.c_int32),
        ("max_new_tokens", ctypes.c_int32),
        ("top_k", ctypes.c_int32),
        ("n_keep", ctypes.c_int32),
        ("top_p", ctypes.c_float),
        ("temperature", ctypes.c_float),
        ("repeat_penalty", ctypes.c_float),
        ("frequency_penalty", ctypes.c_float),
        ("presence_penalty", ctypes.c_float),
        ("mirostat", ctypes.c_int32),
        ("mirostat_tau", ctypes.c_float),
        ("mirostat_eta", ctypes.c_float),
        ("skip_special_token", ctypes.c_bool),
        ("ignore_eos_token", ctypes.c_bool),
        ("is_async", ctypes.c_bool),
        ("extend_param", RKLLMExtendParam),
    ]


class RKLLMEmbedInput(ctypes.Structure):
    _fields_ = [("embed", ctypes.POINTER(ctypes.c_float)), ("n_tokens", ctypes.c_size_t)]


class RKLLMTokenInput(ctypes.Structure):
    _fields_ = [("input_ids", ctypes.POINTER(ctypes.c_int32)), ("n_tokens", ctypes.c_size_t)]


# The three anonymous nested structs of RKLLMMultiModalInput, named here so ctypes can build them.
class RKLLMImageInput(ctypes.Structure):
    _fields_ = [
        ("image_embed", ctypes.POINTER(ctypes.c_float)),
        ("n_image_tokens", ctypes.c_size_t),
        ("n_image", ctypes.c_size_t),
        ("image_start", ctypes.c_char_p),
        ("image_end", ctypes.c_char_p),
        ("image_content", ctypes.c_char_p),
        ("image_width", ctypes.c_size_t),
        ("image_height", ctypes.c_size_t),
    ]


class RKLLMVideoInput(ctypes.Structure):
    _fields_ = [
        ("video_embed", ctypes.POINTER(ctypes.c_float)),
        ("n_frame_tokens", ctypes.c_size_t),
        ("n_frame_per_video", ctypes.c_size_t),
        ("n_video", ctypes.c_size_t),
        ("video_start", ctypes.c_char_p),
        ("video_end", ctypes.c_char_p),
        ("video_content", ctypes.c_char_p),
        ("frame_width", ctypes.c_size_t),
        ("frame_height", ctypes.c_size_t),
    ]


class RKLLMAudioInput(ctypes.Structure):
    _fields_ = [
        ("audio_embed", ctypes.POINTER(ctypes.c_float)),
        ("n_audio_tokens", ctypes.c_size_t),
        ("n_audio", ctypes.c_size_t),
        ("audio_start", ctypes.c_char_p),
        ("audio_end", ctypes.c_char_p),
        ("audio_content", ctypes.c_char_p),
    ]


class RKLLMMultiModalInput(ctypes.Structure):
    _fields_ = [
        ("prompt", ctypes.c_char_p),
        ("image", RKLLMImageInput),
        ("video", RKLLMVideoInput),
        ("audio", RKLLMAudioInput),
    ]


class _RKLLMInputUnion(ctypes.Union):
    _fields_ = [
        ("prompt_input", ctypes.c_char_p),
        ("embed_input", RKLLMEmbedInput),
        ("token_input", RKLLMTokenInput),
        ("multimodal_input", RKLLMMultiModalInput),
    ]


class RKLLMInput(ctypes.Structure):
    _anonymous_ = ("input_data",)  # the header's anonymous union: prompt_input etc. read as direct members
    _fields_ = [
        ("role", ctypes.c_char_p),
        ("enable_thinking", ctypes.c_bool),
        ("input_type", ctypes.c_int),
        ("input_data", _RKLLMInputUnion),
    ]


class RKLLMLoraParam(ctypes.Structure):
    _fields_ = [("lora_adapter_name", ctypes.c_char_p)]


class RKLLMPromptCacheParam(ctypes.Structure):
    _fields_ = [("save_prompt_cache", ctypes.c_int), ("prompt_cache_path", ctypes.c_char_p)]


class RKLLMSamplingParam(ctypes.Structure):
    _fields_ = [
        ("top_k", ctypes.c_int32),
        ("top_p", ctypes.c_float),
        ("temperature", ctypes.c_float),
        ("repeat_penalty", ctypes.c_float),
        ("frequency_penalty", ctypes.c_float),
        ("presence_penalty", ctypes.c_float),
        ("mirostat", ctypes.c_int32),
        ("mirostat_tau", ctypes.c_float),
        ("mirostat_eta", ctypes.c_float),
    ]


class RKLLMInferParam(ctypes.Structure):
    _fields_ = [
        ("mode", ctypes.c_int),
        ("lora_params", ctypes.POINTER(RKLLMLoraParam)),
        ("prompt_cache_params", ctypes.POINTER(RKLLMPromptCacheParam)),
        ("sampling_params", ctypes.POINTER(RKLLMSamplingParam)),
        ("keep_history", ctypes.c_int),
        ("max_new_tokens", ctypes.c_int32),
    ]


class RKLLMResultLastHiddenLayer(ctypes.Structure):
    _fields_ = [
        ("hidden_states", ctypes.POINTER(ctypes.c_float)),
        ("embd_size", ctypes.c_int),
        ("num_tokens", ctypes.c_int),
    ]


class RKLLMResultLogits(ctypes.Structure):
    _fields_ = [
        ("logits", ctypes.POINTER(ctypes.c_float)),
        ("vocab_size", ctypes.c_int),
        ("num_tokens", ctypes.c_int),
    ]


class RKLLMPerfStat(ctypes.Structure):
    _fields_ = [
        ("prefill_time_ms", ctypes.c_float),
        ("prefill_tokens", ctypes.c_int),
        ("generate_time_ms", ctypes.c_float),
        ("generate_tokens", ctypes.c_int),
        ("memory_usage_mb", ctypes.c_float),
    ]


class RKLLMResult(ctypes.Structure):
    _fields_ = [
        ("text", ctypes.c_char_p),
        ("token_id", ctypes.c_int32),
        ("last_hidden_layer", RKLLMResultLastHiddenLayer),
        ("logits", RKLLMResultLogits),
        ("perf", RKLLMPerfStat),
    ]


# int (*LLMResultCallback)(RKLLMResult*, void* userdata, LLMCallState) and the two optional
# callbacks of RKLLMCallback (unused here: the .rkllm carries its own tokenizer and embedding layer).
LLMResultCallback = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.POINTER(RKLLMResult), ctypes.c_void_p, ctypes.c_int)
LLMTokenizerCallback = ctypes.CFUNCTYPE(
    ctypes.c_int, ctypes.c_void_p, ctypes.c_char_p, ctypes.c_int32, ctypes.POINTER(ctypes.c_int32), ctypes.c_int32)
LLMGetEmbedCallback = ctypes.CFUNCTYPE(
    ctypes.c_int, ctypes.c_void_p, ctypes.POINTER(ctypes.c_int32), ctypes.c_uint64, ctypes.c_void_p, ctypes.c_uint64)


class RKLLMCallback(ctypes.Structure):
    _fields_ = [
        ("result_callback", LLMResultCallback),
        ("result_userdata", ctypes.c_void_p),
        ("tokenizer_callback", LLMTokenizerCallback),
        ("tokenizer_userdata", ctypes.c_void_p),
        ("embed_callback", LLMGetEmbedCallback),
        ("embed_userdata", ctypes.c_void_p),
    ]


# ---------------------------------------------------------------- RKNN C API (librknnrt.so 2.3.2, rknn_api.h)
#
# Only what the vision encoder needs: rknn_init/query/set_core_mask/inputs_set/run/outputs_get/
# outputs_release/destroy and the four structs they take. Same rule as above: header order, header widths.

# The ctypes names mirror the C names on purpose: rkllm_layout_check.cpp prints them and its output is diffed against ours.
class rknn_input_output_num(ctypes.Structure):
    _fields_ = [("n_input", ctypes.c_uint32), ("n_output", ctypes.c_uint32)]


class rknn_tensor_attr(ctypes.Structure):
    _fields_ = [
        ("index", ctypes.c_uint32),
        ("n_dims", ctypes.c_uint32),
        ("dims", ctypes.c_uint32 * 16),
        ("name", ctypes.c_char * 256),
        ("n_elems", ctypes.c_uint32),
        ("size", ctypes.c_uint32),
        ("fmt", ctypes.c_int),
        ("type", ctypes.c_int),
        ("qnt_type", ctypes.c_int),
        ("fl", ctypes.c_int8),
        ("zp", ctypes.c_int32),
        ("scale", ctypes.c_float),
        ("w_stride", ctypes.c_uint32),
        ("size_with_stride", ctypes.c_uint32),
        ("pass_through", ctypes.c_uint8),
        ("h_stride", ctypes.c_uint32),
    ]


class rknn_input(ctypes.Structure):
    _fields_ = [
        ("index", ctypes.c_uint32),
        ("buf", ctypes.c_void_p),
        ("size", ctypes.c_uint32),
        ("pass_through", ctypes.c_uint8),
        ("type", ctypes.c_int),
        ("fmt", ctypes.c_int),
    ]


class rknn_output(ctypes.Structure):
    _fields_ = [
        ("want_float", ctypes.c_uint8),
        ("is_prealloc", ctypes.c_uint8),
        ("index", ctypes.c_uint32),
        ("buf", ctypes.c_void_p),
        ("size", ctypes.c_uint32),
    ]


# struct -> member paths in header order; a dotted path descends into an inline nested struct.
# rkllm_layout_check.cpp lists the same members in the same order.
LAYOUT: dict[str, tuple[type, list[str]]] = {
    "RKLLMExtendParam": (RKLLMExtendParam, ["base_domain_id", "embed_flash", "enabled_cpus_num", "enabled_cpus_mask",
                                            "n_batch", "use_cross_attn", "reserved"]),
    "RKLLMParam": (RKLLMParam, ["model_path", "max_context_len", "max_new_tokens", "top_k", "n_keep", "top_p",
                                "temperature", "repeat_penalty", "frequency_penalty", "presence_penalty", "mirostat",
                                "mirostat_tau", "mirostat_eta", "skip_special_token", "ignore_eos_token", "is_async",
                                "extend_param"]),
    "RKLLMEmbedInput": (RKLLMEmbedInput, ["embed", "n_tokens"]),
    "RKLLMTokenInput": (RKLLMTokenInput, ["input_ids", "n_tokens"]),
    "RKLLMMultiModalInput": (RKLLMMultiModalInput, [
        "prompt",
        "image", "image.image_embed", "image.n_image_tokens", "image.n_image", "image.image_start", "image.image_end",
        "image.image_content", "image.image_width", "image.image_height",
        "video", "video.video_embed", "video.n_frame_tokens", "video.n_frame_per_video", "video.n_video",
        "video.video_start", "video.video_end", "video.video_content", "video.frame_width", "video.frame_height",
        "audio", "audio.audio_embed", "audio.n_audio_tokens", "audio.n_audio", "audio.audio_start", "audio.audio_end",
        "audio.audio_content"]),
    "RKLLMInput": (RKLLMInput, ["role", "enable_thinking", "input_type", "prompt_input", "embed_input", "token_input",
                                "multimodal_input"]),
    "RKLLMLoraParam": (RKLLMLoraParam, ["lora_adapter_name"]),
    "RKLLMPromptCacheParam": (RKLLMPromptCacheParam, ["save_prompt_cache", "prompt_cache_path"]),
    "RKLLMSamplingParam": (RKLLMSamplingParam, ["top_k", "top_p", "temperature", "repeat_penalty", "frequency_penalty",
                                                "presence_penalty", "mirostat", "mirostat_tau", "mirostat_eta"]),
    "RKLLMInferParam": (RKLLMInferParam, ["mode", "lora_params", "prompt_cache_params", "sampling_params",
                                          "keep_history", "max_new_tokens"]),
    "RKLLMResultLastHiddenLayer": (RKLLMResultLastHiddenLayer, ["hidden_states", "embd_size", "num_tokens"]),
    "RKLLMResultLogits": (RKLLMResultLogits, ["logits", "vocab_size", "num_tokens"]),
    "RKLLMPerfStat": (RKLLMPerfStat, ["prefill_time_ms", "prefill_tokens", "generate_time_ms", "generate_tokens",
                                      "memory_usage_mb"]),
    "RKLLMResult": (RKLLMResult, ["text", "token_id", "last_hidden_layer", "logits", "perf"]),
    "RKLLMCallback": (RKLLMCallback, ["result_callback", "result_userdata", "tokenizer_callback", "tokenizer_userdata",
                                      "embed_callback", "embed_userdata"]),
    "rknn_input_output_num": (rknn_input_output_num, ["n_input", "n_output"]),
    "rknn_tensor_attr": (rknn_tensor_attr, ["index", "n_dims", "dims", "name", "n_elems", "size", "fmt", "type",
                                            "qnt_type", "fl", "zp", "scale", "w_stride", "size_with_stride",
                                            "pass_through", "h_stride"]),
    "rknn_input": (rknn_input, ["index", "buf", "size", "pass_through", "type", "fmt"]),
    "rknn_output": (rknn_output, ["want_float", "is_prealloc", "index", "buf", "size"]),
}


def _offset(cls: type, path: str) -> int:
    off = 0
    for part in path.split("."):
        off += getattr(cls, part).offset
        cls = dict(cls._fields_).get(part)  # type: ignore[attr-defined]  # None past an anonymous-union member
    return off


def layout_report() -> list[str]:
    """One line per struct size and member offset, worded exactly like rkllm_layout_check.cpp."""
    out: list[str] = []
    for name, (cls, members) in LAYOUT.items():
        out.append(f"{name} sizeof {ctypes.sizeof(cls)}")
        out.extend(f"{name}.{m} offset {_offset(cls, m)}" for m in members)
    return out


# ---------------------------------------------------------------- runtime wrappers
#
# The HTTP layer talks to a runtime object (generate / abort / close) and an encoder object (encode + geometry). These
# two classes bind the vendor libraries; test_rkllm_server.py binds fakes. Nothing from here down needs the NPU to import.

class Perf(NamedTuple):
    """RKLLMPerfStat of the FINISH event."""

    prefill_ms: float
    prefill_tokens: int
    generate_ms: float
    generate_tokens: int
    memory_mb: float


class Run(NamedTuple):
    """What one rkllm_run did."""

    rc: int  # rkllm_run's return code; -1 with no events = the runtime refused the request
    tokens: int  # token events seen (NORMAL + WAITING): every sampled token, the stop token included
    eos: bool  # the last text event was empty: EOS is a special token and skip_special_token drops its text
    error: str  # "" or what ended the run: a RUN_ERROR event, or a failure inside the callback
    perf: Perf | None


class Sampling(NamedTuple):
    top_k: int
    top_p: float
    temperature: float
    repeat_penalty: float
    frequency_penalty: float
    presence_penalty: float


class ImageBatch(NamedTuple):
    """Encoder output for the <image> tags of one prompt, in order."""

    embeds: list  # one flat float32 array per image, opaque to the HTTP layer
    tokens: int  # n_image_tokens per image
    width: int  # the encoder's input size, which the demo passes as image_width / image_height
    height: int


class _RunState:
    __slots__ = ("eos", "error", "on_text", "perf", "stopped", "tokens")

    def __init__(self, on_text: Callable[[str], bool]):
        self.on_text, self.tokens, self.eos, self.error, self.perf, self.stopped = on_text, 0, False, "", None, False


class RKLLMRuntime:
    """librkllmrt.so behind generate(): rkllm_init once, then one rkllm_run at a time (the caller holds the lock)."""

    def __init__(self, lib_path: str, model_path: str, ctx_size: int, cpu_mask: int):
        lib = ctypes.CDLL(lib_path)
        lib.rkllm_createDefaultParam.restype = RKLLMParam
        lib.rkllm_createDefaultParam.argtypes = []
        lib.rkllm_init.restype = ctypes.c_int
        lib.rkllm_init.argtypes = [ctypes.POINTER(ctypes.c_void_p), ctypes.POINTER(RKLLMParam),
                                   ctypes.POINTER(RKLLMCallback)]
        lib.rkllm_run.restype = ctypes.c_int
        lib.rkllm_run.argtypes = [ctypes.c_void_p, ctypes.POINTER(RKLLMInput), ctypes.POINTER(RKLLMInferParam),
                                  ctypes.c_void_p]
        lib.rkllm_abort.restype = ctypes.c_int
        lib.rkllm_abort.argtypes = [ctypes.c_void_p]
        lib.rkllm_destroy.restype = ctypes.c_int
        lib.rkllm_destroy.argtypes = [ctypes.c_void_p]
        lib.rkllm_set_chat_template.restype = ctypes.c_int
        lib.rkllm_set_chat_template.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_char_p, ctypes.c_char_p]
        self._lib = lib
        self._state: _RunState | None = None
        # Start from the vendor defaults (skip_special_token, embed_flash, n_batch, the reserved tail) and set only
        # what the seat configures. The default enabled_cpus_mask is the A76 cluster, so num and mask both follow --cpu-mask.
        self._param = param = lib.rkllm_createDefaultParam()
        param.model_path = model_path.encode()
        param.max_context_len = ctx_size
        param.max_new_tokens = ctx_size  # every request overrides it through RKLLMInferParam.max_new_tokens
        param.extend_param.base_domain_id = BASE_DOMAIN_ID
        param.extend_param.enabled_cpus_num = bin(cpu_mask).count("1")
        param.extend_param.enabled_cpus_mask = cpu_mask
        self._result_cb = LLMResultCallback(self._on_result)  # referenced for the handle's whole life
        self._cb = RKLLMCallback()
        self._cb.result_callback = self._result_cb
        handle = ctypes.c_void_p()
        rc = lib.rkllm_init(ctypes.byref(handle), ctypes.byref(param), ctypes.byref(self._cb))
        if rc != 0:
            raise RuntimeError(f"rkllm_init failed (rc={rc}); the runtime's own message is printed above")
        self._handle: ctypes.c_void_p | None = handle
        # The empty template turns the runtime's own chat wrapping off (see the module docstring).
        rc = lib.rkllm_set_chat_template(handle, b"", b"", b"")
        if rc != 0:
            self.close()
            raise RuntimeError(f"rkllm_set_chat_template failed (rc={rc})")

    def _on_result(self, res, _userdata, state) -> int:
        st = self._state
        if st is None:
            return 0
        try:
            r = res.contents
            if state == RKLLM_RUN_FINISH:
                p = r.perf
                st.perf = Perf(round(p.prefill_time_ms, 2), p.prefill_tokens, round(p.generate_time_ms, 2),
                               p.generate_tokens, round(p.memory_usage_mb, 1))
            elif state == RKLLM_RUN_ERROR:
                st.error = st.error or "the runtime reported RKLLM_RUN_ERROR"
            elif not st.stopped:  # NORMAL or WAITING; anything after we asked for a stop is dropped
                st.tokens += 1
                if state == RKLLM_RUN_NORMAL:
                    text = r.text.decode("utf-8", "replace") if r.text else ""
                    st.eos = not text
                    if text and not st.on_text(text):
                        st.stopped = True
                        self._lib.rkllm_abort(self._handle)
        except Exception as e:  # noqa: BLE001 - an exception must not cross the C frame; end the run and report it
            st.error, st.stopped = st.error or f"{type(e).__name__}: {e}", True
            self._lib.rkllm_abort(self._handle)
        return 0

    def generate(self, prompt: str, images: ImageBatch | None, sampling: Sampling, max_new: int,
                 on_text: Callable[[str], bool]) -> Run:
        """One run. on_text(chunk) returns False to stop it; generation is aborted from inside the callback."""
        prompt_b = prompt.encode("utf-8")
        inp = RKLLMInput()  # ctypes zero-fills
        inp.role = b"user"
        inp.enable_thinking = False  # ignored under the empty template; the prompt carries the think block
        embed = None
        if images is None:
            inp.input_type = RKLLM_INPUT_PROMPT
            inp.prompt_input = prompt_b
        else:
            import numpy as np

            embed = np.ascontiguousarray(np.concatenate(images.embeds), dtype=np.float32)  # kept alive through the run
            inp.input_type = RKLLM_INPUT_MULTIMODAL
            mm = inp.multimodal_input
            mm.prompt = prompt_b
            mm.image.image_embed = embed.ctypes.data_as(ctypes.POINTER(ctypes.c_float))
            mm.image.n_image_tokens = images.tokens
            mm.image.n_image = len(images.embeds)
            mm.image.image_start, mm.image.image_end, mm.image.image_content = IMAGE_TAGS
            mm.image.image_width, mm.image.image_height = images.width, images.height
        sp = RKLLMSamplingParam(sampling.top_k, sampling.top_p, sampling.temperature, sampling.repeat_penalty,
                                sampling.frequency_penalty, sampling.presence_penalty, 0, 5.0, 0.1)
        ip = RKLLMInferParam()
        ip.mode = RKLLM_INFER_GENERATE
        ip.keep_history = 0  # every request is stateless: the client sends the whole conversation
        ip.max_new_tokens = max_new
        ip.sampling_params = ctypes.pointer(sp)
        st = self._state = _RunState(on_text)
        try:
            rc = self._lib.rkllm_run(self._handle, ctypes.byref(inp), ctypes.byref(ip), None)
        finally:
            self._state = None
        del embed
        return Run(rc, st.tokens, st.eos, st.error, st.perf)

    def abort(self) -> None:
        """Ask a running generation to stop; safe from any thread (measured)."""
        if self._handle is not None:
            self._lib.rkllm_abort(self._handle)

    def close(self) -> None:
        handle, self._handle = self._handle, None
        if handle is not None:
            self._lib.rkllm_destroy(handle)


def _resize_square(src, out_h: int, out_w: int):
    """The vendor demo's preprocessing of one RGB uint8 image (image_enc.cc / img_encoder.cpp, cv2 calls in numpy).

    expand2square pads the short side to a square filled with mid-gray (the demo's Scalar(127.5) saturates to 128 in
    an 8-bit Mat), then cv::resize INTER_LINEAR to the encoder's input: pixel-centre mapping, border replicated,
    and NO antialiasing on a downscale, which is why Pillow's filtering resize is not used. The padded square is
    never built: a 16000x4000 upload would need 768 MB for it, so each tap is gathered from the picture, or is gray
    where it falls in the padding.
    """
    import numpy as np

    h, w = src.shape[:2]
    side = max(h, w)
    y_off, x_off = (side - h) // 2, (side - w) // 2

    def axis(n_out: int):
        f = np.clip((np.arange(n_out) + 0.5) * (side / n_out) - 0.5, 0, side - 1)
        i0 = f.astype(np.int64)
        return i0, np.minimum(i0 + 1, side - 1), (f - i0).astype(np.float32)

    def tap(iy, ix):  # square coordinates -> (out_h, out_w, 3) float32
        yy, xx = iy - y_off, ix - x_off
        inside = ((yy >= 0) & (yy < h))[:, None] & ((xx >= 0) & (xx < w))[None, :]
        px = src[np.clip(yy, 0, h - 1)][:, np.clip(xx, 0, w - 1)].astype(np.float32)
        return np.where(inside[..., None], px, np.float32(128))

    y0, y1, wy = axis(out_h)
    x0, x1, wx = axis(out_w)
    wx, wy = wx[None, :, None], wy[:, None, None]
    top = tap(y0, x0) * (1 - wx) + tap(y0, x1) * wx
    bot = tap(y1, x0) * (1 - wx) + tap(y1, x1) * wx
    return np.clip(np.rint(top * (1 - wy) + bot * wy), 0, 255).astype(np.uint8)


def preprocess_image(data: bytes, out_h: int, out_w: int):
    """Encoded png/jpeg/webp bytes -> the (out_h, out_w, 3) uint8 RGB array the vision encoder takes."""
    import numpy as np
    from PIL import Image, ImageOps, UnidentifiedImageError

    try:
        img = Image.open(io.BytesIO(data), formats=IMAGE_FORMATS)
        if img.width * img.height > MAX_IMAGE_PIXELS:
            raise ApiError(400, f"image larger than {MAX_IMAGE_PIXELS // 1_000_000} megapixels")
        img = ImageOps.exif_transpose(img).convert("RGB")  # cv2.imread applies the EXIF orientation and drops alpha
    except (UnidentifiedImageError, OSError, ValueError) as e:
        raise ApiError(400, "could not decode the image: png, jpeg and webp are supported") from e
    return _resize_square(np.asarray(img), out_h, out_w)


class RknnEncoder:
    """The vision-encoder .rknn on librknnrt.so, driven like the vendor demo's init_imgenc / run_imgenc."""

    def __init__(self, lib_path: str, model_path: str):
        lib = ctypes.CDLL(lib_path)
        lib.rknn_init.restype = ctypes.c_int
        lib.rknn_init.argtypes = [ctypes.POINTER(ctypes.c_uint64), ctypes.c_char_p, ctypes.c_uint32, ctypes.c_uint32,
                                  ctypes.c_void_p]
        lib.rknn_query.restype = ctypes.c_int
        lib.rknn_query.argtypes = [ctypes.c_uint64, ctypes.c_int, ctypes.c_void_p, ctypes.c_uint32]
        lib.rknn_set_core_mask.restype = ctypes.c_int
        lib.rknn_set_core_mask.argtypes = [ctypes.c_uint64, ctypes.c_int]
        lib.rknn_inputs_set.restype = ctypes.c_int
        lib.rknn_inputs_set.argtypes = [ctypes.c_uint64, ctypes.c_uint32, ctypes.POINTER(rknn_input)]
        lib.rknn_run.restype = ctypes.c_int
        lib.rknn_run.argtypes = [ctypes.c_uint64, ctypes.c_void_p]
        lib.rknn_outputs_get.restype = ctypes.c_int
        lib.rknn_outputs_get.argtypes = [ctypes.c_uint64, ctypes.c_uint32, ctypes.POINTER(rknn_output), ctypes.c_void_p]
        lib.rknn_outputs_release.restype = ctypes.c_int
        lib.rknn_outputs_release.argtypes = [ctypes.c_uint64, ctypes.c_uint32, ctypes.POINTER(rknn_output)]
        lib.rknn_destroy.restype = ctypes.c_int
        lib.rknn_destroy.argtypes = [ctypes.c_uint64]
        self._lib = lib
        ctx = ctypes.c_uint64()
        rc = lib.rknn_init(ctypes.byref(ctx), model_path.encode(), 0, 0, None)  # size 0 = model_path is a file path
        if rc != 0:
            raise RuntimeError(f"rknn_init failed (rc={rc}) for {model_path}")
        self._ctx: int | None = ctx.value
        try:
            rc = lib.rknn_set_core_mask(self._ctx, RKNN_NPU_CORE_0_1_2)
            if rc != 0:
                raise RuntimeError(f"rknn_set_core_mask failed (rc={rc})")
            io_num = rknn_input_output_num()
            rc = lib.rknn_query(self._ctx, RKNN_QUERY_IN_OUT_NUM, ctypes.byref(io_num), ctypes.sizeof(io_num))
            if rc != 0 or io_num.n_input != 1 or io_num.n_output < 1:
                raise RuntimeError(f"unexpected encoder I/O (rc={rc}, inputs={io_num.n_input}, outputs={io_num.n_output})")
            attrs = []
            for cmd, n in ((RKNN_QUERY_INPUT_ATTR, io_num.n_input), (RKNN_QUERY_OUTPUT_ATTR, io_num.n_output)):
                row = []
                for i in range(n):
                    a = rknn_tensor_attr()
                    a.index = i
                    if lib.rknn_query(self._ctx, cmd, ctypes.byref(a), ctypes.sizeof(a)) != 0:
                        raise RuntimeError("rknn_query of a tensor attribute failed")
                    row.append(a)
                attrs.append(row)
        except Exception:
            self.close()
            raise
        inp, outs = attrs[0][0], attrs[1]
        if inp.fmt == RKNN_TENSOR_NCHW:
            self.height, self.width = inp.dims[2], inp.dims[3]
        else:
            self.height, self.width = inp.dims[1], inp.dims[2]
        # First dimension above 1 is the token count, the one after it the embedding size (the demo's loop).
        self.tokens = self.embed = 0
        for i in range(4):
            if outs[0].dims[i] > 1:
                self.tokens, self.embed = outs[0].dims[i], outs[0].dims[i + 1]
                break
        self.n_output = io_num.n_output  # >1 = deepstack outputs, interleaved per token as the demo does
        if not (self.height and self.width and self.tokens and self.embed):
            self.close()
            raise RuntimeError("could not read the encoder's input size and output shape")

    def encode(self, data: bytes):
        """Encoded image bytes -> flat float32 embedding, tokens x n_output x embed values."""
        import numpy as np

        pix = np.ascontiguousarray(preprocess_image(data, self.height, self.width))
        inp = rknn_input()
        inp.index, inp.buf, inp.size = 0, pix.ctypes.data, pix.nbytes
        inp.type, inp.fmt = RKNN_TENSOR_UINT8, RKNN_TENSOR_NHWC  # the runtime converts to the model's own fp16 input
        if self._lib.rknn_inputs_set(self._ctx, 1, ctypes.byref(inp)) != 0 or self._lib.rknn_run(self._ctx, None) != 0:
            raise RuntimeError("the vision encoder failed to run")
        outs = (rknn_output * self.n_output)()
        for o in outs:
            o.want_float = 1
        if self._lib.rknn_outputs_get(self._ctx, self.n_output, outs, None) != 0:
            raise RuntimeError("the vision encoder returned no output")
        try:
            parts = [np.ctypeslib.as_array(ctypes.cast(o.buf, ctypes.POINTER(ctypes.c_float)),
                                           shape=(o.size // 4,)).copy() for o in outs]
        finally:
            self._lib.rknn_outputs_release(self._ctx, self.n_output, outs)
        if self.n_output == 1:
            emb = parts[0]
        else:
            emb = np.stack([p.reshape(self.tokens, self.embed) for p in parts], axis=1).reshape(-1)
        if emb.size != self.tokens * self.embed * self.n_output:
            raise RuntimeError(f"encoder output has {emb.size} values, expected {self.tokens * self.embed * self.n_output}")
        return emb

    def close(self) -> None:
        ctx, self._ctx = self._ctx, None
        if ctx is not None:
            self._lib.rknn_destroy(ctx)


# ---------------------------------------------------------------- requests

class ApiError(Exception):
    """A request the server answers with an OpenAI-style error body."""

    def __init__(self, status: int, message: str):
        super().__init__(message)
        self.status, self.message = status, message

    def body(self) -> dict:
        kind = "server_error" if self.status >= 500 else "invalid_request_error"
        return {"error": {"message": self.message, "type": kind, "param": None, "code": None}}


def _flatten(content, images: list, allow_images: bool) -> str:
    """One message's content as text. Each image part becomes the <image> tag the runtime expands; its URL joins `images`."""
    if content is None:
        return ""
    if isinstance(content, str):
        return content
    if not isinstance(content, list):
        raise ApiError(400, "message content must be a string or a list of parts")
    out = []
    for part in content:
        kind = part.get("type") if isinstance(part, dict) else None
        if kind == "text" and isinstance(part.get("text"), str):
            out.append(part["text"])
        elif kind == "image_url":
            if not allow_images:
                raise ApiError(400, "images are only accepted in user messages")
            ref = part.get("image_url")
            url = ref.get("url") if isinstance(ref, dict) else ref
            if not isinstance(url, str):
                raise ApiError(400, "image_url needs a url")
            images.append(url)
            out.append(IMAGE_PLACEHOLDER)
        else:
            raise ApiError(400, f"unsupported content part {kind!r}: only text and image_url parts are accepted")
    return "".join(out)


def render_prompt(messages, thinking: bool) -> tuple[str, list]:
    """Qwen3.5 ChatML for OpenAI messages, following the model's chat_template.jinja (its tool branches aside).

    Returns the prompt and the image URLs, one per <image> tag in it.
    """
    if not isinstance(messages, list) or not messages or not all(isinstance(m, dict) for m in messages):
        raise ApiError(400, "messages must be a non-empty list of objects")
    last_user = max((i for i, m in enumerate(messages) if m.get("role") == "user"), default=-1)
    if last_user < 0:
        raise ApiError(400, "no user message found")
    images: list = []
    out = []
    for i, m in enumerate(messages):
        role = "system" if m.get("role") == "developer" else m.get("role")
        if role not in ("system", "user", "assistant"):
            raise ApiError(400, f"unsupported message role {m.get('role')!r}: use system, user or assistant")
        if m.get("tool_calls"):
            raise ApiError(400, "tool calls are not supported by this seat")
        content = _flatten(m.get("content"), images, role == "user").strip()
        if role == "system":
            if i:
                raise ApiError(400, "a system message must be the first message")
            out.append(f"<|im_start|>system\n{content}<|im_end|>\n")
        elif role == "user":
            out.append(f"<|im_start|>user\n{content}<|im_end|>\n")
        else:
            reasoning = m.get("reasoning_content")
            if not isinstance(reasoning, str):
                reasoning = ""
                if "</think>" in content:
                    reasoning = content.split("</think>")[0].rstrip("\n").split("<think>")[-1].lstrip("\n")
                    content = content.split("</think>")[-1].lstrip("\n")
            if i > last_user:  # only an assistant turn after the last user turn keeps its reasoning
                out.append(f"<|im_start|>assistant\n<think>\n{reasoning.strip()}\n</think>\n\n{content}<|im_end|>\n")
            else:
                out.append(f"<|im_start|>assistant\n{content}<|im_end|>\n")
    out.append("<|im_start|>assistant\n" + (THINK_ON if thinking else THINK_OFF))
    return "".join(out), images


def decode_data_uri(url: str) -> bytes:
    """Bytes of a base64 png/jpeg/webp data URI. The server never fetches a URL."""
    head, comma, payload = url.partition(",")
    params = head[5:].split(";") if head.startswith("data:") else []
    if not comma or not params or params[0].lower() not in IMAGE_MIMES or "base64" not in [p.lower() for p in params[1:]]:
        raise ApiError(400, "image_url must be a base64 data: URI of a png, jpeg or webp image; the server does not fetch URLs")
    try:
        return base64.b64decode("".join(payload.split()), validate=True)  # MIME-wrapped lines are fine, stray bytes are not
    except ValueError as e:
        raise ApiError(400, "image_url data is not valid base64") from e


class ChatRequest(NamedTuple):
    prompt: str
    images: list  # decoded image bytes, one per <image> tag in the prompt
    sampling: Sampling
    max_tokens: int
    stops: list
    stream: bool
    include_usage: bool
    thinking: bool


def _number(body: dict, keys: tuple, default: float, lo: float, hi: float) -> float:
    for key in keys:
        v = body.get(key)
        if v is None:
            continue
        if isinstance(v, bool) or not isinstance(v, (int, float)) or not lo <= v <= hi:
            raise ApiError(400, f"{key} must be a number between {lo:g} and {hi:g}")
        return float(v)
    return default


def _integer(body: dict, key: str, default: int) -> int:
    v = body.get(key)
    if v is None:
        return default
    if isinstance(v, float) and v.is_integer():  # a client that serialises 40 as 40.0
        v = int(v)
    if isinstance(v, bool) or not isinstance(v, int):
        raise ApiError(400, f"{key} must be an integer")
    return v


def build_request(body: dict, ctx_size: int, has_vision: bool) -> ChatRequest:
    """Validate an /v1/chat/completions body. Fields the seat cannot honour but may ignore are simply not read."""
    if body.get("tools"):
        raise ApiError(400, "tool calling is not supported by this seat")
    kwargs = body.get("chat_template_kwargs")
    thinking = isinstance(kwargs, dict) and kwargs.get("enable_thinking") is True
    prompt, urls = render_prompt(body.get("messages"), thinking)
    if urls and not has_vision:
        raise ApiError(400, "this seat has no vision encoder: start it with --vision-encoder to send images")
    images = [decode_data_uri(u) for u in urls]
    if urls and prompt.count(IMAGE_PLACEHOLDER) != len(urls):
        # the runtime pairs each <image> in the prompt with an embedding, so a stray literal would read past the buffer
        raise ApiError(400, f'the text "{IMAGE_PLACEHOLDER}" is reserved for attached images')
    max_tokens = _integer(body, "max_tokens", _integer(body, "max_completion_tokens", -1))
    if max_tokens != -1 and max_tokens < 1:
        raise ApiError(400, "max_tokens must be a positive integer")
    temperature = _number(body, ("temperature",), DEFAULT_TEMPERATURE, 0, 5)
    top_p = _number(body, ("top_p",), DEFAULT_TOP_P, 0, 1)
    top_k = _integer(body, "top_k", DEFAULT_TOP_K)
    if top_k <= 0:  # llama.cpp's 0 means "off", which the runtime has no value for: use the default
        top_k = DEFAULT_TOP_K
    if temperature == 0 or top_p == 0:  # greedy: top_k 1 is deterministic whatever the runtime does with temperature 0
        top_k, top_p, temperature = 1, 1.0, 1.0
    sampling = Sampling(top_k, top_p, temperature,
                        _number(body, ("repeat_penalty", "repetition_penalty"), 1.0, 0.01, 10),
                        _number(body, ("frequency_penalty",), 0.0, -2, 2), _number(body, ("presence_penalty",), 0.0, -2, 2))
    stop = body.get("stop")
    stops = [stop] if isinstance(stop, str) else [] if stop is None else stop
    if not isinstance(stops, list) or not all(isinstance(s, str) for s in stops):
        raise ApiError(400, "stop must be a string or a list of strings")
    options = body.get("stream_options")
    return ChatRequest(prompt, images, sampling, ctx_size if max_tokens == -1 else min(max_tokens, ctx_size),
                       [s for s in stops if s], body.get("stream") is True,
                       isinstance(options, dict) and options.get("include_usage") is True, thinking)


def _partial_suffix(text: str, needle: str) -> int:
    """Length of the longest proper prefix of `needle` that `text` ends with."""
    return next((k for k in range(min(len(needle) - 1, len(text)), 0, -1) if text.endswith(needle[:k])), 0)


class Emitter:
    """Generated text -> (field, text) pieces for the client.

    A stop string cuts the text where it starts; a tail that could still grow into one is held back, so a partial stop
    string never reaches the client. With thinking on, the runtime's output begins inside the think block (the prompt
    ended with <think>), so everything before </think> is `reasoning_content` and the rest, minus its leading newlines
    (the template's lstrip), is `content`.
    """

    def __init__(self, stops: list, thinking: bool):
        self.stops = stops
        self.stopped = False  # a stop string was found: the caller aborts the run
        self._hold = ""  # text withheld until it can no longer become a stop string
        self._think = thinking  # still inside the think block
        self._tag = ""  # tail that may be the start of </think>
        self._lstrip = False  # just left the think block: drop the newlines that follow

    def feed(self, text: str) -> list:
        buf = self._hold + text
        cut = min((i for i in map(buf.find, self.stops) if i >= 0), default=-1)
        if cut >= 0:
            self.stopped, self._hold = True, ""
            return self._route(buf[:cut])
        keep = max((_partial_suffix(buf, s) for s in self.stops), default=0)
        self._hold = buf[len(buf) - keep:] if keep else ""
        return self._route(buf[:len(buf) - keep])

    def flush(self) -> list:
        """The end of the run: whatever was held back was never a stop string."""
        text, self._hold = self._hold, ""
        pieces = self._route(text)
        if self._tag:
            pieces.append(("reasoning_content", self._tag))
            self._tag = ""
        return pieces

    def _route(self, text: str) -> list:
        out = []
        while text:
            if self._think:
                buf = self._tag + text
                at = buf.find("</think>")
                if at < 0:
                    keep = _partial_suffix(buf, "</think>")
                    self._tag = buf[len(buf) - keep:] if keep else ""
                    if len(buf) > keep:
                        out.append(("reasoning_content", buf[:len(buf) - keep]))
                    return out
                self._think, self._tag, self._lstrip = False, "", True
                if at:
                    out.append(("reasoning_content", buf[:at]))
                text = buf[at + len("</think>"):]
                continue
            if self._lstrip:
                text = text.lstrip("\n")
                if not text:
                    return out
                self._lstrip = False
            out.append(("content", text))
            return out
        return out


def _usage(run: Run) -> dict:
    prompt = run.perf.prefill_tokens if run.perf else 0
    return {"prompt_tokens": prompt, "completion_tokens": run.tokens, "total_tokens": prompt + run.tokens}


def _timings(perf: Perf | None) -> dict:
    """llama.cpp's `timings` from RKLLMPerfStat. predicted_* is the decode stage (n - 1 tokens, see the docstring)."""
    p = perf or Perf(0.0, 0, 0.0, 0, 0.0)
    out = {"prompt_n": p.prefill_tokens, "prompt_ms": p.prefill_ms, "predicted_n": p.generate_tokens,
           "predicted_ms": p.generate_ms}
    if p.prefill_ms > 0:
        out["prompt_per_second"] = round(p.prefill_tokens * 1000.0 / p.prefill_ms, 2)
    if p.generate_ms > 0:
        out["predicted_per_second"] = round(p.generate_tokens * 1000.0 / p.generate_ms, 2)
    return out


# ---------------------------------------------------------------- http

def _client_gone(conn: socket.socket) -> bool:
    """True once the peer has closed its end. The request body is fully read, so a readable socket means EOF."""
    try:
        readable, _, _ = select.select([conn], [], [], 0)
        return bool(readable) and conn.recv(1, socket.MSG_PEEK) == b""
    except OSError:
        return True


class _Buffer:
    """Non-streaming answer: collect the pieces, send one JSON body at the end."""

    def __init__(self, handler: Handler, model: str):
        self.h, self.model = handler, model
        self.parts = {"content": "", "reasoning_content": ""}

    def put(self, pieces: list) -> None:
        for field, text in pieces:
            self.parts[field] += text

    def finish(self, reason: str, usage: dict, timings: dict, _include_usage: bool) -> None:
        message = {"role": "assistant", "content": self.parts["content"]}
        if self.parts["reasoning_content"]:
            message["reasoning_content"] = self.parts["reasoning_content"]
        self.h.send_json(200, {"id": "chatcmpl-" + uuid.uuid4().hex[:24], "object": "chat.completion",
                               "created": int(time.time()), "model": self.model,
                               "choices": [{"index": 0, "message": message, "finish_reason": reason}],
                               "usage": usage, "timings": timings})

    def fail(self, message: str, status: int = 500) -> None:
        raise ApiError(status, message)


class _Stream:
    """Server-sent events. The response starts at the first piece, so a run the runtime refuses still gets a JSON error."""

    def __init__(self, handler: Handler, model: str):
        self.h, self.model = handler, model
        self.id = "chatcmpl-" + uuid.uuid4().hex[:24]
        self.created = int(time.time())
        self.started = False

    def _frame(self, obj: dict) -> None:
        self.h.wfile.write(b"data: " + json.dumps(obj).encode("utf-8") + b"\n\n")

    def _chunk(self, delta: dict, finish: str | None, **extra) -> None:
        self._frame({"id": self.id, "object": "chat.completion.chunk", "created": self.created, "model": self.model,
                     "choices": [{"index": 0, "delta": delta, "finish_reason": finish}], **extra})

    def start(self) -> None:
        if not self.started:
            self.h.send_response(200)
            self.h.send_header("Content-Type", "text/event-stream")
            self.h.send_header("Cache-Control", "no-cache")
            self.h.end_headers()
            self.h.responded = self.started = True
            self._chunk({"role": "assistant", "content": ""}, None)

    def put(self, pieces: list) -> None:
        for field, text in pieces:
            self.start()
            self._chunk({field: text}, None)

    def finish(self, reason: str, usage: dict, timings: dict, include_usage: bool) -> None:
        self.start()
        self._chunk({}, reason, timings=timings)
        if include_usage:  # the OpenAI usage frame, with llama.cpp's timings alongside (the harness client reads both)
            self._frame({"id": self.id, "object": "chat.completion.chunk", "created": self.created, "model": self.model,
                         "choices": [], "usage": usage, "timings": timings})
        self.h.wfile.write(b"data: [DONE]\n\n")

    def fail(self, message: str, status: int = 500) -> None:
        if not self.started:
            raise ApiError(status, message)
        self._frame({"error": {"message": message, "type": "server_error"}})
        self.h.wfile.write(b"data: [DONE]\n\n")


class App:
    """State shared by the handler threads."""

    def __init__(self, name: str, ctx_size: int):
        self.name, self.ctx_size = name, ctx_size
        self.runtime = None  # set by the loader once rkllm_init returns
        self.vision = None
        self.ready = threading.Event()
        self.closing = False
        self.gen_lock = threading.Lock()  # the NPU is one device: one generation at a time, the rest queue here

    def shutdown(self, wait_sec: float = 15.0) -> None:
        """Abort a running generation, let it reach its FINISH event, then release the NPU."""
        self.closing = True
        if self.runtime is not None:
            self.runtime.abort()
            self.gen_lock.acquire(timeout=wait_sec)
            self.runtime.close()
        if self.vision is not None:
            self.vision.close()


class Handler(BaseHTTPRequestHandler):
    server_version = "rkllm-server/1"
    timeout = 120  # a peer that stalls mid-request, or stops reading a stream, is dropped instead of holding the NPU
    responded = False

    def setup(self) -> None:
        super().setup()
        # SSE frames are tiny and one token apart; without this Nagle can hold each behind the previous frame's ACK.
        self.connection.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)

    def log_message(self, fmt, *args) -> None:  # /health is polled every second while loading: no per-request noise
        pass

    def send_json(self, code: int, obj: dict) -> None:
        body = json.dumps(obj).encode("utf-8")
        self.responded = True
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:
        app: App = self.server.app
        path = urlparse(self.path).path
        if path == "/health":
            ready = app.ready.is_set()
            return self.send_json(200 if ready else 503, {"status": "ok" if ready else "loading"})
        if path == "/v1/models":
            return self.send_json(200, {"object": "list", "data": [{
                "id": app.name, "object": "model", "created": 0, "owned_by": "rkllm", "max_model_len": app.ctx_size}]})
        self.send_json(404, ApiError(404, "not found").body())

    def do_POST(self) -> None:
        if urlparse(self.path).path != "/v1/chat/completions":
            return self.send_json(404, ApiError(404, "not found").body())
        try:
            self._chat(self.server.app)
        except ApiError as e:
            self.send_json(e.status, e.body())
        except Exception as e:  # noqa: BLE001 - the 500 guard: an internal failure never puts a stack trace on the wire
            print(f"rkllm server: request failed: {type(e).__name__}: {e}", file=sys.stderr, flush=True)
            if not self.responded:
                self.send_json(500, ApiError(500, f"{type(e).__name__}: {e}").body())

    def _read_json(self) -> dict:
        try:
            n = int(self.headers.get("Content-Length") or 0)
        except ValueError:
            n = 0
        if n <= 0:
            raise ApiError(400, "a JSON request body with a Content-Length is required")
        if n > MAX_BODY:
            raise ApiError(413, f"request body over {MAX_BODY >> 20} MiB")
        try:
            body = json.loads(self.rfile.read(n))
        except (TimeoutError, socket.timeout) as e:
            raise ApiError(408, "timed out reading the request body") from e
        except ValueError as e:
            raise ApiError(400, "request body is not valid JSON") from e
        if not isinstance(body, dict):
            raise ApiError(400, "request body must be a JSON object")
        return body

    def _acquire(self, app: App) -> bool:
        """Queue for the NPU. False when the client hung up (or the server is closing) while it waited."""
        while not app.gen_lock.acquire(timeout=0.25):
            if app.closing or _client_gone(self.connection):
                return False
        if app.closing:
            app.gen_lock.release()
            return False
        return True

    def _chat(self, app: App) -> None:
        if not app.ready.is_set() or app.closing:
            raise ApiError(503, "the model is loading" if not app.closing else "the server is shutting down")
        req = build_request(self._read_json(), app.ctx_size, app.vision is not None)
        if not self._acquire(app):
            return
        try:
            self._generate(app, req)
        finally:
            app.gen_lock.release()

    def _generate(self, app: App, req: ChatRequest) -> None:
        started = time.monotonic()
        images = None
        if req.images:
            embeds = [app.vision.encode(data) for data in req.images]
            images = ImageBatch(embeds, app.vision.tokens, app.vision.width, app.vision.height)
        emit = Emitter(req.stops, req.thinking)
        out = (_Stream if req.stream else _Buffer)(self, app.name)
        gone = []

        def on_text(text: str) -> bool:
            try:
                if _client_gone(self.connection):
                    raise ConnectionError("client closed the connection")
                out.put(emit.feed(text))
            except OSError:
                gone.append(True)
                return False
            return not emit.stopped

        run = app.runtime.generate(req.prompt, images, req.sampling, req.max_tokens, on_text)
        if gone:
            return  # nobody left to answer; the run was aborted
        if app.closing:  # SIGTERM aborted this run: an answer cut short must not read as a finished one
            return out.fail("the server is shutting down; the answer was cut short", 503)
        if run.error or run.rc != 0:
            if run.rc != 0 and not run.tokens and not run.error:
                raise ApiError(400, f"the runtime refused the request (rkllm_run rc={run.rc}); usually the prompt is "
                                    f"longer than the {app.ctx_size}-token window")
            return out.fail(run.error or f"rkllm_run failed (rc={run.rc})")
        # Token count == the cap means the cap ended the run, unless the last event was EOS (a special token: no text).
        reason = "length" if run.tokens >= req.max_tokens and not run.eos and not emit.stopped else "stop"
        usage, timings = _usage(run), _timings(run.perf)
        try:
            out.put(emit.flush())
            out.finish(reason, usage, timings, req.include_usage)
        except OSError:
            return  # the client left before the last frame
        wall = time.monotonic() - started
        print(f"rkllm server: chat stream={req.stream} prompt_n={usage['prompt_tokens']} "
              f"predicted_n={usage['completion_tokens']} pp={timings.get('prompt_per_second', 0)} tok/s "
              f"tg={timings.get('predicted_per_second', 0)} tok/s wall={wall:.2f}s finish={reason}",
              file=sys.stderr, flush=True)


class _Server(ThreadingHTTPServer):
    """ThreadingHTTPServer that carries the App and copes with an IPv6 loopback."""

    def __init__(self, addr: tuple, handler, app: App):
        self.app = app
        if ":" in addr[0]:
            self.address_family = socket.AF_INET6
        super().__init__(addr, handler)

    def handle_error(self, request, client_address) -> None:
        if not isinstance(sys.exc_info()[1], (ConnectionError, TimeoutError, socket.timeout)):  # a peer that left is no error
            super().handle_error(request, client_address)


def make_server(app: App, host: str, port: int) -> ThreadingHTTPServer:
    return _Server((host, port), Handler, app)


def _is_loopback(host: str) -> bool:
    if host == "localhost":
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


def _pin_to_cpus(mask: int) -> None:
    """Confine this process's threads to the CPUs of the seat's mask. Call before any thread starts: threads inherit."""
    if not hasattr(os, "sched_setaffinity"):  # not Linux
        return
    cpus = {i for i in range(mask.bit_length()) if mask >> i & 1}
    try:
        os.sched_setaffinity(0, cpus)
    except OSError as e:  # a mask naming CPUs this machine lacks: the runtime will say so at init
        print(f"rkllm server: could not pin to CPUs {sorted(cpus)}: {e}", file=sys.stderr)


def main(argv: list | None = None) -> int:
    home = os.environ.get("RKNPU_HOME", "")
    lib_dir = os.path.join(home, "lib") if home else ""
    ap = argparse.ArgumentParser(prog="rkllm_server", description="OpenAI-compatible server for one RKLLM NPU seat")
    ap.add_argument("--model", help="the .rkllm file")
    ap.add_argument("--vision-encoder", help="the vision encoder .rknn: enables image_url parts")
    ap.add_argument("--ctx-size", type=int, default=DEFAULT_CTX, help="context window in tokens (default %(default)s)")
    ap.add_argument("--cpu-mask", default="0x0f", help="hex mask of the CPUs the runtime uses (default %(default)s: A55 cluster)")
    ap.add_argument("--served-name", help="model id reported by /v1/models (default: the model file's name)")
    ap.add_argument("--port", type=int, help="port to listen on")
    ap.add_argument("--host", default="127.0.0.1", help="loopback address to bind (default %(default)s)")
    ap.add_argument("--lib", default=os.path.join(lib_dir, "librkllmrt.so") if lib_dir else "librkllmrt.so",
                    help="the RKLLM runtime (default $RKNPU_HOME/lib/librkllmrt.so, else found on LD_LIBRARY_PATH)")
    ap.add_argument("--rknn-lib", default=os.path.join(lib_dir, "librknnrt.so") if lib_dir else "librknnrt.so",
                    help="the RKNN runtime for the vision encoder (default $RKNPU_HOME/lib/librknnrt.so)")
    ap.add_argument("--print-layout", action="store_true", help="print the ctypes struct sizes/offsets and exit")
    args = ap.parse_args(argv)
    if args.print_layout:
        print("\n".join(layout_report()))
        return 0
    if not args.model or args.port is None:
        ap.error("--model and --port are required")
    if not _is_loopback(args.host):
        print(f"rkllm server: refusing to bind non-loopback address {args.host!r}", file=sys.stderr)
        return 2
    try:
        cpu_mask = int(args.cpu_mask, 16)
    except ValueError:
        cpu_mask = 0
    if not 0 < cpu_mask <= 0xFFFFFFFF:
        ap.error("--cpu-mask must be a non-zero hex mask such as 0x0f")
    if args.ctx_size < 64:
        ap.error("--ctx-size must be at least 64")
    for path in (args.model, args.vision_encoder):
        if path and not os.path.isfile(path):
            print(f"rkllm server: no such file: {path}", file=sys.stderr)
            return 2

    _pin_to_cpus(cpu_mask)
    app = App(args.served_name or os.path.splitext(os.path.basename(args.model))[0], args.ctx_size)
    try:
        srv = make_server(app, args.host, args.port)
    except OSError as e:
        print(f"rkllm server: cannot listen on {args.host}:{args.port}: {e}", file=sys.stderr)
        return 2
    failure: list = []

    def load() -> None:
        t0 = time.monotonic()
        try:
            app.runtime = RKLLMRuntime(args.lib, args.model, args.ctx_size, cpu_mask)
            if args.vision_encoder:
                app.vision = RknnEncoder(args.rknn_lib, args.vision_encoder)
        except Exception as e:  # noqa: BLE001 - a seat that cannot load must exit, not answer 503 forever
            failure.append(e)
            threading.Thread(target=srv.shutdown, daemon=True).start()
            return
        app.ready.set()
        print(f"rkllm server: ready model={app.name} ctx={args.ctx_size} cpu_mask={cpu_mask:#04x} "
              f"vision={'yes' if app.vision else 'no'} loaded in {time.monotonic() - t0:.1f}s", file=sys.stderr, flush=True)

    def stop(_signum, _frame) -> None:  # serve_forever() runs on this thread: shutdown() must be called from another
        threading.Thread(target=srv.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    threading.Thread(target=load, name="rkllm-load", daemon=True).start()
    print(f"rkllm server: listening on http://{args.host}:{srv.server_address[1]} (loading {args.model})",
          file=sys.stderr, flush=True)
    try:
        srv.serve_forever()
    finally:
        srv.server_close()
        app.shutdown()
    if failure:
        print(f"rkllm server: could not load the model: {failure[0]}", file=sys.stderr)
        return 3
    return 0


if __name__ == "__main__":
    sys.exit(main())
