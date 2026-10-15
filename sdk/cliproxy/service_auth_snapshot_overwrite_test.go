package cliproxy

import (
	"context"
	"testing"

	internalregistry "github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func newSnapshotOverwriteTestService(t *testing.T, authID string) (*Service, *coreauth.Manager, *coreauth.Auth) {
	t.Helper()
	reg := internalregistry.GetGlobalRegistry()
	reg.UnregisterClient(authID)
	t.Cleanup(func() { reg.UnregisterClient(authID) })
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&syncTestExecutor{})
	auth := &coreauth.Auth{
		ID:         authID,
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Prefix:     "old",
		Attributes: map[string]string{"plan_type": "pro", "path": "/path/to/" + authID + ".json"},
	}
	registered, errRegister := manager.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	return &Service{cfg: &config.Config{}, coreManager: manager}, manager, registered
}

func persistPrefix(t *testing.T, manager *coreauth.Manager, base *coreauth.Auth, prefix string) *coreauth.Auth {
	t.Helper()
	next := base.Clone()
	next.Prefix = prefix
	updated, errUpdate := manager.Update(context.Background(), next)
	if errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}
	return updated
}

func revisioned(auth *coreauth.Auth, rev uint64) watcher.AuthUpdate {
	update := watcher.AuthUpdate{Action: watcher.AuthUpdateActionModify, ID: auth.ID, Auth: auth}
	update.SetRevision(rev)
	return update
}

// A file snapshot read before a management persist can be applied after it.
// The persist's own update must then restore the persisted state. Issue 6559.
func TestHandleAuthUpdates_PersistRestoresStateOverwrittenByEarlierFileSnapshot(t *testing.T) {
	service, manager, registered := newSnapshotOverwriteTestService(t, "codex-snapshot-overwrite-auth")

	staleSnapshot := registered.Clone()
	staleSnapshot.Generation = 0 // File-watcher snapshots are unversioned.
	persisted := persistPrefix(t, manager, registered, "new")

	service.handleAuthUpdate(context.Background(), revisioned(staleSnapshot, 1))
	if current, _ := manager.GetByID(registered.ID); current.Prefix != "old" {
		t.Fatalf("setup: snapshot prefix = %q, want old", current.Prefix)
	}
	service.handleAuthUpdate(context.Background(), revisioned(persisted, 2))

	current, ok := manager.GetByID(registered.ID)
	if !ok || current.Prefix != "new" {
		t.Fatalf("prefix = %q, want persisted prefix new", current.Prefix)
	}
}

// The restore must not let a versioned update older than the state that the
// snapshot replaced win. That update stays stale.
func TestHandleAuthUpdates_OlderPersistDoesNotRestoreOverSnapshot(t *testing.T) {
	service, manager, registered := newSnapshotOverwriteTestService(t, "codex-snapshot-older-persist-auth")

	older := persistPrefix(t, manager, registered, "older")
	newer := persistPrefix(t, manager, older, "newer")
	snapshot := newer.Clone()
	snapshot.Generation = 0
	snapshot.Prefix = "snapshot"

	service.handleAuthUpdate(context.Background(), revisioned(snapshot, 1))
	service.handleAuthUpdate(context.Background(), revisioned(older, 2))

	current, ok := manager.GetByID(registered.ID)
	if !ok || current.Prefix != "snapshot" {
		t.Fatalf("prefix = %q, want snapshot; an older persist must stay stale", current.Prefix)
	}
}
