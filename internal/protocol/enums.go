package protocol

import (
	"slices"
	"strings"
)

// TransportMode defines how the Hub communicates with a machine.
type TransportMode string

const (
	TransportLocalWS     TransportMode = "local_ws"
	TransportRemoteWS    TransportMode = "remote_ws"
	TransportSSHOutbound TransportMode = "ssh_outbound"
)

var AllTransportModes = []TransportMode{
	TransportLocalWS,
	TransportRemoteWS,
	TransportSSHOutbound,
}

func (v TransportMode) IsValid() bool { return slices.Contains(AllTransportModes, v) }

// MachineStatus represents the live connectivity status of a machine.
type MachineStatus string

const (
	MachineOnline   MachineStatus = "online"
	MachineOffline  MachineStatus = "offline"
	MachineDegraded MachineStatus = "degraded"
)

var AllMachineStatuses = []MachineStatus{
	MachineOnline,
	MachineOffline,
	MachineDegraded,
}

func (v MachineStatus) IsValid() bool { return slices.Contains(AllMachineStatuses, v) }

// Adapter identifies an execution adapter for a task or attempt.
type Adapter string

const (
	AdapterAgy       Adapter = "agy"
	AdapterClaude    Adapter = "claude"
	AdapterCodex     Adapter = "codex"
	AdapterSSHDeploy Adapter = "ssh_deploy"
	AdapterManual    Adapter = "manual"
)

var AllAdapters = []Adapter{
	AdapterAgy,
	AdapterClaude,
	AdapterCodex,
	AdapterSSHDeploy,
	AdapterManual,
}

func (v Adapter) IsValid() bool { return slices.Contains(AllAdapters, v) }

// SessionAdapter identifies adapters that back persistent ACP agent sessions.
type SessionAdapter string

const (
	SessionAdapterAgy    SessionAdapter = "agy"
	SessionAdapterClaude SessionAdapter = "claude"
	SessionAdapterCodex  SessionAdapter = "codex"
)

var AllSessionAdapters = []SessionAdapter{
	SessionAdapterAgy,
	SessionAdapterClaude,
	SessionAdapterCodex,
}

func (v SessionAdapter) IsValid() bool { return slices.Contains(AllSessionAdapters, v) }

// TaskState represents the lifecycle state of a logical task.
type TaskState string

const (
	TaskStateQueued          TaskState = "queued"
	TaskStateWaitingResource TaskState = "waiting_resource"
	TaskStateRunning         TaskState = "running"
	TaskStateWaitingApproval TaskState = "waiting_approval"
	TaskStateInReview        TaskState = "in_review"
	TaskStateDone            TaskState = "done"
	TaskStateFailed          TaskState = "failed"
	TaskStateCancelled       TaskState = "cancelled"
)

var AllTaskStates = []TaskState{
	TaskStateQueued,
	TaskStateWaitingResource,
	TaskStateRunning,
	TaskStateWaitingApproval,
	TaskStateInReview,
	TaskStateDone,
	TaskStateFailed,
	TaskStateCancelled,
}

func (v TaskState) IsValid() bool { return slices.Contains(AllTaskStates, v) }

// TaskCreatedBy records who created a task.
type TaskCreatedBy string

const (
	TaskCreatedByHuman      TaskCreatedBy = "human"
	TaskCreatedByPA         TaskCreatedBy = "pa"
	TaskCreatedByAnnotation TaskCreatedBy = "annotation"
	TaskCreatedByTemplate   TaskCreatedBy = "template"
	TaskCreatedByCLI        TaskCreatedBy = "cli"
)

var AllTaskCreatedBy = []TaskCreatedBy{
	TaskCreatedByHuman,
	TaskCreatedByPA,
	TaskCreatedByAnnotation,
	TaskCreatedByTemplate,
	TaskCreatedByCLI,
}

func (v TaskCreatedBy) IsValid() bool { return slices.Contains(AllTaskCreatedBy, v) }

// SessionRuntimeState tracks an underlying ACP worker session lifecycle.
type SessionRuntimeState string

const (
	SessionStarting   SessionRuntimeState = "starting"
	SessionLive       SessionRuntimeState = "live"
	SessionClosing    SessionRuntimeState = "closing"
	SessionIdleClosed SessionRuntimeState = "idle_closed"
	SessionDead       SessionRuntimeState = "dead"
)

var AllSessionRuntimeStates = []SessionRuntimeState{
	SessionStarting,
	SessionLive,
	SessionClosing,
	SessionIdleClosed,
	SessionDead,
}

