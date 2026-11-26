package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/oklog/ulid/v2"

	"github.com/Hebi8/hebi8-Orbit/internal/gitops"
	"github.com/Hebi8/hebi8-Orbit/internal/protocol"
	"github.com/Hebi8/hebi8-Orbit/internal/sshcfg"
	"github.com/Hebi8/hebi8-Orbit/internal/store"
)

var sha256Regex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ServerOptions configures an orbit-hub Server instance.
type ServerOptions struct {
	HubConfig           protocol.HubConfig
	Store               *store.Store
	WebFS               fs.FS
	RyugiRunner         RyugiRunner
	HeartbeatInterval   time.Duration
	LeaseTTL            time.Duration
	MaintenanceInterval time.Duration
}

// Server serves the Orbit REST API, client/runner WebSockets, blob storage, and embedded SPA.
type Server struct {
	mu                sync.RWMutex
	registryRefreshMu sync.Mutex
	hubCfg            protocol.HubConfig
	yamlProjects protocol.ProjectsConfig
	yamlMachines protocol.MachinesConfig
	projects     protocol.ProjectsConfig
	machines     protocol.MachinesConfig
	dynNet       store.DynamicNetworkSettings
	sshCfgOpts   sshcfg.MatchOptions

	gcPaused    bool
	gcReason    string
	gcExpiresAt *int64

	heartbeatInterval   time.Duration
	leaseTTL            time.Duration
	maintenanceInterval time.Duration

	paMu                         sync.Mutex
	paBeforeCommitHook           func(fromSeq, highSeq int64)
	hebi8Mu                      sync.Mutex
	hebi8BeforeCommitHook        func() error
	deliveryLocks                *deliveryLocker
	testHookMu                   sync.Mutex
	removeFrozenDirFn            func(string) error
	manualImportBeforeInsertHook func()

	wsTicketsMu sync.Mutex
	wsTickets   map[string]wsTicketEntry

	clientWSMu              sync.Mutex
	clientWSConns           map[*clientWSConn]struct{}
	inFlightRevokes         map[string]chan struct{}
	wsBeforeReplayWriteHook func()
	wsInsideWriteHook       func()

	handoffMu    sync.Mutex
	handoffCodes map[string]authHandoffEntry

	clientTouchMu   sync.Mutex
	clientLastTouch map[string]time.Time

	pairLimiter *pairingRateLimiter

	store   *store.Store
	broker  *EventBroker
	runners *RunnerGateway
	ryugi   *RyugiBridge
	webFS   fs.FS
	router  http.Handler
	bgWG    sync.WaitGroup
}

// NewServer creates and wires a new orbit-hub Server.
func NewServer(ctx context.Context, opts ServerOptions) (*Server, error) {
	if opts.Store == nil {
		return nil, fmt.Errorf("store is required")
	}
	cfg := opts.HubConfig
	if err := ValidateListenAddr(cfg); err != nil {
		return nil, err
	}
	cfg.DataDir = protocol.ExpandPath(cfg.DataDir)
	cfg.ConfigDir = protocol.ExpandPath(cfg.ConfigDir)
	cfg.RyugiServerHome = protocol.ExpandPath(cfg.RyugiServerHome)

	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "git"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir data git dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "blobs"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir data blobs dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "deliveries"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir data deliveries dir: %w", err)
	}
	hebi8BaseDir := filepath.Join(cfg.DataDir, "hebi8-line")
	hebi8NodesDir := filepath.Join(hebi8BaseDir, "nodes")
	if err := os.MkdirAll(hebi8NodesDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir data hebi8-line dir: %w", err)
	}
	if err := os.Chmod(hebi8BaseDir, 0o700); err != nil {
		return nil, fmt.Errorf("chmod data hebi8-line dir: %w", err)
	}
	if err := os.Chmod(hebi8NodesDir, 0o700); err != nil {
		return nil, fmt.Errorf("chmod data hebi8-line nodes dir: %w", err)
	}

	broker := NewEventBroker()
	runner := opts.RyugiRunner
	if runner == nil {
		runner = ExecRyugiRunner{
			Bin:        cfg.RyugiBin,
			ServerHome: cfg.RyugiServerHome,
		}
	}

	hbInterval := opts.HeartbeatInterval
	if hbInterval <= 0 {
		hbInterval = 10 * time.Second
	}
	leaseTTL := opts.LeaseTTL
	if leaseTTL <= 0 {
		leaseTTL = 45 * time.Second
	}
	maintInterval := opts.MaintenanceInterval
	if maintInterval <= 0 {
		maintInterval = 5 * time.Second
	}

	s := &Server{
		hubCfg:              cfg,
		yamlProjects:        protocol.ProjectsConfig{SchemaVersion: 1, Projects: map[string]protocol.ProjectConfig{}},
		yamlMachines:        protocol.MachinesConfig{SchemaVersion: 1, Machines: map[string]protocol.MachineConfig{}},
		projects:            protocol.ProjectsConfig{SchemaVersion: 1, Projects: map[string]protocol.ProjectConfig{}},
		machines:            protocol.MachinesConfig{SchemaVersion: 1, Machines: map[string]protocol.MachineConfig{}},
		heartbeatInterval:   hbInterval,
		leaseTTL:            leaseTTL,
		maintenanceInterval: maintInterval,
		handoffCodes:        map[string]authHandoffEntry{},
		pairLimiter:         newPairingRateLimiter(),
		deliveryLocks:       newDeliveryLocker(),
		store:               opts.Store,
		broker:              broker,
		runners:             NewRunnerGateway(),
		webFS:               opts.WebFS,
	}
	s.ryugi = NewRyugiBridge(RyugiBridgeConfig{
		Store:         opts.Store,
		Broker:        broker,
		Runner:        runner,
		DataDir:       cfg.DataDir,
		Host:          cfg.RyugiHost,
		URLHost:       cfg.RyugiURLHost,
		Port:          cfg.RyugiPort,
		PublicOrigin:  cfg.RyugiPublicOrigin,
		PollInterval:  time.Duration(cfg.RyugiPollIntervalSeconds) * time.Second,
		IsPaused:      s.IsGCPaused,
		DeliveryLocks: s.deliveryLocks,
	})

	// Best-effort initial config load if projects.yaml / machines.yaml exist in configDir
	if cfg.ConfigDir != "" {
		if _, err := s.ReloadConfig(ctx); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	} else {
		if err := s.refreshEffectiveRegistry(ctx); err != nil {
			return nil, err
		}
	}

	// Calibrate permissions on existing active=1 frozen Ryugi snapshots on startup (preserving comments.json/.state)
	if err := s.ryugi.CalibrateActiveBindings(ctx); err != nil {
		return nil, err
	}

	s.router = s.buildRouter()
	// Retry any pending frozen delivery directory cleanups recorded in SQLite from prior runs.
	s.retryPendingDeliveryCleanups(ctx)

	return s, nil
}

