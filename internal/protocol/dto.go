package protocol

//go:generate go run ../../cmd/gen-ts -out ../../web/src/generated/protocol.ts

// APIError is the uniform error response envelope returned by orbit-hub.
type APIError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// ResourceState describes the probed state of an exclusive machine resource.
type ResourceState struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// ProjectView is the read model for a logical project in bootstrap & lists.
type ProjectView struct {
	ProjectID         string                         `json:"projectId"`
	DisplayName       string                         `json:"displayName"`
	RepoIdentity      string                         `json:"repoIdentity"`
	DefaultBranch     string                         `json:"defaultBranch"`
	MirrorBarePath    string                         `json:"mirrorBarePath"`
	DefaultCapability string                         `json:"defaultCapability"`
	DefaultAdapter    Adapter                        `json:"defaultAdapter"`
	IDEScheme         IDEScheme                      `json:"ideScheme"`
	Anchors           map[string]ProjectAnchorConfig `json:"anchors"`
	ActiveTaskCount   int                            `json:"activeTaskCount"`
	BlockedTaskCount  int                            `json:"blockedTaskCount"`
	Archived          bool                           `json:"archived"`
	UpdatedAt         int64                          `json:"updatedAt"`
}

// MachineView is the runtime view of a registered machine.
type MachineView struct {
	MachineID     string                   `json:"machineId"`
	DisplayName   string                   `json:"displayName"`
	TransportMode TransportMode            `json:"transportMode"`
	TailscaleHost string                   `json:"tailscaleHost,omitempty"`
	SSHAlias      string                   `json:"sshAlias,omitempty"`
	Status        MachineStatus            `json:"status"`
	Capabilities  []string                 `json:"capabilities"`
	Resources     map[string]ResourceState `json:"resources"`
	RunnerVersion string                   `json:"runnerVersion,omitempty"`
	LastSeenAt    *int64                   `json:"lastSeenAt,omitempty"`
	UpdatedAt     int64                    `json:"updatedAt"`
}

// InboxItemView represents an open or historical low-noise inbox card.
type InboxItemView struct {
	InboxID    string        `json:"inboxId"`
	ProjectID  string        `json:"projectId"`
	TaskID     string        `json:"taskId,omitempty"`
	Category   InboxCategory `json:"category"`
	RefID      string        `json:"refId"`
	Title      string        `json:"title"`
	Summary    string        `json:"summary"`
	Priority   int           `json:"priority"`
	State      InboxState    `json:"state"`
	CreatedAt  int64         `json:"createdAt"`
	UpdatedAt  int64         `json:"updatedAt"`
	ResolvedAt *int64        `json:"resolvedAt,omitempty"`
}

// BootstrapResponse is captured in a single read-only SQLite transaction.
type BootstrapResponse struct {
	Projects       []ProjectView   `json:"projects"`
	Machines       []MachineView   `json:"machines"`
	OpenInboxItems []InboxItemView `json:"openInboxItems"`
	ActiveTasks    []TaskSummary   `json:"activeTasks"`
	PAThreads      []PAThread      `json:"paThreads"`
	PAState        PAStateView     `json:"paState"`
	SnapshotSeq    int64           `json:"snapshotSeq"`
}

// BlobManifestItem references a content-addressed attachment blob.
type BlobManifestItem struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// DeliveryManifest captures immutable stats & artifact references for a delivery.
type DeliveryManifest struct {
	FilesChanged   int                `json:"filesChanged"`
	Insertions     int                `json:"insertions"`
	Deletions      int                `json:"deletions"`
	Blobs          []BlobManifestItem `json:"blobs,omitempty"`
	RyugiArtifacts []string           `json:"ryugiArtifacts,omitempty"`
	WorktreePath   string             `json:"worktreePath,omitempty"`
}

// RyugiBindingView represents a registered frozen Ryugi artifact view.
type RyugiBindingView struct {
	BindingID          string `json:"bindingId"`
	DeliveryID         string `json:"deliveryId"`
	ProjectID          string `json:"projectId"`
	TaskID             string `json:"taskId"`
	FrozenProjectDir   string `json:"frozenProjectDir"`
	ArtifactLocator    string `json:"artifactLocator"`
	DerivedArtifactID  string `json:"derivedArtifactId"`
	OpenURL            string `json:"openUrl"`
	LastThreadReceipt  string `json:"lastThreadReceipt,omitempty"`
	LastChangesReceipt string `json:"lastChangesReceipt,omitempty"`
	Active             bool   `json:"active"`
	CreatedAt          int64  `json:"createdAt"`
	UpdatedAt          int64  `json:"updatedAt"`
}

