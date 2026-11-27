package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
)

func backfillV6ProjectsTx(ctx context.Context, tx *sql.Tx, now int64) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT project_id, display_name, repo_identity, config_json
		FROM projects
		WHERE project_id NOT IN (?, ?)
	`, protocol.ScratchProjectID, protocol.PAProjectID)
	if err != nil {
		return err
	}
	type projRow struct {
		id, name, repoID, cfgJSON string
	}
	var list []projRow
	for rows.Next() {
		var r projRow
		if err := rows.Scan(&r.id, &r.name, &r.repoID, &r.cfgJSON); err != nil {
			rows.Close()
			return err
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, r := range list {
		if err := ensureDefaultPartAndLocationsTx(ctx, tx, r.id, r.name, r.repoID, r.cfgJSON, now); err != nil {
			return fmt.Errorf("backfill project %s: %w", r.id, err)
		}
	}
	return nil
}

func inferPartKind(repoIdentity string) protocol.ProjectPartKind {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(repoIdentity)), "folder:") {
		return protocol.ProjectPartFolder
	}
	return protocol.ProjectPartGit
}

func defaultIsolationForKind(kind protocol.ProjectPartKind) protocol.LocationIsolation {
	if kind == protocol.ProjectPartFolder {
		return protocol.IsolationInPlace
	}
	return protocol.IsolationWorktree
}

func ensureDefaultPartAndLocationsTx(ctx context.Context, tx *sql.Tx, projectID, displayName, repoIdentity, cfgJSON string, now int64) error {
	if projectID == "" || protocol.IsVirtualProjectID(projectID) {
		return nil
	}
	var cfg protocol.ProjectConfig
	_ = json.Unmarshal([]byte(cfgJSON), &cfg)

	var firstPartID string
	var firstPartKind string
	err := tx.QueryRowContext(ctx, `
		SELECT part_id, kind FROM project_parts
		WHERE project_id = ?
		ORDER BY sort_order ASC, created_at ASC
		LIMIT 1
	`, projectID).Scan(&firstPartID, &firstPartKind)
	if errors.Is(err, sql.ErrNoRows) {
		kind := inferPartKind(repoIdentity)
		firstPartID = newID("prt")
		firstPartKind = string(kind)
		partName := strings.TrimSpace(displayName)
		if partName == "" {
			partName = projectID
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO project_parts (part_id, project_id, name, kind, repo_identity, sort_order, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 0, ?, ?)
		`, firstPartID, projectID, partName, string(kind), strings.TrimSpace(repoIdentity), now, now); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	if len(cfg.Anchors) == 0 {
		return nil
	}

	// Collect existing (machine, absPath) across all parts of this project to stay strictly non-destructive.
	existing := map[string]bool{}
	hasDefault := false
	lRows, err := tx.QueryContext(ctx, `
		SELECT machine_id, abs_path, is_default, part_id FROM project_locations WHERE project_id = ?
	`, projectID)
	if err != nil {
		return err
	}
	for lRows.Next() {
		var m, p, pid string
		var isDef int
		if err := lRows.Scan(&m, &p, &isDef, &pid); err != nil {
			lRows.Close()
			return err
		}
		existing[m+"|"+filepath.Clean(p)] = true
		if pid == firstPartID && isDef != 0 {
			hasDefault = true
		}
	}
	lRows.Close()

	// Sort anchor keys deterministically, placing defaultCapability first.
	keys := make([]string, 0, len(cfg.Anchors))
	for k := range cfg.Anchors {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i] == cfg.DefaultCapability {
			return true
		}
		if keys[j] == cfg.DefaultCapability {
			return false
		}
		return keys[i] < keys[j]
	})

	iso := defaultIsolationForKind(protocol.ProjectPartKind(firstPartKind))
	if protocol.ProjectPartKind(firstPartKind) != protocol.ProjectPartFolder {
		var inherited string
		if err := tx.QueryRowContext(ctx, `
			SELECT l.isolation
			FROM project_locations l
			JOIN project_parts p ON p.part_id = l.part_id
			WHERE l.project_id = ? AND p.kind != 'folder'
			ORDER BY l.updated_at DESC
			LIMIT 1
		`, projectID).Scan(&inherited); err == nil && protocol.LocationIsolation(inherited).IsValid() {
			iso = protocol.LocationIsolation(inherited)
		}
	}
	for _, k := range keys {
		a := cfg.Anchors[k]
		m := strings.TrimSpace(a.Machine)
		p := filepath.Clean(strings.TrimSpace(a.RepoPath))
		if m == "" || p == "" || p == "." {
			continue
		}
		dedupKey := m + "|" + p
		if existing[dedupKey] {
			continue
		}
		existing[dedupKey] = true
		isDef := 0
		if !hasDefault {
			isDef = 1
			hasDefault = true
		}
		locID := newID("loc")
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO project_locations (
				location_id, part_id, project_id, machine_id, abs_path, is_default, isolation, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, locID, firstPartID, projectID, m, p, isDef, string(iso), now, now); err != nil {
			return err
		}
	}
	return nil
}

func generateDiscoveryID() string {
	hexPart, err := randomSecretHex(3)
	if err != nil || len(hexPart) < 6 {
		return "orb-" + strings.ToLower(newID("d")[2:8])
	}
	return "orb-" + hexPart[:6]
}

func ensureV6NetworkDefaultsTx(ctx context.Context, tx *sql.Tx, now int64) error {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT value_json FROM hub_settings WHERE key = 'network_settings'`).Scan(&raw)
	var ns DynamicNetworkSettings
	if err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &ns)
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	changed := false
	if strings.TrimSpace(ns.DiscoveryID) == "" {
		ns.DiscoveryID = generateDiscoveryID()
		changed = true
	}
	if len(ns.DutyPriority) == 0 {
		mRows, err := tx.QueryContext(ctx, `
			SELECT machine_id FROM machines
			WHERE role = 'full'
			ORDER BY CASE WHEN transport_mode = 'local_ws' THEN 0 ELSE 1 END, machine_id ASC
		`)
		if err == nil {
			for mRows.Next() {
				var mid string
				if mRows.Scan(&mid) == nil && mid != "" {
					ns.DutyPriority = append(ns.DutyPriority, mid)
				}
			}
			mRows.Close()
		}
		if len(ns.DutyPriority) > 0 {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	b, err := json.Marshal(ns)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO hub_settings (key, value_json, updated_at)
		VALUES ('network_settings', ?, ?)
		ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at
	`, string(b), now)
	return err
}

// EnsureDiscoveryID returns the workspace's opaque discovery ID (e.g. orb-9f4a2c), creating one if missing.
func (s *Store) EnsureDiscoveryID(ctx context.Context) (string, error) {
	ns, err := s.GetDynamicNetworkSettings(ctx)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(ns.DiscoveryID) != "" {
		return ns.DiscoveryID, nil
	}
	ns.DiscoveryID = generateDiscoveryID()
	if err := s.SetHubSetting(ctx, "network_settings", ns); err != nil {
		return "", err
	}
	return ns.DiscoveryID, nil
}

