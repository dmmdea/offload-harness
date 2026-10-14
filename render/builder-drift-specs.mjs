// render/builder-drift-specs.mjs — which recipe widgets of which `render/wf-*.mjs` builder were copied from
// which ComfyUI workflow template, and where each one lives on both sides. Read by builder-drift.mjs.
//
// An item pairs a TEMPLATE locator with a BUILDER locator:
//   W(sg, id, type, w)            widget index `w` of node `id` (in subgraph `sg`, or the root when sg is null)
//   I(id, sgName, input)          the widget a root subgraph instance promotes, found by the subgraph INPUT's
//                                 name (the instance's widgets_values follow the order of the widget-typed
//                                 subgraph inputs). Use it where the sampler's own widget is linked to the
//                                 instance: the sampler's serialized value is then stale and the instance's is
//                                 the one that runs.
//   D(sg, id, type, a, b)         template-only wiring check: the inputs `a` and `b` of the node are fed by
//                                 DIFFERENT sources (the defect of upstream workflow_templates PR #1308 was one
//                                 subgraph input feeding both the start and the end percent of a node)
//   B(node, cls, input)           input `input` of API node `node`, which must be class `cls`
// Where a template routes a value through a primitive and a switch node, the locator points at the primitive
// the active branch reads (the effective value), not at the sampler's stale widget.
//
// "Recipe-relevant" means a widget that sets how the model samples or conditions: steps, cfg, sampler,
// scheduler, denoise, shift, noise scale, sigma lists, LoRA name and strength, guider cfg, strengths and
// percents, tile and preprocess parameters. Not recipe: seeds, prompts, model and file names (the fleet binds
// those per machine), output names, sizes the caller chooses.
//
// A builder that deliberately differs from its template carries `delta: { kind: "deliberate", reason }`. A
// difference found by the first guard run that nobody has judged yet is `kind: "unreviewed"`: the manifest
// records both digests, the guard stays green, and a further change on either side turns it red. A widget
// that matches needs no declaration.
import { buildAceStep } from "./wf-acestep.mjs";
import { buildH3AV } from "./wf-h3-av.mjs";
import { buildHiDreamO1 } from "./wf-hidream-o1.mjs";
import { buildKrea2 } from "./wf-krea2.mjs";
import { buildLtx25I2V } from "./wf-ltx25-i2v.mjs";
import { buildQwenImage, QWEN_IMAGE_PRESETS } from "./wf-qwen-image.mjs";
import { buildQwenImage21, buildQwenImage21Edit } from "./wf-qwen-image-21.mjs";
import { buildQwenImageEdit, QWEN_EDIT_PRESETS } from "./wf-qwen-image-edit.mjs";
import { buildQwenInpaint, QWEN_INPAINT_PRESETS } from "./wf-qwen-inpaint.mjs";
import { buildWanAnimate2 } from "./wf-wan-animate2.mjs";
import { buildWan22I2V } from "./wf-wan22-i2v.mjs";

const W = (sg, id, type, w) => ({ kind: "w", sg, id, type, w });
const I = (id, sgName, input) => ({ kind: "inst", sg: null, id, type: `subgraph:${sgName}`, input });
const D = (sg, id, type, a, b) => ({ kind: "distinct", sg, id, type, inputs: [a, b] });
const B = (node, cls, input) => ({ node, cls, input });
const item = (key, t, b, delta) => ({ key, t, b, ...(delta ? { delta } : {}) });

// The KSampler family shares five widgets in the same order: seed, control, steps, cfg, sampler, scheduler, denoise.
const ksampler = (sg, id, node, { steps = true, cfg = true } = {}) => [
  ...(steps ? [item("ksampler.steps", W(sg, id, "KSampler", 2), B(node, "KSampler", "steps"))] : []),
  ...(cfg ? [item("ksampler.cfg", W(sg, id, "KSampler", 3), B(node, "KSampler", "cfg"))] : []),
  item("ksampler.sampler_name", W(sg, id, "KSampler", 4), B(node, "KSampler", "sampler_name")),
  item("ksampler.scheduler", W(sg, id, "KSampler", 5), B(node, "KSampler", "scheduler")),
  item("ksampler.denoise", W(sg, id, "KSampler", 6), B(node, "KSampler", "denoise")),
];

const SEED = 1;

