package mediaremote

// ResumeVerifies reports whether --resume would take the file at out for the finished picture of a job that names the size w x h:
// the file is a PNG that decodes whole and has that size (a side of 0 is not named). It is the check RunBatch makes before it renders a
// job, exported for the batch verb's --dry-run so the verb can say which rows a resume would skip. The caller gives it the size the
// family renders (config.Config.SnapImageDims), which is what the jobs carry by then.
func ResumeVerifies(out string, w, h int) bool { return out != "" && pngVerifies(out, w, h) }