func loadProjectPartsTx(ctx context.Context, tx *sql.Tx, projectID string) (map[string][]protocol.ProjectPartView, error) {
	partsByProject := map[string][]protocol.ProjectPartView{}
	partIndex := map[string]*protocol.ProjectPartView{}

	var pRows *sql.Rows
	var err error
	if projectID != "" {
		pRows, err = tx.QueryContext(ctx, `
			SELECT part_id, project_id, name, kind, repo_identity, sort_order
			FROM project_parts
			WHERE project_id = ?
			ORDER BY sort_order ASC, created_at ASC
		`, projectID)
	} else {
		pRows, err = tx.QueryContext(ctx, `
			SELECT part_id, project_id, name, kind, repo_identity, sort_order
			FROM project_parts
			ORDER BY project_id ASC, sort_order ASC, created_at ASC
		`)
	}
	if err != nil {
		return nil, err
	}
	for pRows.Next() {
		var p protocol.ProjectPartView
		var kindStr string
		if err := pRows.Scan(&p.PartID, &p.ProjectID, &p.Name, &kindStr, &p.RepoIdentity, &p.SortOrder); err != nil {
			pRows.Close()
			return nil, err
		}
		p.Kind = protocol.ProjectPartKind(kindStr)
		p.Locations = []protocol.ProjectLocationView{}
		partsByProject[p.ProjectID] = append(partsByProject[p.ProjectID], p)
	}
	if err := pRows.Err(); err != nil {
		pRows.Close()
		return nil, err
	}
	pRows.Close()

	// Build pointer map after all parts are appended (slice growth safe).
	for pid := range partsByProject {
		list := partsByProject[pid]
		for i := range list {
			partIndex[list[i].PartID] = &list[i]
		}
	}

	var lRows *sql.Rows
	if projectID != "" {
		lRows, err = tx.QueryContext(ctx, `
			SELECT l.location_id, l.part_id, l.machine_id, l.abs_path, l.is_default, l.isolation, l.created_at, l.updated_at,
			       r.exists_flag, r.is_git, r.branch, r.head_sha, r.dirty, r.ahead, r.behind, r.checked_at
			FROM project_locations l
			LEFT JOIN location_runtime_state r ON r.location_id = l.location_id
			WHERE l.project_id = ?
			ORDER BY l.is_default DESC, l.created_at ASC
		`, projectID)
	} else {
		lRows, err = tx.QueryContext(ctx, `
			SELECT l.location_id, l.part_id, l.machine_id, l.abs_path, l.is_default, l.isolation, l.created_at, l.updated_at,
			       r.exists_flag, r.is_git, r.branch, r.head_sha, r.dirty, r.ahead, r.behind, r.checked_at
			FROM project_locations l
			LEFT JOIN location_runtime_state r ON r.location_id = l.location_id
			ORDER BY l.is_default DESC, l.created_at ASC
		`)
	}
	if err != nil {
		return nil, err
	}
	for lRows.Next() {
		var loc protocol.ProjectLocationView
		var isDef int
		var isoStr string
		var rExists, rIsGit, rDirty, rAhead, rBehind, rChecked sql.NullInt64
		var rBranch, rHead sql.NullString
		if err := lRows.Scan(
			&loc.LocationID, &loc.PartID, &loc.MachineID, &loc.AbsPath, &isDef, &isoStr, &loc.CreatedAt, &loc.UpdatedAt,
			&rExists, &rIsGit, &rBranch, &rHead, &rDirty, &rAhead, &rBehind, &rChecked,
		); err != nil {
			lRows.Close()
			return nil, err
		}
		loc.IsDefault = isDef != 0
		loc.Isolation = protocol.LocationIsolation(isoStr)
		if rChecked.Valid {
			loc.Runtime = &protocol.LocationRuntimeState{
				Exists:    rExists.Valid && rExists.Int64 != 0,
				IsGit:     rIsGit.Valid && rIsGit.Int64 != 0,
				Branch:    rBranch.String,
				HeadSHA:   rHead.String,
				Dirty:     rDirty.Valid && rDirty.Int64 != 0,
				Ahead:     int(rAhead.Int64),
				Behind:    int(rBehind.Int64),
				CheckedAt: rChecked.Int64,
			}
		}
		if p, ok := partIndex[loc.PartID]; ok {
			p.Locations = append(p.Locations, loc)
		}
	}
	if err := lRows.Err(); err != nil {
		lRows.Close()
		return nil, err
	}
	lRows.Close()

	return partsByProject, nil
}