// ---------------------------------------------------------------------------------------------
// ACE-Step 1.5 turbo (text to music)
// ---------------------------------------------------------------------------------------------
const acestep = {
  builder: "wf-acestep",
  build: buildAceStep,
  views: [{
    name: "default",
    template: "audio_ace_step1_5_xl_turbo",
    args: { prompt: "tags", seed: SEED },
    items: [
      item("ksampler.steps", W(null, 3, "KSampler", 2), B("8", "KSampler", "steps")),
      item("ksampler.cfg", W(null, 3, "KSampler", 3), B("8", "KSampler", "cfg")),
      item("ksampler.sampler_name", W(null, 3, "KSampler", 4), B("8", "KSampler", "sampler_name")),
      item("ksampler.scheduler", W(null, 3, "KSampler", 5), B("8", "KSampler", "scheduler")),
      item("ksampler.denoise", W(null, 3, "KSampler", 6), B("8", "KSampler", "denoise")),
      item("model_sampling.shift", W(null, 78, "ModelSamplingAuraFlow", 0), B("7", "ModelSamplingAuraFlow", "shift")),
      item("encoder.timesignature", W(null, 94, "TextEncodeAceStepAudio1.5", 6), B("4", "TextEncodeAceStepAudio1.5", "timesignature")),
      item("encoder.language", W(null, 94, "TextEncodeAceStepAudio1.5", 7), B("4", "TextEncodeAceStepAudio1.5", "language")),
      item("encoder.generate_audio_codes", W(null, 94, "TextEncodeAceStepAudio1.5", 9), B("4", "TextEncodeAceStepAudio1.5", "generate_audio_codes")),
      item("encoder.cfg_scale", W(null, 94, "TextEncodeAceStepAudio1.5", 10), B("4", "TextEncodeAceStepAudio1.5", "cfg_scale")),
      item("encoder.temperature", W(null, 94, "TextEncodeAceStepAudio1.5", 11), B("4", "TextEncodeAceStepAudio1.5", "temperature")),
      item("encoder.top_p", W(null, 94, "TextEncodeAceStepAudio1.5", 12), B("4", "TextEncodeAceStepAudio1.5", "top_p")),
      item("encoder.top_k", W(null, 94, "TextEncodeAceStepAudio1.5", 13), B("4", "TextEncodeAceStepAudio1.5", "top_k")),
      item("encoder.min_p", W(null, 94, "TextEncodeAceStepAudio1.5", 14), B("4", "TextEncodeAceStepAudio1.5", "min_p")),
    ],
  }],
};

// ---------------------------------------------------------------------------------------------
// MiniMax-H3 (text and image to video with audio). Two templates (t2v, i2v), one builder, two paths
// (turbo LoRA, the default; hero = the non-LoRA path). The template's steps live in two primitives behind
// an If/Else switch, so the locator points at the primitive of the path being checked.
// ---------------------------------------------------------------------------------------------
const H3_SG = "Image to Video (MiniMax H3)";
const H3_TEMPLATES = {
  t2v: { template: "video_minimax_h3_t2v", sampler: 123, scheduler: 124, heroSteps: 137, turboSteps: 138, lora: 134 },
  i2v: { template: "video_minimax_h3_i2v", sampler: 17, scheduler: 9, heroSteps: 124, turboSteps: 125, lora: 121 },
};
function h3View(kind, path) {
  const t = H3_TEMPLATES[kind];
  const turbo = path === "turbo";
  const steps = turbo
    ? item("scheduler.steps", W(H3_SG, t.turboSteps, "PrimitiveInt", 0), B("124", "BasicScheduler", "steps"), {
        kind: "unreviewed",
        reason: "the turbo-LoRA branch of the template sets 6 steps; the builder runs 8 (the LoRA file is the 8-step one and 8 was the bake-off recipe). The builder header cites an older template package.",
      })
    : item("scheduler.steps", W(H3_SG, t.heroSteps, "PrimitiveInt", 0), B("124", "BasicScheduler", "steps"));
  return {
    name: `${kind}-${path}`,
    template: t.template,
    args: { prompt: "p", seed: SEED, hero: !turbo, ...(kind === "i2v" ? { imagePath: "in.png" } : {}) },
    items: [
      item("sampler.sampler_name", W(H3_SG, t.sampler, "KSamplerSelect", 0), B("123", "KSamplerSelect", "sampler_name")),
      item("scheduler.scheduler", W(H3_SG, t.scheduler, "BasicScheduler", 0), B("124", "BasicScheduler", "scheduler")),
      item("scheduler.denoise", W(H3_SG, t.scheduler, "BasicScheduler", 2), B("124", "BasicScheduler", "denoise")),
      steps,
      ...(turbo ? [
        item("lora.name", W(H3_SG, t.lora, "LoraLoaderModelOnly", 0), B("134", "LoraLoaderModelOnly", "lora_name")),
        item("lora.strength", W(H3_SG, t.lora, "LoraLoaderModelOnly", 1), B("134", "LoraLoaderModelOnly", "strength_model")),
      ] : []),
    ],
  };
}
const h3 = {
  builder: "wf-h3-av",
  build: buildH3AV,
  views: [h3View("t2v", "turbo"), h3View("t2v", "hero"), h3View("i2v", "turbo"), h3View("i2v", "hero")],
};

