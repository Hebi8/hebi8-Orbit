package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

type resolvedRoute struct {
	MachineID     string
	MachineCfg    protocol.MachineConfig
	Capability    string
	Adapter       protocol.Adapter
	Anchor        protocol.ProjectAnchorConfig
	HasAnchor     bool
	PartID        string
	LocationID    string
	Isolation     protocol.LocationIsolation
	IsPlainFolder bool
}

// resolveExecutionRoute resolves capability, anchor, machine, and adapter from project & machine registries.
func (s *Server) resolveExecutionRoute(
	proj protocol.ProjectView,
	reqCap string,
	reqAdapter protocol.Adapter,
	reqMachineID ...string,
) (resolvedRoute, error) {
	mid := ""
	if len(reqMachineID) > 0 {
		mid = reqMachineID[0]
	}
	return s.resolveExecutionRouteWithLocation(proj, reqCap, reqAdapter, mid, "", "")
}

func (s *Server) resolveExecutionRouteWithLocation(
	proj protocol.ProjectView,
	reqCap string,
	reqAdapter protocol.Adapter,
	reqMachineID string,
	reqPartID string,
	reqLocationID string,
) (resolvedRoute, error) {
	s.mu.RLock()
	machines := make(map[string]protocol.MachineConfig, len(s.machines.Machines))
	for k, v := range s.machines.Machines {
		machines[k] = v
	}
	s.mu.RUnlock()

	explicitMachine := strings.TrimSpace(reqMachineID)
	explicitPart := strings.TrimSpace(reqPartID)
	explicitLoc := strings.TrimSpace(reqLocationID)

	// 3-level Part -> Location resolution when explicit part/location is requested, or for standard non-deploy tasks with bound parts.
	if !protocol.IsVirtualProjectID(proj.ProjectID) && len(proj.Parts) > 0 && strings.TrimSpace(reqCap) != "deploy-node" {
		var matchedPart *protocol.ProjectPartView
		var matchedLoc *protocol.ProjectLocationView

		if explicitLoc != "" {
			for i := range proj.Parts {
				for j := range proj.Parts[i].Locations {
					if proj.Parts[i].Locations[j].LocationID == explicitLoc {
						matchedPart = &proj.Parts[i]
						matchedLoc = &proj.Parts[i].Locations[j]
						break
					}
				}
				if matchedLoc != nil {
					break
				}
			}
			if matchedLoc == nil {
				return resolvedRoute{}, fmt.Errorf("location %q not found in project %q", explicitLoc, proj.ProjectID)
			}
		} else if explicitPart != "" {
			for i := range proj.Parts {
				if proj.Parts[i].PartID == explicitPart {
					matchedPart = &proj.Parts[i]
					break
				}
			}
			if matchedPart == nil {
				return resolvedRoute{}, fmt.Errorf("part %q not found in project %q", explicitPart, proj.ProjectID)
			}
			for j := range matchedPart.Locations {
				if explicitMachine != "" && matchedPart.Locations[j].MachineID == explicitMachine {
					matchedLoc = &matchedPart.Locations[j]
					break
				}
				if explicitMachine == "" && matchedPart.Locations[j].IsDefault {
					matchedLoc = &matchedPart.Locations[j]
					break
				}
			}
			if matchedLoc == nil && len(matchedPart.Locations) > 0 && explicitMachine == "" {
				matchedLoc = &matchedPart.Locations[0]
			}
			if matchedLoc == nil {
				return resolvedRoute{}, fmt.Errorf("part %q has no bound location for machine %q", matchedPart.Name, explicitMachine)
			}
		} else if strings.TrimSpace(reqCap) == "" {
			if explicitMachine != "" {
				for i := range proj.Parts {
					for j := range proj.Parts[i].Locations {
						if proj.Parts[i].Locations[j].MachineID == explicitMachine {
							matchedPart = &proj.Parts[i]
							matchedLoc = &proj.Parts[i].Locations[j]
							break
						}
					}
					if matchedLoc != nil {
						break
					}
				}
			} else if len(proj.Parts[0].Locations) > 0 {
				matchedPart = &proj.Parts[0]
				for j := range matchedPart.Locations {
					if matchedPart.Locations[j].IsDefault {
						matchedLoc = &matchedPart.Locations[j]
						break
					}
				}
				if matchedLoc == nil {
					matchedLoc = &matchedPart.Locations[0]
				}
			}
		}

		if matchedPart != nil && matchedLoc != nil {
			mCfg, hasM := machines[matchedLoc.MachineID]
			if !hasM && len(machines) > 0 && (explicitLoc != "" || explicitPart != "") {
				return resolvedRoute{}, fmt.Errorf("unknown machine %q", matchedLoc.MachineID)
			}
			capName := strings.TrimSpace(reqCap)
			if capName == "" {
				capName = proj.DefaultCapability
			}
			if capName == "" {
				capName = "primary-dev"
			}
			adapter := reqAdapter
			if adapter == "" {
				if proj.DefaultAdapter != "" {
					adapter = proj.DefaultAdapter
				} else if reported := s.runners.ReportedAdapters(matchedLoc.MachineID); len(reported) > 0 {
					adapter = reported[0]
				} else {
					adapter = protocol.AdapterAgy
				}
			}
			iso := matchedLoc.Isolation
			isPlain := matchedPart.Kind == protocol.ProjectPartFolder
			if isPlain {
				iso = protocol.IsolationInPlace
			} else if iso == "" {
				iso = protocol.IsolationWorktree
			}
			return resolvedRoute{
				MachineID:  matchedLoc.MachineID,
				MachineCfg: mCfg,
				Capability: capName,
				Adapter:    adapter,
				Anchor: protocol.ProjectAnchorConfig{
					Machine:  matchedLoc.MachineID,
					RepoPath: matchedLoc.AbsPath,
				},
				HasAnchor:     true,
				PartID:        matchedPart.PartID,
				LocationID:    matchedLoc.LocationID,
				Isolation:     iso,
				IsPlainFolder: isPlain,
			}, nil
		}
	}

	capName := strings.TrimSpace(reqCap)
	if capName == "" {
		capName = proj.DefaultCapability
	}
	if capName == "" {
		capName = "primary-dev"
	}

	anchor, hasAnchor := proj.Anchors[capName]

	machineID := ""
	var mCfg protocol.MachineConfig

	if explicitMachine != "" {
		cfg, ok := machines[explicitMachine]
		if !ok {
			return resolvedRoute{}, fmt.Errorf("unknown machine %q", explicitMachine)
		}
		machineID = explicitMachine
		mCfg = cfg
		if !protocol.IsVirtualProjectID(proj.ProjectID) && len(proj.Anchors) > 0 {
			if strings.TrimSpace(reqCap) != "" {
				a, ok := proj.Anchors[capName]
				if !ok || a.Machine != explicitMachine {
					return resolvedRoute{}, fmt.Errorf("project %q has no anchor for capability %q on machine %q", proj.ProjectID, capName, explicitMachine)
				}
				anchor = a
				hasAnchor = true
			} else if hasAnchor && anchor.Machine == explicitMachine {
				// Default capability already targets explicitMachine; keep it.
			} else {
				// Deterministic search across sorted capability names for an anchor on explicitMachine
				capNames := make([]string, 0, len(proj.Anchors))
				for cName := range proj.Anchors {
					capNames = append(capNames, cName)
				}
				sort.Strings(capNames)
				matched := false
				for _, cName := range capNames {
					a := proj.Anchors[cName]
					if a.Machine == explicitMachine {
						capName = cName
						anchor = a
						hasAnchor = true
						matched = true
						break
					}
				}
				if !matched {
					return resolvedRoute{}, fmt.Errorf("project %q is not anchored on machine %q", proj.ProjectID, explicitMachine)
				}
			}
		}
	}

	if machineID == "" && hasAnchor && anchor.Machine != "" {
		machineID = anchor.Machine
		if cfg, ok := machines[machineID]; ok {
			mCfg = cfg
		}
	}

	sortedIDs := make([]string, 0, len(machines))
	for id := range machines {
		sortedIDs = append(sortedIDs, id)
	}
	sort.Strings(sortedIDs)

	// Prefer connected online machines supporting the requested adapter when routing dynamically.
	if machineID == "" && reqAdapter != "" {
		for _, id := range sortedIDs {
			m := machines[id]
			if m.Transport == protocol.TransportSSHOutbound || !s.runners.IsConnected(id) {
				continue
			}
			if slices.Contains(s.runners.ReportedAdapters(id), reqAdapter) {
				machineID = id
				mCfg = m
				break
			}
		}
	}

	// For virtual projects (_scratch / _pa), prefer online machines with at least one ready adapter (local_ws first).
	if machineID == "" && protocol.IsVirtualProjectID(proj.ProjectID) {
		for _, preferLocal := range []bool{true, false} {
			for _, id := range sortedIDs {
				m := machines[id]
				if m.Transport == protocol.TransportSSHOutbound || !s.runners.IsConnected(id) {
					continue
				}
				if preferLocal && m.Transport != protocol.TransportLocalWS {
					continue
				}
				if len(s.runners.ReportedAdapters(id)) > 0 {
					machineID = id
					mCfg = m
					break
				}
			}
			if machineID != "" {
				break
			}
		}
	}

	if machineID == "" {
		// Find first machine advertising capName (prefer online)
		for _, onlyOnline := range []bool{true, false} {
			for _, id := range sortedIDs {
				m := machines[id]
				if onlyOnline && !s.runners.IsConnected(id) {
					continue
				}
				if slices.Contains(m.Capabilities, capName) {
					machineID = id
					mCfg = m
					break
				}
			}
			if machineID != "" {
				break
			}
		}
	}
	if machineID == "" {
		for _, id := range sortedIDs {
			m := machines[id]
			if m.Transport == protocol.TransportLocalWS {
				machineID = id
				mCfg = m
				break
			}
		}
	}
	if machineID == "" {
		for _, id := range sortedIDs {
			m := machines[id]
			if m.Transport != protocol.TransportSSHOutbound {
				machineID = id
				mCfg = m
				break
			}
		}
	}
	if machineID == "" && len(sortedIDs) > 0 {
		machineID = sortedIDs[0]
		mCfg = machines[machineID]
	}
	if machineID == "" {
		machineID = "hub-node"
	}

	adapter := reqAdapter
	if adapter == "" {
		if hasAnchor && anchor.DeployStrategy != nil {
			adapter = protocol.AdapterSSHDeploy
		} else if mCfg.Transport == protocol.TransportSSHOutbound || capName == "deploy-node" {
			adapter = protocol.AdapterSSHDeploy
		} else if protocol.IsVirtualProjectID(proj.ProjectID) {
			reported := s.runners.ReportedAdapters(machineID)
			if len(reported) > 0 {
				if proj.DefaultAdapter != "" && slices.Contains(reported, proj.DefaultAdapter) {
					adapter = proj.DefaultAdapter
				} else {
					adapter = reported[0]
				}
			} else if proj.DefaultAdapter != "" {
				adapter = proj.DefaultAdapter
			} else {
				adapter = protocol.AdapterAgy
			}
		} else if proj.DefaultAdapter != "" {
			adapter = proj.DefaultAdapter
		} else if reported := s.runners.ReportedAdapters(machineID); len(reported) > 0 {
			adapter = reported[0]
		} else {
			adapter = protocol.AdapterAgy
		}
	}
	// If route explicitly points at an ssh_outbound deploy target, executor is unconditionally ssh_deploy
	if mCfg.Transport == protocol.TransportSSHOutbound || (hasAnchor && anchor.DeployStrategy != nil) {
		adapter = protocol.AdapterSSHDeploy
	}

	route := resolvedRoute{
		MachineID:  machineID,
		MachineCfg: mCfg,
		Capability: capName,
		Adapter:    adapter,
		Anchor:     anchor,
		HasAnchor:  hasAnchor,
	}
	// Populate matching PartID/LocationID/Isolation from project_parts when routing via capability/anchor.
	if !protocol.IsVirtualProjectID(proj.ProjectID) && len(proj.Parts) > 0 {
		var matchedPart *protocol.ProjectPartView
		var matchedLoc *protocol.ProjectLocationView
		wantPath := ""
		if hasAnchor && strings.TrimSpace(anchor.RepoPath) != "" {
			wantPath = filepath.Clean(protocol.ExpandPath(anchor.RepoPath))
		}
		for i := range proj.Parts {
			for j := range proj.Parts[i].Locations {
				loc := &proj.Parts[i].Locations[j]
				if loc.MachineID != machineID {
					continue
				}
				if wantPath != "" {
					// When target anchor specifies an explicit RepoPath, ONLY match locations with
					// that exact normalized path — never borrow an unrelated location on the same machine.
					if filepath.Clean(protocol.ExpandPath(loc.AbsPath)) == wantPath {
						matchedPart = &proj.Parts[i]
						matchedLoc = loc
						break
					}
					continue
				}
				if matchedLoc == nil || loc.IsDefault {
					matchedPart = &proj.Parts[i]
					matchedLoc = loc
				}
			}
			if matchedLoc != nil && wantPath != "" {
				break
			}
		}
		if matchedPart != nil && matchedLoc != nil {
			iso := matchedLoc.Isolation
			isPlain := matchedPart.Kind == protocol.ProjectPartFolder
			if isPlain {
				iso = protocol.IsolationInPlace
			} else if iso == "" {
				iso = protocol.IsolationWorktree
			}
			route.PartID = matchedPart.PartID
			route.LocationID = matchedLoc.LocationID
			route.Isolation = iso
			route.IsPlainFolder = isPlain
			if !route.HasAnchor {
				route.Anchor = protocol.ProjectAnchorConfig{
					Machine:  matchedLoc.MachineID,
					RepoPath: matchedLoc.AbsPath,
				}
				route.HasAnchor = true
			}
		} else if wantPath != "" {
			// Anchor had an explicit path with no matching bound location on machineID:
			// keep PartID/LocationID empty and use project-level Git isolation preference (default worktree).
			iso := protocol.IsolationWorktree
			for i := range proj.Parts {
				if proj.Parts[i].Kind == protocol.ProjectPartFolder {
					continue
				}
				for j := range proj.Parts[i].Locations {
					if proj.Parts[i].Locations[j].Isolation.IsValid() {
						iso = proj.Parts[i].Locations[j].Isolation
						break
					}
				}
				if iso != protocol.IsolationWorktree {
					break
				}
			}
			route.Isolation = iso
		}
	}
	return route, nil
}

