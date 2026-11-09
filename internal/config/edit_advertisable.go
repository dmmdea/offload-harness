package config

// EditAdvertisable reports whether this node has a generative-edit binding it can serve: the default binding names the edit runner
// and a UNet, or at least one named edit family (gen_edit_families) resolves to one. It is the edit twin of ImageGenAdvertisable,
// the one predicate the fleet advertisement, the media-job door and admission read for the image-edit task.
func (c Config) EditAdvertisable() bool {
	for _, fi := range c.EditFamilies() {
		eff := c
		if !fi.Default {
			var err error
			if eff, _, err = c.ResolveEditFamily(fi.Name); err != nil {
				continue
			}
		}
		if eff.GenEditScript != "" && eff.GenEditUnet != "" {
			return true
		}
	}
	return false
}
