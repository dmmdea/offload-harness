// wf-upscale.mjs — ESRGAN-family image UPSCALE graph (core ComfyUI nodes only).
// LoadImage → UpscaleModelLoader → ImageUpscaleWithModel → [ImageScaleBy | ImageScale] → SaveImage.
// The model's native factor (4x for "4x-UltraSharp.pth") is what ImageUpscaleWithModel
// produces; `scale` rescales THAT result (scale 2 on a 4x model = 4x then 0.5), and
// width+height pin the output exactly. ImageUpscaleWithModel tiles on OOM by itself, so
// there is no tile knob. Which model a machine upscales with is per-machine config
// (upscale_model), never shared code.

const METHODS = ["nearest-exact", "bilinear", "area", "bicubic", "lanczos"];

// nativeFactor reads the factor an ESRGAN-family filename advertises ("4x-UltraSharp",
// "RealESRGAN_x4plus", "2x_NMKD"). Unknown names are assumed 4x — the de-facto family
// default — which only matters when `scale` is given (the graph then rescales from 4).
export function nativeFactor(model) {
  const m = /(?:^|[^a-z0-9])(\d+)x(?![a-z0-9]*x)/i.exec(String(model || "")) || /x(\d+)(?:plus|$|[^a-z0-9])/i.exec(String(model || ""));
  const f = m ? Number(m[1]) : 0;
  return f > 0 ? f : 4;
}

export function buildUpscale({ image, model, scale, width, height, method = "lanczos" } = {}) {
  if (!image) throw new Error("buildUpscale: image (staged input filename) is required");
  if (!model) throw new Error("buildUpscale: model (upscale_models filename) is required");
  if (!METHODS.includes(method)) throw new Error(`buildUpscale: method must be one of ${METHODS.join("|")}, got ${method}`);
  const hasW = width != null && width !== 0, hasH = height != null && height !== 0;
  if (hasW !== hasH) throw new Error("buildUpscale: width and height must be given together");
  if (hasW && !(Number.isInteger(width) && width > 0 && Number.isInteger(height) && height > 0)) {
    throw new Error("buildUpscale: width and height must be positive integers");
  }
  if (scale != null && !(Number(scale) > 0)) throw new Error("buildUpscale: scale must be > 0");
  const g = {
    "1": { class_type: "LoadImage", inputs: { image } },
    "2": { class_type: "UpscaleModelLoader", inputs: { model_name: model } },
    "3": { class_type: "ImageUpscaleWithModel", inputs: { upscale_model: ["2", 0], image: ["1", 0] } },
  };
  let last = ["3", 0];
  if (hasW) {
    g["4"] = { class_type: "ImageScale", inputs: { upscale_method: method, width, height, crop: "disabled", image: last } };
    last = ["4", 0];
  } else if (scale != null) {
    const f = nativeFactor(model);
    if (Number(scale) !== f) {
      g["4"] = { class_type: "ImageScaleBy", inputs: { upscale_method: method, scale_by: Number(scale) / f, image: last } };
      last = ["4", 0];
    }
  }
  g["9"] = { class_type: "SaveImage", inputs: { filename_prefix: "upscale", images: last } };
  return g;
}