// syncProjectAnchorsMirrorTx refreshes config_json.anchors from project_locations so legacy consumers stay consistent.
func syncProjectAnchorsMirrorTx(ctx context.Context, tx *sql.Tx, projectID string, now int64) error {
	var cfgJSON string
	if err := tx.QueryRowContext(ctx, `SELECT config_json FROM projects WHERE project_id = ?`, projectID).Scan(&cfgJSON); err != nil {
		return err
	}
	var cfg protocol.ProjectConfig
	_ = json.Unmarshal([]byte(cfgJSON), &cfg)
	if cfg.Anchors == nil {
		cfg.Anchors = map[string]protocol.ProjectAnchorConfig{}
	}
	if cfg.DefaultCapability == "" {
		cfg.DefaultCapability = "primary-dev"
	}

	partsMap, err := loadProjectPartsTx(ctx, tx, projectID)
	if err != nil {
		return err
	}
	parts := partsMap[projectID]
	newAnchors := map[string]protocol.ProjectAnchorConfig{}
	var primaryLoc *protocol.ProjectLocationView
	for _, part := range parts {
		if len(part.Locations) == 0 {
			continue
		}
		picked := part.Locations[0]
		for _, loc := range part.Locations {
			if loc.IsDefault {
				picked = loc
				break
			}
		}
		primaryLoc = &picked
		break
	}
	if primaryLoc != nil {
		newAnchors[cfg.DefaultCapability] = protocol.ProjectAnchorConfig{
			Machine:  primaryLoc.MachineID,
			RepoPath: primaryLoc.AbsPath,
		}
		for _, part := range parts {
			for _, loc := range part.Locations {
				if loc.MachineID == primaryLoc.MachineID && loc.AbsPath == primaryLoc.AbsPath {
					continue
				}
				key := loc.MachineID
				if _, exists := newAnchors[key]; exists {
					key = loc.LocationID
				}
				newAnchors[key] = protocol.ProjectAnchorConfig{
					Machine:  loc.MachineID,
					RepoPath: loc.AbsPath,
				}
			}
		}
	}
	cfg.Anchors = newAnchors
	updatedCfg, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE projects SET config_json = ?, updated_at = ? WHERE project_id = ?`, string(updatedCfg), now, projectID)
	return err
}

// UpdateProjectSettings patches project name, default adapter, shared env, and system prompt.
func (s *Store) UpdateProjectSettings(ctx context.Context, projectID string, req protocol.UpdateProjectSettingsRequest, actorID string) (protocol.ProjectView, protocol.EventRecord, error) {
	var pv protocol.ProjectView
	var ev protocol.EventRecord
	now := time.Now().UnixMilli()
	if strings.TrimSpace(projectID) == "" || protocol.IsVirtualProjectID(projectID) {
		return pv, ev, fmt.Errorf("%w: invalid projectId", ErrInvalidInput)
	}
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var displayName, cfgJSON, sysPrompt, sharedEnvJSON string
		var archived int
		err := tx.QueryRowContext(ctx, `
			SELECT display_name, config_json, system_prompt, shared_env_json, archived
			FROM projects WHERE project_id = ?
		`, projectID).Scan(&displayName, &cfgJSON, &sysPrompt, &sharedEnvJSON, &archived)
		if errors.Is(err, sql.ErrNoRows) || archived != 0 {
			return fmt.Errorf("%w: project %s", ErrNotFound, projectID)
		}
		if err != nil {
			return err
		}

		var cfg protocol.ProjectConfig
		_ = json.Unmarshal([]byte(cfgJSON), &cfg)

		if req.DisplayName != nil {
			trimmed := strings.TrimSpace(*req.DisplayName)
			if trimmed == "" {
				return fmt.Errorf("%w: displayName cannot be empty", ErrInvalidInput)
			}
			displayName = trimmed
			cfg.DisplayName = trimmed
		}
		if req.DefaultAdapter != nil {
			if !protocol.IsSessionAdapterImplemented(*req.DefaultAdapter) {
				return fmt.Errorf("%w: unsupported adapter %q", ErrInvalidInput, *req.DefaultAdapter)
			}
			cfg.DefaultAdapter = *req.DefaultAdapter
		}
		if req.SystemPrompt != nil {
			sysPrompt = *req.SystemPrompt
		}
		if req.SharedEnv != nil {
			cleaned := make([]protocol.ProjectEnvVar, 0, len(req.SharedEnv))
			seen := map[string]bool{}
			for _, item := range req.SharedEnv {
				k := strings.TrimSpace(item.Key)
				if k == "" || seen[k] {
					continue
				}
				seen[k] = true
				cleaned = append(cleaned, protocol.ProjectEnvVar{
					Key:    k,
					Value:  item.Value,
					Secret: item.Secret,
				})
			}
			b, err := json.Marshal(cleaned)
			if err != nil {
				return err
			}
			sharedEnvJSON = string(b)
		}

		updatedCfg, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE projects
			SET display_name = ?, config_json = ?, system_prompt = ?, shared_env_json = ?, updated_at = ?
			WHERE project_id = ?
		`, displayName, string(updatedCfg), sysPrompt, sharedEnvJSON, now, projectID); err != nil {
			return err
		}
		ev, err = insertEventTx(ctx, tx, eventInsert{
			StreamID:   "global",
			ProjectID:  projectID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventProjectChanged,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"projectId": projectID,
				"action":    "settings_updated",
			},
			CreatedAt: now,
		})
		return err
	})
	if err != nil {
		return pv, ev, err
	}
	pv, err = s.GetProject(ctx, projectID)
	return pv, ev, err
}

// CreateProjectPart adds a new Part (Git repo or plain folder) to an existing project, optionally with its initial Location.
func (s *Store) CreateProjectPart(
	ctx context.Context,
	projectID string,
	name string,
	kind protocol.ProjectPartKind,
	repoIdentity string,
	machineID string,
	absPath string,
	runtimeState *protocol.LocationRuntimeState,
	actorID string,
) (protocol.ProjectView, protocol.ProjectPartView, protocol.EventRecord, error) {
	var pv protocol.ProjectView
	var createdPart protocol.ProjectPartView
	var ev protocol.EventRecord
	now := time.Now().UnixMilli()
	name = strings.TrimSpace(name)
	if name == "" {
		return pv, createdPart, ev, fmt.Errorf("%w: part name is required", ErrInvalidInput)
	}
	if kind == "" {
		kind = protocol.ProjectPartGit
	}
	if !kind.IsValid() {
		return pv, createdPart, ev, fmt.Errorf("%w: invalid part kind %q", ErrInvalidInput, kind)
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var archived int
		err := tx.QueryRowContext(ctx, `SELECT archived FROM projects WHERE project_id = ?`, projectID).Scan(&archived)
		if errors.Is(err, sql.ErrNoRows) || archived != 0 {
			return fmt.Errorf("%w: project %s", ErrNotFound, projectID)
		}
		if err != nil {
			return err
		}
		var maxSort int
		_ = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sort_order), -1) FROM project_parts WHERE project_id = ?`, projectID).Scan(&maxSort)
		partID := newID("prt")
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO project_parts (part_id, project_id, name, kind, repo_identity, sort_order, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, partID, projectID, name, string(kind), strings.TrimSpace(repoIdentity), maxSort+1, now, now); err != nil {
			return err
		}
		createdPart = protocol.ProjectPartView{
			PartID:       partID,
			ProjectID:    projectID,
			Name:         name,
			Kind:         kind,
			RepoIdentity: strings.TrimSpace(repoIdentity),
			SortOrder:    maxSort + 1,
			Locations:    []protocol.ProjectLocationView{},
		}
		if strings.TrimSpace(machineID) != "" && strings.TrimSpace(absPath) != "" {
			cleanPath := filepath.Clean(strings.TrimSpace(absPath))
			locID := newID("loc")
			iso := defaultIsolationForKind(kind)
			if kind != protocol.ProjectPartFolder {
				var inherited string
				if err := tx.QueryRowContext(ctx, `
					SELECT l.isolation
					FROM project_locations l
					JOIN project_parts p ON p.part_id = l.part_id
					WHERE l.project_id = ? AND p.kind != 'folder'
					ORDER BY l.updated_at DESC
					LIMIT 1
				`, projectID).Scan(&inherited); err == nil && protocol.LocationIsolation(inherited).IsValid() {
					iso = protocol.LocationIsolation(inherited)
				}
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO project_locations (location_id, part_id, project_id, machine_id, abs_path, is_default, isolation, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?)
			`, locID, partID, projectID, strings.TrimSpace(machineID), cleanPath, string(iso), now, now); err != nil {
				return err
			}
			if runtimeState != nil {
				if err := upsertLocationRuntimeStateTx(ctx, tx, locID, *runtimeState); err != nil {
					return err
				}
			}
			createdPart.Locations = append(createdPart.Locations, protocol.ProjectLocationView{
				LocationID: locID,
				PartID:     partID,
				MachineID:  strings.TrimSpace(machineID),
				AbsPath:    cleanPath,
				IsDefault:  true,
				Isolation:  iso,
				Runtime:    runtimeState,
				CreatedAt:  now,
				UpdatedAt:  now,
			})
		}
		if err := syncProjectAnchorsMirrorTx(ctx, tx, projectID, now); err != nil {
			return err
		}
		ev, err = insertEventTx(ctx, tx, eventInsert{
			StreamID:   "global",
			ProjectID:  projectID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventProjectChanged,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"projectId": projectID,
				"partId":    partID,
				"action":    "part_created",
			},
			CreatedAt: now,
		})
		return err
	})
	if err != nil {
		return pv, createdPart, ev, err
	}
	pv, err = s.GetProject(ctx, projectID)
	return pv, createdPart, ev, err
}

