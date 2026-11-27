package hub

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

// effectiveDutyPriority builds the ordered list of full-role machines eligible for primary controller duty
// and deterministically computes activeDutyMachineId (highest priority online node).
func (s *Server) effectiveDutyPriority(ctx context.Context, machines []protocol.MachineView) ([]string, string) {
	s.mu.RLock()
	savedOrder := append([]string(nil), s.dynNet.DutyPriority...)
	s.mu.RUnlock()

	if machines == nil {
		yamlMachines := s.effectiveYAMLMachines()
		if boot, err := s.store.Bootstrap(ctx, yamlMachines); err == nil {
			machines = boot.Machines
		}
	}

	fullByID := map[string]protocol.MachineView{}
	var remaining []protocol.MachineView
	for _, m := range machines {
		if m.TransportMode == protocol.TransportSSHOutbound || m.Role == protocol.MachineRoleWorker {
			continue
		}
		fullByID[m.MachineID] = m
	}

	priority := make([]string, 0, len(fullByID))
	seen := map[string]bool{}
	for _, id := range savedOrder {
		if _, ok := fullByID[id]; ok && !seen[id] {
			seen[id] = true
			priority = append(priority, id)
		}
	}
	for id, m := range fullByID {
		if !seen[id] {
			remaining = append(remaining, m)
		}
	}
	sort.Slice(remaining, func(i, j int) bool {
		iLocal := remaining[i].TransportMode == protocol.TransportLocalWS
		jLocal := remaining[j].TransportMode == protocol.TransportLocalWS
		if iLocal != jLocal {
			return iLocal
		}
		return remaining[i].MachineID < remaining[j].MachineID
	})
	for _, m := range remaining {
		priority = append(priority, m.MachineID)
	}

	activeDuty := ""
	for _, id := range priority {
		m := fullByID[id]
		if m.Status == protocol.MachineOnline || s.runners.IsConnected(id) || m.TransportMode == protocol.TransportLocalWS {
			activeDuty = id
			break
		}
	}
	if activeDuty == "" && len(priority) > 0 {
		activeDuty = priority[0]
	}
	return priority, activeDuty
}

func (s *Server) inspectMachineDirectory(ctx context.Context, machineID, rawPath string) (protocol.CheckRepoResponse, int, string, error) {
	s.mu.RLock()
	mCfg, exists := s.machines.Machines[machineID]
	s.mu.RUnlock()
	if !exists {
		return protocol.CheckRepoResponse{}, http.StatusNotFound, "ERR_MACHINE_NOT_FOUND", fmt.Errorf("machine %q not found", machineID)
	}
	allowedRoots := s.store.GetMachineAllowedRoots(ctx, machineID)
	rpcArgs := map[string]any{"path": rawPath}
	if len(allowedRoots) > 0 {
		rpcArgs["allowedRoots"] = allowedRoots
	}

	if s.runners.IsConnected(machineID) && s.runners.HasCapability(machineID, protocol.RunnerCapFSPlainV1) {
		res, err := s.runners.CallRPC(ctx, machineID, "inspect_dir", rpcArgs, 15*time.Second)
		if err != nil {
			return protocol.CheckRepoResponse{}, http.StatusBadGateway, "ERR_RUNNER_RPC", err
		}
		var out protocol.CheckRepoResponse
		if raw, err := json.Marshal(res.Data); err == nil {
			_ = json.Unmarshal(raw, &out)
		}
		if !res.OK && out.Error == "" {
			out.Error = res.Error
		}
		return out, http.StatusOK, "", nil
	}
	if s.runners.IsConnected(machineID) && s.runners.HasCapability(machineID, protocol.RunnerCapFSV1) {
		res, err := s.runners.CallRPC(ctx, machineID, "check_repo", rpcArgs, 15*time.Second)
		if err != nil {
			return protocol.CheckRepoResponse{}, http.StatusBadGateway, "ERR_RUNNER_RPC", err
		}
		var out protocol.CheckRepoResponse
		if raw, err := json.Marshal(res.Data); err == nil {
			_ = json.Unmarshal(raw, &out)
		}
		if out.Valid {
			out.Exists = true
			out.IsGit = true
		}
		if !res.OK && out.Error == "" {
			out.Error = res.Error
		}
		return out, http.StatusOK, "", nil
	}
	if mCfg.Transport == protocol.TransportLocalWS {
		out := gitops.InspectDirectory(ctx, rawPath, allowedRoots)
		return out, http.StatusOK, "", nil
	}
	return protocol.CheckRepoResponse{}, http.StatusServiceUnavailable, "ERR_MACHINE_OFFLINE", errors.New("target machine runner is offline")
}

