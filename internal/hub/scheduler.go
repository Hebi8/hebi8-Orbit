package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

type resolvedRoute struct {
	MachineID  string
	MachineCfg protocol.MachineConfig
	Capability string
	Adapter    protocol.Adapter
	Anchor     protocol.ProjectAnchorConfig
	HasAnchor  bool
}

// resolveExecutionRoute resolves capability, anchor, machine, and adapter from project & machine registries.
func (s *Server) resolveExecutionRoute(
	proj protocol.ProjectView,
	reqCap string,
	reqAdapter protocol.Adapter,
) (resolvedRoute, error) {
	s.mu.RLock()
	machines := s.machines.Machines
	s.mu.RUnlock()

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
	if hasAnchor && anchor.Machine != "" {
		machineID = anchor.Machine
		if cfg, ok := machines[machineID]; ok {
			mCfg = cfg
		}
	}

	if machineID == "" {
		// Find first machine advertising capName, or fallback to local_ws
		for id, m := range machines {
			for _, c := range m.Capabilities {
				if c == capName {
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
		for id, m := range machines {
			if m.Transport == protocol.TransportLocalWS {
				machineID = id
				mCfg = m
				break
			}
		}
	}
	if machineID == "" {
		for id, m := range machines {
			machineID = id
			mCfg = m
			break
		}
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
		} else if proj.DefaultAdapter != "" {
			adapter = proj.DefaultAdapter
		} else {
			adapter = protocol.AdapterAgy
		}
	}
	// If route explicitly points at an ssh_outbound deploy target, executor is unconditionally ssh_deploy
	if mCfg.Transport == protocol.TransportSSHOutbound || (hasAnchor && anchor.DeployStrategy != nil) {
		adapter = protocol.AdapterSSHDeploy
	}

	return resolvedRoute{
		MachineID:  machineID,
		MachineCfg: mCfg,
		Capability: capName,
		Adapter:    adapter,
		Anchor:     anchor,
		HasAnchor:  hasAnchor,
	}, nil
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
	if reported := s.runners.ReportedAdapters(route.MachineID); len(reported) > 0 {
		if !slices.Contains(reported, route.Adapter) {
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
	return filepath.Join(cfg.DataDir, "worktrees", proj.ProjectID)
}

func buildOrbitSystemPromptAppend(taskID, projectID, attemptID string) string {
	return fmt.Sprintf(
		"hebi8-Orbit Execution Contract: project=%s task=%s attempt=%s. "+
			"Your working directory is an isolated task worktree. "+
			"Keep architectural decisions in repository docs/decisions/ (never write AGENTS.md/CLAUDE.md). "+
			"When your work is ready to deliver, run `orbit deliver --summary \"<concise summary>\" --final` "+
			"(if using native swarm, kill all workers before passing `--final`; omit `--final` only for intermediate preview snapshots).",
		projectID, taskID, attemptID,
	)
}

func (s *Server) buildAttemptDispatchFrame(
	ctx context.Context,
	att store.AttemptRecord,
	proj protocol.ProjectView,
	anchor protocol.ProjectAnchorConfig,
	promptOverride string,
	swarmIdleTimeoutMs int64,
) protocol.AttemptDispatchFrame {
	prompt := strings.TrimSpace(promptOverride)
	if prompt == "" && att.TriggerPayloadJSON != "" {
		var tp map[string]any
		if err := json.Unmarshal([]byte(att.TriggerPayloadJSON), &tp); err == nil {
			if p, ok := tp["prompt"].(string); ok {
				prompt = p
			}
			if swarmIdleTimeoutMs <= 0 {
				if v, ok := tp["swarmIdleTimeoutMs"].(float64); ok && v > 0 {
					swarmIdleTimeoutMs = int64(v)
				}
			}
		}
	}
	if prompt == "" {
		if task, err := s.store.GetTask(ctx, att.TaskID); err == nil {
			prompt = task.Goal
		}
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

	worktreeRoot := s.resolveWorktreeRoot(proj, effAnchor)
	worktreePath := att.WorktreePath
	if worktreePath == "" {
		if provSID != "" && expectedCWD != "" {
			worktreePath = expectedCWD
		} else {
			worktreePath = filepath.Join(worktreeRoot, att.TaskID, att.AttemptID)
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
		RepoPath:           protocol.ExpandPath(effAnchor.RepoPath),
		RepoIdentity:       proj.RepoIdentity,
		WorktreeRoot:       worktreeRoot,
		WorktreePath:       worktreePath,
		MirrorBarePath:     proj.MirrorBarePath,
		BaseSHA:            att.BaseSHA,
		DefaultBranch:      proj.DefaultBranch,
		DesiredConfig:      att.DesiredConfig,
		SystemPromptAppend: buildOrbitSystemPromptAppend(att.TaskID, att.ProjectID, att.AttemptID),
		InputDelivery:      inputDlv,
		Prompt:             prompt,
		RequiredResources:  att.RequiredResources,
		SwarmIdleTimeoutMs: swarmIdleTimeoutMs,
	}
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
	// B10: Atomically bind runner_instance_id BEFORE sending attempt.dispatch (only if still leased & !cancel_requested),
	// so any concurrent cancel sees a dispatched attempt and persists cancel_requested=1.
	bound, err := s.store.TryBindAttemptDispatched(ctx, att.AttemptID, instID)
	if err != nil || !bound {
		return false
	}
	frame := s.buildAttemptDispatchFrame(ctx, att, proj, anchor, promptOverride, swarmIdleTimeoutMs)
	if _, err := s.runners.DispatchAttempt(att.MachineID, frame); err != nil {
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
	if route.Adapter == protocol.AdapterSSHDeploy {
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
	})
	if err != nil || att == nil {
		return
	}
	if len(events) > 0 {
		s.broker.Broadcast(events...)
	}
	s.dispatchAttemptRecord(ctx, *att, "", 0)
}

// StartMaintenance launches the background lease expiration scanner goroutine.
func (s *Server) StartMaintenance(ctx context.Context, wg *sync.WaitGroup) {
	interval := s.maintenanceInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, events, err := s.store.ScanExpiredAttempts(ctx, time.Now().UnixMilli())
				if err == nil && len(events) > 0 {
					s.broker.Broadcast(events...)
				}
			}
		}
	}()
}