// ---------------------------------------------------------------------------------------------
// HiDream-O1 (base and dev templates)
// ---------------------------------------------------------------------------------------------
function hidreamView(variant) {
  const dev = variant === "dev";
  return {
    name: variant,
    template: dev ? "image_hidream_o1_dev" : "image_hidream_o1",
    args: { prompt: "p", ckpt: "c.safetensors", variant, seed: SEED },
    items: [
      item("sampler.add_noise", W(null, 108, "SamplerCustom", 0), B("3", "SamplerCustom", "add_noise")),
      item("sampler.cfg", W(null, 108, "SamplerCustom", 3), B("3", "SamplerCustom", "cfg")),
      item("scheduler.scheduler", W(null, 112, "BasicScheduler", 0), B("22", "BasicScheduler", "scheduler")),
      item("scheduler.steps", W(null, 112, "BasicScheduler", 1), B("22", "BasicScheduler", "steps")),
      item("scheduler.denoise", W(null, 112, "BasicScheduler", 2), B("22", "BasicScheduler", "denoise")),
      item("model_noise_scale", W(null, 124, "ModelNoiseScale", 0), B("20", "ModelNoiseScale", "noise_scale")),
      ...(dev ? [
        item("sampler_lcm.s_noise", W(null, 125, "SamplerLCM", 0), B("23", "SamplerLCM", "s_noise")),
        item("sampler_lcm.s_noise_end", W(null, 125, "SamplerLCM", 1), B("23", "SamplerLCM", "s_noise_end")),
        item("sampler_lcm.noise_clip_std", W(null, 125, "SamplerLCM", 2), B("23", "SamplerLCM", "noise_clip_std")),
      ] : [
        item("sampler_select.sampler_name", W(null, 230, "KSamplerSelect", 0), B("23", "KSamplerSelect", "sampler_name")),
        item("seam.start_percent", W(null, 232, "HiDreamO1PatchSeamSmoothing", 0), B("21", "HiDreamO1PatchSeamSmoothing", "start_percent")),
        item("seam.end_percent", W(null, 232, "HiDreamO1PatchSeamSmoothing", 1), B("21", "HiDreamO1PatchSeamSmoothing", "end_percent")),
        item("seam.pattern", W(null, 232, "HiDreamO1PatchSeamSmoothing", 2), B("21", "HiDreamO1PatchSeamSmoothing", "pattern")),
        item("seam.passes", W(null, 232, "HiDreamO1PatchSeamSmoothing", 3), B("21", "HiDreamO1PatchSeamSmoothing", "passes")),
        item("seam.blend", W(null, 232, "HiDreamO1PatchSeamSmoothing", 4), B("21", "HiDreamO1PatchSeamSmoothing", "blend")),
        item("seam.strength", W(null, 232, "HiDreamO1PatchSeamSmoothing", 5), B("21", "HiDreamO1PatchSeamSmoothing", "strength")),
      ]),
    ],
  };
}
const hidream = { builder: "wf-hidream-o1", build: buildHiDreamO1, views: [hidreamView("base"), hidreamView("dev")] };

// ---------------------------------------------------------------------------------------------
// Krea 2 Turbo (text to image)
// ---------------------------------------------------------------------------------------------
const KREA_SG = "Text to Image (Krea-2 Turbo)";
const krea2 = {
  builder: "wf-krea2",
  build: buildKrea2,
  views: [{
    name: "default",
    template: "image_krea2_turbo_t2i",
    args: { prompt: "p", unet: "u.safetensors", seed: SEED },
    items: [
      ...ksampler(KREA_SG, 3, "9"),
      item("clip_loader.type", W(KREA_SG, 11, "CLIPLoader", 1), B("2", "CLIPLoader", "type")),
    ],
  }],
};