func (v SessionRuntimeState) IsValid() bool { return slices.Contains(AllSessionRuntimeStates, v) }

// TriggerKind classifies why an attempt was started.
type TriggerKind string

const (
	TriggerInitial         TriggerKind = "initial"
	TriggerFollowup        TriggerKind = "followup"
	TriggerAnnotationBatch TriggerKind = "annotation_batch"
	TriggerRetry           TriggerKind = "retry"
	TriggerDeploy          TriggerKind = "deploy"
	TriggerWorkspaceImport TriggerKind = "workspace_import"
	TriggerManualImport    TriggerKind = "manual_import"
)

var AllTriggerKinds = []TriggerKind{
	TriggerInitial,
	TriggerFollowup,
	TriggerAnnotationBatch,
	TriggerRetry,
	TriggerDeploy,
	TriggerWorkspaceImport,
	TriggerManualImport,
}

func (v TriggerKind) IsValid() bool { return slices.Contains(AllTriggerKinds, v) }

// DeployPhase tracks resumable SSH deployment phases.
type DeployPhase string

const (
	DeployPhaseAwaitingApproval DeployPhase = "awaiting_approval"
	DeployPhasePushing          DeployPhase = "pushing"
	DeployPhaseSwitching        DeployPhase = "switching"
	DeployPhaseVerifying        DeployPhase = "verifying"
	DeployPhaseDone             DeployPhase = "done"
)

var AllDeployPhases = []DeployPhase{
	DeployPhaseAwaitingApproval,
	DeployPhasePushing,
	DeployPhaseSwitching,
	DeployPhaseVerifying,
	DeployPhaseDone,
}

func (v DeployPhase) IsValid() bool { return slices.Contains(AllDeployPhases, v) }

// AttemptState represents the single-source attempt state machine.
type AttemptState string

const (
	AttemptLeased            AttemptState = "leased"
	AttemptPreparing         AttemptState = "preparing"
	AttemptWaitingResource   AttemptState = "waiting_resource"
	AttemptRunning           AttemptState = "running"
	AttemptSwarmMonitoring   AttemptState = "swarm_monitoring"
	AttemptWaitingApproval   AttemptState = "waiting_approval"
	AttemptSucceeded         AttemptState = "succeeded"
	AttemptNeedsVerification AttemptState = "needs_verification"
	AttemptFailed            AttemptState = "failed"
	AttemptExpired           AttemptState = "expired"
	AttemptCancelled         AttemptState = "cancelled"
)

var AllAttemptStates = []AttemptState{
	AttemptLeased,
	AttemptPreparing,
	AttemptWaitingResource,
	AttemptRunning,
	AttemptSwarmMonitoring,
	AttemptWaitingApproval,
	AttemptSucceeded,
	AttemptNeedsVerification,
	AttemptFailed,
	AttemptExpired,
	AttemptCancelled,
}

// ActiveAttemptStates are states where an attempt holds exclusivity on its task & session.
var ActiveAttemptStates = []AttemptState{
	AttemptLeased,
	AttemptPreparing,
	AttemptWaitingResource,
	AttemptRunning,
	AttemptSwarmMonitoring,
	AttemptWaitingApproval,
}

// ActiveDeployAttemptStates are states where an ssh_deploy attempt holds exclusivity on target_env.
var ActiveDeployAttemptStates = []AttemptState{
	AttemptLeased,
	AttemptPreparing,
	AttemptRunning,
}

func (v AttemptState) IsValid() bool { return slices.Contains(AllAttemptStates, v) }

// ResumeState remembers which state to return to after pending approvals resolve.
type ResumeState string

const (
	ResumeStateRunning         ResumeState = "running"
	ResumeStateSwarmMonitoring ResumeState = "swarm_monitoring"
)

var AllResumeStates = []ResumeState{
	ResumeStateRunning,
	ResumeStateSwarmMonitoring,
}

func (v ResumeState) IsValid() bool { return slices.Contains(AllResumeStates, v) }

// CompletionSource records how an attempt concluded.
type CompletionSource string

const (
	CompletionExplicitDeliver CompletionSource = "explicit_deliver"
	CompletionPromptEndTurn   CompletionSource = "prompt_end_turn"
	CompletionQuietTimeout    CompletionSource = "quiet_timeout"
	CompletionSSHVerified     CompletionSource = "ssh_verified"
	CompletionManualImport    CompletionSource = "manual_import"
)

