package hub

import (
	"context"
	"encoding/json"
	"fmt"
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
	reqMachineID ...string,
) (resolvedRoute, error) {
	s.mu.RLock()
	machines := make(map[string]protocol.MachineConfig, len(s.machines.Machines))
	for k, v := range s.machines.Machines {
		machines[k] = v
	}
	s.mu.RUnlock()

	explicitMachine := ""
	if len(reqMachineID) > 0 {
		explicitMachine = strings.TrimSpace(reqMachineID[0])
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
		if proj.ProjectID != protocol.ScratchProjectID && len(proj.Anchors) > 0 {
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

	repoPath := strings.TrimSpace(effAnchor.RepoPath)
	if !isRemoteMachine {
		repoPath = protocol.ExpandPath(repoPath)
	}
	mirrorBarePath := proj.MirrorBarePath
	if isRemoteMachine && proj.ProjectID != protocol.ScratchProjectID && strings.TrimSpace(proj.MirrorBarePath) != "" {
		// Only switch to Hub HTTP bundle transport when the connected Runner negotiates bundle.v1,
		// so older Runners with pre-configured remote mirror paths continue working cleanly.
		if s.runners.HasCapability(att.MachineID, protocol.RunnerCapBundleV1) {
			mirrorBarePath = "orbit-hub://" + proj.ProjectID
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
		WorktreeRoot:       worktreeRoot,
		WorktreePath:       worktreePath,
		MirrorBarePath:     mirrorBarePath,
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
	frame := s.buildAttemptDispatchFrame(ctx, att, proj, anchor, promptOverride, swarmIdleTimeoutMs)
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