// AnnotationAnchor describes either a Diff line-range anchor or a Ryugi artifact anchor.
type AnnotationAnchor struct {
	// Diff fields
	Path          string `json:"path,omitempty"`
	Side          string `json:"side,omitempty"`
	StartLine     int    `json:"startLine,omitempty"`
	EndLine       int    `json:"endLine,omitempty"`
	ContextBefore string `json:"contextBefore,omitempty"`
	ContextAfter  string `json:"contextAfter,omitempty"`
	BaseSHA       string `json:"baseSha,omitempty"`
	HeadSHA       string `json:"headSha,omitempty"`
	// Ryugi fields
	Artifact  string `json:"artifact,omitempty"`
	Section   string `json:"section,omitempty"`
	ThreadID  string `json:"threadId,omitempty"`
	BlockID   string `json:"blockId,omitempty"`
	Quote     string `json:"quote,omitempty"`
	ElementID string `json:"elementId,omitempty"`
	Page      string `json:"page,omitempty"`
	Verdict   string `json:"verdict,omitempty"`
}

// AnnotationView represents a unified review annotation from Ryugi or Diff Drawer.
type AnnotationView struct {
	AnnotationID         string           `json:"annotationId"`
	ProjectID            string           `json:"projectId"`
	TaskID               string           `json:"taskId"`
	DeliveryID           string           `json:"deliveryId"`
	Source               AnnotationSource `json:"source"`
	ExternalKey          string           `json:"externalKey,omitempty"`
	Anchor               AnnotationAnchor `json:"anchor"`
	Body                 string           `json:"body"`
	Status               AnnotationStatus `json:"status"`
	TriggeredAttemptID   string           `json:"triggeredAttemptId,omitempty"`
	ResolutionDeliveryID string           `json:"resolutionDeliveryId,omitempty"`
	ResolutionNote       string           `json:"resolutionNote,omitempty"`
	CreatedAt            int64            `json:"createdAt"`
	ResolvedAt           *int64           `json:"resolvedAt,omitempty"`
}

// DeliveryView represents an immutable delivery record.
type DeliveryView struct {
	DeliveryID        string             `json:"deliveryId"`
	ProjectID         string             `json:"projectId"`
	TaskID            string             `json:"taskId"`
	AttemptID         string             `json:"attemptId"`
	SourceMachineID   string             `json:"sourceMachineId"`
	RepoIdentity      string             `json:"repoIdentity"`
	BaseSHA           string             `json:"baseSha"`
	HeadSHA           string             `json:"headSha"`
	GitRef            string             `json:"gitRef"`
	Kind              DeliveryKind       `json:"kind"`
	Summary           string             `json:"summary"`
	Manifest          DeliveryManifest   `json:"manifest"`
	FrozenSnapshotDir string             `json:"frozenSnapshotDir,omitempty"`
	RyugiSyncState    RyugiSyncState     `json:"ryugiSyncState"`
	LateArrival       bool               `json:"lateArrival"`
	Pinned            bool               `json:"pinned"`
	CreatedAt         int64              `json:"createdAt"`
	RyugiBindings     []RyugiBindingView `json:"ryugiBindings,omitempty"`
	OpenAnnotations   int                `json:"openAnnotations"`
}

// AttemptView represents one execution attempt of a task.
type AttemptView struct {
	AttemptID        string            `json:"attemptId"`
	TaskID           string            `json:"taskId"`
	ProjectID        string            `json:"projectId"`
	AttemptNo        int               `json:"attemptNo"`
	MachineID        string            `json:"machineId"`
	Adapter          Adapter           `json:"adapter"`
	TriggerKind      TriggerKind       `json:"triggerKind"`
	WorktreePath     string            `json:"worktreePath,omitempty"`
	BaseSHA          string            `json:"baseSha,omitempty"`
	OutputDeliveryID string            `json:"outputDeliveryId,omitempty"`
	State            AttemptState      `json:"state"`
	CompletionSource *CompletionSource `json:"completionSource,omitempty"`
	ErrorCode        string            `json:"errorCode,omitempty"`
	ErrorDetail      string            `json:"errorDetail,omitempty"`
	StartedAt        int64             `json:"startedAt"`
	FinishedAt       *int64            `json:"finishedAt,omitempty"`
}