// UpdateProjectPart updates mutable metadata (name, kind, sortOrder) on an existing project part.
func (s *Store) UpdateProjectPart(
	ctx context.Context,
	projectID string,
	partID string,
	req protocol.UpdateProjectPartRequest,
	actorID string,
) (protocol.ProjectView, protocol.ProjectPartView, protocol.EventRecord, error) {
	var pv protocol.ProjectView
	var updatedPart protocol.ProjectPartView
	var ev protocol.EventRecord
	now := time.Now().UnixMilli()
	projectID = strings.TrimSpace(projectID)
	partID = strings.TrimSpace(partID)
	if projectID == "" || protocol.IsVirtualProjectID(projectID) || partID == "" {
		return pv, updatedPart, ev, fmt.Errorf("%w: invalid projectId or partId", ErrInvalidInput)
	}
	if req.Name == nil && req.Kind == nil && req.SortOrder == nil {
		return pv, updatedPart, ev, fmt.Errorf("%w: at least one field (name, kind, sortOrder) is required", ErrInvalidInput)
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var curName, curKind string
		var curSort int
		err := tx.QueryRowContext(ctx, `
			SELECT name, kind, sort_order
			FROM project_parts
			WHERE project_id = ? AND part_id = ?
		`, projectID, partID).Scan(&curName, &curKind, &curSort)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: part %s", ErrNotFound, partID)
		}
		if err != nil {
			return err
		}

		if req.Name != nil {
			trimmed := strings.TrimSpace(*req.Name)
			if trimmed == "" {
				return fmt.Errorf("%w: part name cannot be empty", ErrInvalidInput)
			}
			curName = trimmed
		}
		kindChangedToFolder := false
		if req.Kind != nil {
			if !req.Kind.IsValid() {
				return fmt.Errorf("%w: invalid part kind %q", ErrInvalidInput, *req.Kind)
			}
			if *req.Kind == protocol.ProjectPartFolder && curKind != string(protocol.ProjectPartFolder) {
				kindChangedToFolder = true
			}
			curKind = string(*req.Kind)
		}
		if req.SortOrder != nil {
			curSort = *req.SortOrder
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE project_parts
			SET name = ?, kind = ?, sort_order = ?, updated_at = ?
			WHERE project_id = ? AND part_id = ?
		`, curName, curKind, curSort, now, projectID, partID); err != nil {
			return err
		}
		if kindChangedToFolder {
			if _, err := tx.ExecContext(ctx, `
				UPDATE project_locations
				SET isolation = ?, updated_at = ?
				WHERE project_id = ? AND part_id = ?
			`, string(protocol.IsolationInPlace), now, projectID, partID); err != nil {
				return err
			}
		}
		if err := syncProjectAnchorsMirrorTx(ctx, tx, projectID, now); err != nil {
			return err
		}
		ev, err = insertEventTx(ctx, tx, eventInsert{
			StreamID:   "global",
			ProjectID:  projectID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventProjectChanged,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"projectId": projectID,
				"partId":    partID,
				"name":      curName,
				"kind":      curKind,
				"action":    "part_updated",
			},
			CreatedAt: now,
		})
		return err
	})
	if err != nil {
		return pv, updatedPart, ev, err
	}
	pv, err = s.GetProject(ctx, projectID)
	if err != nil {
		return pv, updatedPart, ev, err
	}
	for _, p := range pv.Parts {
		if p.PartID == partID {
			updatedPart = p
			break
		}
	}
	return pv, updatedPart, ev, nil
}

// DeleteProjectPart removes a part and its cascaded locations from a project.
func (s *Store) DeleteProjectPart(ctx context.Context, projectID, partID, actorID string) (protocol.ProjectView, protocol.EventRecord, error) {
	var pv protocol.ProjectView
	var ev protocol.EventRecord
	now := time.Now().UnixMilli()
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM project_parts WHERE project_id = ? AND part_id = ?`, projectID, partID)
		if err != nil {
			return err
		}
		if aff, _ := res.RowsAffected(); aff == 0 {
			return fmt.Errorf("%w: part %s", ErrNotFound, partID)
		}
		if err := syncProjectAnchorsMirrorTx(ctx, tx, projectID, now); err != nil {
			return err
		}
		ev, err = insertEventTx(ctx, tx, eventInsert{
			StreamID:   "global",
			ProjectID:  projectID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventProjectChanged,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"projectId": projectID,
				"partId":    partID,
				"action":    "part_deleted",
			},
			CreatedAt: now,
		})
		return err
	})
	if err != nil {
		return pv, ev, err
	}
	pv, err = s.GetProject(ctx, projectID)
	return pv, ev, err
}