func (s *Server) scanMachineRepos(ctx context.Context, machineID string) ([]gitops.ScannedGitRepo, error) {
	s.mu.RLock()
	mCfg, exists := s.machines.Machines[machineID]
	s.mu.RUnlock()
	if !exists {
		return nil, nil
	}
	allowedRoots := s.store.GetMachineAllowedRoots(ctx, machineID)
	rpcArgs := map[string]any{}
	if len(allowedRoots) > 0 {
		rpcArgs["allowedRoots"] = allowedRoots
	}

	if s.runners.IsConnected(machineID) && s.runners.HasCapability(machineID, protocol.RunnerCapFSPlainV1) {
		res, err := s.runners.CallRPC(ctx, machineID, "scan_repos", rpcArgs, 10*time.Second)
		if err != nil || !res.OK {
			return nil, err
		}
		if rawRepos, ok := res.Data["repos"]; ok {
			b, _ := json.Marshal(rawRepos)
			var repos []gitops.ScannedGitRepo
			_ = json.Unmarshal(b, &repos)
			return repos, nil
		}
		return nil, nil
	}
	if mCfg.Transport == protocol.TransportLocalWS {
		return gitops.ScanGitSuggestions(ctx, allowedRoots)
	}
	return nil, nil
}

func (s *Server) readMachineFolderFile(ctx context.Context, machineID, rootDir, relPath string) ([]byte, string, int, string, error) {
	s.mu.RLock()
	mCfg, exists := s.machines.Machines[machineID]
	s.mu.RUnlock()
	if !exists {
		return nil, "", http.StatusNotFound, "ERR_MACHINE_NOT_FOUND", fmt.Errorf("machine %q not found", machineID)
	}
	allowedRoots := s.store.GetMachineAllowedRoots(ctx, machineID)
	rpcArgs := map[string]any{
		"rootDir": rootDir,
		"relPath": relPath,
	}
	if len(allowedRoots) > 0 {
		rpcArgs["allowedRoots"] = allowedRoots
	}

	if s.runners.IsConnected(machineID) && s.runners.HasCapability(machineID, protocol.RunnerCapFSPlainV1) {
		res, err := s.runners.CallRPC(ctx, machineID, "read_folder_file", rpcArgs, 20*time.Second)
		if err != nil {
			return nil, "", http.StatusBadGateway, "ERR_RUNNER_RPC", err
		}
		if !res.OK {
			return nil, "", http.StatusNotFound, "ERR_FILE_READ", errors.New(res.Error)
		}
		b64, _ := res.Data["contentBase64"].(string)
		name, _ := res.Data["filename"].(string)
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, "", http.StatusBadGateway, "ERR_DECODE", err
		}
		if name == "" {
			name = filepath.Base(relPath)
		}
		return data, name, http.StatusOK, "", nil
	}
	if mCfg.Transport == protocol.TransportLocalWS {
		data, name, err := gitops.ReadFolderFile(rootDir, relPath, allowedRoots, 20*1024*1024)
		if err != nil {
			return nil, "", http.StatusNotFound, "ERR_FILE_READ", err
		}
		return data, name, http.StatusOK, "", nil
	}
	return nil, "", http.StatusServiceUnavailable, "ERR_MACHINE_OFFLINE", errors.New("machine runner is offline")
}

func checkRepoToRuntimeState(c protocol.CheckRepoResponse) *protocol.LocationRuntimeState {
	return &protocol.LocationRuntimeState{
		Exists:    c.Exists,
		IsGit:     c.IsGit,
		Branch:    c.DefaultBranch,
		HeadSHA:   c.HeadSHA,
		Dirty:     c.Dirty,
		Ahead:     c.Ahead,
		Behind:    c.Behind,
		CheckedAt: time.Now().UnixMilli(),
	}
}

func (s *Server) handleUpdateProjectSettings(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	var req protocol.UpdateProjectSettingsRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	pv, ev, err := s.store.UpdateProjectSettings(r.Context(), projectID, req, actorFromContext(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_PROJECT_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrInvalidInput) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_INPUT", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PROJECT_UPDATE", err.Error())
		return
	}
	_ = s.refreshEffectiveRegistry(context.WithoutCancel(r.Context()))
	s.broker.Broadcast(ev)
	writeJSON(w, http.StatusOK, protocol.CreateProjectResponse{Project: pv})
}

