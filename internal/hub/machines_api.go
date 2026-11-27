package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"github.com/Hebi8/hebi8-Orbit/internal/discovery"
	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

// refreshEffectiveRegistry merges static YAML machines/projects with dynamic SQLite rows
// (YAML always wins on identical IDs) and refreshes dynamic network settings into memory.
// Serialized end-to-end by registryRefreshMu so overlapping DB mutations can never publish
// a stale snapshot over a newer commit.
func (s *Server) refreshEffectiveRegistry(ctx context.Context) error {
	s.registryRefreshMu.Lock()
	defer s.registryRefreshMu.Unlock()

	dynMachines, err := s.store.ListRegistryMachines(ctx)
	if err != nil {
		return err
	}
	dynProjects, err := s.store.ListDynamicProjects(ctx)
	if err != nil {
		return err
	}
	dynNet, err := s.store.GetDynamicNetworkSettings(ctx)
	if err != nil {
		return err
	}

	removedSet := map[string]bool{}
	for _, id := range dynNet.RemovedMachines {
		if t := strings.TrimSpace(id); t != "" {
			removedSet[t] = true
		}
	}

	s.mu.Lock()
	mergedMachines := make(map[string]protocol.MachineConfig, len(s.yamlMachines.Machines)+len(dynMachines))
	for id, m := range dynMachines {
		if removedSet[id] {
			continue
		}
		mergedMachines[id] = m
	}
	for id, m := range s.yamlMachines.Machines {
		if removedSet[id] {
			continue
		}
		if _, isPaired := dynMachines[id]; isPaired {
			continue
		}
		mergedMachines[id] = m
	}

	mergedProjects := make(map[string]protocol.ProjectConfig, len(s.yamlProjects.Projects)+len(dynProjects))
	for id, p := range dynProjects {
		mergedProjects[id] = p
	}
	for id, p := range s.yamlProjects.Projects {
		mergedProjects[id] = p
	}

	s.machines = protocol.MachinesConfig{
		SchemaVersion: 1,
		Machines:      mergedMachines,
	}
	s.projects = protocol.ProjectsConfig{
		SchemaVersion: 1,
		Projects:      mergedProjects,
	}
	s.dynNet = dynNet
	s.mu.Unlock()
	return nil
}

func (s *Server) effectiveWorkspaceName() string {
	s.mu.RLock()
	dyn := strings.TrimSpace(s.dynNet.WorkspaceName)
	yamlName := strings.TrimSpace(s.hubCfg.WorkspaceName)
	s.mu.RUnlock()
	if dyn != "" {
		return dyn
	}
	if yamlName != "" {
		return yamlName
	}
	return "Home LAN"
}

func (s *Server) effectiveYAMLMachinesLocked() map[string]protocol.MachineConfig {
	removedSet := map[string]bool{}
	for _, id := range s.dynNet.RemovedMachines {
		if t := strings.TrimSpace(id); t != "" {
			removedSet[t] = true
		}
	}
	out := make(map[string]protocol.MachineConfig, len(s.yamlMachines.Machines))
	for k, v := range s.yamlMachines.Machines {
		if removedSet[k] {
			continue
		}
		out[k] = v
	}
	return out
}

func (s *Server) effectiveYAMLMachines() map[string]protocol.MachineConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.effectiveYAMLMachinesLocked()
}

func (s *Server) enrichMachineViews(ctx context.Context, machines []protocol.MachineView) []protocol.MachineView {
	yamlMachines := s.effectiveYAMLMachines()

	out := make([]protocol.MachineView, len(machines))
	for i, mv := range machines {
		ym, inYAML := yamlMachines[mv.MachineID]
		if inYAML {
			mv.Source = protocol.MachineSourceYAML
			mv.LockedFields = []string{"transport", "capabilities"}
			if ym.DisplayName != "" {
				mv.LockedFields = append(mv.LockedFields, "displayName")
			}
		} else {
			mv.Source = protocol.MachineSourcePaired
		}

		// Live WS presence overrides stale DB state if connected
		if liveCaps := s.runners.Capabilities(mv.MachineID); len(liveCaps) > 0 {
			mv.RunnerCapabilities = liveCaps
		}

		settings, _, _ := s.store.GetMachineAgentSettings(ctx, mv.MachineID)
		mv.Agents = mergeMachineAgents(mv, ym, inYAML, settings)
		if len(mv.AgentQuotas) > 0 {
			qByAdp := make(map[protocol.Adapter]protocol.AgentQuotaSnapshot, len(mv.AgentQuotas))
			for _, q := range mv.AgentQuotas {
				qByAdp[q.Adapter] = q
			}
			for j := range mv.Agents {
				if q, ok := qByAdp[mv.Agents[j].Adapter]; ok {
					qCopy := q
					mv.Agents[j].Quota = &qCopy
				}
			}
		}
		out[i] = mv
	}
	return out
}

func mergeMachineAgents(
	mv protocol.MachineView,
	ym protocol.MachineConfig,
	inYAML bool,
	settings map[string]protocol.RunnerAgentConfig,
) []protocol.AgentProbe {
	byAdapter := map[protocol.Adapter]protocol.AgentProbe{}
	for _, p := range mv.Agents {
		byAdapter[p.Adapter] = p
	}
	readySet := map[protocol.Adapter]bool{}
	for _, a := range mv.Adapters {
		readySet[a] = true
	}

	result := make([]protocol.AgentProbe, 0, len(protocol.Phase1ImplementedSessionAdapters))
	for _, adp := range protocol.Phase1ImplementedSessionAdapters {
		probe, hasProbe := byAdapter[adp]
		if !hasProbe {
			probe = protocol.AgentProbe{
				Adapter: adp,
				Enabled: true,
				State:   protocol.AgentStateNotInstalled,
				Source:  "none",
			}
			if readySet[adp] {
				probe.State = protocol.AgentStateReady
				probe.Source = "file"
			}
		}

		var locked []string
		if inYAML {
			if yCfg, ok := ym.Adapters[string(adp)]; ok {
				if strings.TrimSpace(yCfg.Bin) != "" {
					locked = append(locked, "bin")
					if probe.BridgeBin == "" {
						probe.BridgeBin = yCfg.Bin
					}
				}
				if len(yCfg.DefaultArgs) > 0 {
					locked = append(locked, "args")
				}
				if yCfg.MaxLiveWorkers > 0 {
					locked = append(locked, "maxLiveWorkers")
					if probe.MaxLiveWorkers == 0 {
						probe.MaxLiveWorkers = yCfg.MaxLiveWorkers
					}
				}
			}
		}

		if sc, ok := settings[string(adp)]; ok {
			if sc.Enabled != nil {
				probe.Enabled = *sc.Enabled
				if !*sc.Enabled {
					probe.State = protocol.AgentStateDisabled
				}
			}
			if sc.Bin != "" && !slices.Contains(locked, "bin") {
				probe.BridgeBin = sc.Bin
			}
			if sc.DefaultModel != "" {
				probe.DefaultModel = sc.DefaultModel
			}
			if sc.MaxLiveWorkers > 0 && !slices.Contains(locked, "maxLiveWorkers") {
				probe.MaxLiveWorkers = sc.MaxLiveWorkers
			}
		}
		probe.LockedFields = locked
		result = append(result, probe)
	}
	return result
}

