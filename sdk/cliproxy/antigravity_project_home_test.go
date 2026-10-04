package cliproxy

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestAntigravityProjectOverrideHomeAuthority(t *testing.T) {
	for _, remoteProject := range []string{"", "default-cli-project"} {
		t.Run("remote="+remoteProject, func(t *testing.T) {
			local := &config.Config{Host: "127.0.0.1", Port: 8317}
			local.Home.Enabled = true
			local.Home.NodeID = "synthetic-node"
			local.Antigravity.ProjectID = "local-project-must-not-override-home"
			remote := &config.Config{}
			remote.Antigravity.ProjectID = remoteProject
			client, _ := newHomePluginTaskTestClient(t, nil, 0)
			service := &Service{cfg: local}
			work, err := service.stageHomeOverlayWithClient(context.Background(), remote, client)
			if err != nil {
				t.Fatal(err)
			}
			if work.config == nil || work.config.Antigravity.ProjectID != remoteProject {
				t.Fatal("local project override changed authoritative Home provider configuration")
			}
			if work.config.Host != local.Host || work.config.Port != local.Port || work.config.Home.NodeID != local.Home.NodeID {
				t.Fatal("project override changed local transport or Home identity")
			}
			if local.Antigravity.ProjectID != "local-project-must-not-override-home" || remote.Antigravity.ProjectID != remoteProject {
				t.Fatal("Home overlay changed its source configurations")
			}
		})
	}
}
