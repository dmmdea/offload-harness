package pipeline

import (
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// editLaneNeed is the need a generative edit would ask of this box's lane (resolveLane's edit branch): the edit binding the request
// resolves to, with the request's own megapixels and placement applied, as runEditImageGenerative applies them. why is not empty
// when the lane is not in question (no edit route here, an unknown family, a canvas or placement the run itself refuses).
func (p *Pipeline) editLaneNeed(req core.Request, token string) (mediaNeed, string) {
	cfg, _, err := p.cfg.ResolveEditFamily(paramStr(req.Params, "family"))
	if err != nil {
		return mediaNeed{}, err.Error()
	}
	if cfg.GenEditScript == "" || cfg.GenEditUnet == "" {
		return mediaNeed{}, "no generative edit route configured"
	}
	if cfg, err = mediacap.ApplyEditRequestKnobs(cfg, req.Params); err != nil {
		return mediaNeed{}, err.Error()
	}
	return editNeed(cfg, token), ""
}