// TaskSummary represents a logical task in list & timeline headers.
type TaskSummary struct {
	TaskID              string        `json:"taskId"`
	ProjectID           string        `json:"projectId"`
	ParentTaskID        string        `json:"parentTaskId,omitempty"`
	Title               string        `json:"title"`
	Goal                string        `json:"goal"`
	RequiredCapability  string        `json:"requiredCapability"`
	PreferredAdapter    Adapter       `json:"preferredAdapter"`
	State               TaskState     `json:"state"`
	WaitingReason       string        `json:"waitingReason,omitempty"`
	ActiveAttemptID     string        `json:"activeAttemptId,omitempty"`
	ActiveAttemptState  *AttemptState `json:"activeAttemptState,omitempty"`
	LatestDeliveryID    string        `json:"latestDeliveryId,omitempty"`
	LatestDeliveryHead  string        `json:"latestDeliveryHead,omitempty"`
	OpenAnnotationCount int           `json:"openAnnotationCount"`
	DeliveryCount       int           `json:"deliveryCount"`
	QueuedInputCount    int           `json:"queuedInputCount"`
	WorkerCount         int           `json:"workerCount"`
	CreatedBy           TaskCreatedBy `json:"createdBy"`
	CreatedAt           int64         `json:"createdAt"`
	UpdatedAt           int64         `json:"updatedAt"`
}

// EventRecord is a single row from the unified append-only events log.
type EventRecord struct {
	Seq        int64           `json:"seq"`
	EventID    string          `json:"eventId"`
	StreamID   string          `json:"streamId"`
	ProjectID  string          `json:"projectId,omitempty"`
	TaskID     string          `json:"taskId,omitempty"`
	AttemptID  string          `json:"attemptId,omitempty"`
	ActorKind  ActorKind       `json:"actorKind"`
	ActorID    string          `json:"actorId"`
	Kind       EventKind       `json:"kind"`
	Visibility EventVisibility `json:"visibility"`
	Payload    map[string]any  `json:"payload"`
	CreatedAt  int64           `json:"createdAt"`
}

// TaskTimelineResponse powers the center workbench timeline pane.
type TaskTimelineResponse struct {
	Task          TaskSummary         `json:"task"`
	Attempts      []AttemptView       `json:"attempts"`
	Deliveries    []DeliveryView      `json:"deliveries"`
	Annotations   []AnnotationView    `json:"annotations"`
	PendingInputs []PendingInput      `json:"pendingInputs"`
	Approvals     []ApprovalView      `json:"approvals"`
	Sources       []TaskSource        `json:"sources"`
	BoardMembers  []BoardMemberCursor `json:"boardMembers"`
	Events        []EventRecord       `json:"events"`
}

// DiffFileStat summarizes one changed file in a delivery diff.
type DiffFileStat struct {
	Path       string `json:"path"`
	OldPath    string `json:"oldPath,omitempty"`
	Status     string `json:"status"`
	Insertions int    `json:"insertions"`
	Deletions  int    `json:"deletions"`
	Binary     bool   `json:"binary"`
}

// DeliveryDetailResponse is returned by GET /api/deliveries/:deliveryId.
type DeliveryDetailResponse struct {
	Delivery      DeliveryView       `json:"delivery"`
	RyugiBindings []RyugiBindingView `json:"ryugiBindings"`
	Annotations   []AnnotationView   `json:"annotations"`
	DiffFiles     []DiffFileStat     `json:"diffFiles"`
	Project       ProjectView        `json:"project"`
}

// DeliveryDiffResponse is returned by GET /api/deliveries/:deliveryId/diff.
type DeliveryDiffResponse struct {
	DeliveryID string         `json:"deliveryId"`
	BaseSHA    string         `json:"baseSha"`
	HeadSHA    string         `json:"headSha"`
	File       string         `json:"file,omitempty"`
	Files      []DiffFileStat `json:"files"`
	DiffText   string         `json:"diffText"`
}

