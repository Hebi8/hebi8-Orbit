package protocol

// LiveAttemptInfo reports an in-memory running attempt (or locally finalized attempt with unacked outbox)
// during runner.hello reconciliation.
type LiveAttemptInfo struct {
	AttemptID    string       `json:"attemptId"`
	LeaseToken   string       `json:"leaseToken"`
	State        AttemptState `json:"state"`
	PendingFinal bool         `json:"pendingFinal,omitempty"`
}

// HandledApprovalDecision reports an approval already applied locally to ACP
// whose approval_ack may not have reached Hub before a transient disconnect.
type HandledApprovalDecision struct {
	ApprovalID string        `json:"approvalId"`
	DecisionID string        `json:"decisionId"`
	AttemptID  string        `json:"attemptId,omitempty"`
	Outcome    ApprovalState `json:"outcome"`
}

// AgentTestResult records the outcome of an active connectivity/auth test for one adapter.
type AgentTestResult struct {
	OK        bool   `json:"ok"`
	At        int64  `json:"at"`
	LatencyMs int64  `json:"latencyMs,omitempty"`
	Error     string `json:"error,omitempty"`
}

// AgentModeOption describes one real ACP permission/operating mode advertised by an adapter.
type AgentModeOption struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// AgentProbe captures the probed runtime status and metadata of one ACP adapter on a machine.
type AgentProbe struct {
	Adapter        Adapter           `json:"adapter"`
	State          AgentState        `json:"state"`
	Enabled        bool              `json:"enabled"`
	BridgeBin      string            `json:"bridgeBin,omitempty"`
	BridgeVersion  string            `json:"bridgeVersion,omitempty"`
	MainCLI        string            `json:"mainCli,omitempty"`
	Source         string            `json:"source,omitempty"` // flag | file | settings | managed | path | none
	HintCode       string            `json:"hintCode,omitempty"`
	Detail         string            `json:"detail,omitempty"`
	DefaultModel   string            `json:"defaultModel,omitempty"`
	Models         []string          `json:"models,omitempty"`
	DefaultMode    string            `json:"defaultMode,omitempty"`
	Modes          []AgentModeOption `json:"modes,omitempty"`
	ModeViaLegacy  bool              `json:"modeViaLegacy,omitempty"`
	DefaultEffort  string            `json:"defaultEffort,omitempty"`
	Efforts        []string          `json:"efforts,omitempty"`
	AuthChecked    bool              `json:"authChecked,omitempty"`
	MaxLiveWorkers int               `json:"maxLiveWorkers,omitempty"`
	LockedFields   []string          `json:"lockedFields,omitempty"`
	LastTest       *AgentTestResult    `json:"lastTest,omitempty"`
	Quota          *AgentQuotaSnapshot `json:"quota,omitempty"`
	ProbedAt       int64               `json:"probedAt,omitempty"`
}

// RunnerAgentConfig carries Hub-managed dynamic overrides for one adapter on a machine.
type RunnerAgentConfig struct {
	Enabled        *bool    `json:"enabled,omitempty"`
	Bin            string   `json:"bin,omitempty"`
	Args           []string `json:"args,omitempty"`
	DefaultModel   string   `json:"defaultModel,omitempty"`
	MaxLiveWorkers int      `json:"maxLiveWorkers,omitempty"`
}

// RunnerConfigPayload holds the dynamic per-machine configuration pushed from Hub to Runner.
type RunnerConfigPayload struct {
	ConfigRev int64                        `json:"configRev"`
	Agents    map[string]RunnerAgentConfig `json:"agents,omitempty"`
}

// RunnerConfigFrame pushes updated dynamic agent settings to a live Runner over /ws/runner.
type RunnerConfigFrame struct {
	Type      RunnerWSMessageType          `json:"type"`
	ConfigRev int64                        `json:"configRev"`
	Agents    map[string]RunnerAgentConfig `json:"agents,omitempty"`
}

// RunnerAgentsReportFrame reports probed agent statuses from Runner to Hub.
type RunnerAgentsReportFrame struct {
	Type      RunnerWSMessageType `json:"type"`
	MachineID string              `json:"machineId"`
	ConfigRev int64               `json:"configRev"`
	Agents    []AgentProbe        `json:"agents"`
}

// RunnerQuotasReportFrame reports real collected agent usage quotas from Runner to Hub.
type RunnerQuotasReportFrame struct {
	Type      RunnerWSMessageType  `json:"type"`
	MachineID string               `json:"machineId"`
	Quotas    []AgentQuotaSnapshot `json:"quotas"`
}

// RunnerRPCFrame sends a control-plane RPC request from Hub to a live Runner.
type RunnerRPCFrame struct {
	Type      RunnerWSMessageType `json:"type"`
	RequestID string              `json:"requestId"`
	Op        string              `json:"op"` // probe | test_agent | install_bridge | list_dirs | check_repo
	Args      map[string]any      `json:"args,omitempty"`
}