func (s *Server) buildSetupStatus(ctx context.Context, machines []protocol.MachineView, currentMachineID string) protocol.SetupStatusView {
	done, _ := s.store.IsOnboardingDone(ctx)
	readyAgents := 0
	onlineMachines := 0
	for _, m := range machines {
		if m.Status == protocol.MachineOnline && m.TransportMode != protocol.TransportSSHOutbound {
			onlineMachines++
			for _, ag := range m.Agents {
				if ag.State == protocol.AgentStateReady && ag.Enabled {
					readyAgents++
				}
			}
		}
	}
	tsAvail, tsShared, tsOrigin, _, _ := s.probeTailscaleState(ctx)
	discID, _ := s.store.EnsureDiscoveryID(ctx)
	dutyPriority, activeDuty := s.effectiveDutyPriority(ctx, machines)
	return protocol.SetupStatusView{
		NeedsOnboarding:     !done,
		ReadyAgents:         readyAgents,
		OnlineMachines:      onlineMachines,
		CurrentMachineID:    currentMachineID,
		DiscoveryID:         discID,
		DutyPriority:        dutyPriority,
		ActiveDutyMachineID: activeDuty,
		HubOrigin:           s.effectiveHubOrigin(),
		TailscaleAvailable:  tsAvail,
		TailscaleShared:     tsShared,
		TailscaleOrigin:     tsOrigin,
	}
}

func (s *Server) handleListMachines(w http.ResponseWriter, r *http.Request) {
	yamlMachines := s.effectiveYAMLMachines()

	boot, err := s.store.Bootstrap(r.Context(), yamlMachines)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_LIST_MACHINES", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, protocol.ListMachinesResponse{
		Machines: s.enrichMachineViews(r.Context(), boot.Machines),
	})
}

func (s *Server) handleUpdateMachine(w http.ResponseWriter, r *http.Request) {
	machineID := chi.URLParam(r, "machineId")
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "failed reading request body")
		return
	}
	var rawFields map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &rawFields); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}

	yamlMachines := s.effectiveYAMLMachines()
	ym, inYAML := yamlMachines[machineID]
	if inYAML {
		lockedKeys := map[string]bool{
			"transport":     true,
			"transportMode": true,
			"capabilities":  true,
			"adapters":      true,
			"tokenHash":     true,
		}
		if strings.TrimSpace(ym.DisplayName) != "" {
			lockedKeys["displayName"] = true
		}
		for k := range rawFields {
			if lockedKeys[k] {
				writeAPIError(w, http.StatusConflict, "ERR_FILE_MANAGED", fmt.Sprintf("field %q is managed in machines.yaml and cannot be modified via API", k))
				return
			}
		}
	}

	var req protocol.UpdateMachineRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	if req.DisplayAlias != nil || req.Role != nil || req.AllowedRoots != nil || req.SSHAlias != nil {
		ev, err := s.store.UpdateMachineMetadata(r.Context(), machineID, req, actorFromContext(r.Context()))
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "ERR_MACHINE_NOT_FOUND", err.Error())
			return
		}
		if errors.Is(err, store.ErrInvalidInput) {
			writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_INPUT", err.Error())
			return
		}
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "ERR_UPDATE_MACHINE", err.Error())
			return
		}
		_ = s.refreshEffectiveRegistry(context.WithoutCancel(r.Context()))
		s.broker.Broadcast(ev)
	}
	s.handleListMachines(w, r)
}

func removeMachineFromYAMLFile(configDir, machineID string) error {
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		return nil
	}
	machPath := filepath.Join(configDir, "machines.yaml")
	cfg, err := protocol.LoadMachinesConfig(machPath)
	if err != nil {
		return err
	}
	if _, exists := cfg.Machines[machineID]; !exists {
		return nil
	}
	delete(cfg.Machines, machineID)
	out, err := yaml.Marshal(&cfg)
	if err != nil {
		return err
	}
	tmpPath := machPath + ".tmp"
	if err := os.WriteFile(tmpPath, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, machPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func (s *Server) handleDeleteMachine(w http.ResponseWriter, r *http.Request) {
	machineID := chi.URLParam(r, "machineId")
	ev, revokedClientIDs, err := s.store.RemoveMachine(r.Context(), machineID, actorFromContext(r.Context()))
	if errors.Is(err, store.ErrCannotDeleteLocal) {
		writeAPIError(w, http.StatusConflict, "ERR_CANNOT_DELETE_LOCAL", "local hub machine cannot be removed")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_MACHINE_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_REMOVE_MACHINE", err.Error())
		return
	}

	// Best-effort sync removal from ConfigDir/machines.yaml if present & writable;
	// durable tombstone in SQLite network_settings guarantees no resurrection even if YAML is read-only.
	s.mu.Lock()
	delete(s.yamlMachines.Machines, machineID)
	cfgDir := s.hubCfg.ConfigDir
	s.mu.Unlock()
	_ = removeMachineFromYAMLFile(cfgDir, machineID)

	for _, cid := range revokedClientIDs {
		s.revokeClientTokenSessions(cid)
	}
	s.runners.DisconnectMachine(machineID, "machine removed")
	if err := s.refreshEffectiveRegistry(context.WithoutCancel(r.Context())); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_REGISTRY_REFRESH", err.Error())
		return
	}
	s.broker.Broadcast(ev)
	writeJSON(w, http.StatusOK, map[string]any{"removed": true, "machineId": machineID})
}

