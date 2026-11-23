package protocol_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

func TestSQLInClauseAndEnums(t *testing.T) {
	clause := protocol.SQLInClause(protocol.AllApprovalDecidedBy)
	if clause != "'human', 'policy'" {
		t.Fatalf("unexpected SQLInClause: %s", clause)
	}
	if protocol.ApprovalDecidedBy("pa").IsValid() {
		t.Fatalf("expected 'pa' to be invalid ApprovalDecidedBy")
	}
	if !protocol.AttemptWaitingResource.IsValid() {
		t.Fatalf("expected waiting_resource to be valid AttemptState")
	}
	if len(protocol.ExportedEnums()) < 20 {
		t.Fatalf("expected all protocol enums exported, got %d", len(protocol.ExportedEnums()))
	}
	if len(protocol.ExportedDTOTypes()) < 20 {
		t.Fatalf("expected all protocol DTOs exported, got %d", len(protocol.ExportedDTOTypes()))
	}
}

func TestLoadExampleConfigs(t *testing.T) {
	root := filepath.Join("..", "..", "configs")

	hubCfg, err := protocol.LoadHubConfig(filepath.Join(root, "hub.example.yaml"))
	if err != nil {
		t.Fatalf("LoadHubConfig failed: %v", err)
	}
	if hubCfg.ListenAddr != "127.0.0.1:4310" || !strings.Contains(hubCfg.HubOrigin, "example.ts.net") {
		t.Fatalf("unexpected hub example config: %+v", hubCfg)
	}

	machinesCfg, err := protocol.LoadMachinesConfig(filepath.Join(root, "machines.example.yaml"))
	if err != nil {
		t.Fatalf("LoadMachinesConfig failed: %v", err)
	}
	if len(machinesCfg.Machines) != 4 {
		t.Fatalf("expected 4 example machines, got %d", len(machinesCfg.Machines))
	}
	if machinesCfg.Machines["deploy-vps"].Transport != protocol.TransportSSHOutbound {
		t.Fatalf("expected deploy-vps ssh_outbound transport")
	}

	projectsCfg, err := protocol.LoadProjectsConfig(filepath.Join(root, "projects.example.yaml"))
	if err != nil {
		t.Fatalf("LoadProjectsConfig failed: %v", err)
	}
	if len(projectsCfg.Projects) != 2 {
		t.Fatalf("expected 2 example projects, got %d", len(projectsCfg.Projects))
	}
	if projectsCfg.Projects["demo-app"].IDEScheme != protocol.IDECursor {
		t.Fatalf("expected demo-app ideScheme cursor")
	}
}
