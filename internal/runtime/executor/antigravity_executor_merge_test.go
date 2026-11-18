package executor

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestAntigravityCoolingPolicyPreservesTOSViolation(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.Config
	}{
		{name: "missing"},
		{name: "disabled", cfg: &config.Config{}},
		{name: "enabled", cfg: &config.Config{Antigravity: config.AntigravityConfig{ModelLevelCooling: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := NewAntigravityExecutor(tc.cfg)
			err := executor.antigravityStatusErr(context.Background(), nil, "", "gemini-pro", http.StatusForbidden, []byte(antigravityTOSBody))
			if err.StatusCode() != http.StatusForbidden || !err.tosViolation || !err.IsCredentialScoped() {
				t.Fatalf("TOS suspension lost terminal credential scope: %#v", err)
			}
			if err.RetryAfter() != nil {
				t.Fatalf("TOS suspension gained a retry delay: %v", err.RetryAfter())
			}
			if err.tosAppealURL != "https://forms.gle/hGzM9MEUv2azZsrb9" || err.tosAppealURLText != "Submit Appeal" {
				t.Fatalf("TOS suspension lost appeal metadata: %#v", err)
			}
		})
	}
}
