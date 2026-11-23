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
	Task        TaskSummary      `json:"task"`
	Attempts    []AttemptView    `json:"attempts"`
	Deliveries  []DeliveryView   `json:"deliveries"`
	Annotations []AnnotationView `json:"annotations"`
	Events      []EventRecord    `json:"events"`
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
	RyugiError    string             `json:"ryugiError,omitempty"`
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