// SetRemoveFrozenDirHook overrides the frozen directory removal function (for tests).
func (s *Server) SetRemoveFrozenDirHook(fn func(string) error) {
	s.testHookMu.Lock()
	s.removeFrozenDirFn = fn
	s.testHookMu.Unlock()
}

// SetManualImportBeforeInsertHook installs a test hook invoked in handleManualImport right after
// the pending-cleanup pre-check and right before CreateManualImportDelivery's write transaction.
func (s *Server) SetManualImportBeforeInsertHook(fn func()) {
	s.testHookMu.Lock()
	s.manualImportBeforeInsertHook = fn
	s.testHookMu.Unlock()
}

// Handler returns the root HTTP handler for mounting in net/http.Server or httptest.Server.
func (s *Server) Handler() http.Handler { return s.router }

// RyugiBridge returns the underlying RyugiBridge (for poller lifecycle & tests).
func (s *Server) RyugiBridge() *RyugiBridge { return s.ryugi }

// EventBroker returns the WebSocket event broker.
func (s *Server) EventBroker() *EventBroker { return s.broker }

// RunnerGateway returns the active Runner WebSocket gateway.
func (s *Server) RunnerGateway() *RunnerGateway { return s.runners }

// SetPABeforeCommitHook registers a test hook invoked after locking H and before committing a PA turn.
func (s *Server) SetPABeforeCommitHook(fn func(fromSeq, highSeq int64)) {
	s.paMu.Lock()
	s.paBeforeCommitHook = fn
	s.paMu.Unlock()
}

// SetHebi8BeforeCommitHook registers a test hook invoked inside CommitHebi8Fold right before SQLite commit.
func (s *Server) SetHebi8BeforeCommitHook(fn func() error) {
	s.mu.Lock()
	s.hebi8BeforeCommitHook = fn
	s.mu.Unlock()
}

func (s *Server) getHebi8BeforeCommitHook() func() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hebi8BeforeCommitHook
}

// Shutdown closes active runner/client WebSocket connections and waits for background tasks.
func (s *Server) Shutdown() {
	s.closeAllClientWS()
	s.runners.CloseAll()
	s.bgWG.Wait()
}

func (s *Server) hubConfigSnapshot() protocol.HubConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hubCfg
}

// ReloadConfig loads projects.yaml and machines.yaml from ConfigDir and syncs them into SQLite.
func (s *Server) ReloadConfig(ctx context.Context) (protocol.ReloadConfigResponse, error) {
	cfg := s.hubConfigSnapshot()
	projPath := filepath.Join(cfg.ConfigDir, "projects.yaml")
	machPath := filepath.Join(cfg.ConfigDir, "machines.yaml")

	projects := protocol.ProjectsConfig{SchemaVersion: 1, Projects: map[string]protocol.ProjectConfig{}}
	if _, err := os.Stat(projPath); err == nil {
		p, err := protocol.LoadProjectsConfig(projPath)
		if err != nil {
			return protocol.ReloadConfigResponse{}, err
		}
		projects = p
	} else if !errors.Is(err, os.ErrNotExist) {
		return protocol.ReloadConfigResponse{}, err
	}

	machines := protocol.MachinesConfig{SchemaVersion: 1, Machines: map[string]protocol.MachineConfig{}}
	if _, err := os.Stat(machPath); err == nil {
		m, err := protocol.LoadMachinesConfig(machPath)
		if err != nil {
			return protocol.ReloadConfigResponse{}, err
		}
		machines = m
	} else if !errors.Is(err, os.ErrNotExist) {
		return protocol.ReloadConfigResponse{}, err
	}

	if err := s.store.SyncRegistry(ctx, cfg.DataDir, projects, machines); err != nil {
		return protocol.ReloadConfigResponse{}, err
	}

	// Ensure bare mirrors exist with pre-receive hook for every configured project
	for projectID := range projects.Projects {
		mirrorPath := filepath.Join(cfg.DataDir, "git", projectID+".git")
		if err := gitops.EnsureBareMirror(ctx, mirrorPath); err != nil {
			return protocol.ReloadConfigResponse{}, fmt.Errorf("ensure bare mirror for %s: %w", projectID, err)
		}
	}

	s.mu.Lock()
	s.yamlProjects = projects
	s.yamlMachines = machines
	s.mu.Unlock()

	if err := s.refreshEffectiveRegistry(ctx); err != nil {
		return protocol.ReloadConfigResponse{}, err
	}

	// Ensure bare mirrors exist for dynamic UI-created projects as well
	s.mu.RLock()
	effProjects := make([]string, 0, len(s.projects.Projects))
	for pid := range s.projects.Projects {
		effProjects = append(effProjects, pid)
	}
	effMachCount := len(s.machines.Machines)
	s.mu.RUnlock()

	for _, projectID := range effProjects {
		mirrorPath := filepath.Join(cfg.DataDir, "git", projectID+".git")
		if err := gitops.EnsureBareMirror(ctx, mirrorPath); err != nil {
			return protocol.ReloadConfigResponse{}, fmt.Errorf("ensure bare mirror for %s: %w", projectID, err)
		}
	}

	return protocol.ReloadConfigResponse{
		ProjectsLoaded: len(effProjects),
		MachinesLoaded: effMachCount,
	}, nil
}

