package registry

import (
	"encoding/json"
	"sync"
)

// forkAntigravityModelIDs are served by Antigravity but absent from the
// upstream remote catalog. Their definitions come from the embedded catalog.
var forkAntigravityModelIDs = []string{"gemini-3.1-pro-high"}

var (
	forkAntigravityModelsOnce sync.Once
	forkAntigravityModels     []*ModelInfo
)

func embeddedForkAntigravityModels() []*ModelInfo {
	forkAntigravityModelsOnce.Do(func() {
		var embedded staticModelsJSON
		if err := json.Unmarshal(embeddedModelsJSON, &embedded); err != nil {
			return
		}
		for _, id := range forkAntigravityModelIDs {
			for _, model := range embedded.Antigravity {
				if model != nil && model.ID == id {
					forkAntigravityModels = append(forkAntigravityModels, model)
					break
				}
			}
		}
	})
	return forkAntigravityModels
}

// applyForkCatalogAdditions appends fork-served Antigravity models that the
// catalog lacks. A catalog entry with the same ID takes precedence.
func applyForkCatalogAdditions(catalog *staticModelsJSON) {
	if catalog == nil {
		return
	}
	present := make(map[string]bool, len(catalog.Antigravity))
	for _, model := range catalog.Antigravity {
		if model != nil {
			present[model.ID] = true
		}
	}
	for _, model := range cloneModelInfos(embeddedForkAntigravityModels()) {
		if !present[model.ID] {
			catalog.Antigravity = append(catalog.Antigravity, model)
		}
	}
}