// ManualImportRequest is submitted by `orbit deliver` in Phase 0.
type ManualImportRequest struct {
	DeliveryID      string             `json:"deliveryId"`
	ProjectID       string             `json:"projectId"`
	TaskID          string             `json:"taskId,omitempty"`
	Title           string             `json:"title,omitempty"`
	Summary         string             `json:"summary"`
	SourceMachineID string             `json:"sourceMachineId,omitempty"`
	BaseSHA         string             `json:"baseSha"`
	HeadSHA         string             `json:"headSha"`
	GitRef          string             `json:"gitRef"`
	Kind            DeliveryKind       `json:"kind,omitempty"`
	Blobs           []BlobManifestItem `json:"blobs,omitempty"`
	RyugiArtifacts  []string           `json:"ryugiArtifacts,omitempty"`
	WorktreePath    string             `json:"worktreePath,omitempty"`
	IdempotencyKey  string             `json:"idempotencyKey,omitempty"`
}

// ManualImportResponse returns the created delivery and associated task/attempt/bindings.
type ManualImportResponse struct {
	Task          TaskSummary        `json:"task"`
	Attempt       AttemptView        `json:"attempt"`
	Delivery      DeliveryView       `json:"delivery"`
	RyugiBindings []RyugiBindingView `json:"ryugiBindings"`
	InboxItem     InboxItemView      `json:"inboxItem"`
	Created       bool               `json:"created"`
	RyugiError    string             `json:"ryugiError,omitempty"`
}

// CompleteReviewResponse is returned by POST /api/deliveries/:deliveryId/complete-review.
type CompleteReviewResponse struct {
	DeliveryID string     `json:"deliveryId"`
	InboxID    string     `json:"inboxId"`
	InboxState InboxState `json:"inboxState"`
	ResolvedAt int64      `json:"resolvedAt"`
}

// CreateAnnotationRequest creates a review annotation anchored on a delivery.
type CreateAnnotationRequest struct {
	ProjectID string           `json:"projectId,omitempty"`
	TaskID    string           `json:"taskId,omitempty"`
	Source    AnnotationSource `json:"source,omitempty"`
	Anchor    AnnotationAnchor `json:"anchor"`
	Body      string           `json:"body"`
}

// CreateAnnotationResponse returns the created (or idempotently matched) annotation.
type CreateAnnotationResponse struct {
	Annotation AnnotationView `json:"annotation"`
	Created    bool           `json:"created"`
}

// RyugiSyncResponse summarizes a manual or scheduled Ryugi sync operation.
type RyugiSyncResponse struct {
	DeliveryID       string             `json:"deliveryId"`
	RyugiSyncState   RyugiSyncState     `json:"ryugiSyncState"`
	BindingsSynced   int                `json:"bindingsSynced"`
	NewAnnotations   int                `json:"newAnnotations"`
	ReceiptsAcked    int                `json:"receiptsAcked"`
	RyugiBindings    []RyugiBindingView `json:"ryugiBindings"`
	Errors           []string           `json:"errors,omitempty"`
}

// ReloadConfigResponse reports counts after reloading YAML registries.
type ReloadConfigResponse struct {
	ProjectsLoaded int `json:"projectsLoaded"`
	MachinesLoaded int `json:"machinesLoaded"`
}

// GCPauseRequest toggles the Hub maintenance GC pause lock.
type GCPauseRequest struct {
	Paused     bool   `json:"paused"`
	Reason     string `json:"reason,omitempty"`
	TTLSeconds int    `json:"ttlSeconds,omitempty"`
}

// GCPauseResponse reports current maintenance GC pause lock state.
type GCPauseResponse struct {
	Paused    bool   `json:"paused"`
	Reason    string `json:"reason,omitempty"`
	ExpiresAt *int64 `json:"expiresAt,omitempty"`
}

// WSServerMessage is the wire frame sent over /ws/client.
type WSServerMessage struct {
	Type          WSMessageType `json:"type"`
	Events        []EventRecord `json:"events,omitempty"`
	Event         *EventRecord  `json:"event,omitempty"`
	ReplayHeadSeq int64         `json:"replayHeadSeq,omitempty"`
	ServerTime    int64         `json:"serverTime,omitempty"`
}