func (s *Server) buildRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(s.securityHeadersMiddleware)

	// Unauthenticated lightweight health probe (exposes only opaque discoveryId — never projects/tasks/names)
	r.Get("/api/health", func(w http.ResponseWriter, r *http.Request) {
		discID, _ := s.store.EnsureDiscoveryID(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          true,
			"service":     "hebi8-orbit-hub",
			"version":     "0.3.2",
			"discoveryId": discID,
		})
	})

	// One-time code claim endpoints (self-authenticated via code, strict origin/rate limits)
	r.Post("/api/pairing/claim", s.handleClaimPairingCode)
	r.Post("/api/auth/handoff/claim", s.handleClaimAuthHandoff)
	r.Post("/api/auth/pair-client", s.handlePairClient)

	// WebSocket endpoints handle their own auth prior to upgrade
	r.Get("/ws/client", s.handleClientWS)
	r.Get("/ws/runner", s.handleRunnerWS)

	// Content-addressed blob endpoints accept both standard API auth AND authenticated Runners (B12)
	r.Route("/api/blobs", func(blobs chi.Router) {
		blobs.Use(s.blobAuthMiddleware)
		blobs.Put("/{sha256}", s.handlePutBlob)
		blobs.Get("/{sha256}", s.handleGetBlob)
		blobs.Head("/{sha256}", s.handleHeadBlob)
	})

	// Runner-authenticated git bundle transport for remote_ws cross-OS project deliveries
	r.Route("/api/runner", func(rr chi.Router) {
		rr.Use(s.runnerAuthMiddleware)
		rr.Get("/projects/{projectId}/mirror-heads", s.handleGetRunnerMirrorHeads)
		rr.Put("/projects/{projectId}/deliveries/{deliveryId}/bundle", s.handlePutRunnerDeliveryBundle)
		rr.Get("/projects/{projectId}/deliveries/{deliveryId}/bundle", s.handleGetRunnerDeliveryBundle)
	})

	// Authenticated REST API
	r.Route("/api", func(api chi.Router) {
		api.Use(s.authMiddleware)

		api.Post("/ws-ticket", s.handleCreateWSTicket)
		api.Post("/auth/handoff", s.handleCreateAuthHandoff)
		api.Post("/auth/logout", s.handleLogoutClient)
		api.Get("/clients", s.handleListClients)
		api.Delete("/clients/{clientId}", s.handleRevokeClient)
		api.Get("/bootstrap", s.handleBootstrap)
		api.Post("/onboarding/complete", s.handleCompleteOnboarding)

		api.Post("/pairing/codes", s.handleCreatePairingCode)
		api.Get("/pairing/codes/{pairingId}", s.handleGetPairingCode)
		api.Delete("/pairing/codes/{pairingId}", s.handleRevokePairingCode)

		api.Get("/machines", s.handleListMachines)
		api.Patch("/machines/{machineId}", s.handleUpdateMachine)
		api.Delete("/machines/{machineId}", s.handleDeleteMachine)
		api.Post("/machines/{machineId}/agents/probe", s.handleProbeMachineAgents)
		api.Post("/machines/{machineId}/agents/{adapter}/test", s.handleTestMachineAgent)
		api.Post("/machines/{machineId}/agents/{adapter}/install", s.handleInstallMachineAgent)
		api.Patch("/machines/{machineId}/agents/{adapter}", s.handleUpdateMachineAgent)
		api.Get("/machines/{machineId}/dirs", s.handleListMachineDirs)
		api.Get("/machines/{machineId}/ssh-candidates", s.handleGetMachineSSHCandidates)

		api.Post("/projects", s.handleCreateProject)
		api.Patch("/projects/{projectId}", s.handleUpdateProjectSettings)
		api.Delete("/projects/{projectId}", s.handleDeleteProject)
		api.Post("/projects/{projectId}/parts", s.handleCreateProjectPart)
		api.Delete("/projects/{projectId}/parts/{partId}", s.handleDeleteProjectPart)
		api.Post("/projects/{projectId}/parts/{partId}/locations", s.handleCreateProjectLocation)
		api.Patch("/projects/{projectId}/parts/{partId}/locations/{locationId}", s.handleUpdateProjectLocation)
		api.Delete("/projects/{projectId}/parts/{partId}/locations/{locationId}", s.handleDeleteProjectLocation)
		api.Post("/projects/{projectId}/locations/refresh", s.handleRefreshProjectLocations)
		api.Get("/projects/{projectId}/suggestions", s.handleGetProjectSuggestions)
		api.Post("/projects/{projectId}/parts/{partId}/suggestions/dismiss", s.handleDismissProjectSuggestion)

		api.Get("/network/settings", s.handleGetNetworkSettings)
		api.Patch("/network/settings", s.handleUpdateNetworkSettings)
		api.Get("/network/discover", s.handleDiscoverNetwork)
		api.Post("/network/discover", s.handleDiscoverNetwork)
		api.Post("/network/tailscale-enable", s.handleTailscaleEnable)
		api.Post("/network/tailscale-disable", s.handleTailscaleDisable)

		api.Get("/tasks", s.handleListAllTasks)
		api.Get("/projects/{projectId}/tasks", s.handleListProjectTasks)
		api.Post("/projects/{projectId}/tasks", s.handleCreateTask)

		api.Post("/tasks/{taskId}/bind-project", s.handleBindScratchTask)
		api.Get("/tasks/{taskId}/timeline", s.handleGetTaskTimeline)
		api.Post("/tasks/{taskId}/followup", s.handleFollowupTask)
		api.Post("/tasks/{taskId}/cancel", s.handleCancelTask)
		api.Post("/tasks/{taskId}/retry", s.handleRetryTask)
		api.Post("/tasks/{taskId}/archive", s.handleArchiveTask)
		api.Post("/tasks/{taskId}/unarchive", s.handleUnarchiveTask)
		api.Delete("/tasks/{taskId}", s.handleDeleteTask)
		api.Get("/tasks/{taskId}/folder-file", s.handleDownloadTaskFolderFile)
		api.Post("/tasks/{taskId}/folder-changes/promote", s.handlePromoteFolderChanges)

		api.Get("/tasks/{taskId}/queue", s.handleListQueue)
		api.Get("/tasks/{taskId}/queue/{inputId}", s.handleGetQueueInput)
		api.Post("/tasks/{taskId}/queue", s.handleEnqueueInput)
		api.Patch("/tasks/{taskId}/queue/{inputId}", s.handleUpdateQueueInput)
		api.Delete("/tasks/{taskId}/queue/{inputId}", s.handleDeleteQueueInput)
		api.Post("/tasks/{taskId}/queue/{inputId}/steer", s.handleSteerQueueInput)
		api.Post("/tasks/{taskId}/queue/reorder", s.handleReorderQueue)

		api.Post("/tasks/{taskId}/dispatch-annotations", s.handleDispatchAnnotations)

		api.Get("/tasks/{taskId}/board", s.handleGetTaskBoard)
		api.Post("/tasks/{taskId}/board/messages", s.handlePostBoardMessage)
		api.Get("/tasks/{taskId}/workers/{workerName}/stream", s.handleGetWorkerStream)

		api.Get("/tasks/{taskId}/sources", s.handleListTaskSources)
		api.Post("/tasks/{taskId}/sources", s.handleAddTaskSource)
		api.Delete("/tasks/{taskId}/sources/{sourceId}", s.handleDeleteTaskSource)

		api.Get("/attempts/{attemptId}/raw", s.handleGetAttemptRaw)

		api.Get("/approvals", s.handleListApprovals)
		api.Post("/approvals/{approvalId}/decide", s.handleDecideApproval)

		api.Post("/pa/brief", s.handlePABrief)
		api.Get("/pa/threads", s.handleListPAThreads)
		api.Post("/pa/threads", s.handleCreatePAThread)
		api.Get("/pa/threads/{threadId}", s.handleGetPAThread)
		api.Post("/pa/chat", s.handlePAChat)
		api.Post("/pa/reset-session", s.handlePAResetSession)

		api.Get("/hebi8/main", s.handleHebi8Main)
		api.Post("/hebi8/fold", s.handleHebi8Fold)
		api.Get("/hebi8/line", s.handleHebi8Line)
		api.Get("/hebi8/line/{nodeId}/expand", s.handleHebi8Expand)
		api.Post("/hebi8/line/{nodeId}/pin", s.handleHebi8Pin)

		api.Post("/deliveries/manual-import", s.handleManualImport)
		api.Get("/artifacts", s.handleListGlobalArtifacts)
		api.Delete("/annotations/{annotationId}", s.handleDeleteAnnotation)
		api.Get("/deliveries/{deliveryId}", s.handleGetDelivery)
		api.Get("/deliveries/{deliveryId}/diff", s.handleGetDeliveryDiff)
		api.Post("/deliveries/{deliveryId}/annotations", s.handleCreateAnnotation)
		api.Post("/deliveries/{deliveryId}/ryugi/sync", s.handleSyncDeliveryRyugi)
		api.Post("/deliveries/{deliveryId}/ryugi/open", s.handleOpenDeliveryRyugi)
		api.Get("/deliveries/{deliveryId}/ryugi/{bindingId}/content", s.handleGetDeliveryRyugiContent)
		api.Post("/deliveries/{deliveryId}/complete-review", s.handleCompleteDeliveryReview)

		api.Post("/admin/reload-config", s.handleReloadConfig)
		api.Post("/admin/gc-pause", s.handleGCPause)
	})

	// Embedded React SPA fallback for all non-API / non-WS routes
	r.NotFound(s.handleSPA)
	return r
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	yamlMachineMeta := s.yamlMachines.Machines
	s.mu.RUnlock()

	resp, err := s.store.Bootstrap(r.Context(), yamlMachineMeta)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_BOOTSTRAP_FAILED", err.Error())
		return
	}
	resp.WorkspaceName = s.effectiveWorkspaceName()
	resp.Machines = s.enrichMachineViews(r.Context(), resp.Machines)
	setup := s.buildSetupStatus(r.Context(), resp.Machines, clientMachineIDFromContext(r.Context()))
	setup.CurrentClientID = clientTokenIDFromContext(r.Context())
	resp.Setup = &setup
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListGlobalArtifacts(w http.ResponseWriter, r *http.Request) {
	arts, err := s.store.ListGlobalArtifacts(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_ARTIFACTS_LIST", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, protocol.ListArtifactsResponse{Artifacts: arts})
}

func (s *Server) handleDeleteAnnotation(w http.ResponseWriter, r *http.Request) {
	annID := chi.URLParam(r, "annotationId")
	res, ev, err := s.store.DeleteOpenAnnotation(r.Context(), annID, actorFromContext(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_ANNOTATION_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeAPIError(w, http.StatusConflict, "ERR_ANNOTATION_CONFLICT", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_ANNOTATION_DELETE", err.Error())
		return
	}
	if ev != nil {
		s.broker.Broadcast(*ev)
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleListAllTasks(w http.ResponseWriter, r *http.Request) {
	limit := 80
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			limit = v
		}
	}
	excludeProj := strings.TrimSpace(r.URL.Query().Get("excludeProject"))
	projectID := strings.TrimSpace(r.URL.Query().Get("projectId"))
	searchQuery := strings.TrimSpace(r.URL.Query().Get("q"))
	archivedFilter := strings.TrimSpace(r.URL.Query().Get("archived"))
	tasks, err := s.store.ListRecentTasksFiltered(r.Context(), limit, searchQuery, excludeProj, projectID, archivedFilter)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_LIST_TASKS_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

func (s *Server) handleListProjectTasks(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	archivedFilter := strings.TrimSpace(r.URL.Query().Get("archived"))
	tasks, err := s.store.ListProjectTasks(r.Context(), projectID, archivedFilter)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_PROJECT_NOT_FOUND", "project not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_LIST_TASKS_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

func (s *Server) handleGetTaskTimeline(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskId")
	var beforeSeq int64
	if raw := r.URL.Query().Get("beforeSeq"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_BEFORE_SEQ", "beforeSeq must be a non-negative integer")
			return
		}
		beforeSeq = v
	}
	includeRaw := r.URL.Query().Get("includeRaw") == "1" || r.URL.Query().Get("includeRaw") == "true"

	tl, err := s.store.GetTaskTimeline(r.Context(), taskID, beforeSeq, includeRaw)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", "task not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_TIMELINE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tl)
}

func (s *Server) handleManualImport(w http.ResponseWriter, r *http.Request) {
	var req protocol.ManualImportRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	if strings.TrimSpace(req.ProjectID) == "" {
		writeAPIError(w, http.StatusBadRequest, "ERR_MISSING_PROJECT_ID", "projectId is required")
		return
	}
	if strings.TrimSpace(req.DeliveryID) == "" {
		writeAPIError(w, http.StatusBadRequest, "ERR_MISSING_DELIVERY_ID", "deliveryId is required")
		return
	}
	if strings.TrimSpace(req.BaseSHA) == "" || strings.TrimSpace(req.HeadSHA) == "" {
		writeAPIError(w, http.StatusBadRequest, "ERR_MISSING_SHA", "baseSha and headSha are required")
		return
	}
	expectedRef := "refs/orbit/deliveries/" + req.DeliveryID
	if req.GitRef == "" {
		req.GitRef = expectedRef
	}
	if req.GitRef != expectedRef {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_GIT_REF", "gitRef must equal refs/orbit/deliveries/<deliveryId>")
		return
	}

	proj, err := s.store.GetProject(r.Context(), req.ProjectID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_PROJECT_NOT_FOUND", "project not registered in hub")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_PROJECT_LOOKUP", err.Error())
		return
	}

	// Verify bare mirror contains delivery ref pointing to headSha and contains baseSha
	if err := gitops.VerifyMirrorDelivery(r.Context(), proj.MirrorBarePath, req.DeliveryID, req.BaseSHA, req.HeadSHA); err != nil {
		status := http.StatusBadRequest
		code := "ERR_MIRROR_VERIFY_FAILED"
		if errors.Is(err, gitops.ErrDeliveryRefConflict) {
			status = http.StatusConflict
			code = "ERR_DELIVERY_REF_CONFLICT"
		}
		writeAPIError(w, status, code, err.Error())
		return
	}

	// Verify every declared blob exists in delivery_blobs, matches size, and is present on disk
	for _, b := range req.Blobs {
		if !sha256Regex.MatchString(b.SHA256) {
			writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_BLOB_SHA", "invalid blob sha256: "+b.SHA256)
			return
		}
		rec, err := s.store.GetBlobRecord(r.Context(), b.SHA256)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "ERR_BLOB_MISSING", "declared blob not uploaded: "+b.SHA256)
			return
		}
		if rec.SizeBytes != b.Size {
			writeAPIError(w, http.StatusBadRequest, "ERR_BLOB_SIZE_MISMATCH", fmt.Sprintf("declared blob %s size %d does not match stored size %d", b.SHA256, b.Size, rec.SizeBytes))
			return
		}
		if _, err := os.Stat(rec.StoragePath); err != nil {
			writeAPIError(w, http.StatusBadRequest, "ERR_BLOB_MISSING", "declared blob file missing on disk: "+b.SHA256)
			return
		}
	}

	// Compute authoritative diff stats & Ryugi artifact locators from bare mirror
	diffFiles, err := gitops.ComputeMirrorDiffStats(r.Context(), proj.MirrorBarePath, req.BaseSHA, req.HeadSHA)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_DIFF_STATS_FAILED", err.Error())
		return
	}
	ins, del := 0, 0
	for _, f := range diffFiles {
		ins += f.Insertions
		del += f.Deletions
	}
	discoveredArtifacts, err := gitops.DiscoverRyugiArtifactsInMirror(r.Context(), proj.MirrorBarePath, req.HeadSHA)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_DISCOVER_ARTIFACTS", err.Error())
		return
	}

	manifest := protocol.DeliveryManifest{
		FilesChanged:   len(diffFiles),
		Insertions:     ins,
		Deletions:      del,
		Blobs:          req.Blobs,
		RyugiArtifacts: discoveredArtifacts,
		WorktreePath:   req.WorktreePath,
	}

	idempKey := req.IdempotencyKey
	if idempKey == "" {
		idempKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	}

	// Serialize same-deliveryID re-import against pending frozen directory cleanups and in-flight
	// Ryugi registrations using the shared per-deliveryID lifecycle lock.
	// Release the lock BEFORE invoking s.ryugi.RegisterDelivery below (no re-entrant locking).
	unlockDelivery := s.deliveryLocks.Lock(strings.TrimSpace(req.DeliveryID))
	if hasPending, pErr := s.store.HasPendingDeliveryCleanup(r.Context(), req.DeliveryID); pErr != nil {
		unlockDelivery()
		writeAPIError(w, http.StatusInternalServerError, "ERR_DELIVERY_CLEANUP_CHECK", pErr.Error())
		return
	} else if hasPending {
		if clErr := s.cleanupPendingDeliveryDirLocked(r.Context(), req.DeliveryID); clErr != nil {
			unlockDelivery()
			writeAPIError(w, http.StatusConflict, "ERR_DELIVERY_CLEANUP_PENDING", fmt.Sprintf("previous delivery %s snapshot cleanup failed: %v", req.DeliveryID, clErr))
			return
		}
	}

	s.testHookMu.Lock()
	preInsertHook := s.manualImportBeforeInsertHook
	s.testHookMu.Unlock()
	if preInsertHook != nil {
		preInsertHook()
	}

	res, err := s.store.CreateManualImportDelivery(r.Context(), store.ManualImportParams{
		DeliveryID:      req.DeliveryID,
		ProjectID:       req.ProjectID,
		TaskID:          req.TaskID,
		Title:           req.Title,
		Summary:         req.Summary,
		SourceMachineID: req.SourceMachineID,
		RepoIdentity:    proj.RepoIdentity,
		BaseSHA:         req.BaseSHA,
		HeadSHA:         req.HeadSHA,
		GitRef:          req.GitRef,
		Kind:            req.Kind,
		Manifest:        manifest,
		WorktreePath:    req.WorktreePath,
		IdempotencyKey:  idempKey,
		ActorID:         actorFromContext(r.Context()),
	})
	unlockDelivery()
	if errors.Is(err, store.ErrDeliveryCleanupPending) {
		writeAPIError(w, http.StatusConflict, "ERR_DELIVERY_CLEANUP_PENDING", err.Error())
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_TASK_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, store.ErrOwnershipMismatch) {
		writeAPIError(w, http.StatusBadRequest, "ERR_OWNERSHIP_MISMATCH", err.Error())
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeAPIError(w, http.StatusConflict, "ERR_DELIVERY_CONFLICT", err.Error())
		return
	}
	if errors.Is(err, store.ErrBlobMismatch) {
		writeAPIError(w, http.StatusBadRequest, "ERR_BLOB_SIZE_MISMATCH", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_MANUAL_IMPORT_FAILED", err.Error())
		return
	}

	// Broadcast committed delivery events immediately
	s.broker.Broadcast(res.Events...)

	// Decoupled idempotent Ryugi frozen snapshot export & registration (never tears down committed code delivery!)
	resp := protocol.ManualImportResponse{
		Task:          res.Task,
		Attempt:       res.Attempt,
		Delivery:      res.Delivery,
		RyugiBindings: []protocol.RyugiBindingView{},
		InboxItem:     res.InboxItem,
		Created:       res.Created,
	}
	if len(discoveredArtifacts) > 0 {
		// On exact idempotent replay where Ryugi is already ready, return existing bindings directly
		// without re-invoking `ryugi start` or emitting duplicate delivery.ryugi_updated events.
		needsRegister := res.Created ||
			res.Delivery.RyugiSyncState == protocol.RyugiSyncPending ||
			res.Delivery.RyugiSyncState == protocol.RyugiSyncFailed
		if needsRegister {
			bindings, regErr := s.ryugi.RegisterDelivery(r.Context(), req.DeliveryID)
			if bindings != nil {
				resp.RyugiBindings = bindings
			}
			if regErr != nil {
				resp.RyugiError = regErr.Error()
			}
		}
		if updatedDlv, updatedBindings, _, _, err := s.store.GetDeliveryDetail(r.Context(), req.DeliveryID); err == nil {
			resp.Delivery = updatedDlv
			resp.RyugiBindings = updatedBindings
		}
	}

	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, resp)
}

