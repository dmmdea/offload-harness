package pipeline

import "github.com/dmmdea/offload-harness/internal/config"

// ImageBatchDefaults are the batch verb's own --negative, --width, --height, --steps and --seed: the value a row without its own takes.
// A zero field is unset.
type ImageBatchDefaults struct {
	Negative                   string
	Width, Height, Steps, Seed int
}

// ApplyImageBatchDefaults returns the jobs with the verb's defaults laid under each row and every size snapped to what the family renders.
// A row's own value wins. A seed default S gives the row at index i (0-based, over the jobs of the file) the seed S+i, so the seeds are
// the same on every run of the file, and on a --resume, whichever rows were already rendered. The size is snapped to the grid of the
// binding the family resolves to on this machine (config.Config.SnapImageDims, the renderer's own floor), so the size a row asks for,
// the size it renders at and the size --resume checks the picture against are one number; a family this machine does not define (carried
// to a node by name) is left as asked. The input is not modified.
func ApplyImageBatchDefaults(cfg config.Config, family string, jobs []ImageBatchJob, d ImageBatchDefaults) []ImageBatchJob {
	eff, _, ferr := cfg.ResolveImageFamily(family)
	out := make([]ImageBatchJob, len(jobs))
	for i, j := range jobs {
		if j.Negative == "" {
			j.Negative = d.Negative
		}
		if j.Width <= 0 {
			j.Width = d.Width
		}
		if j.Height <= 0 {
			j.Height = d.Height
		}
		if j.Steps <= 0 {
			j.Steps = d.Steps
		}
		if j.Seed <= 0 && d.Seed > 0 {
			j.Seed = d.Seed + i
		}
		if ferr == nil {
			j.Width, j.Height = eff.SnapImageDims(j.Width, j.Height)
		}
		out[i] = j
	}
	return out
}
