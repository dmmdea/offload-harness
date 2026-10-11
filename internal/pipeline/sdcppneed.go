package pipeline

// What the sd.cpp lanes declare to the host-RAM guard (F60).
//
// The class default (hostneed.ClassDefault) is the largest COMFYUI family a box binds, so an sd.cpp-only box (the shape of the
// small nodes) declared nothing for its image, video and animate lanes, and the 5.5 to 6.2 GiB of weights `--offload-to-cpu`
// parks in RAM there were outside the brake on the boxes whose memory matters most. Each of those lanes now declares the
// LARGER of the class default (unchanged: on a box that also binds ComfyUI families it is the proxy it was, an upper bound
// that is not the lane's own size) and the weights the lane parks itself (hostneed.ForSdcpp: the bound files in full, when
// its extra args carry --offload-to-cpu). The own figure is an estimate and is worded as one; a lane whose files cannot be
// sized keeps the class default. The need also carries the lane's footprint key, so the footprint store's measured runs raise
// the estimate after three, like a ComfyUI family's (calibratedRAM; it only raises and leaves 0 alone).
//
// Not covered: the audio.cpp voice and music lanes (not sd.cpp: they keep the class default) and the CLI's `gpu reserve`
// around an sd.cpp helper (hostneed.Resolve recognises only the ComfyUI helpers), which still declares the class default or
// what --ram says.

import (
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/hostneed"
)

// sdcppDeclared is the declaration of an sd.cpp lane: files are its bound weight files, extra its extra args, family the
// word the estimate uses for it, proxy the config the class default is read from.
func sdcppDeclared(proxy config.Config, family string, files []hostneed.SdcppFile, extra []string) func(float64) hostneed.Need {
	return func(vram float64) hostneed.Need {
		f := hostneed.Facts{Cfg: proxy, VRAMGiB: vram}
		class := hostneed.ClassDefault(f)
		if own, ok := hostneed.ForSdcpp(family, files, extra, f); ok && own.GiB > class.GiB {
			return own
		}
		return class
	}
}

// imageSdcppFiles are the weight files of the sd.cpp image binding (the default or a named family's overlay).
func imageSdcppFiles(cfg config.Config) []hostneed.SdcppFile {
	return []hostneed.SdcppFile{
		{Label: "sdcpp_model", Path: cfg.SdcppModel}, {Label: "sdcpp_vae", Path: cfg.SdcppVAE},
		{Label: "sdcpp_clip_l", Path: cfg.SdcppClipL}, {Label: "sdcpp_clip_g", Path: cfg.SdcppClipG},
		{Label: "sdcpp_t5xxl", Path: cfg.SdcppT5}, {Label: "sdcpp_llm", Path: cfg.SdcppLLM},
	}
}

// videoSdcppFiles are the weight files of an sd.cpp video family binding (the high-noise expert of a Wan 2.2 pair included).
func videoSdcppFiles(fb config.VideoFamilyBinding) []hostneed.SdcppFile {
	return []hostneed.SdcppFile{
		{Label: "sdcpp_model", Path: fb.SdcppModel}, {Label: "sdcpp_high_noise_model", Path: fb.SdcppHighNoiseModel},
		{Label: "sdcpp_vae", Path: fb.SdcppVAE}, {Label: "sdcpp_t5xxl", Path: fb.SdcppT5xxl},
	}
}

// animateSdcppFiles are the weight files of the sd.cpp animate binding. The depth model is a separate, small step run by its own
// binary and is not counted.
func animateSdcppFiles(cfg config.Config) []hostneed.SdcppFile {
	return []hostneed.SdcppFile{
		{Label: "animategen_sdcpp_model", Path: cfg.AnimateGenSdcppModel}, {Label: "animategen_sdcpp_vae", Path: cfg.AnimateGenSdcppVAE},
		{Label: "animategen_sdcpp_t5xxl", Path: cfg.AnimateGenSdcppT5xxl},
	}
}

// sdcppImageNeed is the need of the sd.cpp image lane for the resolved binding cfg: the whole node (sd.cpp has no ComfyUI
// instance to bind to a card), declaring its own parked bytes and recorded under the lane's footprint key.
func (p *Pipeline) sdcppImageNeed(cfg config.Config, family, quant, token string) mediaNeed {
	return wholeNeed(token).
		declaresRAM(sdcppDeclared(p.cfg, family, imageSdcppFiles(cfg), cfg.SdcppExtraArgs)).
		withFootprint(footKey{family, quant, "image-gen"})
}
