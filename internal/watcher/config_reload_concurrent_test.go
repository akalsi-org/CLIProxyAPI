package watcher

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// A management save can land while a reload is in progress. The reload must
// record the hash of the bytes it applied, not of a second read. Otherwise the
// save's own reload matches that hash, skips, and leaves the old config active.
func TestReloadConfigIfChangedRecordsAppliedBytesWhenSaveLandsDuringReload(t *testing.T) {
	tmp := t.TempDir()
	authDir := filepath.Join(tmp, "auths")
	if errMkdir := os.MkdirAll(authDir, 0o700); errMkdir != nil {
		t.Fatalf("create auth dir: %v", errMkdir)
	}
	cfgPath := filepath.Join(tmp, "config.yaml")
	first := []byte("port: 8317\napi-keys:\n  - first-key\n")
	second := []byte("port: 8317\napi-keys:\n  - second-key\n")
	if errWrite := os.WriteFile(cfgPath, first, 0o600); errWrite != nil {
		t.Fatalf("write first config: %v", errWrite)
	}

	var applied []string
	w := &Watcher{
		configPath:      cfgPath,
		authDir:         authDir,
		mirroredAuthDir: authDir,
		lastAuthHashes:  make(map[string]string),
	}
	w.reloadCallback = func(cfg *config.Config) {
		key := ""
		if len(cfg.APIKeys) > 0 {
			key = cfg.APIKeys[0]
		}
		applied = append(applied, key)
		if len(applied) == 1 {
			// Simulate a concurrent management save after this reload read the file.
			if errWrite := os.WriteFile(cfgPath, second, 0o600); errWrite != nil {
				t.Errorf("write second config: %v", errWrite)
			}
		}
	}
	w.SetConfig(&config.Config{AuthDir: authDir})

	w.reloadConfigIfChanged()
	sum := sha256.Sum256(first)
	w.clientsMutex.RLock()
	gotHash := w.lastConfigHash
	w.clientsMutex.RUnlock()
	if want := hex.EncodeToString(sum[:]); gotHash != want {
		t.Fatalf("lastConfigHash = %s, want hash of applied bytes %s", gotHash, want)
	}

	// The save's own reload must apply the second config.
	w.reloadConfigIfChanged()
	if len(applied) != 2 || applied[0] != "first-key" || applied[1] != "second-key" {
		t.Fatalf("applied configs = %q, want [first-key second-key]", applied)
	}
}