// PendingInput represents a queued follow-up message or annotation batch waiting
// to be consumed by a task session (rendered as dashed bubbles at the tail of the chat stream).
type PendingInput struct {
	InputID             string             `json:"inputId"`
	TaskID              string             `json:"taskId"`
	ProjectID           string             `json:"projectId"`
	Kind                PendingInputKind   `json:"kind"`
	Source              PendingInputSource `json:"source"`
	Body                string             `json:"body"`
	Payload             map[string]any     `json:"payload"`
	Position            int                `json:"position"`
	State               PendingInputState  `json:"state"`
	CreatedAt           int64              `json:"createdAt"`
	UpdatedAt           int64              `json:"updatedAt"`
	DispatchedAttemptID string             `json:"dispatchedAttemptId,omitempty"`
}

// BoardMessage is one monotonic #seq message on a task's shared queue message board.
type BoardMessage struct {
	Seq           int64              `json:"seq"`
	MessageID     string             `json:"messageId"`
	TaskID        string             `json:"taskId"`
	ProjectID     string             `json:"projectId"`
	AuthorName    string             `json:"authorName"`
	TargetMention string             `json:"targetMention,omitempty"`
	Body          string             `json:"body"`
	Status        BoardMessageStatus `json:"status"`
	ExternalKey   string             `json:"externalKey,omitempty"`
	CreatedAt     int64              `json:"createdAt"`
}

// BoardMemberCursor tracks a native swarm or cross-machine worker on a task's board.
type BoardMemberCursor struct {
	TaskID          string           `json:"taskId"`
	ProjectID       string           `json:"projectId"`
	MemberName      string           `json:"memberName"`
	MachineID       string           `json:"machineId,omitempty"`
	Adapter         Adapter          `json:"adapter,omitempty"`
	State           BoardMemberState `json:"state"`
	LastConsumedSeq int64            `json:"lastConsumedSeq"`
	CurrentStep     string           `json:"currentStep,omitempty"`
	WorkerTaskID    string           `json:"workerTaskId,omitempty"`
	UpdatedAt       int64            `json:"updatedAt"`
}

// TaskBoardResponse is returned by GET /api/tasks/:taskId/board.
type TaskBoardResponse struct {
	TaskID    string              `json:"taskId"`
	LatestSeq int64               `json:"latestSeq"`
	Members   []BoardMemberCursor `json:"members"`
	Messages  []BoardMessage      `json:"messages"`
}

// WorkerStreamResponse is returned by GET /api/tasks/:taskId/workers/:workerName/stream.
type WorkerStreamResponse struct {
	TaskID     string             `json:"taskId"`
	WorkerName string             `json:"workerName"`
	Member     *BoardMemberCursor `json:"member,omitempty"`
	Events     []EventRecord      `json:"events"`
}

// TaskSource is a file, link, or nmem reference attached to a session's Sources card.
type TaskSource struct {
	SourceID  string         `json:"sourceId"`
	TaskID    string         `json:"taskId"`
	ProjectID string         `json:"projectId"`
	Kind      TaskSourceKind `json:"kind"`
	Label     string         `json:"label"`
	URIOrRef  string         `json:"uriOrRef"`
	CreatedAt int64          `json:"createdAt"`
}

// ApprovalOption describes one selectable choice on an approval request.
type ApprovalOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
}