// RunnerRPCResultFrame returns the result of a RunnerRPCFrame back to Hub.
type RunnerRPCResultFrame struct {
	Type      RunnerWSMessageType `json:"type"`
	RequestID string              `json:"requestId"`
	OK        bool                `json:"ok"`
	Data      map[string]any      `json:"data,omitempty"`
	Error     string              `json:"error,omitempty"`
}

// RunnerHelloFrame is the first frame sent by orbit-runner on every /ws/runner connect.
type RunnerHelloFrame struct {
	Type                     RunnerWSMessageType       `json:"type"`
	ProtocolVersion          int                       `json:"protocolVersion"`
	MachineID                string                    `json:"machineId"`
	RunnerInstanceID         string                    `json:"runnerInstanceId"`
	RunnerVersion            string                    `json:"runnerVersion"`
	OS                       string                    `json:"os,omitempty"`
	Arch                     string                    `json:"arch,omitempty"`
	Hostname                 string                    `json:"hostname,omitempty"`
	Capabilities             []string                  `json:"capabilities,omitempty"`
	Adapters                 []Adapter                 `json:"adapters,omitempty"`
	AgentProbes              []AgentProbe              `json:"agentProbes,omitempty"`
	AgentQuotas              []AgentQuotaSnapshot      `json:"agentQuotas,omitempty"`
	ConfigRev                int64                     `json:"configRev,omitempty"`
	LiveAttempts             []LiveAttemptInfo         `json:"liveAttempts"`
	LivePendingApprovalIDs   []string                  `json:"livePendingApprovalIds"`
	HandledApprovalDecisions []HandledApprovalDecision `json:"handledApprovalDecisions"`
	Resources                map[string]ResourceState  `json:"resources"`
}

// ApprovalDecisionPayload carries a human approval decision (or expiration unblock) to a live runner handle.
type ApprovalDecisionPayload struct {
	ApprovalID    string            `json:"approvalId"`
	DecisionID    string            `json:"decisionId"`
	AttemptID     string            `json:"attemptId"`
	PayloadSHA256 string            `json:"payloadSha256"`
	OptionID      string            `json:"optionId"`
	DecidedBy     ApprovalDecidedBy `json:"decidedBy"`
	Expired       bool              `json:"expired,omitempty"`
}

// HubWelcomeReconcile carries continuous outbox watermarks, stale attempt aborts,
// and pending approval decisions to redeliver after a transient disconnect.
type HubWelcomeReconcile struct {
	AckedOutboxByAttempt map[string]int64            `json:"ackedOutboxByAttempt"`
	AbortAttemptIDs      []string                    `json:"abortAttemptIds"`
	RedeliverApprovals   []ApprovalDecisionPayload   `json:"redeliverApprovals"`
	PendingBoardMessages []BoardMessageDispatchFrame `json:"pendingBoardMessages,omitempty"`
}

// HubWelcomeFrame is Hub's reconciliation response to runner.hello.
type HubWelcomeFrame struct {
	Type                RunnerWSMessageType  `json:"type"`
	ServerTime          int64                `json:"serverTime"`
	HeartbeatIntervalMs int64                `json:"heartbeatIntervalMs"`
	LeaseTtlMs          int64                `json:"leaseTtlMs"`
	Capabilities        []string             `json:"capabilities,omitempty"`
	Config              *RunnerConfigPayload `json:"config,omitempty"`
	Reconcile           HubWelcomeReconcile  `json:"reconcile"`
}

// InputDeliveryRef identifies an immutable upstream delivery used to seed a worktree.
type InputDeliveryRef struct {
	DeliveryID string             `json:"deliveryId"`
	GitRef     string             `json:"gitRef"`
	HeadSHA    string             `json:"headSha"`
	Blobs      []BlobManifestItem `json:"blobs,omitempty"`
}

// AttemptDispatchFrame instructs a Runner to execute an Attempt in an isolated worktree or in-place folder.
type AttemptDispatchFrame struct {
	Type               RunnerWSMessageType `json:"type"`
	AttemptID          string              `json:"attemptId"`
	TaskID             string              `json:"taskId"`
	ProjectID          string              `json:"projectId"`
	LeaseToken         string              `json:"leaseToken"`
	Adapter            Adapter             `json:"adapter"`
	WBSessionID        string              `json:"wbSessionId"`
	ProviderSessionID  string              `json:"providerSessionId,omitempty"`
	ExpectedSessionCWD string              `json:"expectedSessionCwd,omitempty"`
	LocationID         string              `json:"locationId,omitempty"`
	Isolation          LocationIsolation   `json:"isolation,omitempty"`
	IsPlainFolder      bool                `json:"isPlainFolder,omitempty"`
	RepoPath           string              `json:"repoPath,omitempty"`
	WorktreeRoot       string              `json:"worktreeRoot,omitempty"`
	WorktreePath       string              `json:"worktreePath,omitempty"`
	MirrorBarePath     string              `json:"mirrorBarePath,omitempty"`
	RepoIdentity       string              `json:"repoIdentity,omitempty"`
	BaseSHA            string              `json:"baseSha,omitempty"`
	DefaultBranch      string              `json:"defaultBranch,omitempty"`
	DesiredConfig          map[string]string   `json:"desiredConfig,omitempty"`
	ExplicitFullPermission bool                `json:"explicitFullPermission,omitempty"`
	ProjectEnv             map[string]string   `json:"projectEnv,omitempty"`
	SystemPromptAppend string              `json:"systemPromptAppend,omitempty"`
	InputDelivery      *InputDeliveryRef   `json:"inputDelivery,omitempty"`
	Prompt             string              `json:"prompt"`
	RequiredResources  []string            `json:"requiredResources,omitempty"`
	SwarmIdleTimeoutMs int64               `json:"swarmIdleTimeoutMs,omitempty"`
}

