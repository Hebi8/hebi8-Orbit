package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

func TestV6MigrationAndThreeLevelProjects(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "orbit.sqlite")

	st, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// Sync a YAML machine + project and verify lossless v6 part/location creation
	err = st.SyncRegistry(ctx, tmpDir, protocol.ProjectsConfig{
		Projects: map[string]protocol.ProjectConfig{
			"orbit": {
				DisplayName:       "hebi8-Orbit",
				RepoIdentity:      "github.com/hebi8/hebi8-orbit",
				DefaultBranch:     "main",
				DefaultCapability: "primary-dev",
				DefaultAdapter:    protocol.AdapterAgy,
				Anchors: map[string]protocol.ProjectAnchorConfig{
					"primary-dev": {Machine: "dev-node", RepoPath: "/srv/dev/hebi8-Orbit"},
				},
			},
		},
	}, protocol.MachinesConfig{
		Machines: map[string]protocol.MachineConfig{
			"dev-node": {
				DisplayName:  "dev-node",
				Transport:    protocol.TransportLocalWS,
				Capabilities: []string{"primary-dev"},
			},
			"vps": {
				DisplayName:  "VPS",
				Transport:    protocol.TransportSSHOutbound,
				Capabilities: []string{"deploy"},
			},
		},
	})
	if err != nil {
		t.Fatalf("sync registry: %v", err)
	}

	proj, err := st.GetProject(ctx, "orbit")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if len(proj.Parts) != 1 {
		t.Fatalf("expected 1 default part, got %d", len(proj.Parts))
	}
	if proj.Parts[0].Kind != protocol.ProjectPartGit {
		t.Fatalf("expected git part kind, got %s", proj.Parts[0].Kind)
	}
	if len(proj.Parts[0].Locations) != 1 || !proj.Parts[0].Locations[0].IsDefault {
		t.Fatalf("expected 1 default location, got %+v", proj.Parts[0].Locations)
	}

	// Add a second location on another machine to part 0
	proj, loc2, _, err := st.CreateProjectLocation(ctx, "orbit", proj.Parts[0].PartID, protocol.CreateProjectLocationRequest{
		MachineID: "mini",
		AbsPath:   "/srv/mini/dev/hebi8-Orbit",
		IsDefault: false,
	}, &protocol.LocationRuntimeState{
		Exists: true,
		IsGit:  true,
		Branch: "main",
		Dirty:  true,
		Ahead:  2,
	}, "", "human")
	if err != nil {
		t.Fatalf("create second location: %v", err)
	}
	if len(proj.Parts[0].Locations) != 2 {
		t.Fatalf("expected 2 locations, got %d", len(proj.Parts[0].Locations))
	}
	if loc2.Runtime == nil || !loc2.Runtime.Dirty || loc2.Runtime.Ahead != 2 {
		t.Fatalf("expected runtime persisted, got %+v", loc2.Runtime)
	}

	// Add a plain folder part
	proj, part2, _, err := st.CreateProjectPart(ctx, "orbit", "设计稿", protocol.ProjectPartFolder, "", "mini", "/srv/mini/Designs", &protocol.LocationRuntimeState{
		Exists: true,
		IsGit:  false,
	}, "human")
	if err != nil {
		t.Fatalf("create folder part: %v", err)
	}
	if part2.Kind != protocol.ProjectPartFolder || len(part2.Locations) != 1 || part2.Locations[0].Isolation != protocol.IsolationInPlace {
		t.Fatalf("unexpected folder part: %+v", part2)
	}
	if len(proj.Parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(proj.Parts))
	}

	// Update project settings (SystemPrompt + SharedEnv)
	sysPrompt := "Always write clean Go tests."
	proj, _, err = st.UpdateProjectSettings(ctx, "orbit", protocol.UpdateProjectSettingsRequest{
		SystemPrompt: &sysPrompt,
		SharedEnv: []protocol.ProjectEnvVar{
			{Key: "NODE_ENV", Value: "development", Secret: false},
			{Key: "API_TOKEN", Value: "secret-val", Secret: true},
		},
	}, "human")
	if err != nil {
		t.Fatalf("update project settings: %v", err)
	}
	if proj.SystemPrompt != sysPrompt || len(proj.SharedEnv) != 2 || !proj.SharedEnv[1].Secret {
		t.Fatalf("unexpected project settings: %+v", proj)
	}

	// Dismiss suggestion & verify
	if err := st.DismissProjectSuggestion(ctx, "orbit", proj.Parts[0].PartID, "home-node", "/srv/home-node/orbit"); err != nil {
		t.Fatalf("dismiss suggestion: %v", err)
	}
	dismissed, err := st.ListDismissedSuggestions(ctx, "orbit")
	if err != nil {
		t.Fatalf("list dismissed: %v", err)
	}
	if !dismissed[proj.Parts[0].PartID+"|home-node|/srv/home-node/orbit"] {
		t.Fatalf("expected dismissed key present, got %+v", dismissed)
	}

	// Update machine metadata (role + allowedRoots)
	workerRole := protocol.MachineRoleWorker
	if _, err := st.UpdateMachineMetadata(ctx, "vps", protocol.UpdateMachineRequest{
		Role:         &workerRole,
		AllowedRoots: []string{"/srv/apps", "/srv/apps"},
	}, "human"); err != nil {
		t.Fatalf("update machine metadata: %v", err)
	}

	// Verify discoveryId initialized & Bootstrap includes parts/roles/allowedRoots
	did, err := st.EnsureDiscoveryID(ctx)
	if err != nil || len(did) < 10 {
		t.Fatalf("unexpected discoveryId: %q err=%v", did, err)
	}
	boot, err := st.Bootstrap(ctx, nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	foundOrbit := false
	for _, p := range boot.Projects {
		if p.ProjectID == "orbit" {
			foundOrbit = true
			if len(p.Parts) != 2 || p.SystemPrompt != sysPrompt || len(p.SharedEnv) != 2 {
				t.Fatalf("bootstrap missing v3 project fields: %+v", p)
			}
		}
	}
	if !foundOrbit {
		t.Fatal("orbit project not in bootstrap")
	}

	// Verify FolderChanges persistence & promotion flag on attempt
	taskRes, err := st.CreateTask(ctx, CreateTaskParams{
		ProjectID:     "orbit",
		Goal:          "Draft copy",
		MachineID:     "dev-node",
		PartID:        part2.PartID,
		LocationID:    part2.Locations[0].LocationID,
		Isolation:     protocol.IsolationInPlace,
		CreateAttempt: true,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if taskRes.Attempt == nil || taskRes.Attempt.Isolation != protocol.IsolationInPlace {
		t.Fatalf("expected in_place attempt, got %+v", taskRes.Attempt)
	}
	if err := st.SaveAttemptFolderChanges(ctx, taskRes.Attempt.AttemptID, protocol.FolderChangesSummary{
		TotalAdds: 12,
		TotalDels: 3,
		Files: []protocol.FolderChangeItem{
			{Path: "README.md", Kind: "mod", Insertions: 12, Deletions: 3},
		},
	}); err != nil {
		t.Fatalf("save folder changes: %v", err)
	}
	if err := st.MarkAttemptFolderChangesPromoted(ctx, taskRes.Attempt.AttemptID); err != nil {
		t.Fatalf("mark promoted: %v", err)
	}
	tl, err := st.GetTaskTimeline(ctx, taskRes.Task.TaskID, 0, false)
	if err != nil {
		t.Fatalf("get timeline: %v", err)
	}
	if len(tl.Attempts) != 1 || tl.Attempts[0].FolderChanges == nil || !tl.Attempts[0].FolderChanges.Promoted || tl.Attempts[0].FolderChanges.TotalAdds != 12 {
		t.Fatalf("unexpected attempt folder changes: %+v", tl.Attempts)
	}

	// Verify pre-v6 hot backup runs cleanly when upgrading a simulated v5 DB
	v5Path := filepath.Join(tmpDir, "v5.sqlite")
	st5, err := Open(ctx, v5Path)
	if err != nil {
		t.Fatalf("open v5 base: %v", err)
	}
	if _, err := st5.WriteDB().ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 6`); err != nil {
		t.Fatalf("simulate v5: %v", err)
	}
	_ = st5.Close()

	stUp, err := Open(ctx, v5Path)
	if err != nil {
		t.Fatalf("re-open upgraded v6: %v", err)
	}
	_ = stUp.Close()
	if _, err := os.Stat(v5Path + ".pre-v6.bak"); err != nil {
		t.Fatalf("expected .pre-v6.bak backup file: %v", err)
	}
}

func TestProjectPartUpdateAndKeepEmptyPartOnDeleteLastLocation(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	st, err := Open(ctx, filepath.Join(tmpDir, "orbit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	err = st.SyncRegistry(ctx, tmpDir, protocol.ProjectsConfig{
		Projects: map[string]protocol.ProjectConfig{
			"orbit": {
				DisplayName:       "hebi8-Orbit",
				RepoIdentity:      "github.com/hebi8/hebi8-orbit",
				DefaultBranch:     "main",
				DefaultCapability: "primary-dev",
				DefaultAdapter:    protocol.AdapterAgy,
				Anchors: map[string]protocol.ProjectAnchorConfig{
					"primary-dev": {Machine: "hub-node", RepoPath: "/home/orbit/repo"},
				},
			},
		},
	}, protocol.MachinesConfig{
		Machines: map[string]protocol.MachineConfig{
			"hub-node": {
				DisplayName:  "hub-node",
				Transport:    protocol.TransportLocalWS,
				Capabilities: []string{"primary-dev"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	proj, err := st.GetProject(ctx, "orbit")
	if err != nil {
		t.Fatal(err)
	}
	if len(proj.Parts) != 1 || len(proj.Parts[0].Locations) != 1 {
		t.Fatalf("expected 1 part with 1 location, got %+v", proj.Parts)
	}
	part1 := proj.Parts[0]
	loc1 := part1.Locations[0]

	// Update part name & sort order independently
	newName := "Core Backend"
	newOrder := 5
	proj, updatedPart, ev, err := st.UpdateProjectPart(ctx, "orbit", part1.PartID, protocol.UpdateProjectPartRequest{
		Name:      &newName,
		SortOrder: &newOrder,
	}, "human")
	if err != nil {
		t.Fatalf("UpdateProjectPart failed: %v", err)
	}
	if updatedPart.Name != "Core Backend" || updatedPart.SortOrder != 5 || ev.Kind != protocol.EventProjectChanged {
		t.Fatalf("unexpected updated part=%+v ev=%+v", updatedPart, ev)
	}

	// Empty name rejected with ErrInvalidInput; missing part returns ErrNotFound
	blank := "   "
	if _, _, _, err := st.UpdateProjectPart(ctx, "orbit", part1.PartID, protocol.UpdateProjectPartRequest{Name: &blank}, "human"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for blank part name, got %v", err)
	}
	if _, _, _, err := st.UpdateProjectPart(ctx, "orbit", "part_missing", protocol.UpdateProjectPartRequest{Name: &newName}, "human"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown part, got %v", err)
	}

	// Add a second part with 1 location, then delete the last location of part1:
	// part1 MUST remain with empty non-nil Locations slice, and anchors mirror shifts to part2.
	proj, part2, _, err := st.CreateProjectPart(ctx, "orbit", "Design Docs", protocol.ProjectPartFolder, "", "hub-node", "/home/orbit/docs", nil, "human")
	if err != nil {
		t.Fatal(err)
	}
	proj, _, err = st.DeleteProjectLocation(ctx, "orbit", part1.PartID, loc1.LocationID, "human")
	if err != nil {
		t.Fatalf("delete last location of part1: %v", err)
	}
	if len(proj.Parts) != 2 {
		t.Fatalf("deleting last location must retain part, got %d parts", len(proj.Parts))
	}
	var foundPart1 *protocol.ProjectPartView
	for i := range proj.Parts {
		if proj.Parts[i].PartID == part1.PartID {
			foundPart1 = &proj.Parts[i]
		}
	}
	if foundPart1 == nil || foundPart1.Locations == nil || len(foundPart1.Locations) != 0 {
		t.Fatalf("expected part1 retained with empty non-nil locations slice, got %+v", foundPart1)
	}
	if proj.Anchors["primary-dev"].RepoPath != "/home/orbit/docs" {
		t.Fatalf("expected anchors mirror to fall back to part2 location, got %+v", proj.Anchors)
	}

	// Delete last location of part2 as well -> both parts retained empty AND stale anchors cleared!
	proj, _, err = st.DeleteProjectLocation(ctx, "orbit", part2.PartID, part2.Locations[0].LocationID, "human")
	if err != nil {
		t.Fatalf("delete last location of part2: %v", err)
	}
	if len(proj.Parts) != 2 {
		t.Fatalf("expected both empty parts retained, got %d", len(proj.Parts))
	}
	if len(proj.Anchors) != 0 {
		t.Fatalf("expected anchors mirror cleared when zero locations remain, got %+v", proj.Anchors)
	}

	// Explicit DeleteProjectPart removes the entire part
	proj, _, err = st.DeleteProjectPart(ctx, "orbit", part1.PartID, "human")
	if err != nil {
		t.Fatalf("delete part1: %v", err)
	}
	if len(proj.Parts) != 1 || proj.Parts[0].PartID != part2.PartID {
		t.Fatalf("expected only part2 remaining after DeleteProjectPart, got %+v", proj.Parts)
	}
}