func (s *Server) handleProbeMachineAgents(w http.ResponseWriter, r *http.Request) {
	machineID := chi.URLParam(r, "machineId")
	if !s.runners.IsConnected(machineID) {
		writeAPIError(w, http.StatusServiceUnavailable, "ERR_MACHINE_OFFLINE", "machine runner is not connected")
		return
	}
	if !s.runners.HasCapability(machineID, protocol.RunnerCapAgentsV1) {
		writeAPIError(w, http.StatusNotImplemented, "ERR_CAPABILITY_UNSUPPORTED", "runner version does not support remote agent probing")
		return
	}
	res, err := s.runners.CallRPC(r.Context(), machineID, "probe", nil, 25*time.Second)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "ERR_RUNNER_RPC", err.Error())
		return
	}
	if !res.OK {
		writeAPIError(w, http.StatusBadGateway, "ERR_RUNNER_PROBE", res.Error)
		return
	}
	probes := decodeProbesFromRPC(res.Data)
	if len(probes) > 0 {
		if ev, err := s.store.SaveMachineAgentProbes(r.Context(), machineID, probes, nil); err == nil && ev != nil {
			s.broker.Broadcast(*ev)
		}
	}
	quotas := decodeQuotasFromRPC(res.Data)
	if len(quotas) > 0 {
		if ev, err := s.store.SaveMachineAgentQuotas(r.Context(), machineID, quotas); err == nil && ev != nil {
			s.broker.Broadcast(*ev)
		}
	}
	writeJSON(w, http.StatusOK, protocol.MachineAgentActionResponse{
		MachineID:   machineID,
		Agents:      probes,
		AgentQuotas: quotas,
	})
}

func (s *Server) handleRefreshMachineQuotas(w http.ResponseWriter, r *http.Request) {
	machineID := chi.URLParam(r, "machineId")
	s.mu.RLock()
	_, exists := s.machines.Machines[machineID]
	s.mu.RUnlock()
	if !exists {
		writeAPIError(w, http.StatusNotFound, "ERR_MACHINE_NOT_FOUND", "machine not found")
		return
	}
	if !s.runners.IsConnected(machineID) {
		writeAPIError(w, http.StatusServiceUnavailable, "ERR_MACHINE_OFFLINE", "machine runner is not connected")
		return
	}
	if !s.runners.HasCapability(machineID, protocol.RunnerCapQuotasV1) {
		writeAPIError(w, http.StatusNotImplemented, "ERR_CAPABILITY_UNSUPPORTED", "runner version does not support quota probing")
		return
	}
	res, err := s.runners.CallRPC(r.Context(), machineID, "probe_quotas", map[string]any{"force": true}, 25*time.Second)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "ERR_RUNNER_RPC", err.Error())
		return
	}
	if !res.OK {
		writeAPIError(w, http.StatusBadGateway, "ERR_RUNNER_QUOTAS", res.Error)
		return
	}
	quotas := decodeQuotasFromRPC(res.Data)
	if quotas == nil {
		quotas = []protocol.AgentQuotaSnapshot{}
	}
	if ev, err := s.store.SaveMachineAgentQuotas(r.Context(), machineID, quotas); err == nil && ev != nil {
		s.broker.Broadcast(*ev)
	}
	writeJSON(w, http.StatusOK, protocol.MachineQuotasResponse{
		MachineID:   machineID,
		AgentQuotas: quotas,
	})
}

func (s *Server) handleTestMachineAgent(w http.ResponseWriter, r *http.Request) {
	machineID := chi.URLParam(r, "machineId")
	adp := protocol.Adapter(chi.URLParam(r, "adapter"))
	if !protocol.SessionAdapter(adp).IsValid() {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_ADAPTER", "unsupported adapter")
		return
	}
	if !s.runners.IsConnected(machineID) {
		writeAPIError(w, http.StatusServiceUnavailable, "ERR_MACHINE_OFFLINE", "machine runner is not connected")
		return
	}
	if !s.runners.HasCapability(machineID, protocol.RunnerCapAgentsV1) {
		writeAPIError(w, http.StatusNotImplemented, "ERR_CAPABILITY_UNSUPPORTED", "runner does not support remote agent test")
		return
	}
	res, err := s.runners.CallRPC(r.Context(), machineID, "test_agent", map[string]any{"adapter": string(adp)}, 60*time.Second)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "ERR_RUNNER_RPC", err.Error())
		return
	}
	probes := decodeProbesFromRPC(res.Data)
	probe := decodeSingleProbeFromRPC(res.Data)
	if len(probes) > 0 {
		if ev, err := s.store.SaveMachineAgentProbes(r.Context(), machineID, probes, nil); err == nil && ev != nil {
			s.broker.Broadcast(*ev)
		}
	}
	if !res.OK && probe == nil {
		writeAPIError(w, http.StatusBadGateway, "ERR_AGENT_TEST", res.Error)
		return
	}
	writeJSON(w, http.StatusOK, protocol.MachineAgentActionResponse{
		MachineID: machineID,
		Adapter:   adp,
		Probe:     probe,
		Agents:    probes,
	})
}

func (s *Server) handleInstallMachineAgent(w http.ResponseWriter, r *http.Request) {
	machineID := chi.URLParam(r, "machineId")
	adp := protocol.Adapter(chi.URLParam(r, "adapter"))
	if _, ok := protocol.PinnedBridgePackages[adp]; !ok {
		writeAPIError(w, http.StatusBadRequest, "ERR_INSTALL_UNSUPPORTED", "automatic bridge installation is only supported for claude and codex")
		return
	}
	if !s.runners.IsConnected(machineID) {
		writeAPIError(w, http.StatusServiceUnavailable, "ERR_MACHINE_OFFLINE", "machine runner is not connected")
		return
	}
	if !s.runners.HasCapability(machineID, protocol.RunnerCapAgentsV1) {
		writeAPIError(w, http.StatusNotImplemented, "ERR_CAPABILITY_UNSUPPORTED", "runner does not support remote bridge install")
		return
	}
	res, err := s.runners.CallRPC(r.Context(), machineID, "install_bridge", map[string]any{"adapter": string(adp)}, 180*time.Second)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "ERR_RUNNER_RPC", err.Error())
		return
	}
	probes := decodeProbesFromRPC(res.Data)
	probe := decodeSingleProbeFromRPC(res.Data)
	if len(probes) > 0 {
		if ev, err := s.store.SaveMachineAgentProbes(r.Context(), machineID, probes, nil); err == nil && ev != nil {
			s.broker.Broadcast(*ev)
		}
	}
	if !res.OK {
		msg := res.Error
		if msg == "" {
			msg = "bridge installation failed"
		}
		writeAPIError(w, http.StatusBadGateway, "ERR_AGENT_INSTALL", msg)
		return
	}
	writeJSON(w, http.StatusOK, protocol.MachineAgentActionResponse{
		MachineID: machineID,
		Adapter:   adp,
		Probe:     probe,
		Agents:    probes,
	})
}

