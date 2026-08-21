// wf-upscale.mjs — ESRGAN-family image UPSCALE graph (core ComfyUI nodes only).
// LoadImage → UpscaleModelLoader → ImageUpscaleWithModel → [ImageScale | ImageScaleBy] → SaveImage.
// The model's native factor (4x for "4x-UltraSharp.pth") is what ImageUpscaleWithModel
// produces. width+height pin the output exactly (ImageScale) — the runner converts a
// requested `scale` into this form by measuring the source, so the result is exact for
// ANY model. `scale` here is the fallback shape for a source the runner could not measure:
// it rescales the model output by scale/nativeFactor (ImageScaleBy), so it is only as
// right as the factor read from the filename. ImageUpscaleWithModel tiles on OOM by
// itself, so there is no tile knob. Which model a machine upscales with is per-machine
// config (upscale_model), never shared code.

const METHODS = ["nearest-exact", "bilinear", "area", "bicubic", "lanczos"];
// ComfyUI core limits (nodes.py MAX_RESOLUTION and ImageScaleBy's scale_by range): a value
// outside them is rejected server-side AFTER the GPU slot + cold start, so fail here.
export const MAX_RESOLUTION = 16384;
export const SCALE_BY_MIN = 0.01;
export const SCALE_BY_MAX = 8.0;

// nativeFactor reads the factor an ESRGAN-family filename advertises — a leading or
// delimited "<N>x" token ("4x-UltraSharp", "2xLexicaRRDBNet_Sharp", "8x_NMKD") or an
// "x<N>" token ("RealESRGAN_x4plus", "HAT_SRx4", "realesr-general-x4v3"). Unknown names
// are assumed 4x, the de-facto family default; that assumption only reaches a render
// through the ImageScaleBy fallback described above.
export function nativeFactor(model) {
  const s = String(model || "");
  const lead = /(?:^|[^a-z0-9])(\d+)x(?![0-9])/i.exec(s);
  const trail = /(?:^|[^a-z0-9])(?:[a-z]*[^a-z0-9]?)?x(\d+)(?![0-9])/i.exec(s) || /x(\d+)(?![0-9])/i.exec(s);
  const f = lead ? Number(lead[1]) : trail ? Number(trail[1]) : 0;
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
  if (hasW && (width > MAX_RESOLUTION || height > MAX_RESOLUTION)) {
    throw new Error(`buildUpscale: width and height must be <= ${MAX_RESOLUTION} (ComfyUI limit), got ${width}x${height}`);
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
      const by = Number(scale) / f;
      if (by < SCALE_BY_MIN || by > SCALE_BY_MAX) {
        throw new Error(`buildUpscale: scale ${scale} on a ${f}x model needs scale_by ${by}, outside ComfyUI's ${SCALE_BY_MIN}-${SCALE_BY_MAX}`);
      }
      g["4"] = { class_type: "ImageScaleBy", inputs: { upscale_method: method, scale_by: by, image: last } };
      last = ["4", 0];
    }
  }
  g["9"] = { class_type: "SaveImage", inputs: { filename_prefix: "upscale", images: last } };
  return g;
}