// ApprovalView represents an approval record in the strict delivery/crash-window state machine.
type ApprovalView struct {
	ApprovalID       string              `json:"approvalId"`
	ProjectID        string              `json:"projectId"`
	TaskID           string              `json:"taskId"`
	AttemptID        string              `json:"attemptId"`
	Kind             ApprovalKind        `json:"kind"`
	ActionName       string              `json:"actionName"`
	TargetEnv        string              `json:"targetEnv"`
	InputSHA         string              `json:"inputSha"`
	Payload          map[string]any      `json:"payload"`
	PayloadSHA256    string              `json:"payloadSha256"`
	ACPRPCID         string              `json:"acpRpcId,omitempty"`
	RunnerInstanceID string              `json:"runnerInstanceId,omitempty"`
	Options          []ApprovalOption    `json:"options"`
	RequestedBy      ApprovalRequestedBy `json:"requestedBy"`
	State            ApprovalState       `json:"state"`
	DecisionID       string              `json:"decisionId,omitempty"`
	SelectedOptionID string              `json:"selectedOptionId,omitempty"`
	DecidedBy        *ApprovalDecidedBy  `json:"decidedBy,omitempty"`
	DecisionNote     string              `json:"decisionNote,omitempty"`
	ExpiresAt        int64               `json:"expiresAt"`
	CreatedAt        int64               `json:"createdAt"`
	DecidedAt        *int64              `json:"decidedAt,omitempty"`
	ConsumedAt       *int64              `json:"consumedAt,omitempty"`
}

// PAFocusPointer references a pinned project, task, or delivery tracked by the global PA.
type PAFocusPointer struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Label    string `json:"label"`
	PinnedAt int64  `json:"pinnedAt"`
}

// PAStateView exposes the global PA's attention pointers and event watermark (zero fact copies).
type PAStateView struct {
	PAID              string           `json:"paId"`
	ActivePAThreadID  string           `json:"activePaThreadId,omitempty"`
	ActiveWBSessionID string           `json:"activeWbSessionId,omitempty"`
	LastReadEventSeq  int64            `json:"lastReadEventSeq"`
	FocusPointers     []PAFocusPointer `json:"focusPointers"`
	UpdatedAt         int64            `json:"updatedAt"`
}

// PAThread represents one conversation thread in PA Space.
type PAThread struct {
	PAThreadID  string         `json:"paThreadId"`
	Title       string         `json:"title"`
	WBSessionID string         `json:"wbSessionId,omitempty"`
	Status      PAThreadStatus `json:"status"`
	CreatedAt   int64          `json:"createdAt"`
	UpdatedAt   int64          `json:"updatedAt"`
}

// PAActionResult summarizes one audited action executed during a PA turn.
type PAActionResult struct {
	Action  string         `json:"action"`
	Status  string         `json:"status"`
	Summary string         `json:"summary"`
	Data    map[string]any `json:"data,omitempty"`
}

// PAMessage is one message row inside a PAThread.
type PAMessage struct {
	MessageID    string           `json:"messageId"`
	PAThreadID   string           `json:"paThreadId"`
	Role         PAMessageRole    `json:"role"`
	Content      string           `json:"content"`
	DeltaFromSeq int64            `json:"deltaFromSeq,omitempty"`
	DeltaToSeq   int64            `json:"deltaToSeq,omitempty"`
	Actions      []PAActionResult `json:"actions,omitempty"`
	CreatedAt    int64            `json:"createdAt"`
}

// PABriefRequest filters a stateless Brief Me summary.
type PABriefRequest struct {
	SinceHours int    `json:"sinceHours,omitempty"`
	ProjectID  string `json:"projectId,omitempty"`
}

// PABriefResponse is returned by POST /api/pa/brief (<1s stateless event/inbox/task aggregation).
type PABriefResponse struct {
	GeneratedAt      int64           `json:"generatedAt"`
	SnapshotSeq      int64           `json:"snapshotSeq"`
	SinceHours       int             `json:"sinceHours"`
	ProjectID        string          `json:"projectId,omitempty"`
	Summary          string          `json:"summary"`
	OpenInboxCount   int             `json:"openInboxCount"`
	OpenInboxItems   []InboxItemView `json:"openInboxItems"`
	ActiveTasks      []TaskSummary   `json:"activeTasks"`
	BlockedTasks     []TaskSummary   `json:"blockedTasks"`
	RecentDeliveries []DeliveryView  `json:"recentDeliveries"`
	Machines         []MachineView   `json:"machines"`
	RecentEvents     []EventRecord   `json:"recentEvents"`
}

// CreateTaskSourceInput specifies a source to attach during task creation or POST /sources.
type CreateTaskSourceInput struct {
	Kind     TaskSourceKind `json:"kind"`
	Label    string         `json:"label"`
	URIOrRef string         `json:"uriOrRef"`
}