func (s *Server) handleUpdateMachineAgent(w http.ResponseWriter, r *http.Request) {
	machineID := chi.URLParam(r, "machineId")
	adp := protocol.Adapter(chi.URLParam(r, "adapter"))
	if !protocol.SessionAdapter(adp).IsValid() {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_ADAPTER", "unsupported adapter")
		return
	}
	var req protocol.UpdateMachineAgentRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}

	// Enforce YAML field locks: return 409 ERR_FILE_MANAGED if caller modifies YAML-managed fields.
	yamlMachines := s.effectiveYAMLMachines()
	ym, inYAML := yamlMachines[machineID]
	if inYAML {
		if yCfg, ok := ym.Adapters[string(adp)]; ok {
			if req.Bin != nil && strings.TrimSpace(yCfg.Bin) != "" && strings.TrimSpace(*req.Bin) != strings.TrimSpace(yCfg.Bin) {
				writeAPIError(w, http.StatusConflict, "ERR_FILE_MANAGED", "bin is managed in machines.yaml")
				return
			}
			if req.Args != nil && len(yCfg.DefaultArgs) > 0 {
				writeAPIError(w, http.StatusConflict, "ERR_FILE_MANAGED", "args is managed in machines.yaml")
				return
			}
			if req.MaxLiveWorkers != nil && yCfg.MaxLiveWorkers > 0 && *req.MaxLiveWorkers != yCfg.MaxLiveWorkers {
				writeAPIError(w, http.StatusConflict, "ERR_FILE_MANAGED", "maxLiveWorkers is managed in machines.yaml")
				return
			}
		}
	}
	if req.Bin != nil && strings.TrimSpace(*req.Bin) != "" && !filepath.IsAbs(protocol.ExpandPath(*req.Bin)) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_BIN", "custom binary path must be an absolute path")
		return
	}

	_, err := s.store.UpsertMachineAgentSetting(r.Context(), machineID, adp, req)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_MACHINE_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_AGENT_UPDATE", err.Error())
		return
	}

	// Push live runner.config update over WS so Runner hot-applies without restart.
	settings, rev, err := s.store.GetMachineAgentSettings(r.Context(), machineID)
	if err == nil && s.runners.IsConnected(machineID) && s.runners.HasCapability(machineID, protocol.RunnerCapAgentsV1) {
		_ = s.runners.SendConfig(machineID, protocol.RunnerConfigFrame{
			Type:      protocol.RunnerMsgConfig,
			ConfigRev: rev,
			Agents:    settings,
		})
	}

	// Update cached probes enabled state immediately if runner is offline
	probes, _ := s.store.GetMachineAgentProbes(r.Context(), machineID)
	if req.Enabled != nil && len(probes) > 0 {
		for i := range probes {
			if probes[i].Adapter == adp {
				probes[i].Enabled = *req.Enabled
				if !*req.Enabled {
					probes[i].State = protocol.AgentStateDisabled
				}
			}
		}
		if ev, err := s.store.SaveMachineAgentProbes(r.Context(), machineID, probes, nil); err == nil && ev != nil {
			s.broker.Broadcast(*ev)
		}
	} else {
		s.broker.Notify()
	}

	writeJSON(w, http.StatusOK, protocol.MachineAgentActionResponse{
		MachineID: machineID,
		Adapter:   adp,
		Agents:    probes,
	})
}