// CreateProjectLocation explicitly binds a directory on a device to a project part.
func (s *Store) CreateProjectLocation(
	ctx context.Context,
	projectID string,
	partID string,
	req protocol.CreateProjectLocationRequest,
	runtimeState *protocol.LocationRuntimeState,
	repoIdentity string,
	actorID string,
) (protocol.ProjectView, protocol.ProjectLocationView, protocol.EventRecord, error) {
	var pv protocol.ProjectView
	var createdLoc protocol.ProjectLocationView
	var ev protocol.EventRecord
	now := time.Now().UnixMilli()
	machineID := strings.TrimSpace(req.MachineID)
	absPath := filepath.Clean(strings.TrimSpace(req.AbsPath))
	if machineID == "" || absPath == "" || absPath == "." {
		return pv, createdLoc, ev, fmt.Errorf("%w: machineId and absPath are required", ErrInvalidInput)
	}

	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var kindStr, curRepoID string
		err := tx.QueryRowContext(ctx, `
			SELECT kind, repo_identity FROM project_parts WHERE project_id = ? AND part_id = ?
		`, projectID, partID).Scan(&kindStr, &curRepoID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: part %s", ErrNotFound, partID)
		}
		if err != nil {
			return err
		}

		kind := protocol.ProjectPartKind(kindStr)
		iso := req.Isolation
		if iso == "" {
			if kind != protocol.ProjectPartFolder {
				var inherited string
				err := tx.QueryRowContext(ctx, `
					SELECT l.isolation
					FROM project_locations l
					JOIN project_parts p ON p.part_id = l.part_id
					WHERE l.project_id = ? AND p.kind != 'folder'
					ORDER BY l.updated_at DESC
					LIMIT 1
				`, projectID).Scan(&inherited)
				if err == nil && protocol.LocationIsolation(inherited).IsValid() {
					iso = protocol.LocationIsolation(inherited)
				}
			}
			if iso == "" {
				iso = defaultIsolationForKind(kind)
			}
		}
		if !iso.IsValid() {
			return fmt.Errorf("%w: invalid isolation %q", ErrInvalidInput, iso)
		}
		if kind == protocol.ProjectPartFolder {
			iso = protocol.IsolationInPlace
		}

		var existingCount int
		_ = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_locations WHERE part_id = ?`, partID).Scan(&existingCount)
		isDef := req.IsDefault || existingCount == 0
		if isDef {
			if _, err := tx.ExecContext(ctx, `UPDATE project_locations SET is_default = 0 WHERE part_id = ?`, partID); err != nil {
				return err
			}
		}

		locID := newID("loc")
		defInt := 0
		if isDef {
			defInt = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO project_locations (location_id, part_id, project_id, machine_id, abs_path, is_default, isolation, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, locID, partID, projectID, machineID, absPath, defInt, string(iso), now, now); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return fmt.Errorf("%w: location already bound on %s at %s", ErrConflict, machineID, absPath)
			}
			return err
		}
		if runtimeState != nil {
			if err := upsertLocationRuntimeStateTx(ctx, tx, locID, *runtimeState); err != nil {
				return err
			}
		}
		if strings.TrimSpace(curRepoID) == "" && strings.TrimSpace(repoIdentity) != "" {
			_, _ = tx.ExecContext(ctx, `UPDATE project_parts SET repo_identity = ?, updated_at = ? WHERE part_id = ?`, strings.TrimSpace(repoIdentity), now, partID)
		}
		// Remove any matching dismissed suggestion since user explicitly added it.
		_, _ = tx.ExecContext(ctx, `DELETE FROM project_dismissed_suggestions WHERE part_id = ? AND machine_id = ? AND abs_path = ?`, partID, machineID, absPath)

		if err := syncProjectAnchorsMirrorTx(ctx, tx, projectID, now); err != nil {
			return err
		}
		createdLoc = protocol.ProjectLocationView{
			LocationID: locID,
			PartID:     partID,
			MachineID:  machineID,
			AbsPath:    absPath,
			IsDefault:  isDef,
			Isolation:  iso,
			Runtime:    runtimeState,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ev, err = insertEventTx(ctx, tx, eventInsert{
			StreamID:   "global",
			ProjectID:  projectID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventProjectChanged,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"projectId":  projectID,
				"partId":     partID,
				"locationId": locID,
				"action":     "location_added",
			},
			CreatedAt: now,
		})
		return err
	})
	if err != nil {
		return pv, createdLoc, ev, err
	}
	pv, err = s.GetProject(ctx, projectID)
	return pv, createdLoc, ev, err
}

// UpdateProjectLocation updates machine/path binding, isDefault, or isolation on a bound location.
func (s *Store) UpdateProjectLocation(
	ctx context.Context,
	projectID, partID, locationID string,
	req protocol.UpdateProjectLocationRequest,
	actorID string,
) (protocol.ProjectView, protocol.EventRecord, error) {
	return s.UpdateProjectLocationWithRuntime(ctx, projectID, partID, locationID, req, nil, "", actorID)
}

// UpdateProjectLocationWithRuntime updates a bound location and optionally refreshes its runtime state.
func (s *Store) UpdateProjectLocationWithRuntime(
	ctx context.Context,
	projectID, partID, locationID string,
	req protocol.UpdateProjectLocationRequest,
	runtimeState *protocol.LocationRuntimeState,
	repoIdentity string,
	actorID string,
) (protocol.ProjectView, protocol.EventRecord, error) {
	var pv protocol.ProjectView
	var ev protocol.EventRecord
	now := time.Now().UnixMilli()
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var curDef int
		var curMachineID, curAbsPath, curIso, kindStr, curRepoID string
		err := tx.QueryRowContext(ctx, `
			SELECT l.machine_id, l.abs_path, l.is_default, l.isolation, p.kind, p.repo_identity
			FROM project_locations l
			JOIN project_parts p ON p.part_id = l.part_id
			WHERE l.project_id = ? AND l.part_id = ? AND l.location_id = ?
		`, projectID, partID, locationID).Scan(&curMachineID, &curAbsPath, &curDef, &curIso, &kindStr, &curRepoID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: location %s", ErrNotFound, locationID)
		}
		if err != nil {
			return err
		}
		nextMachineID := curMachineID
		if req.MachineID != nil {
			trimmed := strings.TrimSpace(*req.MachineID)
			if trimmed == "" {
				return fmt.Errorf("%w: machineId cannot be empty", ErrInvalidInput)
			}
			nextMachineID = trimmed
		}
		nextAbsPath := curAbsPath
		if req.AbsPath != nil {
			cleaned := filepath.Clean(strings.TrimSpace(*req.AbsPath))
			if cleaned == "" || cleaned == "." {
				return fmt.Errorf("%w: absPath cannot be empty", ErrInvalidInput)
			}
			nextAbsPath = cleaned
		}
		nextDef := curDef
		if req.IsDefault != nil {
			if *req.IsDefault {
				if _, err := tx.ExecContext(ctx, `UPDATE project_locations SET is_default = 0 WHERE part_id = ?`, partID); err != nil {
					return err
				}
				nextDef = 1
			} else {
				nextDef = 0
			}
		}
		nextIso := curIso
		if req.Isolation != nil {
			if !req.Isolation.IsValid() {
				return fmt.Errorf("%w: invalid isolation %q", ErrInvalidInput, *req.Isolation)
			}
			if protocol.ProjectPartKind(kindStr) == protocol.ProjectPartFolder {
				nextIso = string(protocol.IsolationInPlace)
			} else {
				nextIso = string(*req.Isolation)
			}
		}
		pathChanged := req.AbsPath != nil || req.MachineID != nil
		newRepoID := strings.TrimSpace(repoIdentity)
		if runtimeState != nil && !runtimeState.IsGit {
			newRepoID = ""
		}
		var locCount int
		if pathChanged {
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_locations WHERE part_id = ?`, partID).Scan(&locCount); err != nil {
				return err
			}
			curID := strings.TrimSpace(curRepoID)
			if locCount > 1 && curID != "" && newRepoID != "" && !strings.EqualFold(curID, newRepoID) {
				return fmt.Errorf("%w: directory repoIdentity %q conflicts with part repoIdentity %q", ErrRepoIdentityConflict, newRepoID, curID)
			}
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE project_locations
			SET machine_id = ?, abs_path = ?, is_default = ?, isolation = ?, updated_at = ?
			WHERE location_id = ?
		`, nextMachineID, nextAbsPath, nextDef, nextIso, now, locationID); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return fmt.Errorf("%w: location already bound on %s at %s", ErrConflict, nextMachineID, nextAbsPath)
			}
			return err
		}
		if runtimeState != nil {
			if err := upsertLocationRuntimeStateTx(ctx, tx, locationID, *runtimeState); err != nil {
				return err
			}
		}
		if pathChanged {
			if locCount <= 1 {
				if _, err := tx.ExecContext(ctx, `UPDATE project_parts SET repo_identity = ?, updated_at = ? WHERE part_id = ?`, newRepoID, now, partID); err != nil {
					return err
				}
				// Keep project-level repo_identity mirror in sync when this is the sole non-folder part.
				var partCount int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_parts WHERE project_id = ?`, projectID).Scan(&partCount); err != nil {
					return err
				}
				if partCount <= 1 && (kindStr != string(protocol.ProjectPartFolder) || newRepoID != "") {
					var cfgJSON string
					if err := tx.QueryRowContext(ctx, `SELECT config_json FROM projects WHERE project_id = ?`, projectID).Scan(&cfgJSON); err != nil {
						return err
					}
					var cfg protocol.ProjectConfig
					_ = json.Unmarshal([]byte(cfgJSON), &cfg)
					cfg.RepoIdentity = newRepoID
					updatedCfg, err := json.Marshal(cfg)
					if err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx, `UPDATE projects SET repo_identity = ?, config_json = ?, updated_at = ? WHERE project_id = ?`, newRepoID, string(updatedCfg), now, projectID); err != nil {
						return err
					}
				}
			} else if strings.TrimSpace(curRepoID) == "" && newRepoID != "" {
				if _, err := tx.ExecContext(ctx, `UPDATE project_parts SET repo_identity = ?, updated_at = ? WHERE part_id = ?`, newRepoID, now, partID); err != nil {
					return err
				}
			}
		} else if strings.TrimSpace(curRepoID) == "" && newRepoID != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE project_parts SET repo_identity = ?, updated_at = ? WHERE part_id = ?`, newRepoID, now, partID); err != nil {
				return err
			}
		}
		if err := syncProjectAnchorsMirrorTx(ctx, tx, projectID, now); err != nil {
			return err
		}
		ev, err = insertEventTx(ctx, tx, eventInsert{
			StreamID:   "global",
			ProjectID:  projectID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventProjectChanged,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"projectId":  projectID,
				"partId":     partID,
				"locationId": locationID,
				"action":     "location_updated",
			},
			CreatedAt: now,
		})
		return err
	})
	if err != nil {
		return pv, ev, err
	}
	pv, err = s.GetProject(ctx, projectID)
	return pv, ev, err
}

func setProjectGitIsolationTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID string,
	isolation protocol.LocationIsolation,
	actorID string,
	now int64,
) (protocol.EventRecord, bool, error) {
	var ev protocol.EventRecord
	if strings.TrimSpace(projectID) == "" || protocol.IsVirtualProjectID(projectID) || !isolation.IsValid() {
		return ev, false, nil
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE project_locations
		SET isolation = ?, updated_at = ?
		WHERE project_id = ?
		  AND part_id IN (SELECT part_id FROM project_parts WHERE project_id = ? AND kind != 'folder')
		  AND isolation != ?
	`, string(isolation), now, projectID, projectID, string(isolation))
	if err != nil {
		return ev, false, err
	}
	if aff, _ := res.RowsAffected(); aff == 0 {
		return ev, false, nil
	}
	if err := syncProjectAnchorsMirrorTx(ctx, tx, projectID, now); err != nil {
		return ev, false, err
	}
	ev, err = insertEventTx(ctx, tx, eventInsert{
		StreamID:   "global",
		ProjectID:  projectID,
		ActorKind:  protocol.ActorHuman,
		ActorID:    actorID,
		Kind:       protocol.EventProjectChanged,
		Visibility: protocol.VisibilityTimeline,
		Payload: map[string]any{
			"projectId": projectID,
			"isolation": isolation,
			"action":    "project_isolation_updated",
		},
		CreatedAt: now,
	})
	return ev, err == nil, err
}