func (s *Server) handleGetDelivery(w http.ResponseWriter, r *http.Request) {
	deliveryID := chi.URLParam(r, "deliveryId")
	dlv, bindings, annotations, proj, err := s.store.GetDeliveryDetail(r.Context(), deliveryID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_DELIVERY_NOT_FOUND", "delivery not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_DELIVERY_DETAIL", err.Error())
		return
	}

	diffFiles, err := gitops.ComputeMirrorDiffStats(r.Context(), proj.MirrorBarePath, dlv.BaseSHA, dlv.HeadSHA)
	if err != nil {
		diffFiles = []protocol.DiffFileStat{}
	}

	writeJSON(w, http.StatusOK, protocol.DeliveryDetailResponse{
		Delivery:      dlv,
		RyugiBindings: bindings,
		Annotations:   annotations,
		DiffFiles:     diffFiles,
		Project:       proj,
	})
}

func (s *Server) handleGetDeliveryDiff(w http.ResponseWriter, r *http.Request) {
	deliveryID := chi.URLParam(r, "deliveryId")
	// Preserve leading/trailing whitespace in file names verbatim (no TrimSpace)
	fileParam := r.URL.Query().Get("file")

	dlv, _, _, proj, err := s.store.GetDeliveryDetail(r.Context(), deliveryID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_DELIVERY_NOT_FOUND", "delivery not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_DELIVERY_LOOKUP", err.Error())
		return
	}

	files, diffText, err := gitops.ComputeMirrorDiff(r.Context(), proj.MirrorBarePath, dlv.BaseSHA, dlv.HeadSHA, fileParam)
	if errors.Is(err, gitops.ErrInvalidPath) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_FILE_PATH", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_DIFF_FAILED", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, protocol.DeliveryDiffResponse{
		DeliveryID: dlv.DeliveryID,
		BaseSHA:    dlv.BaseSHA,
		HeadSHA:    dlv.HeadSHA,
		File:       fileParam,
		Files:      files,
		DiffText:   diffText,
	})
}