func (s *Server) handleListMachineDirs(w http.ResponseWriter, r *http.Request) {
	machineID := chi.URLParam(r, "machineId")
	reqPath := strings.TrimSpace(r.URL.Query().Get("path"))

	s.mu.RLock()
	mCfg, exists := s.machines.Machines[machineID]
	s.mu.RUnlock()
	if !exists {
		writeAPIError(w, http.StatusNotFound, "ERR_MACHINE_NOT_FOUND", "machine not found")
		return
	}

	allowedRoots := s.store.GetMachineAllowedRoots(r.Context(), machineID)
	if s.runners.IsConnected(machineID) && s.runners.HasCapability(machineID, protocol.RunnerCapFSV1) {
		rpcArgs := map[string]any{"path": reqPath}
		if len(allowedRoots) > 0 {
			rpcArgs["allowedRoots"] = allowedRoots
		}
		res, err := s.runners.CallRPC(r.Context(), machineID, "list_dirs", rpcArgs, 15*time.Second)
		if err != nil {
			writeAPIError(w, http.StatusBadGateway, "ERR_RUNNER_RPC", err.Error())
			return
		}
		if !res.OK {
			code, status := browseErrorFromRPCResult(res)
			writeAPIError(w, status, code, res.Error)
			return
		}
		var out protocol.ListDirsResponse
		if raw, err := json.Marshal(res.Data); err == nil {
			_ = json.Unmarshal(raw, &out)
		}
		out.MachineID = machineID
		if out.Entries == nil {
			out.Entries = []protocol.DirEntryView{}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	// Safe local fallback ONLY when target is Hub's own local_ws machine
	if mCfg.Transport == protocol.TransportLocalWS {
		out, err := gitops.ListSubdirs(reqPath, allowedRoots, false)
		if err != nil {
			code, status := gitops.ClassifyBrowseError(err)
			writeAPIError(w, status, code, err.Error())
			return
		}
		out.MachineID = machineID
		writeJSON(w, http.StatusOK, out)
		return
	}

	writeAPIError(w, http.StatusServiceUnavailable, "ERR_MACHINE_OFFLINE", "machine runner is not connected")
}

func browseErrorFromRPCResult(res protocol.RunnerRPCResultFrame) (code string, status int) {
	if rawCode, ok := res.Data["errorCode"].(string); ok && strings.TrimSpace(rawCode) != "" {
		switch strings.TrimSpace(rawCode) {
		case "ERR_PATH_OUTSIDE_ROOT":
			return "ERR_PATH_OUTSIDE_ROOT", http.StatusForbidden
		case "ERR_DIR_NOT_FOUND":
			return "ERR_DIR_NOT_FOUND", http.StatusNotFound
		case "ERR_NOT_A_DIRECTORY":
			return "ERR_NOT_A_DIRECTORY", http.StatusBadRequest
		case "ERR_PERMISSION_DENIED":
			return "ERR_PERMISSION_DENIED", http.StatusForbidden
		default:
			return strings.TrimSpace(rawCode), http.StatusBadRequest
		}
	}
	lower := strings.ToLower(res.Error)
	switch {
	case strings.Contains(lower, "outside allowed root"):
		return "ERR_PATH_OUTSIDE_ROOT", http.StatusForbidden
	case strings.Contains(lower, "does not exist") || strings.Contains(lower, "no such file"):
		return "ERR_DIR_NOT_FOUND", http.StatusNotFound
	case strings.Contains(lower, "not a directory"):
		return "ERR_NOT_A_DIRECTORY", http.StatusBadRequest
	case strings.Contains(lower, "permission denied"):
		return "ERR_PERMISSION_DENIED", http.StatusForbidden
	default:
		return "ERR_LIST_DIRS", http.StatusBadRequest
	}
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req protocol.CreateProjectRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	machineID := strings.TrimSpace(req.MachineID)
	repoPath := strings.TrimSpace(req.RepoPath)
	if machineID == "" || repoPath == "" {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_INPUT", "machineId and repoPath are required")
		return
	}

	s.mu.RLock()
	mCfg, exists := s.machines.Machines[machineID]
	s.mu.RUnlock()
	if !exists {
		writeAPIError(w, http.StatusNotFound, "ERR_MACHINE_NOT_FOUND", "machine not found")
		return
	}

	var repoCheck protocol.CheckRepoResponse
	if req.AllowFolder {
		insp, st, code, err := s.inspectMachineDirectory(r.Context(), machineID, repoPath)
		if err != nil {
			writeAPIError(w, st, code, err.Error())
			return
		}
		repoCheck = insp
	} else if s.runners.IsConnected(machineID) && s.runners.HasCapability(machineID, protocol.RunnerCapFSV1) {
		res, err := s.runners.CallRPC(r.Context(), machineID, "check_repo", map[string]any{"path": repoPath}, 15*time.Second)
		if err != nil {
			writeAPIError(w, http.StatusBadGateway, "ERR_RUNNER_RPC", err.Error())
			return
		}
		if raw, err := json.Marshal(res.Data); err == nil {
			_ = json.Unmarshal(raw, &repoCheck)
		}
		if repoCheck.Valid {
			repoCheck.Exists = true
			repoCheck.IsGit = true
		}
		if !res.OK && repoCheck.Error == "" {
			repoCheck.Error = res.Error
		}
	} else if mCfg.Transport == protocol.TransportLocalWS {
		repoCheck = gitops.CheckGitRepo(r.Context(), repoPath, s.store.GetMachineAllowedRoots(r.Context(), machineID))
		if repoCheck.Valid {
			repoCheck.Exists = true
			repoCheck.IsGit = true
		}
	} else {
		writeAPIError(w, http.StatusServiceUnavailable, "ERR_MACHINE_OFFLINE", "target machine runner is offline")
		return
	}

	if !repoCheck.Valid {
		msg := repoCheck.Error
		if msg == "" {
			msg = "selected directory is not a valid Git repository"
		}
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_REPO", msg)
		return
	}

	projID := sanitizeProjectID(req.ProjectID)
	if projID == "" {
		projID = sanitizeProjectID(repoCheck.DisplayName)
	}
	if projID == "" {
		projID = "project"
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = repoCheck.DisplayName
	}

	capName := "primary-dev"
	if len(mCfg.Capabilities) > 0 {
		capName = mCfg.Capabilities[0]
	}
	adapter := req.DefaultAdapter
	if adapter == "" {
		adapter = s.pickReadyAdapterForMachine(r.Context(), machineID)
	}

	cfg := s.hubConfigSnapshot()
	mirrorPath := ""
	var mirrorPrep func(finalMirrorPath string) error
	if repoCheck.IsGit {
		mirrorPath = filepath.Join(cfg.DataDir, "git", projID+".git")
		mirrorPrep = func(finalMirrorPath string) error {
			return gitops.EnsureBareMirror(context.WithoutCancel(r.Context()), finalMirrorPath)
		}
	}

	projCfg := protocol.ProjectConfig{
		DisplayName:       displayName,
		RepoIdentity:      repoCheck.RepoIdentity,
		DefaultBranch:     repoCheck.DefaultBranch,
		DefaultCapability: capName,
		DefaultAdapter:    adapter,
		IDEScheme:         protocol.IDECursor,
		Anchors: map[string]protocol.ProjectAnchorConfig{
			capName: {
				Machine:  machineID,
				RepoPath: repoCheck.RepoRoot,
			},
		},
	}

	pv, ev, err := s.store.CreateDynamicProject(r.Context(), projID, projCfg, mirrorPath, actorFromContext(r.Context()), mirrorPrep)
	if errors.Is(err, store.ErrConflict) {
		writeAPIError(w, http.StatusConflict, "ERR_PROJECT_EXISTS", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PROJECT_CREATE", err.Error())
		return
	}
	// Persist initial probed runtime state onto the newly created default location!
	if len(pv.Parts) > 0 && len(pv.Parts[0].Locations) > 0 {
		_ = s.store.UpsertLocationRuntimeState(r.Context(), pv.Parts[0].Locations[0].LocationID, *checkRepoToRuntimeState(repoCheck))
		if refreshed, rErr := s.store.GetProject(r.Context(), pv.ProjectID); rErr == nil {
			pv = refreshed
		}
	}

	if err := s.refreshEffectiveRegistry(context.WithoutCancel(r.Context())); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_REGISTRY_REFRESH", err.Error())
		return
	}
	s.broker.Broadcast(ev)
	writeJSON(w, http.StatusCreated, protocol.CreateProjectResponse{Project: pv})
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	ev, err := s.store.DeleteDynamicProject(r.Context(), projectID, actorFromContext(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_PROJECT_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrForbidden) {
		writeAPIError(w, http.StatusConflict, "ERR_FILE_MANAGED", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PROJECT_DELETE", err.Error())
		return
	}
	if err := s.refreshEffectiveRegistry(context.WithoutCancel(r.Context())); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_REGISTRY_REFRESH", err.Error())
		return
	}
	s.broker.Broadcast(ev)
	writeJSON(w, http.StatusOK, map[string]any{"removed": true, "projectId": projectID})
}

func (s *Server) pickReadyAdapterForMachine(ctx context.Context, machineID string) protocol.Adapter {
	if probes, err := s.store.GetMachineAgentProbes(ctx, machineID); err == nil {
		for _, pref := range []protocol.Adapter{protocol.AdapterAgy, protocol.AdapterClaude, protocol.AdapterCodex} {
			for _, p := range probes {
				if p.Adapter == pref && p.State == protocol.AgentStateReady && p.Enabled {
					return pref
				}
			}
		}
	}
	if reported := s.runners.ReportedAdapters(machineID); len(reported) > 0 {
		return reported[0]
	}
	return protocol.AdapterAgy
}

func sanitizeProjectID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func (s *Server) handleGetNetworkSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildNetworkSettingsView(r.Context()))
}

func (s *Server) handleUpdateNetworkSettings(w http.ResponseWriter, r *http.Request) {
	var req protocol.UpdateNetworkSettingsRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	cur, err := s.store.GetDynamicNetworkSettings(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_SETTINGS_READ", err.Error())
		return
	}
	if req.WorkspaceName != nil {
		cur.WorkspaceName = strings.TrimSpace(*req.WorkspaceName)
	}
	if req.DutyPriority != nil {
		cleaned := make([]string, 0, len(req.DutyPriority))
		seen := map[string]bool{}
		for _, id := range req.DutyPriority {
			t := strings.TrimSpace(id)
			if t == "" || seen[t] {
				continue
			}
			seen[t] = true
			cleaned = append(cleaned, t)
		}
		cur.DutyPriority = cleaned
	}
	if req.ExternalGatewayEnabled != nil {
		v := *req.ExternalGatewayEnabled
		cur.ExternalGatewayEnabled = &v
	}
	ev, err := s.store.SaveDynamicNetworkSettings(r.Context(), cur, actorFromContext(r.Context()))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_SETTINGS_SAVE", err.Error())
		return
	}
	if err := s.refreshEffectiveRegistry(context.WithoutCancel(r.Context())); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_REGISTRY_REFRESH", err.Error())
		return
	}
	s.broker.Broadcast(ev)
	writeJSON(w, http.StatusOK, s.buildNetworkSettingsView(r.Context()))
}