// CreateTaskRequest creates a new task under a real project or virtual _scratch project.
type CreateTaskRequest struct {
	Title                string                  `json:"title,omitempty"`
	Goal                 string                  `json:"goal"`
	ParentTaskID         string                  `json:"parentTaskId,omitempty"`
	RequiredCapability   string                  `json:"requiredCapability,omitempty"`
	RequiredResources    []string                `json:"requiredResources,omitempty"`
	Adapter              Adapter                 `json:"adapter,omitempty"`
	Config               map[string]string       `json:"config,omitempty"`
	ImportDirtyWorkspace bool                    `json:"importDirtyWorkspace,omitempty"`
	Sources              []CreateTaskSourceInput `json:"sources,omitempty"`
	IdempotencyKey       string                  `json:"idempotencyKey,omitempty"`
	SwarmIdleTimeoutMs   int64                   `json:"swarmIdleTimeoutMs,omitempty"`
}

// CreateTaskResponse returns the newly created (or idempotently matched) task and initial attempt.
type CreateTaskResponse struct {
	Task       TaskSummary  `json:"task"`
	Attempt    *AttemptView `json:"attempt,omitempty"`
	Sources    []TaskSource `json:"sources,omitempty"`
	Created    bool         `json:"created"`
	Dispatched bool         `json:"dispatched"`
}

// BindScratchTaskRequest migrates a _scratch free chat task to a real logical project.
type BindScratchTaskRequest struct {
	ProjectID string `json:"projectId"`
}

// BindScratchTaskResponse returns the rebound task summary.
type BindScratchTaskResponse struct {
	Task          TaskSummary `json:"task"`
	FromProjectID string      `json:"fromProjectId"`
	ToProjectID   string      `json:"toProjectId"`
}

// FollowupTaskRequest appends an instruction or switches capability/session for a task.
type FollowupTaskRequest struct {
	Message            string            `json:"message"`
	Mode               FollowupMode      `json:"mode,omitempty"`
	RequiredCapability string            `json:"requiredCapability,omitempty"`
	RequiredResources  []string          `json:"requiredResources,omitempty"`
	Adapter            Adapter           `json:"adapter,omitempty"`
	Config             map[string]string `json:"config,omitempty"`
}

// FollowupTaskResponse reports whether the follow-up was queued, steered, dispatched, or forked.
type FollowupTaskResponse struct {
	Mode         FollowupMode  `json:"mode"`
	Task         TaskSummary   `json:"task"`
	Attempt      *AttemptView  `json:"attempt,omitempty"`
	PendingInput *PendingInput `json:"pendingInput,omitempty"`
	ForkedTaskID string        `json:"forkedTaskId,omitempty"`
	Dispatched   bool          `json:"dispatched"`
}

// RetryTaskRequest retries a failed/expired/waiting_resource/needs_verification task.
type RetryTaskRequest struct {
	VerifiedSite bool   `json:"verifiedSite,omitempty"`
	Note         string `json:"note,omitempty"`
}

// RetryTaskResponse returns the newly created retry attempt.
type RetryTaskResponse struct {
	Task       TaskSummary `json:"task"`
	Attempt    AttemptView `json:"attempt"`
	Dispatched bool        `json:"dispatched"`
}

// CancelTaskRequest cancels an active attempt on a task.
type CancelTaskRequest struct {
	Reason string `json:"reason,omitempty"`
}

// CancelTaskResponse reports the cancellation dispatch state.
type CancelTaskResponse struct {
	TaskID         string `json:"taskId"`
	AttemptID      string `json:"attemptId,omitempty"`
	CancelSent     bool   `json:"cancelSent"`
	ImmediateState string `json:"immediateState,omitempty"`
}

// AttemptRawEventsResponse returns lazy-loaded raw_acp events for one attempt.
type AttemptRawEventsResponse struct {
	AttemptID string        `json:"attemptId"`
	Events    []EventRecord `json:"events"`
}

// CreatePendingInputRequest adds a queued input item directly to a task's queue.
type CreatePendingInputRequest struct {
	Kind    PendingInputKind   `json:"kind,omitempty"`
	Source  PendingInputSource `json:"source,omitempty"`
	Body    string             `json:"body"`
	Payload map[string]any     `json:"payload,omitempty"`
}