// enforcePostRouteGuard blocks non-human callers (such as PA) from directly launching ssh_deploy.
func enforcePostRouteGuard(actorKind protocol.ActorKind, route resolvedRoute) error {
	if route.Adapter == protocol.AdapterSSHDeploy || route.MachineCfg.Transport == protocol.TransportSSHOutbound {
		if actorKind != protocol.ActorHuman {
			return fmt.Errorf("non-human actor %q is forbidden from directly executing ssh_deploy; request human deploy approval instead", actorKind)
		}
	}
	return nil
}

// validateAdapterSupport enforces B18: rejects unimplemented Phase 3 adapters (claude/codex) with 501
// and checks machine/runner-advertised adapters so unimplemented adapters can never silently run agy.
func (s *Server) validateAdapterSupport(route resolvedRoute) (int, string, error) {
	if !protocol.SessionAdapter(route.Adapter).IsValid() {
		return http.StatusOK, "", nil
	}
	if !protocol.IsSessionAdapterImplemented(route.Adapter) {
		return http.StatusNotImplemented, "ERR_ADAPTER_NOT_IMPLEMENTED", fmt.Errorf(
			"adapter %q is not implemented in Phase 1 (supported session adapters: agy)", route.Adapter,
		)
	}
	reported := s.runners.ReportedAdapters(route.MachineID)
	if len(reported) > 0 || s.runners.HasExplicitAdapterReport(route.MachineID) {
		if !slices.Contains(reported, route.Adapter) {
			if len(reported) == 0 {
				return http.StatusBadRequest, "ERR_ADAPTER_UNSUPPORTED", fmt.Errorf(
					"machine %s currently has no ready agents; configure or enable an agent first",
					route.MachineID,
				)
			}
			return http.StatusBadRequest, "ERR_ADAPTER_UNSUPPORTED", fmt.Errorf(
				"connected runner on machine %s does not advertise adapter %q (advertised: %v)",
				route.MachineID, route.Adapter, reported,
			)
		}
	} else if len(route.MachineCfg.Adapters) > 0 {
		if _, ok := route.MachineCfg.Adapters[string(route.Adapter)]; !ok {
			return http.StatusBadRequest, "ERR_ADAPTER_UNSUPPORTED", fmt.Errorf(
				"machine %s does not configure adapter %q", route.MachineID, route.Adapter,
			)
		}
	}
	return http.StatusOK, "", nil
}