// ---------------------------------------------------------------------------------------------
// LTX-2.5 distilled image to video with audio
// ---------------------------------------------------------------------------------------------
const LTX_SG = "Image to Video (LTX-2.5)";
const ltx25 = {
  builder: "wf-ltx25-i2v",
  build: buildLtx25I2V,
  views: [{
    name: "default",
    template: "video_ltx2_5_i2v",
    args: { imagePath: "in.png", prompt: "p", seed: SEED },
    items: [
      item("preprocess.img_compression", W(LTX_SG, 350, "LTXVPreprocess", 0), B("3", "LTXVPreprocess", "img_compression")),
      item("resize.longer_size", W(LTX_SG, 351, "ResizeImageMaskNode", 1), B("2", "ResizeImageMaskNode", "resize_type.longer_size")),
      item("resize.scale_method", W(LTX_SG, 351, "ResizeImageMaskNode", 2), B("2", "ResizeImageMaskNode", "scale_method")),
      item("negative.text", W(LTX_SG, 373, "CLIPTextEncode", 0), B("10", "CLIPTextEncode", "text")),
      item("conditioning.frame_rate", W(LTX_SG, 365, "LTXVConditioning", 0), B("11", "LTXVConditioning", "frame_rate")),
      item("base.i2v_strength", W(LTX_SG, 357, "LTXVImgToVideoInplace", 0), B("13", "LTXVImgToVideoInplace", "strength")),
      item("base.i2v_bypass", W(LTX_SG, 357, "LTXVImgToVideoInplace", 1), B("13", "LTXVImgToVideoInplace", "bypass")),
      item("base.guider_video_cfg", W(LTX_SG, 388, "LTXVDualCFGGuider", 0), B("16", "LTXVDualCFGGuider", "video_cfg")),
      item("base.guider_audio_cfg", W(LTX_SG, 388, "LTXVDualCFGGuider", 1), B("16", "LTXVDualCFGGuider", "audio_cfg")),
      item("base.sampler_name", W(LTX_SG, 352, "KSamplerSelect", 0), B("17", "KSamplerSelect", "sampler_name")),
      item("base.sigmas", W(LTX_SG, 397, "ManualSigmas", 0), B("19", "ManualSigmas", "sigmas")),
      item("refine.i2v_strength", W(LTX_SG, 349, "LTXVImgToVideoInplace", 0), B("23", "LTXVImgToVideoInplace", "strength")),
      item("refine.i2v_bypass", W(LTX_SG, 349, "LTXVImgToVideoInplace", 1), B("23", "LTXVImgToVideoInplace", "bypass")),
      item("refine.guider_video_cfg", W(LTX_SG, 391, "LTXVDualCFGGuider", 0), B("25", "LTXVDualCFGGuider", "video_cfg")),
      item("refine.guider_audio_cfg", W(LTX_SG, 391, "LTXVDualCFGGuider", 1), B("25", "LTXVDualCFGGuider", "audio_cfg")),
      item("refine.sampler_name", W(LTX_SG, 341, "KSamplerSelect", 0), B("26", "KSamplerSelect", "sampler_name")),
      item("refine.sigmas", W(LTX_SG, 396, "ManualSigmas", 0), B("28", "ManualSigmas", "sigmas")),
      item("decode.tile_size", W(LTX_SG, 374, "VAEDecodeTiled", 0), B("31", "VAEDecodeTiled", "tile_size")),
      item("decode.overlap", W(LTX_SG, 374, "VAEDecodeTiled", 1), B("31", "VAEDecodeTiled", "overlap")),
      item("decode.temporal_size", W(LTX_SG, 374, "VAEDecodeTiled", 2), B("31", "VAEDecodeTiled", "temporal_size")),
      item("decode.temporal_overlap", W(LTX_SG, 374, "VAEDecodeTiled", 3), B("31", "VAEDecodeTiled", "temporal_overlap")),
      item("video.fps", W(LTX_SG, 370, "CreateVideo", 0), B("33", "CreateVideo", "fps")),
      item("video.bit_depth", W(LTX_SG, 370, "CreateVideo", 1), B("33", "CreateVideo", "bit_depth")),
    ],
  }],
};

