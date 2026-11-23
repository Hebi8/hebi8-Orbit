package protocol

import "reflect"

// EnumDef describes a string enum exported to TypeScript generation.
type EnumDef struct {
	Name   string
	Values []string
}

func toStrings[T ~string](vals []T) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = string(v)
	}
	return out
}

// ExportedEnums returns the canonical ordered list of all protocol enums.
func ExportedEnums() []EnumDef {
	return []EnumDef{
		{Name: "TransportMode", Values: toStrings(AllTransportModes)},
		{Name: "MachineStatus", Values: toStrings(AllMachineStatuses)},
		{Name: "Adapter", Values: toStrings(AllAdapters)},
		{Name: "SessionAdapter", Values: toStrings(AllSessionAdapters)},
		{Name: "TaskState", Values: toStrings(AllTaskStates)},
		{Name: "TaskCreatedBy", Values: toStrings(AllTaskCreatedBy)},
		{Name: "SessionRuntimeState", Values: toStrings(AllSessionRuntimeStates)},
		{Name: "TriggerKind", Values: toStrings(AllTriggerKinds)},
		{Name: "DeployPhase", Values: toStrings(AllDeployPhases)},
		{Name: "AttemptState", Values: toStrings(AllAttemptStates)},
		{Name: "ResumeState", Values: toStrings(AllResumeStates)},
		{Name: "CompletionSource", Values: toStrings(AllCompletionSources)},
		{Name: "PendingInputKind", Values: toStrings(AllPendingInputKinds)},
		{Name: "PendingInputState", Values: toStrings(AllPendingInputStates)},
		{Name: "DeliveryKind", Values: toStrings(AllDeliveryKinds)},
		{Name: "RyugiSyncState", Values: toStrings(AllRyugiSyncStates)},
		{Name: "AnnotationSource", Values: toStrings(AllAnnotationSources)},
		{Name: "AnnotationStatus", Values: toStrings(AllAnnotationStatuses)},
		{Name: "ApprovalKind", Values: toStrings(AllApprovalKinds)},
		{Name: "ApprovalRequestedBy", Values: toStrings(AllApprovalRequestedBy)},
		{Name: "ApprovalState", Values: toStrings(AllApprovalStates)},
		{Name: "ApprovalDecidedBy", Values: toStrings(AllApprovalDecidedBy)},
		{Name: "ActorKind", Values: toStrings(AllActorKinds)},
		{Name: "EventVisibility", Values: toStrings(AllEventVisibilities)},
		{Name: "InboxCategory", Values: toStrings(AllInboxCategories)},
		{Name: "InboxState", Values: toStrings(AllInboxStates)},
		{Name: "EventKind", Values: toStrings(AllEventKinds)},
		{Name: "IDEScheme", Values: toStrings(AllIDESchemes)},
		{Name: "WSMessageType", Values: toStrings(AllWSMessageTypes)},
	}
}

// ExportedDTOTypes returns the canonical ordered list of Go struct types exported as TS interfaces.
func ExportedDTOTypes() []reflect.Type {
	return []reflect.Type{
		reflect.TypeOf(APIError{}),
		reflect.TypeOf(ResourceState{}),
		reflect.TypeOf(DeployStrategyConfig{}),
		reflect.TypeOf(ProjectAnchorConfig{}),
		reflect.TypeOf(ProjectView{}),
		reflect.TypeOf(MachineView{}),
		reflect.TypeOf(InboxItemView{}),
		reflect.TypeOf(BootstrapResponse{}),
		reflect.TypeOf(BlobManifestItem{}),
		reflect.TypeOf(DeliveryManifest{}),
		reflect.TypeOf(RyugiBindingView{}),
		reflect.TypeOf(AnnotationAnchor{}),
		reflect.TypeOf(AnnotationView{}),
		reflect.TypeOf(DeliveryView{}),
		reflect.TypeOf(AttemptView{}),
		reflect.TypeOf(TaskSummary{}),
		reflect.TypeOf(EventRecord{}),
		reflect.TypeOf(TaskTimelineResponse{}),
		reflect.TypeOf(DiffFileStat{}),
		reflect.TypeOf(DeliveryDetailResponse{}),
		reflect.TypeOf(DeliveryDiffResponse{}),
		reflect.TypeOf(ManualImportRequest{}),
		reflect.TypeOf(ManualImportResponse{}),
		reflect.TypeOf(CreateAnnotationRequest{}),
		reflect.TypeOf(CreateAnnotationResponse{}),
		reflect.TypeOf(RyugiSyncResponse{}),
		reflect.TypeOf(ReloadConfigResponse{}),
		reflect.TypeOf(GCPauseRequest{}),
		reflect.TypeOf(GCPauseResponse{}),
		reflect.TypeOf(WSServerMessage{}),
	}
}