func (s *Server) resolveWorktreeRoot(proj protocol.ProjectView, anchor protocol.ProjectAnchorConfig) string {
	if strings.TrimSpace(anchor.WorktreeRoot) != "" {
		return protocol.ExpandPath(anchor.WorktreeRoot)
	}
	cfg := s.hubConfigSnapshot()
	if proj.ProjectID == protocol.ScratchProjectID {
		return filepath.Join(cfg.DataDir, "scratch")
	}
	if proj.ProjectID == protocol.PAProjectID {
		return filepath.Join(cfg.DataDir, "pa")
	}
	return filepath.Join(cfg.DataDir, "worktrees", proj.ProjectID)
}

func buildOrbitSystemPromptAppend(taskID, projectID, attemptID string, isPlainFolder bool, projectSystemPrompt, parentTaskID, delegatedRole string) string {
	var base string
	switch {
	case projectID == protocol.ScratchProjectID:
		base = fmt.Sprintf(
			"hebi8-Orbit Chat Session: task=%s attempt=%s. "+
				"Answer directly and concisely. You are in a standalone scratch workspace (not bound to a Git project).",
			taskID, attemptID,
		)
	case projectID == protocol.PAProjectID:
		base = fmt.Sprintf(
			"hebi8-Orbit Hebi8 Main Assistant: task=%s attempt=%s. "+
				"You are Hebi8, the user's single continuous assistant inside Orbit. "+
				"When a Fold-Rollover opening (<hebi8_fold_opening>), referenced conversations (<referenced_conversations>), or workspace context (<orbit_workspace>) are provided, "+
				"treat summaries as cache and original transcripts as evidence, synthesize them faithfully, cite specific facts/decisions/files, and answer in the user's language.",
			taskID, attemptID,
		)
	case isPlainFolder:
		base = fmt.Sprintf(
			"hebi8-Orbit Execution Context: project=%s task=%s attempt=%s. "+
				"Your working directory is synced directly with the user's local project directory (in-place mode). "+
				"Edit or read files directly as requested; Orbit automatically tracks file changes when your turn finishes.",
			projectID, taskID, attemptID,
		)
	default:
		base = fmt.Sprintf(
			"hebi8-Orbit Execution Context: project=%s task=%s attempt=%s. "+
				"Your working directory is an isolated Git worktree. "+
				"Orbit automatically packages and syncs any file changes when your turn finishes—no manual delivery command is required.",
			projectID, taskID, attemptID,
		)
	}
	base += "\n\nOrbit Sub-Session Delegation:\n" +
		"- When splitting independent/parallel subtasks or delegating work to another machine, model, or project, prefer Orbit sub-session tools (`orbit_spawn_child`, or `orbit child spawn` in terminal).\n" +
		"- Delegated sub-sessions are live-observable in the Orbit UI so the user can inspect, take over, steer, or approve them directly.\n" +
		"- When a child session finishes, Orbit automatically delivers its summary back to you; do not poll in a loop."
	if strings.TrimSpace(parentTaskID) != "" {
		roleNote := ""
		if r := strings.TrimSpace(delegatedRole); r != "" {
			roleNote = fmt.Sprintf(" (role=%s)", r)
		}
		base += fmt.Sprintf(
			"\n\nDelegated Child Context:\n"+
				"- You are a delegated child session of parent task %s%s.\n"+
				"- Stay focused on your assigned scope and summarize your findings/changes clearly in your final reply; Orbit will automatically report it back to the parent session.",
			strings.TrimSpace(parentTaskID), roleNote,
		)
	}
	if custom := strings.TrimSpace(projectSystemPrompt); custom != "" {
		return base + "\n\nProject Instructions:\n" + custom
	}
	return base
}

