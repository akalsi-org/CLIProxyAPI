package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCodexHTTPWebsocketsConfigCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, raw, path string
		want            bool
	}{
		{"legacy default", "codex: {}\n", "codex.http-websockets", false},
		{"legacy enabled", "codex: {http-websockets: true}\n", "codex.http-websockets", true},
		{"legacy disabled", "codex: {http-websockets: false}\n", "codex.http-websockets", false},
		{"v8 default", "config-version: 8\nupstream: {codex: {}}\n", "upstream.codex.http-websockets", false},
		{"v8 enabled", "config-version: 8\nupstream: {codex: {http-websockets: true}}\n", "upstream.codex.http-websockets", true},
		{"historical v8", "config-version: 8\noauth: {providers: {codex: {http-websockets: true}}}\n", "upstream.codex.http-websockets", true},
		{"canonical false wins", "config-version: 8\ncodex: {http-websockets: true}\nupstream: {codex: {http-websockets: false}}\n", "upstream.codex.http-websockets", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			check := func(name string, value *Config) {
				t.Helper()
				if value.Codex.HTTPWebsockets != tc.want || value.CloneForRuntime().Codex.HTTPWebsockets != tc.want || value.ForAPIKey().Codex.HTTPWebsockets != tc.want {
					t.Fatalf("%s changed http-websockets", name)
				}
			}
			check("parsed", cfg)
			snapshot, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := ParseConfigBytes(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			check("YAML snapshot", restored)
			encoded, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var jsonConfig Config
			if err = json.Unmarshal(encoded, &jsonConfig); err != nil {
				t.Fatal(err)
			}
			check("JSON snapshot", &jsonConfig)
			file := filepath.Join(t.TempDir(), "config.yaml")
			if err = os.WriteFile(file, []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			if err = SaveConfigPreserveComments(file, cfg, false); err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			restored, err = ParseConfigBytes(saved)
			if err != nil {
				t.Fatal(err)
			}
			check("saved", restored)
			if strings.HasPrefix(tc.name, "legacy") && strings.Contains(string(saved), "config-version: 8") {
				t.Fatal("save migrated legacy configuration")
			}
			var doc yaml.Node
			if err = yaml.Unmarshal(saved, &doc); err != nil {
				t.Fatal(err)
			}
			if tc.want && yamlPath(doc.Content[0], tc.path) == nil {
				t.Fatalf("save lost %s", tc.path)
			}
		})
	}
}