// ---------------------------------------------------------------------------------------------
// Qwen-Image 2512 (text to image). Steps and cfg sit in primitives behind switches; the LoRA branch has its own.
// ---------------------------------------------------------------------------------------------
const Q2512_SG = "Text to Image (Qwen-Image 2512)";
function qwenImageView(name) {
  const lightning = name === "lightning4";
  return {
    name,
    template: "image_qwen_Image_2512",
    args: { prompt: "p", unet: "u.safetensors", seed: SEED, ...QWEN_IMAGE_PRESETS[name] },
    items: [
      item("ksampler.steps", W(Q2512_SG, lightning ? 225 : 224, "PrimitiveInt", 0), B("9", "KSampler", "steps")),
      item("ksampler.cfg", W(Q2512_SG, lightning ? 218 : 223, "PrimitiveFloat", 0), B("9", "KSampler", "cfg")),
      item("ksampler.sampler_name", W(Q2512_SG, 230, "KSampler", 4), B("9", "KSampler", "sampler_name")),
      item("ksampler.scheduler", W(Q2512_SG, 230, "KSampler", 5), B("9", "KSampler", "scheduler")),
      item("ksampler.denoise", W(Q2512_SG, 230, "KSampler", 6), B("9", "KSampler", "denoise")),
      item("model_sampling.shift", W(Q2512_SG, 222, "ModelSamplingAuraFlow", 0), B("5", "ModelSamplingAuraFlow", "shift")),
      ...(lightning ? [
        item("lora.name", W(Q2512_SG, 221, "LoraLoaderModelOnly", 0), B("4", "LoraLoaderModelOnly", "lora_name")),
        item("lora.strength", W(Q2512_SG, 221, "LoraLoaderModelOnly", 1), B("4", "LoraLoaderModelOnly", "strength_model")),
      ] : []),
    ],
  };
}
const qwenImage = { builder: "wf-qwen-image", build: buildQwenImage, views: [qwenImageView("full"), qwenImageView("lightning4")] };

// ---------------------------------------------------------------------------------------------
// Qwen-Image-Edit 2511. Only the two presets the template carries (full, 4-step Lightning); the builder's
// 8-step preset has no template counterpart.
// ---------------------------------------------------------------------------------------------
const QEDIT_SG = "Image Edit (Qwen-Image 2511)";
function qwenEditView(name) {
  const lightning = name === "lightning4";
  return {
    name,
    template: "image_qwen_image_edit_2511",
    args: { image: "in.png", prompt: "p", unet: "u.safetensors", seed: SEED, ...QWEN_EDIT_PRESETS[name] },
    items: [
      item("ksampler.steps", W(QEDIT_SG, lightning ? 165 : 166, "PrimitiveInt", 0), B("11", "KSampler", "steps")),
      lightning
        ? item("ksampler.cfg", W(QEDIT_SG, 155, "PrimitiveFloat", 0), B("11", "KSampler", "cfg"))
        : item("ksampler.cfg", W(QEDIT_SG, 154, "PrimitiveFloat", 0), B("11", "KSampler", "cfg"), {
            kind: "unreviewed",
            reason: "the template's non-LoRA CFG primitive is 4; the builder preset is 3.0, which is the value of the KSampler's own widget (stale, because the input is linked to the switch).",
          }),
      item("ksampler.sampler_name", W(QEDIT_SG, 169, "KSampler", 4), B("11", "KSampler", "sampler_name")),
      item("ksampler.scheduler", W(QEDIT_SG, 169, "KSampler", 5), B("11", "KSampler", "scheduler")),
      item("ksampler.denoise", W(QEDIT_SG, 169, "KSampler", 6), B("11", "KSampler", "denoise")),
      item("model_sampling.shift", W(QEDIT_SG, 145, "ModelSamplingAuraFlow", 0), B("7", "ModelSamplingAuraFlow", "shift")),
      ...(lightning ? [
        item("lora.name", W(QEDIT_SG, 153, "LoraLoaderModelOnly", 0), B("6", "LoraLoaderModelOnly", "lora_name")),
        item("lora.strength", W(QEDIT_SG, 153, "LoraLoaderModelOnly", 1), B("6", "LoraLoaderModelOnly", "strength_model")),
      ] : []),
    ],
  };
}
const qwenEdit = { builder: "wf-qwen-image-edit", build: buildQwenImageEdit, views: [qwenEditView("full"), qwenEditView("lightning4")] };