func (s *Server) handleCreateProjectPart(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	var req protocol.CreateProjectPartRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	kind := req.Kind
	repoIdentity := ""
	absPath := strings.TrimSpace(req.AbsPath)
	machineID := strings.TrimSpace(req.MachineID)
	var runtimeSt *protocol.LocationRuntimeState

	if machineID != "" && absPath != "" {
		insp, st, code, err := s.inspectMachineDirectory(r.Context(), machineID, absPath)
		if err != nil {
			writeAPIError(w, st, code, err.Error())
			return
		}
		if !insp.Valid || !insp.Exists {
			msg := insp.Error
			if msg == "" {
				msg = "selected directory does not exist"
			}
			writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_DIR", msg)
			return
		}
		if insp.RepoRoot != "" {
			absPath = insp.RepoRoot
		}
		if kind == "" {
			if insp.IsGit {
				kind = protocol.ProjectPartGit
			} else {
				kind = protocol.ProjectPartFolder
			}
		}
		repoIdentity = insp.RepoIdentity
		runtimeSt = checkRepoToRuntimeState(insp)
		if strings.TrimSpace(req.Name) == "" {
			req.Name = insp.DisplayName
		}
	}

	pv, part, ev, err := s.store.CreateProjectPart(
		r.Context(), projectID, req.Name, kind, repoIdentity, machineID, absPath, runtimeSt, actorFromContext(r.Context()),
	)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_PROJECT_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrInvalidInput) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_INPUT", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PART_CREATE", err.Error())
		return
	}
	_ = s.refreshEffectiveRegistry(context.WithoutCancel(r.Context()))
	s.broker.Broadcast(ev)
	writeJSON(w, http.StatusCreated, map[string]any{"project": pv, "part": part})
}

func (s *Server) handleUpdateProjectPart(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	partID := chi.URLParam(r, "partId")
	var req protocol.UpdateProjectPartRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	pv, part, ev, err := s.store.UpdateProjectPart(r.Context(), projectID, partID, req, actorFromContext(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_PART_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrInvalidInput) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_INPUT", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PART_UPDATE", err.Error())
		return
	}
	_ = s.refreshEffectiveRegistry(context.WithoutCancel(r.Context()))
	s.broker.Broadcast(ev)
	writeJSON(w, http.StatusOK, map[string]any{"project": pv, "part": part})
}

func (s *Server) handleDeleteProjectPart(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	partID := chi.URLParam(r, "partId")
	pv, ev, err := s.store.DeleteProjectPart(r.Context(), projectID, partID, actorFromContext(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_PART_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PART_DELETE", err.Error())
		return
	}
	_ = s.refreshEffectiveRegistry(context.WithoutCancel(r.Context()))
	s.broker.Broadcast(ev)
	writeJSON(w, http.StatusOK, map[string]any{"project": pv})
}

func (s *Server) handleCreateProjectLocation(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	partID := chi.URLParam(r, "partId")
	var req protocol.CreateProjectLocationRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	machineID := strings.TrimSpace(req.MachineID)
	absPath := strings.TrimSpace(req.AbsPath)
	if machineID == "" || absPath == "" {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_INPUT", "machineId and absPath are required")
		return
	}

	insp, st, code, err := s.inspectMachineDirectory(r.Context(), machineID, absPath)
	if err != nil {
		writeAPIError(w, st, code, err.Error())
		return
	}
	if !insp.Valid || !insp.Exists {
		msg := insp.Error
		if msg == "" {
			msg = "selected directory does not exist"
		}
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_DIR", msg)
		return
	}
	if insp.RepoRoot != "" {
		req.AbsPath = insp.RepoRoot
	}
	if !insp.IsGit && req.Isolation == "" {
		req.Isolation = protocol.IsolationInPlace
	}

	pv, loc, ev, err := s.store.CreateProjectLocation(
		r.Context(), projectID, partID, req, checkRepoToRuntimeState(insp), insp.RepoIdentity, actorFromContext(r.Context()),
	)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeAPIError(w, http.StatusConflict, "ERR_LOCATION_EXISTS", err.Error())
		return
	}
	if errors.Is(err, store.ErrInvalidInput) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_INPUT", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_LOCATION_CREATE", err.Error())
		return
	}
	_ = s.refreshEffectiveRegistry(context.WithoutCancel(r.Context()))
	s.broker.Broadcast(ev)
	writeJSON(w, http.StatusCreated, map[string]any{"project": pv, "location": loc})
}