var AllCompletionSources = []CompletionSource{
	CompletionExplicitDeliver,
	CompletionPromptEndTurn,
	CompletionQuietTimeout,
	CompletionSSHVerified,
	CompletionManualImport,
}

func (v CompletionSource) IsValid() bool { return slices.Contains(AllCompletionSources, v) }

// PendingInputKind classifies queued task inputs.
type PendingInputKind string

const (
	PendingInputFollowup        PendingInputKind = "followup"
	PendingInputAnnotationBatch PendingInputKind = "annotation_batch"
)

var AllPendingInputKinds = []PendingInputKind{
	PendingInputFollowup,
	PendingInputAnnotationBatch,
}

func (v PendingInputKind) IsValid() bool { return slices.Contains(AllPendingInputKinds, v) }

// PendingInputState tracks lifecycle of queued task inputs.
type PendingInputState string

const (
	PendingInputQueued     PendingInputState = "queued"
	PendingInputDispatched PendingInputState = "dispatched"
	PendingInputCancelled  PendingInputState = "cancelled"
)

var AllPendingInputStates = []PendingInputState{
	PendingInputQueued,
	PendingInputDispatched,
	PendingInputCancelled,
}

func (v PendingInputState) IsValid() bool { return slices.Contains(AllPendingInputStates, v) }

// DeliveryKind classifies immutable deliveries.
type DeliveryKind string

const (
	DeliveryCodeDiff          DeliveryKind = "code_diff"
	DeliveryRyugiArtifact     DeliveryKind = "ryugi_artifact"
	DeliveryMixed             DeliveryKind = "mixed"
	DeliveryDeployReceipt     DeliveryKind = "deploy_receipt"
	DeliveryWorkspaceSnapshot DeliveryKind = "workspace_snapshot"
)

var AllDeliveryKinds = []DeliveryKind{
	DeliveryCodeDiff,
	DeliveryRyugiArtifact,
	DeliveryMixed,
	DeliveryDeployReceipt,
	DeliveryWorkspaceSnapshot,
}

func (v DeliveryKind) IsValid() bool { return slices.Contains(AllDeliveryKinds, v) }

// RyugiSyncState tracks independent Ryugi frozen snapshot export & registration status.
type RyugiSyncState string

const (
	RyugiSyncNone    RyugiSyncState = "none"
	RyugiSyncPending RyugiSyncState = "pending"
	RyugiSyncReady   RyugiSyncState = "ready"
	RyugiSyncFailed  RyugiSyncState = "failed"
)

var AllRyugiSyncStates = []RyugiSyncState{
	RyugiSyncNone,
	RyugiSyncPending,
	RyugiSyncReady,
	RyugiSyncFailed,
}

func (v RyugiSyncState) IsValid() bool { return slices.Contains(AllRyugiSyncStates, v) }

// AnnotationSource identifies where an annotation originated.
type AnnotationSource string

const (
	AnnotationSourceRyugi       AnnotationSource = "ryugi"
	AnnotationSourceDiffDrawer  AnnotationSource = "diff_drawer"
	AnnotationSourceMobileInbox AnnotationSource = "mobile_inbox"
)

var AllAnnotationSources = []AnnotationSource{
	AnnotationSourceRyugi,
	AnnotationSourceDiffDrawer,
	AnnotationSourceMobileInbox,
}

func (v AnnotationSource) IsValid() bool { return slices.Contains(AllAnnotationSources, v) }

// AnnotationStatus tracks the lifecycle of an annotation.
type AnnotationStatus string

const (
	AnnotationOpen      AnnotationStatus = "open"
	AnnotationQueued    AnnotationStatus = "queued"
	AnnotationAddressed AnnotationStatus = "addressed"
	AnnotationWontfix   AnnotationStatus = "wontfix"
	AnnotationStale     AnnotationStatus = "stale"
)

var AllAnnotationStatuses = []AnnotationStatus{
	AnnotationOpen,
	AnnotationQueued,
	AnnotationAddressed,
	AnnotationWontfix,
	AnnotationStale,
}

func (v AnnotationStatus) IsValid() bool { return slices.Contains(AllAnnotationStatuses, v) }

// ApprovalKind classifies approval requests.
type ApprovalKind string

const (
	ApprovalACPToolPermission ApprovalKind = "acp_tool_permission"
	ApprovalHetzDeploy        ApprovalKind = "hetz_deploy"
	ApprovalExternalAction    ApprovalKind = "external_action"
)

var AllApprovalKinds = []ApprovalKind{
	ApprovalACPToolPermission,
	ApprovalHetzDeploy,
	ApprovalExternalAction,
}