// SetProjectGitIsolation updates isolation uniformly across all non-folder (Git) locations of a project
// so the Worktree toggle is remembered consistently at project granularity across machines/locations.
func (s *Store) SetProjectGitIsolation(
	ctx context.Context,
	projectID string,
	isolation protocol.LocationIsolation,
	actorID string,
) (protocol.EventRecord, error) {
	var ev protocol.EventRecord
	if strings.TrimSpace(projectID) == "" || protocol.IsVirtualProjectID(projectID) {
		return ev, fmt.Errorf("%w: invalid projectId", ErrInvalidInput)
	}
	if !isolation.IsValid() {
		return ev, fmt.Errorf("%w: invalid isolation %q", ErrInvalidInput, isolation)
	}
	now := time.Now().UnixMilli()
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var changed bool
		var err error
		ev, changed, err = setProjectGitIsolationTx(ctx, tx, projectID, isolation, actorID, now)
		if !changed {
			ev = protocol.EventRecord{}
		}
		return err
	})
	return ev, err
}

// DeleteProjectLocation removes a bound location from a part, promoting another to default if needed.
func (s *Store) DeleteProjectLocation(ctx context.Context, projectID, partID, locationID, actorID string) (protocol.ProjectView, protocol.EventRecord, error) {
	var pv protocol.ProjectView
	var ev protocol.EventRecord
	now := time.Now().UnixMilli()
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var wasDef int
		err := tx.QueryRowContext(ctx, `
			SELECT is_default FROM project_locations WHERE project_id = ? AND part_id = ? AND location_id = ?
		`, projectID, partID, locationID).Scan(&wasDef)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: location %s", ErrNotFound, locationID)
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM project_locations WHERE location_id = ?`, locationID); err != nil {
			return err
		}
		if wasDef != 0 {
			var nextLocID string
			if err := tx.QueryRowContext(ctx, `SELECT location_id FROM project_locations WHERE part_id = ? ORDER BY created_at ASC LIMIT 1`, partID).Scan(&nextLocID); err == nil && nextLocID != "" {
				_, _ = tx.ExecContext(ctx, `UPDATE project_locations SET is_default = 1, updated_at = ? WHERE location_id = ?`, now, nextLocID)
			}
		}
		if err := syncProjectAnchorsMirrorTx(ctx, tx, projectID, now); err != nil {
			return err
		}
		ev, err = insertEventTx(ctx, tx, eventInsert{
			StreamID:   "global",
			ProjectID:  projectID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventProjectChanged,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"projectId":  projectID,
				"partId":     partID,
				"locationId": locationID,
				"action":     "location_deleted",
			},
			CreatedAt: now,
		})
		return err
	})
	if err != nil {
		return pv, ev, err
	}
	pv, err = s.GetProject(ctx, projectID)
	return pv, ev, err
}

func upsertLocationRuntimeStateTx(ctx context.Context, tx *sql.Tx, locationID string, st protocol.LocationRuntimeState) error {
	checkedAt := st.CheckedAt
	if checkedAt == 0 {
		checkedAt = time.Now().UnixMilli()
	}
	exInt, gitInt, dirtyInt := 0, 0, 0
	if st.Exists {
		exInt = 1
	}
	if st.IsGit {
		gitInt = 1
	}
	if st.Dirty {
		dirtyInt = 1
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO location_runtime_state (
			location_id, exists_flag, is_git, branch, head_sha, dirty, ahead, behind, checked_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(location_id) DO UPDATE SET
			exists_flag = excluded.exists_flag,
			is_git = excluded.is_git,
			branch = excluded.branch,
			head_sha = excluded.head_sha,
			dirty = excluded.dirty,
			ahead = excluded.ahead,
			behind = excluded.behind,
			checked_at = excluded.checked_at
	`, locationID, exInt, gitInt, st.Branch, st.HeadSHA, dirtyInt, st.Ahead, st.Behind, checkedAt)
	return err
}