func (s *Server) handleCreateAnnotation(w http.ResponseWriter, r *http.Request) {
	deliveryID := chi.URLParam(r, "deliveryId")
	var req protocol.CreateAnnotationRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}
	req.Body = strings.TrimSpace(req.Body)
	if req.Body == "" {
		writeAPIError(w, http.StatusBadRequest, "ERR_EMPTY_BODY", "annotation body is required")
		return
	}

	extKey := ""
	if idemp := strings.TrimSpace(r.Header.Get("Idempotency-Key")); idemp != "" {
		extKey = "idemp:" + idemp
	}

	ann, created, ev, err := s.store.CreateAnnotation(r.Context(), store.CreateAnnotationParams{
		DeliveryID:     deliveryID,
		ClaimedProject: strings.TrimSpace(req.ProjectID),
		ClaimedTask:    strings.TrimSpace(req.TaskID),
		Source:         req.Source,
		ExternalKey:    extKey,
		Anchor:         req.Anchor,
		Body:           req.Body,
		ActorID:        actorFromContext(r.Context()),
	})
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_DELIVERY_NOT_FOUND", "delivery not found")
		return
	}
	if errors.Is(err, store.ErrOwnershipMismatch) {
		writeAPIError(w, http.StatusBadRequest, "ERR_OWNERSHIP_MISMATCH", err.Error())
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeAPIError(w, http.StatusConflict, "ERR_IDEMPOTENCY_CONFLICT", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_CREATE_ANNOTATION", err.Error())
		return
	}

	if ev != nil {
		s.broker.Broadcast(*ev)
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, protocol.CreateAnnotationResponse{
		Annotation: ann,
		Created:    created,
	})
}