// UpdatePendingInputRequest edits the body/payload of a queued pending input.
type UpdatePendingInputRequest struct {
	Body    string         `json:"body"`
	Payload map[string]any `json:"payload,omitempty"`
}

// ReorderPendingInputsRequest specifies the desired ordering of queued input IDs.
type ReorderPendingInputsRequest struct {
	InputIDs []string `json:"inputIds"`
}

// DispatchAnnotationsRequest atomically moves open annotations to queued and runs or queues them.
type DispatchAnnotationsRequest struct {
	AnnotationIDs []string     `json:"annotationIds"`
	Mode          FollowupMode `json:"mode,omitempty"`
	Message       string       `json:"message,omitempty"`
}

// DispatchAnnotationsResponse reports the resulting attempt or queued pending input.
type DispatchAnnotationsResponse struct {
	TaskID         string        `json:"taskId"`
	QueuedCount    int           `json:"queuedCount"`
	Attempt        *AttemptView  `json:"attempt,omitempty"`
	PendingInput   *PendingInput `json:"pendingInput,omitempty"`
	ForkedTaskID   string        `json:"forkedTaskId,omitempty"`
	Dispatched     bool          `json:"dispatched"`
}

// PostBoardMessageRequest posts a message (optionally with @worker mention) to a task's board.
type PostBoardMessageRequest struct {
	AuthorName    string `json:"authorName,omitempty"`
	TargetMention string `json:"targetMention,omitempty"`
	Body          string `json:"body"`
}

// DecideApprovalRequest submits a human decision against a pending approval.
type DecideApprovalRequest struct {
	OptionID              string `json:"optionId"`
	ExpectedAttemptID     string `json:"expectedAttemptId"`
	ExpectedPayloadSHA256 string `json:"expectedPayloadSha256"`
	Note                  string `json:"note,omitempty"`
}

// DecideApprovalResponse returns the updated approval and whether delivery to runner started.
type DecideApprovalResponse struct {
	Approval      ApprovalView `json:"approval"`
	SentToRunner  bool         `json:"sentToRunner"`
}

// CreatePAThreadRequest starts a new PA conversation thread.
type CreatePAThreadRequest struct {
	Title string `json:"title,omitempty"`
}

// PAThreadDetailResponse returns one PA thread, its messages, and current PAState.
type PAThreadDetailResponse struct {
	Thread   PAThread    `json:"thread"`
	Messages []PAMessage `json:"messages"`
	PAState  PAStateView `json:"paState"`
}

// PAChatActionRequest optionally requests a structured deterministic PA control action.
type PAChatActionRequest struct {
	Action             string            `json:"action"` // brief | create_task | send_task_followup | cancel_task | list_projects_and_machines | read_task_timeline
	ProjectID          string            `json:"projectId,omitempty"`
	TaskID             string            `json:"taskId,omitempty"`
	Goal               string            `json:"goal,omitempty"`
	Message            string            `json:"message,omitempty"`
	Mode               FollowupMode      `json:"mode,omitempty"`
	RequiredCapability string            `json:"requiredCapability,omitempty"`
	Adapter            Adapter           `json:"adapter,omitempty"`
	Config             map[string]string `json:"config,omitempty"`
	IdempotencyKey     string            `json:"idempotencyKey,omitempty"`
}

// PAChatRequest sends one user turn to the global PA.
type PAChatRequest struct {
	ThreadID string               `json:"threadId,omitempty"`
	Message  string               `json:"message"`
	Action   *PAChatActionRequest `json:"action,omitempty"`
}

// PAChatResponse returns the PA turn response, updated event watermark, and any executed actions.
type PAChatResponse struct {
	Thread           PAThread         `json:"thread"`
	UserMessage      PAMessage        `json:"userMessage"`
	AssistantMessage PAMessage        `json:"assistantMessage"`
	DeltaFromSeq     int64            `json:"deltaFromSeq"`
	DeltaToSeq       int64            `json:"deltaToSeq"`
	DeltaEventCount  int              `json:"deltaEventCount"`
	FoldedRange      string           `json:"foldedRange,omitempty"`
	Actions          []PAActionResult `json:"actions,omitempty"`
	PAState          PAStateView      `json:"paState"`
}