func (s *Server) handleUpdateProjectLocation(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	partID := chi.URLParam(r, "partId")
	locationID := chi.URLParam(r, "locationId")
	var req protocol.UpdateProjectLocationRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}

	var runtimeSt *protocol.LocationRuntimeState
	repoIdentity := ""
	if req.MachineID != nil || req.AbsPath != nil {
		proj, err := s.store.GetProject(r.Context(), projectID)
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "ERR_PROJECT_NOT_FOUND", err.Error())
			return
		}
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "ERR_PROJECT_LOOKUP", err.Error())
			return
		}
		var curLoc *protocol.ProjectLocationView
		for i := range proj.Parts {
			if proj.Parts[i].PartID != partID {
				continue
			}
			for j := range proj.Parts[i].Locations {
				if proj.Parts[i].Locations[j].LocationID == locationID {
					curLoc = &proj.Parts[i].Locations[j]
					break
				}
			}
			break
		}
		if curLoc == nil {
			writeAPIError(w, http.StatusNotFound, "ERR_LOCATION_NOT_FOUND", fmt.Sprintf("location %s not found", locationID))
			return
		}
		targetMachineID := curLoc.MachineID
		if req.MachineID != nil {
			targetMachineID = strings.TrimSpace(*req.MachineID)
		}
		targetAbsPath := curLoc.AbsPath
		if req.AbsPath != nil {
			targetAbsPath = strings.TrimSpace(*req.AbsPath)
		}
		if targetMachineID == "" || targetAbsPath == "" {
			writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_INPUT", "machineId and absPath cannot be empty")
			return
		}
		insp, st, code, err := s.inspectMachineDirectory(r.Context(), targetMachineID, targetAbsPath)
		if err != nil {
			writeAPIError(w, st, code, err.Error())
			return
		}
		if !insp.Valid || !insp.Exists {
			msg := insp.Error
			if msg == "" {
				msg = "selected directory does not exist"
			}
			writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_DIR", msg)
			return
		}
		if insp.RepoRoot != "" {
			targetAbsPath = insp.RepoRoot
		}
		req.MachineID = &targetMachineID
		req.AbsPath = &targetAbsPath
		runtimeSt = checkRepoToRuntimeState(insp)
		repoIdentity = insp.RepoIdentity
	}

	pv, ev, err := s.store.UpdateProjectLocationWithRuntime(
		r.Context(), projectID, partID, locationID, req, runtimeSt, repoIdentity, actorFromContext(r.Context()),
	)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_LOCATION_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrRepoIdentityConflict) {
		writeAPIError(w, http.StatusConflict, "ERR_REPO_IDENTITY_CONFLICT", err.Error())
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeAPIError(w, http.StatusConflict, "ERR_LOCATION_EXISTS", err.Error())
		return
	}
	if errors.Is(err, store.ErrInvalidInput) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_INPUT", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_LOCATION_UPDATE", err.Error())
		return
	}
	_ = s.refreshEffectiveRegistry(context.WithoutCancel(r.Context()))
	s.broker.Broadcast(ev)
	writeJSON(w, http.StatusOK, map[string]any{"project": pv})
}

func (s *Server) handleDeleteProjectLocation(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	partID := chi.URLParam(r, "partId")
	locationID := chi.URLParam(r, "locationId")
	pv, ev, err := s.store.DeleteProjectLocation(r.Context(), projectID, partID, locationID, actorFromContext(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_LOCATION_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_LOCATION_DELETE", err.Error())
		return
	}
	_ = s.refreshEffectiveRegistry(context.WithoutCancel(r.Context()))
	s.broker.Broadcast(ev)
	writeJSON(w, http.StatusOK, map[string]any{"project": pv})
}

func (s *Server) handleRefreshProjectLocations(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	pv, err := s.store.GetProject(r.Context(), projectID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_PROJECT_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PROJECT_LOOKUP", err.Error())
		return
	}
	for _, part := range pv.Parts {
		for _, loc := range part.Locations {
			insp, _, _, iErr := s.inspectMachineDirectory(r.Context(), loc.MachineID, loc.AbsPath)
			if iErr == nil {
				_ = s.store.UpsertLocationRuntimeState(r.Context(), loc.LocationID, *checkRepoToRuntimeState(insp))
			}
		}
	}
	updated, err := s.store.GetProject(r.Context(), projectID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PROJECT_LOOKUP", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": updated})
}