func formatTailscaleOrigin(dnsName string, port int) string {
	dnsName = strings.TrimRight(strings.TrimSpace(dnsName), ".")
	if dnsName == "" {
		return ""
	}
	if port <= 0 || port == 443 {
		return "https://" + dnsName
	}
	return fmt.Sprintf("https://%s:%d", dnsName, port)
}

func (s *Server) handleDiscoverNetwork(w http.ResponseWriter, r *http.Request) {
	discID, _ := s.store.EnsureDiscoveryID(r.Context())
	_, tsShared, tsOrigin, _, _ := s.probeTailscaleState(r.Context())
	selfOrigin := s.liveEffectiveHubOrigin(tsShared, tsOrigin)
	res := discovery.Discover(r.Context(), discovery.Options{
		SelfDiscoveryID: discID,
		SelfOrigin:      selfOrigin,
		IncludeLocal:    true,
	})
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleTailscaleEnable(w http.ResponseWriter, r *http.Request) {
	if s.hubConfigSnapshot().DevOpen {
		writeAPIError(w, http.StatusBadRequest, "ERR_DEV_OPEN_INCOMPATIBLE", "请先关闭 devOpen 模式后再开启多机共享")
		return
	}
	var req protocol.TailscaleEnableRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	tsBin := findTailscaleBin()
	if tsBin == "" {
		writeAPIError(w, http.StatusPreconditionFailed, "ERR_TAILSCALE_MISSING", "未检测到 Tailscale，请先安装并登录 Tailscale")
		return
	}

	tsState := s.inspectTailscaleServe(r.Context(), tsBin)
	if !tsState.available {
		writeAPIError(w, http.StatusPreconditionFailed, "ERR_TAILSCALE_OFFLINE", "Tailscale 尚未连接，请先登录 Tailscale")
		return
	}
	if tsState.statusErr != nil {
		writeAPIError(w, http.StatusBadGateway, "ERR_TAILSCALE_STATUS", fmt.Sprintf("无法读取现有 Tailscale Serve 映射，为避免覆盖现有服务已中止：%v", tsState.statusErr))
		return
	}
	if tsState.shared && tsState.origin != "" {
		// Already exposed via Tailscale Serve; ensure DB records origin + login
		s.recordTailscaleSharing(context.WithoutCancel(r.Context()), tsState.origin, actorFromContext(r.Context()))
		writeJSON(w, http.StatusOK, protocol.TailscaleEnableResponse{
			Enabled:      true,
			Origin:       tsState.origin,
			TargetOrigin: tsState.origin,
			Settings:     s.buildNetworkSettingsView(r.Context()),
		})
		return
	}

	targetPort := req.Port
	if targetPort <= 0 {
		if tsState.portConflict(443) {
			targetPort = tsState.pickSuggestedFreePort()
		} else {
			targetPort = 443
		}
	}
	if targetPort != 443 && targetPort != 8443 && targetPort != 10000 && (targetPort < 1024 || targetPort > 65535) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_PORT", "invalid https port")
		return
	}

	cfg := s.hubConfigSnapshot()
	targetURL := "http://" + strings.TrimPrefix(strings.TrimPrefix(cfg.ListenAddr, "http://"), "https://")

	// Always verify the exact target port is free of an existing non-Orbit mapping before applying!
	if occupiedBy, occupied := tsState.occupiedPorts[targetPort]; occupied {
		altPort := tsState.pickSuggestedFreePort()
		altCmd := fmt.Sprintf("tailscale serve --bg --https=%d %s", altPort, targetURL)
		writeJSON(w, http.StatusOK, protocol.TailscaleEnableResponse{
			Enabled:        false,
			TargetOrigin:   formatTailscaleOrigin(tsState.selfDNSName, altPort),
			NeedsConfirm:   true,
			SuggestedPort:  altPort,
			ConflictDetail: fmt.Sprintf("%d 端口已被其他 Tailscale 服务占用（%s），请确认改用 %d 端口以保留现有映射", targetPort, occupiedBy, altPort),
			Command:        altCmd,
			Settings:       s.buildNetworkSettingsView(r.Context()),
		})
		return
	}

	targetOrigin := formatTailscaleOrigin(tsState.selfDNSName, targetPort)
	cmdPreview := fmt.Sprintf("tailscale serve --bg --https=%d %s", targetPort, targetURL)
	if !req.Confirm {
		writeJSON(w, http.StatusOK, protocol.TailscaleEnableResponse{
			Enabled:        false,
			TargetOrigin:   targetOrigin,
			NeedsConfirm:   true,
			SuggestedPort:  targetPort,
			ConflictDetail: tsState.hint,
			Command:        cmdPreview,
			Settings:       s.buildNetworkSettingsView(r.Context()),
		})
		return
	}

	execCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(execCtx, tsBin, "serve", "--bg", "--https="+strconv.Itoa(targetPort), targetURL).CombinedOutput()
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "ERR_TAILSCALE_SERVE", fmt.Sprintf("tailscale serve 失败: %s", strings.TrimSpace(string(out))))
		return
	}

	_, _, newOrigin, _, _ := s.probeTailscaleState(r.Context())
	if newOrigin == "" {
		if dnsName := queryTailscaleSelfDNSName(r.Context(), tsBin); dnsName != "" {
			newOrigin = formatTailscaleOrigin(dnsName, targetPort)
		} else {
			newOrigin = targetOrigin
		}
	}
	if newOrigin != "" {
		s.recordTailscaleSharing(context.WithoutCancel(r.Context()), newOrigin, actorFromContext(r.Context()))
	}
	writeJSON(w, http.StatusOK, protocol.TailscaleEnableResponse{
		Enabled:      true,
		Origin:       newOrigin,
		TargetOrigin: newOrigin,
		Command:      cmdPreview,
		Settings:     s.buildNetworkSettingsView(r.Context()),
	})
}