func (v ApprovalKind) IsValid() bool { return slices.Contains(AllApprovalKinds, v) }

// ApprovalRequestedBy identifies the requester of an approval.
type ApprovalRequestedBy string

const (
	ApprovalRequestedByAgent ApprovalRequestedBy = "agent"
	ApprovalRequestedByPA    ApprovalRequestedBy = "pa"
	ApprovalRequestedByHuman ApprovalRequestedBy = "human"
)

var AllApprovalRequestedBy = []ApprovalRequestedBy{
	ApprovalRequestedByAgent,
	ApprovalRequestedByPA,
	ApprovalRequestedByHuman,
}

func (v ApprovalRequestedBy) IsValid() bool { return slices.Contains(AllApprovalRequestedBy, v) }

// ApprovalState tracks the strict approval delivery & crash-window state machine.
type ApprovalState string

const (
	ApprovalPending                 ApprovalState = "pending"
	ApprovalApprovedPendingDelivery ApprovalState = "approved_pending_delivery"
	ApprovalRejectedPendingDelivery ApprovalState = "rejected_pending_delivery"
	ApprovalConsumedAllowed         ApprovalState = "consumed_allowed"
	ApprovalConsumedRejected        ApprovalState = "consumed_rejected"
	ApprovalUncertain               ApprovalState = "uncertain"
	ApprovalExpired                 ApprovalState = "expired"
)

var AllApprovalStates = []ApprovalState{
	ApprovalPending,
	ApprovalApprovedPendingDelivery,
	ApprovalRejectedPendingDelivery,
	ApprovalConsumedAllowed,
	ApprovalConsumedRejected,
	ApprovalUncertain,
	ApprovalExpired,
}

func (v ApprovalState) IsValid() bool { return slices.Contains(AllApprovalStates, v) }

// ApprovalDecidedBy restricts who may decide approvals ('pa' is explicitly disallowed).
type ApprovalDecidedBy string

const (
	ApprovalDecidedByHuman  ApprovalDecidedBy = "human"
	ApprovalDecidedByPolicy ApprovalDecidedBy = "policy"
)

var AllApprovalDecidedBy = []ApprovalDecidedBy{
	ApprovalDecidedByHuman,
	ApprovalDecidedByPolicy,
}

func (v ApprovalDecidedBy) IsValid() bool { return slices.Contains(AllApprovalDecidedBy, v) }

// ActorKind identifies the actor recorded on unified event log rows.
type ActorKind string

const (
	ActorHuman  ActorKind = "human"
	ActorAgent  ActorKind = "agent"
	ActorRunner ActorKind = "runner"
	ActorPA     ActorKind = "pa"
	ActorRyugi  ActorKind = "ryugi"
	ActorSystem ActorKind = "system"
)

var AllActorKinds = []ActorKind{
	ActorHuman,
	ActorAgent,
	ActorRunner,
	ActorPA,
	ActorRyugi,
	ActorSystem,
}

func (v ActorKind) IsValid() bool { return slices.Contains(AllActorKinds, v) }

// EventVisibility separates high-signal events from high-frequency raw ACP telemetry.
type EventVisibility string

const (
	VisibilityTimeline EventVisibility = "timeline"
	VisibilityInbox    EventVisibility = "inbox"
	VisibilityAudit    EventVisibility = "audit"
	VisibilityRawACP   EventVisibility = "raw_acp"
)

var AllEventVisibilities = []EventVisibility{
	VisibilityTimeline,
	VisibilityInbox,
	VisibilityAudit,
	VisibilityRawACP,
}

func (v EventVisibility) IsValid() bool { return slices.Contains(AllEventVisibilities, v) }

// InboxCategory enforces strict low-noise admission into inbox_items.
type InboxCategory string

const (
	InboxApprovalPending InboxCategory = "approval_pending"
	InboxDeliveryReview  InboxCategory = "delivery_review"
	InboxTaskBlocked     InboxCategory = "task_blocked"
	InboxMemoryConflict  InboxCategory = "memory_conflict"
)

var AllInboxCategories = []InboxCategory{
	InboxApprovalPending,
	InboxDeliveryReview,
	InboxTaskBlocked,
	InboxMemoryConflict,
}

func (v InboxCategory) IsValid() bool { return slices.Contains(AllInboxCategories, v) }

// InboxState tracks open/resolved/dismissed inbox items.
type InboxState string

const (
	InboxOpen      InboxState = "open"
	InboxResolved  InboxState = "resolved"
	InboxDismissed InboxState = "dismissed"
)