func (s *Server) buildAttemptDispatchFrame(
	ctx context.Context,
	att store.AttemptRecord,
	proj protocol.ProjectView,
	anchor protocol.ProjectAnchorConfig,
	promptOverride string,
	swarmIdleTimeoutMs int64,
	parentTaskID string,
	delegatedRole string,
) (protocol.AttemptDispatchFrame, error) {
	prompt := strings.TrimSpace(promptOverride)
	usedOverride := prompt != ""
	explicitFullPermission := false
	if att.TriggerPayloadJSON != "" {
		var tp map[string]any
		if err := json.Unmarshal([]byte(att.TriggerPayloadJSON), &tp); err == nil {
			if prompt == "" {
				if p, ok := tp["prompt"].(string); ok {
					prompt = p
				}
			}
			if swarmIdleTimeoutMs <= 0 {
				if v, ok := tp["swarmIdleTimeoutMs"].(float64); ok && v > 0 {
					swarmIdleTimeoutMs = int64(v)
				}
			}
			if ef, ok := tp["explicitFullPermission"].(bool); ok && ef {
				explicitFullPermission = true
			}
		}
	}
	if prompt == "" {
		if task, err := s.store.GetTask(ctx, att.TaskID); err == nil {
			prompt = task.Goal
		}
	}
	if !usedOverride && (len(att.RefTasks) > 0 || proj.ProjectID == protocol.PAProjectID) {
		decorated, decErr := s.decorateAttemptPromptForAttempt(ctx, att, prompt, att.RefTasks)
		if decErr != nil {
			return protocol.AttemptDispatchFrame{}, decErr
		}
		prompt = decorated
	}

	var inputDlv *protocol.InputDeliveryRef
	if att.InputDeliveryID != "" {
		if dlv, _, _, _, err := s.store.GetDeliveryDetail(ctx, att.InputDeliveryID); err == nil {
			inputDlv = &protocol.InputDeliveryRef{
				DeliveryID: dlv.DeliveryID,
				GitRef:     dlv.GitRef,
				HeadSHA:    dlv.HeadSHA,
				Blobs:      dlv.Manifest.Blobs,
			}
		}
	}

	expectedCWD := att.SessionCWD
	provSID := att.ProviderSessionID
	if att.SessionLocked {
		// Never reuse a session locked after needs_verification / swarm_idle_timeout
		provSID = ""
		expectedCWD = ""
	}

	// S2: Prefer the anchor frozen on the AttemptRecord at creation time when available.
	effAnchor := anchor
	if att.Anchor != nil {
		effAnchor = *att.Anchor
	}

	s.mu.RLock()
	targetMachineCfg := s.machines.Machines[att.MachineID]
	s.mu.RUnlock()
	isRemoteMachine := targetMachineCfg.Transport != "" && targetMachineCfg.Transport != protocol.TransportLocalWS

	var worktreeRoot, worktreePath string
	if isRemoteMachine {
		// Remote runners compute their own OS-native worktree directory under their local dataDir
		// unless an explicit anchor WorktreeRoot was configured for that machine.
		worktreeRoot = strings.TrimSpace(effAnchor.WorktreeRoot)
		worktreePath = att.WorktreePath
		if worktreePath == "" && provSID != "" && expectedCWD != "" {
			worktreePath = expectedCWD
		}
	} else {
		worktreeRoot = s.resolveWorktreeRoot(proj, effAnchor)
		worktreePath = att.WorktreePath
		if worktreePath == "" {
			if provSID != "" && expectedCWD != "" {
				worktreePath = expectedCWD
			} else {
				worktreePath = filepath.Join(worktreeRoot, att.TaskID, att.AttemptID)
			}
		}
	}

	isPlainFolder := strings.HasPrefix(strings.ToLower(strings.TrimSpace(proj.RepoIdentity)), "folder:")
	if !isPlainFolder && att.PartID != "" {
		for _, p := range proj.Parts {
			if p.PartID == att.PartID && p.Kind == protocol.ProjectPartFolder {
				isPlainFolder = true
				break
			}
		}
	}
	isolation := att.Isolation
	if isPlainFolder {
		isolation = protocol.IsolationInPlace
	} else if isolation == "" {
		isolation = protocol.IsolationWorktree
	}
	isInPlace := isPlainFolder || isolation == protocol.IsolationInPlace

	repoPath := strings.TrimSpace(effAnchor.RepoPath)
	if !isRemoteMachine {
		repoPath = protocol.ExpandPath(repoPath)
	}
	mirrorBarePath := proj.MirrorBarePath
	if isInPlace {
		mirrorBarePath = ""
		if repoPath != "" {
			worktreePath = repoPath
		}
	} else if isRemoteMachine && !protocol.IsVirtualProjectID(proj.ProjectID) && strings.TrimSpace(proj.MirrorBarePath) != "" {
		// Only switch to Hub HTTP bundle transport when the connected Runner negotiates bundle.v1,
		// so older Runners with pre-configured remote mirror paths continue working cleanly.
		if s.runners.HasCapability(att.MachineID, protocol.RunnerCapBundleV1) {
			mirrorBarePath = "orbit-hub://" + proj.ProjectID
		}
	}

	var projectEnv map[string]string
	if len(proj.SharedEnv) > 0 {
		projectEnv = make(map[string]string, len(proj.SharedEnv))
		for _, item := range proj.SharedEnv {
			if k := strings.TrimSpace(item.Key); k != "" {
				projectEnv[k] = item.Value
			}
		}
	}

	return protocol.AttemptDispatchFrame{
		Type:               protocol.RunnerMsgAttemptDispatch,
		AttemptID:          att.AttemptID,
		TaskID:             att.TaskID,
		ProjectID:          att.ProjectID,
		LeaseToken:         att.LeaseToken,
		Adapter:            att.Adapter,
		WBSessionID:        att.WBSessionID,
		ProviderSessionID:  provSID,
		ExpectedSessionCWD: expectedCWD,
		RepoPath:           repoPath,
		RepoIdentity:       proj.RepoIdentity,
		LocationID:         att.LocationID,
		Isolation:          isolation,
		IsPlainFolder:      isPlainFolder,
		WorktreeRoot:       worktreeRoot,
		WorktreePath:       worktreePath,
		MirrorBarePath:     mirrorBarePath,
		BaseSHA:            att.BaseSHA,
		DefaultBranch:          proj.DefaultBranch,
		DesiredConfig:          att.DesiredConfig,
		ExplicitFullPermission: explicitFullPermission,
		ProjectEnv:             projectEnv,
		SystemPromptAppend: buildOrbitSystemPromptAppend(att.TaskID, att.ProjectID, att.AttemptID, isInPlace, proj.SystemPrompt, parentTaskID, delegatedRole),
		InputDelivery:      inputDlv,
		Prompt:             prompt,
		RequiredResources:  att.RequiredResources,
		SwarmIdleTimeoutMs: swarmIdleTimeoutMs,
	}, nil
}