// ---------------------------------------------------------------------------------------------
// Qwen-Image DiffSynth ControlNet inpaint. The template's Lightning branch is a bypassed LoRA with another
// file name and a markdown note; only the un-distilled recipe is widget data.
// ---------------------------------------------------------------------------------------------
const qwenInpaint = {
  builder: "wf-qwen-inpaint",
  build: buildQwenInpaint,
  views: [{
    name: "full",
    template: "image_qwen_image_controlnet_patch",
    args: { prompt: "p", image: "in.png", mask: "mask.png", unet: "u.safetensors", seed: SEED, ...QWEN_INPAINT_PRESETS.full },
    items: [
      ...ksampler(null, 3, "14"),
      item("model_sampling.shift", W(null, 66, "ModelSamplingAuraFlow", 0), B("5", "ModelSamplingAuraFlow", "shift")),
      item("controlnet.strength", W(null, 69, "QwenImageDiffsynthControlnet", 0), B("10", "QwenImageDiffsynthControlnet", "strength")),
    ],
  }],
};

// ---------------------------------------------------------------------------------------------
// Qwen-Image 2.1 (text to image and multi-reference edit). The KSampler's widgets are linked to the subgraph
// instance, so the instance's promoted widgets are the effective values. The builder takes its step count from
// the model's README, not from the template.
// ---------------------------------------------------------------------------------------------
const Q21_T2I = "Text to Image (Qwen Image 2.1)";
const Q21_EDIT = "Image Edit (Qwen Image 2.1)";
const Q21_STEPS_DELTA = { kind: "deliberate", reason: "the builder's recipe is the model README's (40 steps, header of wf-qwen-image-21.mjs); the template ships 25." };
const qwen21 = {
  builder: "wf-qwen-image-21",
  build: (args) => (args.images ? buildQwenImage21Edit(args) : buildQwenImage21(args)),
  views: [
    {
      name: "t2i",
      template: "image_qwen_image_2_1_t2i",
      args: { prompt: "p", unet: "u.safetensors", clip: "c.safetensors", vae: "v.safetensors", schedule: "comfy", seed: SEED },
      items: [
        item("ksampler.steps", I(459, Q21_T2I, "steps"), B("10", "KSampler", "steps"), Q21_STEPS_DELTA),
        item("ksampler.cfg", I(459, Q21_T2I, "cfg"), B("10", "KSampler", "cfg")),
        item("ksampler.sampler_name", I(459, Q21_T2I, "scheduler"), B("10", "KSampler", "sampler_name")),
        item("ksampler.scheduler", I(459, Q21_T2I, "scheduler_1"), B("10", "KSampler", "scheduler")),
        item("ksampler.denoise", W(Q21_T2I, 458, "KSampler", 6), B("10", "KSampler", "denoise")),
        item("encoder.resolution", W(Q21_T2I, 452, "TextEncodeQwenImage21", 2), B("4", "TextEncodeQwenImage21", "resolution")),
      ],
    },
    {
      name: "edit",
      template: "image_qwen_image_2_1_image_edit",
      args: { images: ["in.png"], prompt: "p", unet: "u.safetensors", clip: "c.safetensors", vae: "v.safetensors", seed: SEED },
      items: [
        item("ksampler.steps", I(459, Q21_EDIT, "steps"), B("10", "KSampler", "steps"), Q21_STEPS_DELTA),
        item("ksampler.cfg", I(459, Q21_EDIT, "cfg"), B("10", "KSampler", "cfg")),
        item("ksampler.sampler_name", I(459, Q21_EDIT, "scheduler"), B("10", "KSampler", "sampler_name")),
        item("ksampler.scheduler", I(459, Q21_EDIT, "scheduler_1"), B("10", "KSampler", "scheduler")),
        item("ksampler.denoise", W(Q21_EDIT, 458, "KSampler", 6), B("10", "KSampler", "denoise")),
        item("encoder.resolution", I(459, Q21_EDIT, "resolution"), B("5", "TextEncodeQwenImage21", "resolution"), {
          kind: "unreviewed",
          reason: "the edit template promotes resolution 0 (each reference keeps its own size); the builder defaults to 1024, which its header calls the official edit default.",
        }),
        item("cache.device", I(459, Q21_EDIT, "device"), B("4", "QwenImage21Cache", "device")),
        item("cache.dtype", I(459, Q21_EDIT, "dtype"), B("4", "QwenImage21Cache", "dtype")),
      ],
    },
  ],
};