var AllInboxStates = []InboxState{
	InboxOpen,
	InboxResolved,
	InboxDismissed,
}

func (v InboxState) IsValid() bool { return slices.Contains(AllInboxStates, v) }

// EventKind enumerates canonical event types stored in events.kind.
type EventKind string

const (
	EventTaskCreated          EventKind = "task.created"
	EventTaskStateChanged     EventKind = "task.state_changed"
	EventTaskCancelled        EventKind = "task.cancelled"
	EventAttemptStarted       EventKind = "attempt.started"
	EventAttemptStateChanged  EventKind = "attempt.state_changed"
	EventAttemptFinished      EventKind = "attempt.finished"
	EventResourceWaiting      EventKind = "resource.waiting"
	EventResourceAcquired     EventKind = "resource.acquired"
	EventDeliveryPublished       EventKind = "delivery.published"
	EventDeliveryVerified        EventKind = "delivery.verified"
	EventDeliveryRyugiUpdated    EventKind = "delivery.ryugi_updated"
	EventDeliveryReviewCompleted EventKind = "delivery.review_completed"
	EventAnnotationCreated       EventKind = "annotation.created"
	EventAnnotationAddressed  EventKind = "annotation.addressed"
	EventApprovalRequested    EventKind = "approval.requested"
	EventApprovalDecided      EventKind = "approval.decided"
	EventApprovalExpired      EventKind = "approval.expired"
	EventApprovalConsumed     EventKind = "approval.consumed"
	EventDeployStarted        EventKind = "deploy.started"
	EventDeployVerified       EventKind = "deploy.verified"
	EventDeployFailed         EventKind = "deploy.failed"
	EventACPMessageBatch      EventKind = "acp.message_batch"
	EventACPThoughtBatch      EventKind = "acp.thought_batch"
	EventACPToolCall          EventKind = "acp.tool_call"
	EventACPUnsolicitedUpdate EventKind = "acp.unsolicited_update"
	EventACPUsage             EventKind = "acp.usage"
	EventPATurn               EventKind = "pa.turn"
	EventPAActionExecuted     EventKind = "pa.action_executed"
)

var AllEventKinds = []EventKind{
	EventTaskCreated,
	EventTaskStateChanged,
	EventTaskCancelled,
	EventAttemptStarted,
	EventAttemptStateChanged,
	EventAttemptFinished,
	EventResourceWaiting,
	EventResourceAcquired,
	EventDeliveryPublished,
	EventDeliveryVerified,
	EventDeliveryRyugiUpdated,
	EventDeliveryReviewCompleted,
	EventAnnotationCreated,
	EventAnnotationAddressed,
	EventApprovalRequested,
	EventApprovalDecided,
	EventApprovalExpired,
	EventApprovalConsumed,
	EventDeployStarted,
	EventDeployVerified,
	EventDeployFailed,
	EventACPMessageBatch,
	EventACPThoughtBatch,
	EventACPToolCall,
	EventACPUnsolicitedUpdate,
	EventACPUsage,
	EventPATurn,
	EventPAActionExecuted,
}

// IDEScheme defines supported URI schemes for one-click IDE jump.
type IDEScheme string

const (
	IDECursor      IDEScheme = "cursor"
	IDEVSCode      IDEScheme = "vscode"
	IDEZed         IDEScheme = "zed"
	IDEAntigravity IDEScheme = "antigravity"
)

var AllIDESchemes = []IDEScheme{
	IDECursor,
	IDEVSCode,
	IDEZed,
	IDEAntigravity,
}

func (v IDEScheme) IsValid() bool { return slices.Contains(AllIDESchemes, v) }

// WSMessageType identifies server-to-client WebSocket frame kinds.
type WSMessageType string

const (
	WSMessageReplay    WSMessageType = "replay"
	WSMessageReplayEnd WSMessageType = "replay_end"
	WSMessageEvent     WSMessageType = "event"
	WSMessagePing      WSMessageType = "ping"
)

var AllWSMessageTypes = []WSMessageType{
	WSMessageReplay,
	WSMessageReplayEnd,
	WSMessageEvent,
	WSMessagePing,
}

// SQLInClause formats a typed string enum slice as a SQL IN list: 'a', 'b', 'c'.
func SQLInClause[T ~string](values []T) string {
	parts := make([]string, len(values))
	for i, v := range values {
		escaped := strings.ReplaceAll(string(v), "'", "''")
		parts[i] = "'" + escaped + "'"
	}
	return strings.Join(parts, ", ")
}
