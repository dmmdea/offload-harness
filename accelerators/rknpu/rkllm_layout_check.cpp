// Prints sizeof / offsetof of every C struct rkllm_server.py mirrors with ctypes, from the vendor headers.
// The output is line-for-line what `python rkllm_server.py --print-layout` prints, so the two can be diffed:
//
//   g++ -std=c++17 -I <dir with rkllm.h and rknn_api.h> -o rkllm_layout_check rkllm_layout_check.cpp
//   diff <(./rkllm_layout_check) <(python rkllm_server.py --print-layout)
//
// rkllm_layout_aarch64.txt is this program's output on an aarch64 board; test_rkllm_server.py compares the ctypes
// layout with it on every run. Re-run the diff, and refresh that file (./rkllm_layout_check > rkllm_layout_aarch64.txt),
// whenever librkllmrt.so or librknnrt.so is upgraded: the runtime reads these structs by raw offset, so a shifted
// field is a crash or a silent wrong sampler, never an error message.
#include <cstddef>  // rkllm.h uses size_t without including it
#include <cstdio>
#include "rkllm.h"
#include "rknn_api.h"

#define SIZE(t) std::printf("%s sizeof %zu\n", #t, sizeof(t))
#define OFF(t, m) std::printf("%s.%s offset %zu\n", #t, #m, offsetof(t, m))