func (s *Server) dispatchAttemptRecord(ctx context.Context, att store.AttemptRecord, promptOverride string, swarmIdleTimeoutMs int64) bool {
	if att.CancelRequested {
		return false
	}
	instID := s.runners.ActiveRunnerInstanceID(att.MachineID)
	if instID == "" {
		return false
	}
	proj, err := s.store.GetProject(ctx, att.ProjectID)
	if err != nil {
		return false
	}
	task, err := s.store.GetTask(ctx, att.TaskID)
	if err != nil {
		return false
	}
	var anchor protocol.ProjectAnchorConfig
	if att.Anchor != nil {
		anchor = *att.Anchor
	} else {
		capName := att.AnchorCapability
		if capName == "" {
			capName = task.RequiredCapability
		}
		route, err := s.resolveExecutionRoute(proj, capName, att.Adapter)
		if err != nil {
			return false
		}
		anchor = route.Anchor
	}
	frame, err := s.buildAttemptDispatchFrame(ctx, att, proj, anchor, promptOverride, swarmIdleTimeoutMs, task.ParentTaskID, task.DelegatedRole)
	if err != nil {
		log.Printf("hub scheduler: aborting dispatch for attempt %s due to prompt/opening build error: %v", att.AttemptID, err)
		if failErr := s.failUndispatchedAttemptDetached(ctx, att.AttemptID, "ERR_HEBI8_OPENING", err.Error()); failErr != nil {
			log.Printf("hub scheduler: failed to mark undispatched attempt %s failed: %v", att.AttemptID, failErr)
		}
		return false
	}
	// B10: Atomically bind runner_instance_id INSIDE the serialized runner write pump right before
	// writing attempt.dispatch (only if still leased & !cancel_requested), so any concurrent cancel
	// either aborts dispatch before it ever touches the wire, or is strictly ordered AFTER dispatch.
	if _, err := s.runners.DispatchAttemptWithHook(att.MachineID, frame, func(activeInstID string) error {
		bound, err := s.store.TryBindAttemptDispatched(ctx, att.AttemptID, activeInstID)
		if err != nil {
			return err
		}
		if !bound {
			return fmt.Errorf("attempt %s already cancelled or bound", att.AttemptID)
		}
		return nil
	}); err != nil {
		return false
	}
	return true
}