// ---------------------------------------------------------------------------------------------
// Wan Animate 2 distilled (character animation). The builder flattens the template's Motion Transfer
// subgraph. The pose values come from the root instance's promoted widgets.
// ---------------------------------------------------------------------------------------------
const WA_SG = "Motion Transfer (Wan Animate 2 Distilled)";
const wanAnimate2 = {
  builder: "wf-wan-animate2",
  build: buildWanAnimate2,
  views: [{
    name: "default",
    template: "video_wan_animate2_distilled",
    args: { refImagePath: "ref.png", driverVideoPath: "drive.mp4", prompt: "p", seed: SEED },
    items: [
      item("scheduler.scheduler", W(WA_SG, 18, "BasicScheduler", 0), B("18", "BasicScheduler", "scheduler")),
      item("scheduler.steps", W(WA_SG, 18, "BasicScheduler", 1), B("18", "BasicScheduler", "steps")),
      item("scheduler.denoise", W(WA_SG, 18, "BasicScheduler", 2), B("18", "BasicScheduler", "denoise")),
      item("model_sampling.shift", W(WA_SG, 95, "ModelSamplingSD3", 0), B("95", "ModelSamplingSD3", "shift")),
      item("sampler.sampler_name", W(WA_SG, 27, "KSamplerSelect", 0), B("27", "KSamplerSelect", "sampler_name")),
      item("sampler.add_noise", W(WA_SG, 19, "SamplerCustom", 0), B("19", "SamplerCustom", "add_noise")),
      item("sampler.cfg", W(WA_SG, 19, "SamplerCustom", 3), B("19", "SamplerCustom", "cfg")),
      item("negative.text", W(WA_SG, 4, "CLIPTextEncode", 0), B("4", "CLIPTextEncode", "text")),
      item("clip_vision.crop", W(WA_SG, 76, "CLIPVisionEncode", 0), B("76", "CLIPVisionEncode", "crop")),
      item("clip_vision_pose.crop", W(WA_SG, 220, "CLIPVisionEncode", 0), B("220", "CLIPVisionEncode", "crop")),
      item("resize.crop", W(WA_SG, 243, "ResizeImageMaskNode", 3), B("243", "ResizeImageMaskNode", "resize_type.crop")),
      item("resize.scale_method", W(WA_SG, 243, "ResizeImageMaskNode", 4), B("243", "ResizeImageMaskNode", "scale_method")),
      item("resize.width", I(261, WA_SG, "resize_type.width"), B("243", "ResizeImageMaskNode", "resize_type.width")),
      item("resize.height", I(261, WA_SG, "resize_type.height_1"), B("243", "ResizeImageMaskNode", "resize_type.height")),
      item("video.length", I(261, WA_SG, "length"), B("247", "WanAnimate2ToVideo", "length")),
      item("video.batch_size", W(WA_SG, 247, "WanAnimate2ToVideo", 3), B("247", "WanAnimate2ToVideo", "batch_size")),
      item("video.frame_offset", I(261, WA_SG, "video_frame_offset_1"), B("247", "WanAnimate2ToVideo", "video_frame_offset")),
      item("pose.strength", I(261, WA_SG, "pose_strength_1"), B("247", "WanAnimate2ToVideo", "pose_strength")),
      item("pose.start_percent", I(261, WA_SG, "pose_start_percent_1"), B("247", "WanAnimate2ToVideo", "pose_start_percent")),
      item("pose.end_percent", I(261, WA_SG, "pose_end_percent_1"), B("247", "WanAnimate2ToVideo", "pose_end_percent")),
      item("reference.strength", I(261, WA_SG, "reference_image_strength_1"), B("247", "WanAnimate2ToVideo", "reference_image_strength")),
      item("pose_cache.device", I(261, WA_SG, "device_1"), B("224", "WanAnimate2Cache", "device"), {
        kind: "deliberate",
        reason: "the builder pins the pose cache to cpu: the template's gpu setting hard-kills ComfyUI mid-step on the reference box (header of wf-wan-animate2.mjs).",
      }),
      item("pose_cache.dtype", I(261, WA_SG, "dtype_1"), B("224", "WanAnimate2Cache", "dtype"), {
        kind: "deliberate",
        reason: "the builder pins the pose cache to default dtype: the template's int8 setting goes with the gpu setting (header of wf-wan-animate2.mjs).",
      }),
      // The PR #1308 defect class: one subgraph input feeding both the start and the end percent.
      item("wiring.pose_start_end_distinct", D(WA_SG, 247, "WanAnimate2ToVideo", "pose_start_percent", "pose_end_percent"), null),
    ],
  }],
};