func (s *Server) handleSyncDeliveryRyugi(w http.ResponseWriter, r *http.Request) {
	deliveryID := chi.URLParam(r, "deliveryId")
	resp, err := s.ryugi.SyncDelivery(r.Context(), deliveryID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_DELIVERY_NOT_FOUND", "delivery not found")
		return
	}
	if errors.Is(err, ErrSyncPaused) {
		writeAPIError(w, http.StatusConflict, "ERR_SYNC_PAUSED", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_RYUGI_SYNC", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleOpenDeliveryRyugi(w http.ResponseWriter, r *http.Request) {
	deliveryID := chi.URLParam(r, "deliveryId")
	bindingID := strings.TrimSpace(r.URL.Query().Get("bindingId"))

	bindings, err := s.ryugi.EnsureDeliveryRegistered(r.Context(), deliveryID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_DELIVERY_NOT_FOUND", "delivery not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "ERR_RYUGI_OPEN", err.Error())
		return
	}
	if bindings == nil {
		bindings = []protocol.RyugiBindingView{}
	}
	openURL := ""
	if bindingID != "" {
		for _, b := range bindings {
			if b.BindingID == bindingID {
				openURL = b.OpenURL
				break
			}
		}
	}
	if openURL == "" && len(bindings) > 0 {
		openURL = bindings[0].OpenURL
	}
	writeJSON(w, http.StatusOK, protocol.RyugiOpenResponse{
		DeliveryID:    deliveryID,
		OpenURL:       openURL,
		RyugiBindings: bindings,
	})
}

func (s *Server) handleGetDeliveryRyugiContent(w http.ResponseWriter, r *http.Request) {
	deliveryID := chi.URLParam(r, "deliveryId")
	bindingID := chi.URLParam(r, "bindingId")

	content, err := s.ryugi.ReadBindingContent(r.Context(), deliveryID, bindingID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_RYUGI_NOT_FOUND", err.Error())
		return
	}
	if errors.Is(err, ErrInvalidArtifactPath) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_ARTIFACT_PATH", err.Error())
		return
	}
	if errors.Is(err, ErrArtifactTooLarge) {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "ERR_ARTIFACT_TOO_LARGE", err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_RYUGI_CONTENT", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, content)
}

func (s *Server) handleCompleteDeliveryReview(w http.ResponseWriter, r *http.Request) {
	deliveryID := chi.URLParam(r, "deliveryId")
	resp, ev, err := s.store.CompleteDeliveryReview(r.Context(), deliveryID, actorFromContext(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_DELIVERY_NOT_FOUND", "delivery not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_COMPLETE_REVIEW", err.Error())
		return
	}
	if ev != nil {
		s.broker.Broadcast(*ev)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handlePutBlob(w http.ResponseWriter, r *http.Request) {
	digest := strings.ToLower(chi.URLParam(r, "sha256"))
	if !sha256Regex.MatchString(digest) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_SHA256", "sha256 must be 64 lowercase hex characters")
		return
	}

	cfg := s.hubConfigSnapshot()
	destDir := filepath.Join(cfg.DataDir, "blobs", digest[0:2], digest[2:4])
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_BLOB_MKDIR", err.Error())
		return
	}
	destPath := filepath.Join(destDir, digest)
	tmpPath := filepath.Join(destDir, ".tmp-"+ulid.Make().String())

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_BLOB_CREATE", err.Error())
		return
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmpPath)
	}()

	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(f, hasher), r.Body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_BLOB_READ", err.Error())
		return
	}
	if err := f.Close(); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_BLOB_CLOSE", err.Error())
		return
	}

	actual := hex.EncodeToString(hasher.Sum(nil))
	if actual != digest {
		writeAPIError(w, http.StatusBadRequest, "ERR_SHA256_MISMATCH", fmt.Sprintf("computed sha256 %s does not match path %s", actual, digest))
		return
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_BLOB_FINALIZE", err.Error())
		return
	}
	if err := s.store.PutBlobRecord(r.Context(), digest, size, destPath); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_BLOB_RECORD", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"sha256": digest,
		"size":   size,
	})
}