func (s *Server) dispatchPendingLeasedForMachine(ctx context.Context, machineID string) {
	atts, err := s.store.ListLeasedAttemptsForMachine(ctx, machineID)
	if err != nil {
		return
	}
	for _, att := range atts {
		s.dispatchAttemptRecord(ctx, att, "", 0)
	}
}

func (s *Server) drainPendingQueues(ctx context.Context) {
	notices, err := s.store.ListTasksPendingQueueDrain(ctx)
	if err != nil {
		return
	}
	for _, n := range notices {
		s.consumeAndDispatchNextPendingInput(ctx, n.TaskID, n.SteerInputID)
	}
}

func (s *Server) consumeAndDispatchNextPendingInput(ctx context.Context, taskID, preferredInputID string) {
	task, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return
	}
	proj, err := s.store.GetProject(ctx, task.ProjectID)
	if err != nil {
		return
	}
	route, err := s.resolveExecutionRoute(proj, task.RequiredCapability, task.PreferredAdapter)
	if err != nil {
		return
	}
	var anchorPtr *protocol.ProjectAnchorConfig
	if route.HasAnchor {
		a := route.Anchor
		anchorPtr = &a
	}
	_, att, events, err := s.store.ConsumeNextPendingInput(ctx, store.ConsumeNextPendingInputParams{
		TaskID:           taskID,
		PreferredInputID: preferredInputID,
		MachineID:        route.MachineID,
		Adapter:          route.Adapter,
		Anchor:           anchorPtr,
		AnchorCapability: route.Capability,
		LeaseTTLMs:       s.leaseTTL.Milliseconds(),
		// NEW-1: Validate the FINAL resolved route (machine, adapter, anchor, actor) inside the consume tx.
		ValidateRoute: func(finalMachineID string, finalAdapter protocol.Adapter, finalAnchor *protocol.ProjectAnchorConfig, finalActor protocol.ActorKind) error {
			s.mu.RLock()
			mCfg, hasMachine := s.machines.Machines[finalMachineID]
			s.mu.RUnlock()
			if !hasMachine && len(s.machines.Machines) > 0 {
				return fmt.Errorf("%w: unknown machine %q", store.ErrInvalidInput, finalMachineID)
			}
			finalRoute := resolvedRoute{
				MachineID:  finalMachineID,
				MachineCfg: mCfg,
				Adapter:    finalAdapter,
			}
			if finalAnchor != nil {
				finalRoute.Anchor = *finalAnchor
				finalRoute.HasAnchor = true
			}
			if finalRoute.Adapter == protocol.AdapterSSHDeploy || finalRoute.MachineCfg.Transport == protocol.TransportSSHOutbound ||
				(finalRoute.HasAnchor && finalRoute.Anchor.DeployStrategy != nil) {
				return fmt.Errorf("%w: ssh_deploy cannot be auto-consumed from queue", store.ErrForbidden)
			}
			if err := enforcePostRouteGuard(finalActor, finalRoute); err != nil {
				return fmt.Errorf("%w: %v", store.ErrForbidden, err)
			}
			if _, _, err := s.validateAdapterSupport(finalRoute); err != nil {
				return fmt.Errorf("%w: %v", store.ErrInvalidInput, err)
			}
			return nil
		},
	})
	if err != nil || att == nil {
		return
	}
	if len(events) > 0 {
		s.broker.Broadcast(events...)
	}
	s.dispatchAttemptRecord(ctx, *att, "", 0)
}

