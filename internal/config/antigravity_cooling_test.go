package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAntigravityModelLevelCoolingLayouts(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"missing", "port: 8317\n", false},
		{"legacy false", "antigravity: {model-level-cooling: false}\n", false},
		{"legacy true", "antigravity: {model-level-cooling: true, sensitive-words: [private]}\n", true},
		{"oauth true", "oauth: {providers: {antigravity: {model-level-cooling: true}}}\n", true},
		{"upstream true", "upstream: {antigravity: {model-level-cooling: true}}\n", true},
		{"canonical false wins", "upstream: {antigravity: {model-level-cooling: false}}\nantigravity: {model-level-cooling: true}\noauth: {providers: {antigravity: {model-level-cooling: true}}}\n", false},
		{"oauth false wins legacy", "antigravity: {model-level-cooling: true}\noauth: {providers: {antigravity: {model-level-cooling: false}}}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Antigravity.ModelLevelCooling != tc.want {
				t.Fatalf("ModelLevelCooling = %v, want %v", cfg.Antigravity.ModelLevelCooling, tc.want)
			}
			migrated, _, err := NormalizeConfigLayout([]byte(tc.raw), true)
			if err != nil {
				t.Fatal(err)
			}
			if err = ValidateV8Config(migrated); err != nil {
				t.Fatalf("invalid migrated config: %v", err)
			}
			reloaded, err := ParseConfigBytes(migrated)
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.Antigravity.ModelLevelCooling != tc.want {
				t.Fatalf("migrated ModelLevelCooling = %v, want %v", reloaded.Antigravity.ModelLevelCooling, tc.want)
			}
			if strings.Contains(tc.raw, "sensitive-words") && (len(reloaded.Antigravity.SensitiveWords) != 1 || reloaded.Antigravity.SensitiveWords[0] != "private") {
				t.Fatal("migration changed unrelated Antigravity configuration")
			}
		})
	}
}

func TestAntigravityModelLevelCoolingJSON(t *testing.T) {
	var cfg AntigravityConfig
	if err := json.Unmarshal([]byte(`{"model-level-cooling":true}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.ModelLevelCooling {
		t.Fatal("JSON model-level-cooling did not enable the policy")
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"model-level-cooling":true`) {
		t.Fatalf("JSON omitted policy: %s", encoded)
	}
}