// UpsertLocationRuntimeState persists live probed runtime status for a bound location.
func (s *Store) UpsertLocationRuntimeState(ctx context.Context, locationID string, st protocol.LocationRuntimeState) error {
	return s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		return upsertLocationRuntimeStateTx(ctx, tx, locationID, st)
	})
}

// DismissProjectSuggestion records a permanently dismissed shallow Git recommendation for a part.
func (s *Store) DismissProjectSuggestion(ctx context.Context, projectID, partID, machineID, absPath string) error {
	machineID = strings.TrimSpace(machineID)
	absPath = filepath.Clean(strings.TrimSpace(absPath))
	if machineID == "" || absPath == "" || absPath == "." {
		return fmt.Errorf("%w: machineId and absPath are required", ErrInvalidInput)
	}
	now := time.Now().UnixMilli()
	return s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var exists string
		if err := tx.QueryRowContext(ctx, `SELECT part_id FROM project_parts WHERE project_id = ? AND part_id = ?`, projectID, partID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: part %s", ErrNotFound, partID)
		} else if err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO project_dismissed_suggestions (part_id, machine_id, abs_path, dismissed_at)
			VALUES (?, ?, ?, ?)
		`, partID, machineID, absPath, now)
		return err
	})
}

// ListDismissedSuggestions returns a set of "partId|machineId|absPath" keys ignored by the user.
func (s *Store) ListDismissedSuggestions(ctx context.Context, projectID string) (map[string]bool, error) {
	out := map[string]bool{}
	rows, err := s.ro.QueryContext(ctx, `
		SELECT d.part_id, d.machine_id, d.abs_path
		FROM project_dismissed_suggestions d
		JOIN project_parts p ON p.part_id = d.part_id
		WHERE p.project_id = ?
	`, projectID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var pid, mid, ap string
		if err := rows.Scan(&pid, &mid, &ap); err != nil {
			return out, err
		}
		out[pid+"|"+mid+"|"+filepath.Clean(ap)] = true
	}
	return out, rows.Err()
}

// UpdateMachineMetadata updates display_alias, role, allowed_roots_json, and/or ssh_alias on a machine.
func (s *Store) UpdateMachineMetadata(ctx context.Context, machineID string, req protocol.UpdateMachineRequest, actorID string) (protocol.EventRecord, error) {
	now := time.Now().UnixMilli()
	var ev protocol.EventRecord
	err := s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		removedSet, err := loadRemovedMachinesSetTx(ctx, tx)
		if err != nil {
			return err
		}
		if removedSet[machineID] {
			return fmt.Errorf("%w: machine %s", ErrNotFound, machineID)
		}
		var curAlias, curRole, curRootsJSON string
		var curSSHAlias sql.NullString
		err = tx.QueryRowContext(ctx, `
			SELECT display_alias, role, allowed_roots_json, ssh_alias FROM machines WHERE machine_id = ?
		`, machineID).Scan(&curAlias, &curRole, &curRootsJSON, &curSSHAlias)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: machine %s", ErrNotFound, machineID)
		}
		if err != nil {
			return err
		}
		if req.DisplayAlias != nil {
			curAlias = strings.TrimSpace(*req.DisplayAlias)
		}
		if req.Role != nil {
			if !req.Role.IsValid() {
				return fmt.Errorf("%w: invalid machine role %q", ErrInvalidInput, *req.Role)
			}
			curRole = string(*req.Role)
		}
		if req.AllowedRoots != nil {
			cleaned := make([]string, 0, len(req.AllowedRoots))
			seen := map[string]bool{}
			for _, r := range req.AllowedRoots {
				t := strings.TrimSpace(r)
				if t == "" || seen[t] {
					continue
				}
				seen[t] = true
				cleaned = append(cleaned, t)
			}
			b, err := json.Marshal(cleaned)
			if err != nil {
				return err
			}
			curRootsJSON = string(b)
		}
		if req.SSHAlias != nil {
			curSSHAlias = sql.NullString{String: strings.TrimSpace(*req.SSHAlias), Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE machines
			SET display_alias = ?, role = ?, allowed_roots_json = ?, ssh_alias = ?, updated_at = ?
			WHERE machine_id = ?
		`, curAlias, curRole, curRootsJSON, curSSHAlias, now, machineID); err != nil {
			return err
		}
		if curSSHAlias.Valid {
			_, _ = tx.ExecContext(ctx, `UPDATE registry_machines SET role = ?, ssh_alias = ? WHERE machine_id = ?`, curRole, curSSHAlias.String, machineID)
		} else {
			_, _ = tx.ExecContext(ctx, `UPDATE registry_machines SET role = ? WHERE machine_id = ?`, curRole, machineID)
		}
		payload := map[string]any{
			"machineId":    machineID,
			"displayAlias": curAlias,
			"role":         curRole,
		}
		if curSSHAlias.Valid {
			payload["sshAlias"] = curSSHAlias.String
		}
		var err2 error
		ev, err2 = insertEventTx(ctx, tx, eventInsert{
			StreamID:   "global",
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventMachineStateChanged,
			Visibility: protocol.VisibilityTimeline,
			Payload:    payload,
			CreatedAt:  now,
		})
		return err2
	})
	return ev, err
}

