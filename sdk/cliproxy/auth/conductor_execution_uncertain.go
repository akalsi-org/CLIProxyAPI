package auth

import (
	"context"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Ambiguous execution failures describe transport uncertainty, not credential availability.
func (m *Manager) recordUncertainExecutionResult(ctx context.Context, auth *Auth, provider, model, routeModel string, opts cliproxyexecutor.Options, err error, ephemeral bool) {
	result := Result{AuthID: auth.ID, Provider: provider, Model: model, RouteModel: routeModel, Error: resultErrorFromError(err), Options: opts}
	if ephemeral {
		m.reportHomeResult(ctx, result, auth)
		return
	}
	m.recordAvailabilityNeutralResult(ctx, result)
}