int main() {
    SIZE(RKLLMExtendParam);
    OFF(RKLLMExtendParam, base_domain_id);
    OFF(RKLLMExtendParam, embed_flash);
    OFF(RKLLMExtendParam, enabled_cpus_num);
    OFF(RKLLMExtendParam, enabled_cpus_mask);
    OFF(RKLLMExtendParam, n_batch);
    OFF(RKLLMExtendParam, use_cross_attn);
    OFF(RKLLMExtendParam, reserved);
    SIZE(RKLLMParam);
    OFF(RKLLMParam, model_path);
    OFF(RKLLMParam, max_context_len);
    OFF(RKLLMParam, max_new_tokens);
    OFF(RKLLMParam, top_k);
    OFF(RKLLMParam, n_keep);
    OFF(RKLLMParam, top_p);
    OFF(RKLLMParam, temperature);
    OFF(RKLLMParam, repeat_penalty);
    OFF(RKLLMParam, frequency_penalty);
    OFF(RKLLMParam, presence_penalty);
    OFF(RKLLMParam, mirostat);
    OFF(RKLLMParam, mirostat_tau);
    OFF(RKLLMParam, mirostat_eta);
    OFF(RKLLMParam, skip_special_token);
    OFF(RKLLMParam, ignore_eos_token);
    OFF(RKLLMParam, is_async);
    OFF(RKLLMParam, extend_param);
    SIZE(RKLLMEmbedInput);
    OFF(RKLLMEmbedInput, embed);
    OFF(RKLLMEmbedInput, n_tokens);
    SIZE(RKLLMTokenInput);
    OFF(RKLLMTokenInput, input_ids);
    OFF(RKLLMTokenInput, n_tokens);
    SIZE(RKLLMMultiModalInput);
    OFF(RKLLMMultiModalInput, prompt);
    OFF(RKLLMMultiModalInput, image);
    OFF(RKLLMMultiModalInput, image.image_embed);
    OFF(RKLLMMultiModalInput, image.n_image_tokens);
    OFF(RKLLMMultiModalInput, image.n_image);
    OFF(RKLLMMultiModalInput, image.image_start);
    OFF(RKLLMMultiModalInput, image.image_end);
    OFF(RKLLMMultiModalInput, image.image_content);
    OFF(RKLLMMultiModalInput, image.image_width);
    OFF(RKLLMMultiModalInput, image.image_height);
    OFF(RKLLMMultiModalInput, video);
    OFF(RKLLMMultiModalInput, video.video_embed);
    OFF(RKLLMMultiModalInput, video.n_frame_tokens);
    OFF(RKLLMMultiModalInput, video.n_frame_per_video);
    OFF(RKLLMMultiModalInput, video.n_video);
    OFF(RKLLMMultiModalInput, video.video_start);
    OFF(RKLLMMultiModalInput, video.video_end);
    OFF(RKLLMMultiModalInput, video.video_content);
    OFF(RKLLMMultiModalInput, video.frame_width);
    OFF(RKLLMMultiModalInput, video.frame_height);
    OFF(RKLLMMultiModalInput, audio);
    OFF(RKLLMMultiModalInput, audio.audio_embed);
    OFF(RKLLMMultiModalInput, audio.n_audio_tokens);
    OFF(RKLLMMultiModalInput, audio.n_audio);
    OFF(RKLLMMultiModalInput, audio.audio_start);
    OFF(RKLLMMultiModalInput, audio.audio_end);
    OFF(RKLLMMultiModalInput, audio.audio_content);
    SIZE(RKLLMInput);
    OFF(RKLLMInput, role);
    OFF(RKLLMInput, enable_thinking);
    OFF(RKLLMInput, input_type);
    OFF(RKLLMInput, prompt_input);
    OFF(RKLLMInput, embed_input);
    OFF(RKLLMInput, token_input);
    OFF(RKLLMInput, multimodal_input);
    SIZE(RKLLMLoraParam);
    OFF(RKLLMLoraParam, lora_adapter_name);
    SIZE(RKLLMPromptCacheParam);
    OFF(RKLLMPromptCacheParam, save_prompt_cache);
    OFF(RKLLMPromptCacheParam, prompt_cache_path);
    SIZE(RKLLMSamplingParam);
    OFF(RKLLMSamplingParam, top_k);
    OFF(RKLLMSamplingParam, top_p);
    OFF(RKLLMSamplingParam, temperature);
    OFF(RKLLMSamplingParam, repeat_penalty);
    OFF(RKLLMSamplingParam, frequency_penalty);
    OFF(RKLLMSamplingParam, presence_penalty);
    OFF(RKLLMSamplingParam, mirostat);
    OFF(RKLLMSamplingParam, mirostat_tau);
    OFF(RKLLMSamplingParam, mirostat_eta);
    SIZE(RKLLMInferParam);
    OFF(RKLLMInferParam, mode);
    OFF(RKLLMInferParam, lora_params);
    OFF(RKLLMInferParam, prompt_cache_params);
    OFF(RKLLMInferParam, sampling_params);
    OFF(RKLLMInferParam, keep_history);
    OFF(RKLLMInferParam, max_new_tokens);
    SIZE(RKLLMResultLastHiddenLayer);
    OFF(RKLLMResultLastHiddenLayer, hidden_states);
    OFF(RKLLMResultLastHiddenLayer, embd_size);
    OFF(RKLLMResultLastHiddenLayer, num_tokens);
    SIZE(RKLLMResultLogits);
    OFF(RKLLMResultLogits, logits);
    OFF(RKLLMResultLogits, vocab_size);
    OFF(RKLLMResultLogits, num_tokens);
    SIZE(RKLLMPerfStat);
    OFF(RKLLMPerfStat, prefill_time_ms);
    OFF(RKLLMPerfStat, prefill_tokens);
    OFF(RKLLMPerfStat, generate_time_ms);
    OFF(RKLLMPerfStat, generate_tokens);
    OFF(RKLLMPerfStat, memory_usage_mb);
    SIZE(RKLLMResult);
    OFF(RKLLMResult, text);
    OFF(RKLLMResult, token_id);
    OFF(RKLLMResult, last_hidden_layer);
    OFF(RKLLMResult, logits);
    OFF(RKLLMResult, perf);
    SIZE(RKLLMCallback);
    OFF(RKLLMCallback, result_callback);
    OFF(RKLLMCallback, result_userdata);
    OFF(RKLLMCallback, tokenizer_callback);
    OFF(RKLLMCallback, tokenizer_userdata);
    OFF(RKLLMCallback, embed_callback);
    OFF(RKLLMCallback, embed_userdata);
    SIZE(rknn_input_output_num);
    OFF(rknn_input_output_num, n_input);
    OFF(rknn_input_output_num, n_output);
    SIZE(rknn_tensor_attr);
    OFF(rknn_tensor_attr, index);
    OFF(rknn_tensor_attr, n_dims);
    OFF(rknn_tensor_attr, dims);
    OFF(rknn_tensor_attr, name);
    OFF(rknn_tensor_attr, n_elems);
    OFF(rknn_tensor_attr, size);
    OFF(rknn_tensor_attr, fmt);
    OFF(rknn_tensor_attr, type);
    OFF(rknn_tensor_attr, qnt_type);
    OFF(rknn_tensor_attr, fl);
    OFF(rknn_tensor_attr, zp);
    OFF(rknn_tensor_attr, scale);
    OFF(rknn_tensor_attr, w_stride);
    OFF(rknn_tensor_attr, size_with_stride);
    OFF(rknn_tensor_attr, pass_through);
    OFF(rknn_tensor_attr, h_stride);
    SIZE(rknn_input);
    OFF(rknn_input, index);
    OFF(rknn_input, buf);
    OFF(rknn_input, size);
    OFF(rknn_input, pass_through);
    OFF(rknn_input, type);
    OFF(rknn_input, fmt);
    SIZE(rknn_output);
    OFF(rknn_output, want_float);
    OFF(rknn_output, is_prealloc);
    OFF(rknn_output, index);
    OFF(rknn_output, buf);
    OFF(rknn_output, size);
    return 0;
}