// UpdateMachineOS persists self-reported OS (e.g. "darwin", "linux") reported in runner.hello.
func (s *Store) UpdateMachineOS(ctx context.Context, machineID, osName string) error {
	osName = strings.TrimSpace(osName)
	if machineID == "" || osName == "" {
		return nil
	}
	return s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE machines SET os_name = ? WHERE machine_id = ?`, osName, machineID)
		return err
	})
}

// SaveAttemptFolderChanges persists lightweight in-place plain-folder change summaries on an attempt.
func (s *Store) SaveAttemptFolderChanges(ctx context.Context, attemptID string, changes protocol.FolderChangesSummary) error {
	if changes.Files == nil {
		changes.Files = []protocol.FolderChangeItem{}
	}
	b, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	return s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE attempts SET folder_changes_json = ? WHERE attempt_id = ?`, string(b), attemptID)
		return err
	})
}

// MarkAttemptFolderChangesPromoted marks an attempt's folderChanges as promoted into Artifacts.
func (s *Store) MarkAttemptFolderChangesPromoted(ctx context.Context, attemptID string) error {
	return s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var raw sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT folder_changes_json FROM attempts WHERE attempt_id = ?`, attemptID).Scan(&raw); err != nil {
			return err
		}
		if !raw.Valid || strings.TrimSpace(raw.String) == "" {
			return nil
		}
		var fc protocol.FolderChangesSummary
		if err := json.Unmarshal([]byte(raw.String), &fc); err != nil {
			return err
		}
		fc.Promoted = true
		b, err := json.Marshal(fc)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE attempts SET folder_changes_json = ? WHERE attempt_id = ?`, string(b), attemptID)
		return err
	})
}

// GetTaskLocationTarget reads the explicit part_id and location_id stored on a task (if any).
func (s *Store) GetTaskLocationTarget(ctx context.Context, taskID string) (string, string, error) {
	var partID, locationID sql.NullString
	err := s.ro.QueryRowContext(ctx, `SELECT part_id, location_id FROM tasks WHERE task_id = ?`, taskID).Scan(&partID, &locationID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return partID.String, locationID.String, err
}

// GetMachineAllowedRoots returns any custom allowed roots saved for a machine.
func (s *Store) GetMachineAllowedRoots(ctx context.Context, machineID string) []string {
	var raw string
	if err := s.ro.QueryRowContext(ctx, `SELECT allowed_roots_json FROM machines WHERE machine_id = ?`, machineID).Scan(&raw); err != nil {
		return nil
	}
	var roots []string
	_ = json.Unmarshal([]byte(raw), &roots)
	return roots
}

// PromoteAttemptFolderDelivery atomically creates a DeliveryWorkspaceSnapshot carrying the promoted
// plain-folder files as content-addressed blobs, increments blob ref_counts, marks folderChanges.promoted=true,
// and emits delivery.published so promoted files appear immediately in Artifacts.
func (s *Store) PromoteAttemptFolderDelivery(
	ctx context.Context,
	att AttemptRecord,
	repoIdentity string,
	summary string,
	blobs []protocol.BlobManifestItem,
	filesChanged int,
	insertions int,
	deletions int,
	actorID string,
) (protocol.DeliveryView, protocol.EventRecord, error) {
	var dlv protocol.DeliveryView
	var ev protocol.EventRecord
	now := time.Now().UnixMilli()
	dlvID := newID("dlv")
	gitRef := "refs/orbit/folder/" + dlvID
	manifest := protocol.DeliveryManifest{
		FilesChanged: filesChanged,
		Insertions:   insertions,
		Deletions:    deletions,
		Blobs:        blobs,
		WorktreePath: att.WorktreePath,
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return dlv, ev, err
	}

	err = s.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if err := checkDeliveryCleanupPendingTx(ctx, tx, dlvID); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, b := range blobs {
			if seen[b.SHA256] {
				continue
			}
			seen[b.SHA256] = true
			if _, err := tx.ExecContext(ctx, `UPDATE delivery_blobs SET ref_count = ref_count + 1 WHERE sha256 = ?`, b.SHA256); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO deliveries (
				delivery_id, project_id, task_id, attempt_id, source_machine_id,
				repo_identity, base_sha, head_sha, git_ref, kind, summary,
				manifest_json, verified_machines_json, ryugi_sync_state, late_arrival, pinned, created_at
			) VALUES (?, ?, ?, ?, ?, ?, '', '', ?, ?, ?, ?, '{}', 'none', 0, 1, ?)
		`, dlvID, att.ProjectID, att.TaskID, att.AttemptID, att.MachineID,
			repoIdentity, gitRef, string(protocol.DeliveryWorkspaceSnapshot), summary,
			string(manifestBytes), now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE tasks SET latest_delivery_id = ?, updated_at = ? WHERE task_id = ?
		`, dlvID, now, att.TaskID); err != nil {
			return err
		}
		var rawFC sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT folder_changes_json FROM attempts WHERE attempt_id = ?`, att.AttemptID).Scan(&rawFC); err == nil && rawFC.Valid && rawFC.String != "" {
			var fc protocol.FolderChangesSummary
			if json.Unmarshal([]byte(rawFC.String), &fc) == nil {
				fc.Promoted = true
				if b, mErr := json.Marshal(fc); mErr == nil {
					_, _ = tx.ExecContext(ctx, `UPDATE attempts SET folder_changes_json = ?, output_delivery_id = COALESCE(NULLIF(output_delivery_id, ''), ?) WHERE attempt_id = ?`, string(b), dlvID, att.AttemptID)
				}
			}
		}
		ev, err = insertEventTx(ctx, tx, eventInsert{
			StreamID:   "task:" + att.TaskID,
			ProjectID:  att.ProjectID,
			TaskID:     att.TaskID,
			AttemptID:  att.AttemptID,
			ActorKind:  protocol.ActorHuman,
			ActorID:    actorID,
			Kind:       protocol.EventDeliveryPublished,
			Visibility: protocol.VisibilityTimeline,
			Payload: map[string]any{
				"deliveryId": dlvID,
				"kind":       protocol.DeliveryWorkspaceSnapshot,
				"summary":    summary,
				"promoted":   true,
			},
			CreatedAt: now,
		})
		return err
	})
	if err != nil {
		return dlv, ev, err
	}
	dlv = protocol.DeliveryView{
		DeliveryID:      dlvID,
		ProjectID:       att.ProjectID,
		TaskID:          att.TaskID,
		AttemptID:       att.AttemptID,
		SourceMachineID: att.MachineID,
		RepoIdentity:    repoIdentity,
		GitRef:          gitRef,
		Kind:            protocol.DeliveryWorkspaceSnapshot,
		Summary:         summary,
		Manifest:        manifest,
		RyugiSyncState:  protocol.RyugiSyncNone,
		Pinned:          true,
		CreatedAt:       now,
	}
	return dlv, ev, nil
}