func (s *Server) handleTailscaleDisable(w http.ResponseWriter, r *http.Request) {
	tsBin := findTailscaleBin()
	if tsBin != "" {
		tsState := s.inspectTailscaleServe(r.Context(), tsBin)
		if tsState.shared && tsState.orbitPort > 0 {
			execCtx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
			defer cancel()
			out, err := exec.CommandContext(execCtx, tsBin, "serve", "--https="+strconv.Itoa(tsState.orbitPort), "off").CombinedOutput()
			if err != nil {
				writeAPIError(w, http.StatusBadGateway, "ERR_TAILSCALE_SERVE", fmt.Sprintf("关闭 Tailscale Serve 失败: %s", strings.TrimSpace(string(out))))
				return
			}
		}
	}
	cur, _ := s.store.GetDynamicNetworkSettings(r.Context())
	cur.TailscaleShared = false
	cur.TailscaleOrigin = ""
	disabled := false
	cur.ExternalGatewayEnabled = &disabled
	if ev, err := s.store.SaveDynamicNetworkSettings(context.WithoutCancel(r.Context()), cur, actorFromContext(r.Context())); err == nil {
		_ = s.refreshEffectiveRegistry(context.WithoutCancel(r.Context()))
		s.broker.Broadcast(ev)
	}
	writeJSON(w, http.StatusOK, protocol.TailscaleDisableResponse{
		Disabled: true,
		Settings: s.buildNetworkSettingsView(r.Context()),
	})
}

func (s *Server) recordTailscaleSharing(ctx context.Context, origin, actorID string) {
	cur, _ := s.store.GetDynamicNetworkSettings(ctx)
	cur.TailscaleShared = true
	cur.TailscaleOrigin = strings.TrimRight(origin, "/")
	if !slices.Contains(cur.ExtraOrigins, cur.TailscaleOrigin) {
		cur.ExtraOrigins = append(cur.ExtraOrigins, cur.TailscaleOrigin)
	}
	if ev, err := s.store.SaveDynamicNetworkSettings(ctx, cur, actorID); err == nil {
		_ = s.refreshEffectiveRegistry(context.WithoutCancel(ctx))
		s.broker.Broadcast(ev)
	}
}

func (s *Server) buildNetworkSettingsView(ctx context.Context) protocol.NetworkSettingsView {
	tsAvail, tsShared, tsOrigin, conflict, conflictDetail := s.probeTailscaleState(ctx)
	s.mu.RLock()
	dyn := s.dynNet
	s.mu.RUnlock()
	if tsShared && tsOrigin != "" && (!dyn.TailscaleShared || strings.TrimRight(dyn.TailscaleOrigin, "/") != strings.TrimRight(tsOrigin, "/")) {
		s.recordTailscaleSharing(context.WithoutCancel(ctx), tsOrigin, "system")
		s.mu.RLock()
		dyn = s.dynNet
		s.mu.RUnlock()
	}
	discID, _ := s.store.EnsureDiscoveryID(ctx)
	dutyPriority, activeDuty := s.effectiveDutyPriority(ctx, nil)
	gwEnabled := tsShared
	if dyn.ExternalGatewayEnabled != nil {
		gwEnabled = *dyn.ExternalGatewayEnabled || tsShared
	}
	gwCount := 0
	if tsShared {
		gwCount = 1
	} else if gwEnabled {
		gwCount = 1
	}
	return protocol.NetworkSettingsView{
		WorkspaceName:          s.effectiveWorkspaceName(),
		DiscoveryID:            discID,
		DutyPriority:           dutyPriority,
		ActiveDutyMachineID:    activeDuty,
		ExternalGatewayEnabled: gwEnabled,
		ExternalGatewayCount:   gwCount,
		HubOrigin:              s.liveEffectiveHubOrigin(tsShared, tsOrigin),
		ExtraOrigins:           dyn.ExtraOrigins,
		ExtraLogins:            dyn.ExtraLogins,
		TailscaleAvailable:     tsAvail,
		TailscaleShared:        tsShared,
		TailscaleOrigin:        tsOrigin,
		TailscalePortConflict:  conflict,
		TailscaleHint:          conflictDetail,
	}
}

func (s *Server) handleCompleteOnboarding(w http.ResponseWriter, r *http.Request) {
	var req protocol.CompleteOnboardingRequest
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&req)
	reason := "completed"
	if req.Skipped {
		reason = "skipped"
	}
	if err := s.store.MarkOnboardingDone(r.Context(), reason); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_ONBOARDING_COMPLETE", err.Error())
		return
	}
	s.broker.Notify()

	yamlMachines := s.effectiveYAMLMachines()
	boot, _ := s.store.Bootstrap(r.Context(), yamlMachines)
	enriched := s.enrichMachineViews(r.Context(), boot.Machines)
	setup := s.buildSetupStatus(r.Context(), enriched, clientMachineIDFromContext(r.Context()))
	setup.CurrentClientID = clientTokenIDFromContext(r.Context())
	writeJSON(w, http.StatusOK, protocol.CompleteOnboardingResponse{
		Completed: true,
		Setup:     setup,
	})
}

