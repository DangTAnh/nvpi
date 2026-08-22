package nvidia

import (
	"sort"
	"strings"

	"glm52-nvidia/internal/models"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
)

// ponytail: fallback only. When the crawler scrape at startup cannot find a
// specifications.contextLength in the playground HTML, we still want Claude
// Code to size its context correctly. 262144 is the smallest NIM_MAX_MODEL_LEN
// observed in the wild — anything smaller is unsafe; anything larger should
// be discovered. If a model proves to be smaller, set it in models.Models.
const (
	fallbackContextLength       = 262144
	fallbackMaxCompletionTokens = 16384
)

// RegistryModels returns cliproxy ModelInfo entries for every playground model.
// ContextLength comes from the crawler scrape (models.Models.ContextLength)
// when available, falling back to fallbackContextLength otherwise.
func RegistryModels() []*cliproxy.ModelInfo {
	ids := make([]string, 0, len(models.Models))
	for id := range models.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*cliproxy.ModelInfo, 0, len(ids))
	for _, id := range ids {
		info := models.Models[id]
		org, _, _ := strings.Cut(id, "/")
		ctxLen := info.ContextLength
		if ctxLen <= 0 {
			ctxLen = fallbackContextLength
		}
		out = append(out, &cliproxy.ModelInfo{
			ID:                  id,
			Object:              "model",
			Created:             0,
			OwnedBy:             org,
			Type:                providerKey,
			ContextLength:       ctxLen,
			MaxCompletionTokens: fallbackMaxCompletionTokens,
		})
	}
	return out
}