func (s *Server) handleGetProjectSuggestions(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	pv, err := s.store.GetProject(r.Context(), projectID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_PROJECT_NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PROJECT_LOOKUP", err.Error())
		return
	}
	dismissed, _ := s.store.ListDismissedSuggestions(r.Context(), projectID)

	// Only Git parts with a non-folder repo identity participate in optional shallow suggestions.
	type partMatcher struct {
		part          protocol.ProjectPartView
		normID        string
		boundMachines map[string]bool
		boundPaths    map[string]bool
	}
	var matchers []partMatcher
	for _, part := range pv.Parts {
		if part.Kind != protocol.ProjectPartGit {
			continue
		}
		normID := strings.ToLower(strings.TrimSpace(part.RepoIdentity))
		if normID == "" || strings.HasPrefix(normID, "folder:") {
			continue
		}
		bm := map[string]bool{}
		bp := map[string]bool{}
		for _, loc := range part.Locations {
			bm[loc.MachineID] = true
			bp[loc.MachineID+"|"+filepath.Clean(loc.AbsPath)] = true
		}
		matchers = append(matchers, partMatcher{
			part:          part,
			normID:        normID,
			boundMachines: bm,
			boundPaths:    bp,
		})
	}
	if len(matchers) == 0 {
		writeJSON(w, http.StatusOK, protocol.ProjectSuggestionsResponse{
			ProjectID:   projectID,
			Suggestions: []protocol.ProjectSuggestionView{},
		})
		return
	}

	s.mu.RLock()
	machineIDs := make([]string, 0, len(s.machines.Machines))
	for id, m := range s.machines.Machines {
		if m.Transport != protocol.TransportSSHOutbound {
			machineIDs = append(machineIDs, id)
		}
	}
	s.mu.RUnlock()
	sort.Strings(machineIDs)

	suggestions := []protocol.ProjectSuggestionView{}
	for _, mid := range machineIDs {
		repos, err := s.scanMachineRepos(r.Context(), mid)
		if err != nil || len(repos) == 0 {
			continue
		}
		for _, repo := range repos {
			candID := strings.ToLower(strings.TrimSpace(repo.RepoIdentity))
			if candID == "" {
				continue
			}
			cleanPath := filepath.Clean(repo.AbsPath)
			for _, m := range matchers {
				if candID != m.normID {
					continue
				}
				if m.boundPaths[mid+"|"+cleanPath] || m.boundMachines[mid] {
					continue
				}
				if dismissed[m.part.PartID+"|"+mid+"|"+cleanPath] {
					continue
				}
				suggestions = append(suggestions, protocol.ProjectSuggestionView{
					PartID:       m.part.PartID,
					MachineID:    mid,
					AbsPath:      cleanPath,
					Branch:       repo.Branch,
					RepoIdentity: repo.RepoIdentity,
				})
			}
		}
	}

	writeJSON(w, http.StatusOK, protocol.ProjectSuggestionsResponse{
		ProjectID:   projectID,
		Suggestions: suggestions,
	})
}

func (s *Server) handleDismissProjectSuggestion(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	partID := chi.URLParam(r, "partId")
	var req protocol.DismissSuggestionRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	if err := s.store.DismissProjectSuggestion(r.Context(), projectID, partID, req.MachineID, req.AbsPath); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "ERR_PART_NOT_FOUND", err.Error())
			return
		}
		writeAPIError(w, http.StatusBadRequest, "ERR_DISMISS_SUGGESTION", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dismissed": true})
}