func (s *Server) handleGetBlob(w http.ResponseWriter, r *http.Request) {
	digest := strings.ToLower(chi.URLParam(r, "sha256"))
	if !sha256Regex.MatchString(digest) {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_SHA256", "invalid sha256")
		return
	}
	rec, err := s.store.GetBlobRecord(r.Context(), digest)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "ERR_BLOB_NOT_FOUND", "blob not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_BLOB_LOOKUP", err.Error())
		return
	}
	f, err := os.Open(rec.StoragePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeAPIError(w, http.StatusNotFound, "ERR_BLOB_NOT_FOUND", "blob file missing")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "ERR_BLOB_OPEN", err.Error())
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ERR_BLOB_STAT", err.Error())
		return
	}

	// Lock Content-Type + attachment disposition BEFORE http.ServeContent so Go never sniffs
	// HTML/JS while still supporting HTTP Range, If-Range, and conditional ETag/ModTime requests.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, digest))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "immutable, max-age=31536000")
	w.Header().Set("ETag", `"`+digest+`"`)
	http.ServeContent(w, r, digest, info.ModTime(), f)
}

func (s *Server) handleHeadBlob(w http.ResponseWriter, r *http.Request) {
	digest := strings.ToLower(chi.URLParam(r, "sha256"))
	if !sha256Regex.MatchString(digest) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	rec, err := s.store.GetBlobRecord(r.Context(), digest)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if _, err := os.Stat(rec.StoragePath); err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(rec.SizeBytes, 10))
	w.Header().Set("ETag", `"`+digest+`"`)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleReloadConfig(w http.ResponseWriter, r *http.Request) {
	resp, err := s.ReloadConfig(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_RELOAD_CONFIG", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGCPause(w http.ResponseWriter, r *http.Request) {
	var req protocol.GCPauseRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "ERR_INVALID_JSON", "invalid request body")
		return
	}

	s.mu.Lock()
	s.gcPaused = req.Paused
	s.gcReason = strings.TrimSpace(req.Reason)
	if req.Paused {
		ttl := req.TTLSeconds
		if ttl <= 0 {
			ttl = 600
		}
		exp := time.Now().Add(time.Duration(ttl) * time.Second).UnixMilli()
		s.gcExpiresAt = &exp
	} else {
		s.gcExpiresAt = nil
		s.gcReason = ""
	}
	resp := protocol.GCPauseResponse{
		Paused:    s.gcPaused,
		Reason:    s.gcReason,
		ExpiresAt: s.gcExpiresAt,
	}
	s.mu.Unlock()

	// When pausing for backup/maintenance, wait until every in-flight Ryugi sync (read->commit->ack) exits.
	if req.Paused {
		s.ryugi.DrainInFlight()
	}

	writeJSON(w, http.StatusOK, resp)
}