// StartMaintenance launches the background lease expiration, approval expiry (B7),
// and durable queue-drain recovery (B9) scanner goroutine.
func (s *Server) StartMaintenance(ctx context.Context, wg *sync.WaitGroup) {
	interval := s.maintenanceInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		runPass := func() {
			now := time.Now().UnixMilli()
			if _, events, err := s.store.ScanExpiredAttempts(ctx, now); err == nil && len(events) > 0 {
				s.broker.Broadcast(events...)
				s.notifyChildTaskProgressAsync(events)
			}
			// B7: Proactively expire timed-out pending approvals even without human click, and unblock runners.
			if notices, events, err := s.store.ScanExpiredApprovals(ctx, now); err == nil {
				if len(events) > 0 {
					s.broker.Broadcast(events...)
				}
				for _, n := range notices {
					if n.MachineID != "" {
						_ = s.runners.SendApprovalDecision(n.MachineID, protocol.ApprovalDecisionFrame{
							Type:          protocol.RunnerMsgApprovalDecide,
							ApprovalID:    n.Decision.ApprovalID,
							DecisionID:    n.Decision.DecisionID,
							AttemptID:     n.Decision.AttemptID,
							PayloadSHA256: n.Decision.PayloadSHA256,
							DecidedBy:     n.Decision.DecidedBy,
							Expired:       true,
						})
					}
				}
			}
			// B9: Recover any durable pending_queue_drain tasks across Hub restarts.
			s.drainPendingQueues(ctx)
			// P1 #5: Recover & retry any durable pending child_parent_notifications across restarts / transient errors.
			s.ProcessPendingChildNotifications(ctx)
		}
		// Run an immediate startup recovery pass
		runPass()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runPass()
			}
		}
	}()
}