func decodeQuotasFromRPC(data map[string]any) []protocol.AgentQuotaSnapshot {
	if data == nil {
		return nil
	}
	raw, ok := data["agentQuotas"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var quotas []protocol.AgentQuotaSnapshot
	if json.Unmarshal(b, &quotas) != nil {
		return nil
	}
	return quotas
}

func decodeProbesFromRPC(data map[string]any) []protocol.AgentProbe {
	if data == nil {
		return nil
	}
	raw, ok := data["agents"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var probes []protocol.AgentProbe
	if json.Unmarshal(b, &probes) != nil {
		return nil
	}
	return probes
}

func decodeSingleProbeFromRPC(data map[string]any) *protocol.AgentProbe {
	if data == nil {
		return nil
	}
	raw, ok := data["probe"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var p protocol.AgentProbe
	if json.Unmarshal(b, &p) != nil {
		return nil
	}
	return &p
}

func findTailscaleBin() string {
	return discovery.FindTailscaleBin()
}

type tailscaleServeInspection struct {
	available     bool
	shared        bool
	selfDNSName   string
	origin        string
	orbitPort     int
	occupiedPorts map[int]string
	hint          string
	statusErr     error
}

func (t tailscaleServeInspection) portConflict(port int) bool {
	_, ok := t.occupiedPorts[port]
	return ok
}

func (t tailscaleServeInspection) pickSuggestedFreePort() int {
	for _, cand := range []int{443, 4443, 8443, 10000, 14443} {
		if _, busy := t.occupiedPorts[cand]; !busy {
			return cand
		}
	}
	return 4443
}

func sameLoopbackProxyTarget(proxyRaw, listenAddr string) bool {
	proxyNorm := strings.TrimRight(strings.TrimSpace(proxyRaw), "/")
	targetNorm := "http://" + strings.TrimRight(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(listenAddr), "http://"), "https://"), "/")
	if strings.EqualFold(proxyNorm, targetNorm) {
		return true
	}
	pu, err1 := url.Parse(proxyNorm)
	tu, err2 := url.Parse(targetNorm)
	if err1 != nil || err2 != nil || pu.Scheme != tu.Scheme {
		return false
	}
	pHost, pPort, err1 := net.SplitHostPort(pu.Host)
	tHost, tPort, err2 := net.SplitHostPort(tu.Host)
	if err1 != nil || err2 != nil || pPort != tPort {
		return false
	}
	return isLoopbackHost(pHost) && isLoopbackHost(tHost)
}

func (s *Server) inspectTailscaleServe(ctx context.Context, tsBin string) tailscaleServeInspection {
	res := tailscaleServeInspection{occupiedPorts: map[int]string{}}
	s.mu.RLock()
	cfgOrigin := strings.TrimRight(strings.TrimSpace(s.hubCfg.HubOrigin), "/")
	dynOrigin := strings.TrimRight(strings.TrimSpace(s.dynNet.TailscaleOrigin), "/")
	listenAddr := strings.TrimSpace(s.hubCfg.ListenAddr)
	s.mu.RUnlock()

	if tsBin == "" {
		// Fall back to saved origin only as display hint when Tailscale CLI is absent
		if strings.Contains(cfgOrigin, ".ts.net") {
			res.origin = cfgOrigin
		} else if dynOrigin != "" {
			res.origin = dynOrigin
		}
		res.hint = "未安装 Tailscale"
		return res
	}
	pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	dnsName := queryTailscaleSelfDNSName(pctx, tsBin)
	if dnsName == "" {
		res.hint = "Tailscale 未登录或离线"
		return res
	}
	res.available = true
	res.selfDNSName = dnsName

	// Always inspect live `tailscale serve status --json` so stopping Serve externally is immediately reflected
	// and never masked by stale DB/YAML origin settings.
	raw, err := exec.CommandContext(pctx, tsBin, "serve", "status", "--json").Output()
	if err != nil {
		res.statusErr = fmt.Errorf("tailscale serve status failed: %w", err)
		res.hint = "无法确认现有 Tailscale Serve 映射状态"
		return res
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "{}" || trimmed == "null" {
		return res
	}
	var statusDoc struct {
		Web map[string]struct {
			Handlers map[string]struct {
				Proxy string `json:"Proxy"`
			} `json:"Handlers"`
		} `json:"Web"`
	}
	if err := json.Unmarshal(raw, &statusDoc); err != nil {
		res.statusErr = fmt.Errorf("parse tailscale serve status json: %w", err)
		res.hint = "无法解析 Tailscale Serve 映射配置"
		return res
	}

	for hostPort, web := range statusDoc.Web {
		portNum := 443
		if idx := strings.LastIndex(hostPort, ":"); idx >= 0 {
			if p, pErr := strconv.Atoi(hostPort[idx+1:]); pErr == nil && p > 0 {
				portNum = p
			}
		}
		for mountPath, h := range web.Handlers {
			if (mountPath == "/" || mountPath == "") && sameLoopbackProxyTarget(h.Proxy, listenAddr) {
				res.shared = true
				res.orbitPort = portNum
				res.origin = "https://" + strings.TrimSuffix(hostPort, ":443")
				return res
			}
		}
		if len(web.Handlers) > 0 && portNum > 0 {
			res.occupiedPorts[portNum] = hostPort
		}
	}
	if occ, busy := res.occupiedPorts[443]; busy {
		res.hint = fmt.Sprintf("443 端口已被其他 Tailscale 服务占用（%s），将建议使用 %d 端口", occ, res.pickSuggestedFreePort())
	}
	return res
}

func (s *Server) probeTailscaleState(ctx context.Context) (available, shared bool, origin string, portConflict bool, hint string) {
	insp := s.inspectTailscaleServe(ctx, findTailscaleBin())
	return insp.available, insp.shared, insp.origin, insp.portConflict(443) || insp.statusErr != nil, insp.hint
}

func queryTailscaleSelfDNSName(ctx context.Context, tsBin string) string {
	raw, err := exec.CommandContext(ctx, tsBin, "status", "--json").Output()
	if err != nil {
		return ""
	}
	var doc struct {
		BackendState string `json:"BackendState"`
		Self         struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.BackendState != "Running" {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(doc.Self.DNSName), ".")
}

func queryTailscaleSelfLogin(ctx context.Context, tsBin string) string {
	raw, err := exec.CommandContext(ctx, tsBin, "status", "--json").Output()
	if err != nil {
		return ""
	}
	var doc struct {
		Self struct {
			UserID int64 `json:"UserID"`
		} `json:"Self"`
		User map[string]struct {
			LoginName string `json:"LoginName"`
		} `json:"User"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	if u, ok := doc.User[strconv.FormatInt(doc.Self.UserID, 10)]; ok {
		return strings.TrimSpace(u.LoginName)
	}
	return ""
}