// IsGCPaused reports whether the maintenance GC lock is currently active.
func (s *Server) IsGCPaused() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.gcPaused {
		return false
	}
	if s.gcExpiresAt != nil && time.Now().UnixMilli() > *s.gcExpiresAt {
		return false
	}
	return true
}

func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ws/") {
		writeAPIError(w, http.StatusNotFound, "ERR_NOT_FOUND", "endpoint not found")
		return
	}
	if s.webFS == nil {
		http.Error(w, "web assets not embedded", http.StatusNotFound)
		return
	}

	reqPath := strings.TrimPrefix(filepath.Clean(r.URL.Path), "/")
	if reqPath == "" || reqPath == "." {
		reqPath = "index.html"
	}

	if f, err := s.webFS.Open(reqPath); err == nil {
		stat, statErr := f.Stat()
		_ = f.Close()
		if statErr == nil && !stat.IsDir() {
			http.FileServer(http.FS(s.webFS)).ServeHTTP(w, r)
			return
		}
	}

	// SPA fallback to index.html
	data, err := fs.ReadFile(s.webFS, "index.html")
	if err != nil {
		http.Error(w, "frontend bundle not built yet (run pnpm --dir web build)", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeAPIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, protocol.APIError{
		Code:    code,
		Message: msg,
	})
}