func (s *Server) handleDownloadTaskFolderFile(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	relPath := strings.TrimSpace(r.URL.Query().Get("path"))
	attemptID := strings.TrimSpace(r.URL.Query().Get("attemptId"))
	if relPath == "" {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_INPUT", "path query parameter is required")
		return
	}
	tl, err := s.store.GetTaskTimeline(r.Context(), taskID, 0, false)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", "task not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_TASK_LOOKUP", err.Error())
		return
	}

	var target *protocol.AttemptView
	for i := len(tl.Attempts) - 1; i >= 0; i-- {
		a := tl.Attempts[i]
		if attemptID != "" && a.AttemptID == attemptID {
			target = &a
			break
		}
		if attemptID == "" && a.WorktreePath != "" && (a.FolderChanges != nil || a.Isolation == protocol.IsolationInPlace) {
			target = &a
			break
		}
	}
	if target == nil || target.WorktreePath == "" {
		writeAPIError(w, http.StatusNotFound, "ERR_FOLDER_ATTEMPT_NOT_FOUND", "no plain-folder execution directory found for task")
		return
	}

	data, name, status, code, err := s.readMachineFolderFile(r.Context(), target.MachineID, target.WorktreePath, relPath)
	if err != nil {
		writeAPIError(w, status, code, err.Error())
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType(data))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handlePromoteFolderChanges(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	var req struct {
		AttemptID string `json:"attemptId,omitempty"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&req)

	tl, err := s.store.GetTaskTimeline(r.Context(), taskID, 0, false)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", "task not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_TASK_LOOKUP", err.Error())
		return
	}

	var target *protocol.AttemptView
	for i := len(tl.Attempts) - 1; i >= 0; i-- {
		a := tl.Attempts[i]
		if req.AttemptID != "" && a.AttemptID == req.AttemptID {
			target = &a
			break
		}
		if req.AttemptID == "" && a.FolderChanges != nil && len(a.FolderChanges.Files) > 0 {
			target = &a
			break
		}
	}
	if target == nil || target.FolderChanges == nil || len(target.FolderChanges.Files) == 0 {
		writeAPIError(w, http.StatusBadRequest, "ERR_NO_FOLDER_CHANGES", "no folder file changes available to promote")
		return
	}

	attRec, err := s.store.GetAttemptRecord(r.Context(), target.AttemptID)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "ERR_ATTEMPT_NOT_FOUND", err.Error())
		return
	}
	proj, err := s.store.GetProject(r.Context(), attRec.ProjectID)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "ERR_PROJECT_NOT_FOUND", err.Error())
		return
	}

	cfg := s.hubConfigSnapshot()
	blobDir := filepath.Join(cfg.DataDir, "blobs")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_BLOB_DIR", err.Error())
		return
	}

	blobs := make([]protocol.BlobManifestItem, 0, len(target.FolderChanges.Files))
	for _, f := range target.FolderChanges.Files {
		if f.Kind == "del" {
			continue
		}
		data, _, status, code, rErr := s.readMachineFolderFile(r.Context(), attRec.MachineID, attRec.WorktreePath, f.Path)
		if rErr != nil {
			writeAPIError(w, status, code, fmt.Sprintf("read %s: %v", f.Path, rErr))
			return
		}
		sum := sha256.Sum256(data)
		shaHex := hex.EncodeToString(sum[:])
		storagePath := filepath.Join(blobDir, shaHex)
		if _, statErr := os.Stat(storagePath); os.IsNotExist(statErr) {
			if wErr := os.WriteFile(storagePath, data, 0o644); wErr != nil {
				writeAPIError(w, http.StatusInternalServerError, "ERR_BLOB_WRITE", wErr.Error())
				return
			}
		}
		if err := s.store.PutBlobRecord(r.Context(), shaHex, int64(len(data)), storagePath); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "ERR_BLOB_RECORD", err.Error())
			return
		}
		blobs = append(blobs, protocol.BlobManifestItem{
			Path:   f.Path,
			SHA256: shaHex,
			Size:   int64(len(data)),
		})
	}
	if len(blobs) == 0 {
		writeAPIError(w, http.StatusBadRequest, "ERR_NO_PROMOTABLE_FILES", "only file deletions were recorded; no artifact files to save")
		return
	}

	summary := fmt.Sprintf("目录产物归档 · %d 个文件", len(blobs))
	dlv, ev, err := s.store.PromoteAttemptFolderDelivery(
		r.Context(),
		attRec,
		proj.RepoIdentity,
		summary,
		blobs,
		len(target.FolderChanges.Files),
		target.FolderChanges.TotalAdds,
		target.FolderChanges.TotalDels,
		actorFromContext(r.Context()),
	)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PROMOTE_FOLDER", err.Error())
		return
	}
	s.broker.Broadcast(ev)
	writeJSON(w, http.StatusCreated, protocol.PromoteFolderChangesResponse{
		TaskID:   taskID,
		Delivery: dlv,
		Promoted: true,
	})
}