// ---------------------------------------------------------------------------------------------
// Wan 2.2 14B image to video, NATIVE path (the builder's default). The fast path's LoRA strengths and split
// are not in the template.
// ---------------------------------------------------------------------------------------------
const W22_SG = "Image to Video (Wan2.2)";
const wan22 = {
  builder: "wf-wan22-i2v",
  build: buildWan22I2V,
  views: [{
    name: "native",
    template: "video_wan2_2_14B_i2v",
    args: { imagePath: "in.png", prompt: "p", seed: SEED },
    items: [
      item("high.steps", W(W22_SG, 128, "PrimitiveInt", 0), B("11", "KSamplerAdvanced", "steps")),
      item("low.steps", W(W22_SG, 128, "PrimitiveInt", 0), B("12", "KSamplerAdvanced", "steps")),
      item("high.cfg", W(W22_SG, 126, "PrimitiveFloat", 0), B("11", "KSamplerAdvanced", "cfg")),
      item("low.cfg", W(W22_SG, 126, "PrimitiveFloat", 0), B("12", "KSamplerAdvanced", "cfg")),
      item("high.end_at_step", W(W22_SG, 127, "PrimitiveInt", 0), B("11", "KSamplerAdvanced", "end_at_step")),
      item("low.start_at_step", W(W22_SG, 127, "PrimitiveInt", 0), B("12", "KSamplerAdvanced", "start_at_step")),
      item("high.sampler_name", W(W22_SG, 86, "KSamplerAdvanced", 5), B("11", "KSamplerAdvanced", "sampler_name")),
      item("high.scheduler", W(W22_SG, 86, "KSamplerAdvanced", 6), B("11", "KSamplerAdvanced", "scheduler")),
      item("high.add_noise", W(W22_SG, 86, "KSamplerAdvanced", 0), B("11", "KSamplerAdvanced", "add_noise")),
      item("high.return_with_leftover_noise", W(W22_SG, 86, "KSamplerAdvanced", 9), B("11", "KSamplerAdvanced", "return_with_leftover_noise")),
      item("low.sampler_name", W(W22_SG, 85, "KSamplerAdvanced", 5), B("12", "KSamplerAdvanced", "sampler_name")),
      item("low.scheduler", W(W22_SG, 85, "KSamplerAdvanced", 6), B("12", "KSamplerAdvanced", "scheduler")),
      item("low.add_noise", W(W22_SG, 85, "KSamplerAdvanced", 0), B("12", "KSamplerAdvanced", "add_noise")),
      item("low.return_with_leftover_noise", W(W22_SG, 85, "KSamplerAdvanced", 9), B("12", "KSamplerAdvanced", "return_with_leftover_noise")),
      item("high.shift", W(W22_SG, 104, "ModelSamplingSD3", 0), B("8", "ModelSamplingSD3", "shift")),
      item("low.shift", W(W22_SG, 103, "ModelSamplingSD3", 0), B("10", "ModelSamplingSD3", "shift")),
      item("negative.text", W(W22_SG, 89, "CLIPTextEncode", 0), B("5", "CLIPTextEncode", "text")),
      item("video.batch_size", W(W22_SG, 98, "WanImageToVideo", 3), B("6", "WanImageToVideo", "batch_size")),
    ],
  }],
};

// Builders copied from a workflow template, in file-name order.
export const BUILDER_SPECS = [acestep, h3, hidream, krea2, ltx25, qwenImage, qwen21, qwenEdit, qwenInpaint, wanAnimate2, wan22]
  .sort((a, b) => (a.builder < b.builder ? -1 : 1));

// Builders that carry no widget copied from a workflow template; builder-drift.mjs fails when a `wf-*.mjs`
// file is in neither list, so a new builder cannot skip the question.
export const NOT_TEMPLATE_DERIVED = {
  "wf-hunyuan15-i2v": "Its header cites a ComfyUI workflow id, not a template of the package, and its recipe is the 480p cfg-distilled GGUF one (50 steps, cfg 1, shift 5); the only matching template in the package is the 720p non-distilled one (20 steps, cfg 6, shift 7), so there is no copied widget to guard.",
  "wf-sdxl-inpaint": "Core-node SDXL inpaint graph written from the node schemas; its steps, cfg and sampler are its own defaults and no workflow template is named.",
  "wf-upscale": "Core-node ESRGAN upscale graph; it has no sampler recipe and names no workflow template.",
};