// AttemptCancelFrame instructs a Runner to cancel an active Attempt.
type AttemptCancelFrame struct {
	Type       RunnerWSMessageType `json:"type"`
	AttemptID  string              `json:"attemptId"`
	LeaseToken string              `json:"leaseToken"`
}

// ApprovalDecisionFrame pushes a human decision (or expiration unblock) for a pending ACP permission request.
type ApprovalDecisionFrame struct {
	Type          RunnerWSMessageType `json:"type"`
	ApprovalID    string              `json:"approvalId"`
	DecisionID    string              `json:"decisionId"`
	AttemptID     string              `json:"attemptId"`
	PayloadSHA256 string              `json:"payloadSha256"`
	OptionID      string              `json:"optionId"`
	DecidedBy     ApprovalDecidedBy   `json:"decidedBy"`
	Expired       bool                `json:"expired,omitempty"`
}

// BoardMessageDispatchFrame delivers a web @worker board message to an active native Swarm coordinator.
type BoardMessageDispatchFrame struct {
	Type          RunnerWSMessageType `json:"type"`
	TaskID        string              `json:"taskId"`
	AttemptID     string              `json:"attemptId"`
	MessageID     string              `json:"messageId"`
	Seq           int64               `json:"seq"`
	AuthorName    string              `json:"authorName"`
	TargetMention string              `json:"targetMention,omitempty"`
	Body          string              `json:"body"`
}

// SessionCloseFrame instructs a Runner to close an idle ACP worker session cleanly.
type SessionCloseFrame struct {
	Type              RunnerWSMessageType `json:"type"`
	WBSessionID       string              `json:"wbSessionId"`
	ProviderSessionID string              `json:"providerSessionId"`
}

// LiveAttemptLease pairs an active attemptId with its leaseToken for heartbeat renewal.
type LiveAttemptLease struct {
	AttemptID  string `json:"attemptId"`
	LeaseToken string `json:"leaseToken"`
}

// RunnerHeartbeatFrame renews active attempt leases and reports machine resource probes.
type RunnerHeartbeatFrame struct {
	Type                   RunnerWSMessageType      `json:"type"`
	MachineID              string                   `json:"machineId"`
	RunnerInstanceID       string                   `json:"runnerInstanceId"`
	Adapters               []Adapter                `json:"adapters,omitempty"`
	LiveAttemptLeaseTokens []LiveAttemptLease       `json:"liveAttemptLeaseTokens"`
	Resources              map[string]ResourceState `json:"resources"`
	WorkerRssMb            int                      `json:"workerRssMb"`
}

// OutboxItem is one strictly ordered entry in an attempt's local SQLite outbox.
type OutboxItem struct {
	OutboxSeq int64           `json:"outboxSeq"`
	EventID   string          `json:"eventId"`
	FrameType OutboxFrameType `json:"frameType"`
	Payload   map[string]any  `json:"payload"`
	TS        int64           `json:"ts"`
}

// AttemptOutboxBatchFrame sends one ordered batch of outbox entries from Runner to Hub.
type AttemptOutboxBatchFrame struct {
	Type       RunnerWSMessageType `json:"type"`
	AttemptID  string              `json:"attemptId"`
	LeaseToken string              `json:"leaseToken"`
	Items      []OutboxItem        `json:"items"`
}

// HubOutboxAckFrame confirms the highest strictly contiguous outboxSeq persisted by Hub.
type HubOutboxAckFrame struct {
	Type           RunnerWSMessageType `json:"type"`
	AttemptID      string              `json:"attemptId"`
	AckedOutboxSeq int64               `json:"ackedOutboxSeq"`
	AcceptedMode   OutboxAcceptedMode  `json:"acceptedMode"`
}

// RunnerFrameHeader peeks the frame type before decoding into a concrete frame struct.
type RunnerFrameHeader struct {
	Type RunnerWSMessageType `json:"type"`
}
