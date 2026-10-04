package registry

import "testing"

func antigravityModelByID(models []*ModelInfo, id string) *ModelInfo {
	for _, model := range models {
		if model != nil && model.ID == id {
			return model
		}
	}
	return nil
}

func TestEmbeddedCatalogServesGemini31ProHigh(t *testing.T) {
	model := antigravityModelByID(GetAntigravityModels(), "gemini-3.1-pro-high")
	if model == nil {
		t.Fatal("embedded Antigravity catalog lacks gemini-3.1-pro-high")
	}
	if model.DisplayName != "Gemini 3.1 Pro (High)" || model.Thinking == nil {
		t.Fatalf("unexpected definition: %#v", model)
	}
}

func TestApplyForkCatalogAdditions(t *testing.T) {
	// A remote catalog without the model gains exactly one copy.
	remote := &staticModelsJSON{Antigravity: []*ModelInfo{{ID: "gemini-3.1-pro-low"}}}
	applyForkCatalogAdditions(remote)
	applyForkCatalogAdditions(remote)
	count := 0
	for _, model := range remote.Antigravity {
		if model.ID == "gemini-3.1-pro-high" {
			count++
		}
	}
	if count != 1 || len(remote.Antigravity) != 2 {
		t.Fatalf("fork additions = %d, catalog size = %d", count, len(remote.Antigravity))
	}

	// Mutating the added copy must not change the embedded definition.
	antigravityModelByID(remote.Antigravity, "gemini-3.1-pro-high").DisplayName = "changed"
	if embeddedForkAntigravityModels()[0].DisplayName == "changed" {
		t.Fatal("fork addition shares the embedded definition")
	}

	// An upstream definition with the same ID takes precedence.
	upstream := &ModelInfo{ID: "gemini-3.1-pro-high", DisplayName: "upstream"}
	remote = &staticModelsJSON{Antigravity: []*ModelInfo{upstream}}
	applyForkCatalogAdditions(remote)
	if len(remote.Antigravity) != 1 || remote.Antigravity[0] != upstream {
		t.Fatal("fork addition replaced or duplicated the upstream definition")
	}

	applyForkCatalogAdditions(nil)
}
