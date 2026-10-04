package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAntigravityProjectIDLayouts(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"missing", "port: 8317\n", ""},
		{"legacy", "antigravity: {project-id: legacy-project, sensitive-words: [private]}\n", "legacy-project"},
		{"oauth", "oauth: {providers: {antigravity: {project-id: oauth-project}}}\n", "oauth-project"},
		{"canonical", "upstream: {antigravity: {project-id: default-cli-project}}\n", "default-cli-project"},
		{"canonical wins", "antigravity: {project-id: legacy-project}\noauth: {providers: {antigravity: {project-id: oauth-project}}}\nupstream: {antigravity: {project-id: canonical-project}}\n", "canonical-project"},
		{"oauth wins legacy", "antigravity: {project-id: legacy-project}\noauth: {providers: {antigravity: {project-id: oauth-project}}}\n", "oauth-project"},
		{"canonical empty wins", "antigravity: {project-id: legacy-project}\noauth: {providers: {antigravity: {project-id: oauth-project}}}\nupstream: {antigravity: {project-id: ''}}\n", ""},
		{"oauth empty wins legacy", "antigravity: {project-id: legacy-project}\noauth: {providers: {antigravity: {project-id: ''}}}\n", ""},
		{"canonical whitespace wins", "antigravity: {project-id: legacy-project}\nupstream: {antigravity: {project-id: '   '}}\n", "   "},
		{"canonical null wins", "antigravity: {project-id: legacy-project}\nupstream: {antigravity: {project-id: null}}\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Antigravity.ProjectID != tc.want {
				t.Fatalf("ProjectID = %q, want %q", cfg.Antigravity.ProjectID, tc.want)
			}
			migrated, _, err := NormalizeConfigLayout([]byte(tc.raw), true)
			if err != nil {
				t.Fatal(err)
			}
			if err = ValidateV8Config(migrated); err != nil {
				t.Fatalf("invalid migration: %v", err)
			}
			reloaded, err := ParseConfigBytes(migrated)
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.Antigravity.ProjectID != tc.want {
				t.Fatalf("migrated ProjectID = %q, want %q", reloaded.Antigravity.ProjectID, tc.want)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err = os.WriteFile(path, migrated, 0600); err != nil {
				t.Fatal(err)
			}
			if err = SaveConfigPreserveComments(path, reloaded, true); err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err = ValidateV8Config(saved); err != nil {
				t.Fatalf("invalid saved config: %v", err)
			}
			roundTrip, err := ParseConfigBytes(saved)
			if err != nil {
				t.Fatal(err)
			}
			if roundTrip.Antigravity.ProjectID != tc.want {
				t.Fatalf("saved ProjectID = %q, want %q", roundTrip.Antigravity.ProjectID, tc.want)
			}
			if strings.Contains(tc.raw, "sensitive-words") && (len(roundTrip.Antigravity.SensitiveWords) != 1 || roundTrip.Antigravity.SensitiveWords[0] != "private") {
				t.Fatal("project migration changed unrelated provider configuration")
			}
		})
	}
}

func TestAntigravityProjectIDJSON(t *testing.T) {
	for _, project := range []string{"", "default-cli-project"} {
		cfg := AntigravityConfig{ProjectID: project}
		encoded, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"project-id"`) != (project != "") {
			t.Fatalf("project-id omitempty failed: %s", encoded)
		}
		var reloaded AntigravityConfig
		if err = json.Unmarshal(encoded, &reloaded); err != nil {
			t.Fatal(err)
		}
		if reloaded.ProjectID != project {
			t.Fatalf("JSON round trip = %q, want %q", reloaded.ProjectID, project)
		}
	}
}
