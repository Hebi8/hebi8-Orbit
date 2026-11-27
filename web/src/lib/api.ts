import type {
  ApprovalState,
  ApprovalView,
  ArchiveTaskResponse,
  AttemptRawEventsResponse,
  BindScratchTaskResponse,
  BoardMessage,
  BootstrapResponse,
  CancelTaskResponse,
  ClaimAuthHandoffResponse,
  ClientDeviceView,
  CompleteOnboardingRequest,
  CompleteOnboardingResponse,
  CompleteReviewResponse,
  CreateAnnotationRequest,
  CreateAnnotationResponse,
  CreateChildTaskRequest,
  CreatePairingCodeRequest,
  CreatePairingCodeResponse,
  CreateProjectLocationRequest,
  CreateProjectPartRequest,
  CreateProjectRequest,
  CreateProjectResponse,
  CreateTaskRequest,
  CreateTaskResponse,
  CreateTaskSourceInput,
  DecideApprovalRequest,
  DecideApprovalResponse,
  DeleteAnnotationResponse,
  DeleteTaskResponse,
  DeliveryDetailResponse,
  DeliveryDiffResponse,
  DismissSuggestionRequest,
  DispatchAnnotationsRequest,
  DispatchAnnotationsResponse,
  FollowupTaskRequest,
  FollowupTaskResponse,
  GlobalArtifactView,
  Hebi8ExpandResponse,
  Hebi8FoldRequest,
  Hebi8FoldResponse,
  Hebi8LineResponse,
  Hebi8MainResponse,
  Hebi8PinResponse,
  ListArtifactsResponse,
  ListChildrenResponse,
  ListClientsResponse,
  ListDirsResponse,
  ListMachinesResponse,
  LogoutClientResponse,
  MachineAgentActionResponse,
  MachineQuotasResponse,
  MachineView,
  NetworkDiscoverResponse,
  NetworkSettingsView,
  PABriefRequest,
  PABriefResponse,
  PAChatRequest,
  PAChatResponse,
  PAStateView,
  PAThread,
  PAThreadDetailResponse,
  PairClientResponse,
  PairingCodeStatusResponse,
  PendingInput,
  PostBoardMessageRequest,
  ProjectLocationView,
  ProjectPartView,
  ProjectSuggestionsResponse,
  ProjectView,
  PromoteFolderChangesResponse,
  RetryTaskRequest,
  RetryTaskResponse,
  RevokeClientResponse,
  RyugiArtifactContentResponse,
  RyugiOpenResponse,
  RyugiSyncResponse,
  SSHCandidatesResponse,
  TailscaleDisableResponse,
  TailscaleEnableRequest,
  TailscaleEnableResponse,
  TaskBoardResponse,
  TaskSource,
  TaskSummary,
  TaskTimelineResponse,
  UpdateMachineAgentRequest,
  UpdateMachineRequest,
  UpdateNetworkSettingsRequest,
  UpdateProjectLocationRequest,
  UpdateProjectPartRequest,
  UpdateProjectSettingsRequest,
  WSTicketResponse,
  WorkerStreamResponse,
} from '../generated/protocol';

const DEV_TOKEN_KEY = 'orbit.devToken';
const CLIENT_TOKEN_KEY = 'orbit.clientToken';
const DESKTOP_FLAG_KEY = 'orbit.desktopShell';
const DESKTOP_HELPER_PORT_KEY = 'orbit.desktopHelperPort';

export function detectAndRememberDesktopShell(): boolean {
  try {
    const sp = new URLSearchParams(window.location.search);
    let dirty = false;
    const isDesktopParam = sp.get('orbitDesktop') === '1';
    if (isDesktopParam) {
      sessionStorage.setItem(DESKTOP_FLAG_KEY, '1');
      sp.delete('orbitDesktop');
      dirty = true;
    }
    const rawHelperPort = sp.get('desktopHelper');
    if (rawHelperPort !== null) {
      const port = Number.parseInt(rawHelperPort, 10);
      if (Number.isInteger(port) && port > 0 && port <= 65535) {
        sessionStorage.setItem(DESKTOP_HELPER_PORT_KEY, String(port));
      }
      sp.delete('desktopHelper');
      dirty = true;
    }
    if (dirty) {
      // Strip query params cleanly without reloading
      const rest = sp.toString();
      const nextURL = window.location.pathname + (rest ? `?${rest}` : '') + window.location.hash;
      window.history.replaceState(null, '', nextURL);
    }
    return isDesktopParam || sessionStorage.getItem(DESKTOP_FLAG_KEY) === '1';
  } catch {
    return false;
  }
}

export function getDesktopHelperPort(): number | null {
  try {
    const raw = sessionStorage.getItem(DESKTOP_HELPER_PORT_KEY);
    if (!raw) return null;
    const port = Number.parseInt(raw, 10);
    return Number.isInteger(port) && port > 0 && port <= 65535 ? port : null;
  } catch {
    return null;
  }
}

export function isDesktopShell(): boolean {
  try {
    return sessionStorage.getItem(DESKTOP_FLAG_KEY) === '1';
  } catch {
    return false;
  }
}

export function openDesktopSetup(target?: { origin?: string; hostName?: string; discoveryId?: string }) {
  const q = new URLSearchParams();
  if (target?.origin) q.set('origin', target.origin);
  if (target?.hostName) q.set('host', target.hostName);
  if (target?.discoveryId) q.set('discoveryId', target.discoveryId);
  const qs = q.toString();
  window.location.href = qs ? `orbit-desktop://setup?${qs}` : 'orbit-desktop://setup';
}

export function getStoredDevToken(): string {
  try {
    return localStorage.getItem(DEV_TOKEN_KEY) ?? '';
  } catch {
    return '';
  }
}

export function getStoredClientToken(): string {
  try {
    return localStorage.getItem(CLIENT_TOKEN_KEY) ?? '';
  } catch {
    return '';
  }
}

export function setStoredDevToken(token: string) {
  try {
    const trimmed = token.trim();
    if (trimmed) localStorage.setItem(DEV_TOKEN_KEY, trimmed);
    else localStorage.removeItem(DEV_TOKEN_KEY);
  } catch {
    // ignore storage errors
  }
}

export function setStoredClientToken(token: string) {
  try {
    const trimmed = token.trim();
    if (trimmed) localStorage.setItem(CLIENT_TOKEN_KEY, trimmed);
    else localStorage.removeItem(CLIENT_TOKEN_KEY);
  } catch {
    // ignore storage errors
  }
}

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

type FetchOptions = Omit<RequestInit, 'body'> & {
  idempotencyKey?: string;
  json?: unknown;
};

async function apiFetch<T>(path: string, options: FetchOptions = {}): Promise<T> {
  const { idempotencyKey, json, ...init } = options;
  const headers = new Headers(init.headers);
  const method = (init.method ?? 'GET').toUpperCase();
  const isWrite = method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE';

  let body: BodyInit | undefined;
  if (json !== undefined) {
    body = JSON.stringify(json);
    headers.set('Content-Type', 'application/json');
  }
  if (isWrite) {
    // CSRF discipline: every state-changing request carries the custom header.
    headers.set('X-Orbit-Request', '1');
  }
  if (idempotencyKey) {
    headers.set('Idempotency-Key', idempotencyKey);
  }
  const clientToken = getStoredClientToken();
  const devToken = getStoredDevToken();
  if (clientToken) {
    headers.set('Authorization', `Bearer ${clientToken}`);
  } else if (devToken) {
    headers.set('X-Orbit-Dev-Token', devToken);
  }

  const resp = await fetch(path, {
    ...init,
    method,
    body,
    headers,
    credentials: 'omit', // Cookie-less auth discipline (RFC 6265 §8.5)
  });

  if (!resp.ok) {
    let message = `HTTP ${resp.status}`;
    let code = '';
    try {
      const errBody = (await resp.json()) as { code?: string; message?: string };
      code = errBody.code ?? '';
      if (errBody.message) {
        message = code ? `${code}: ${errBody.message}` : errBody.message;
      }
    } catch {
      // ignore non-json error bodies
    }
    throw new ApiError(resp.status, code, message);
  }

  return (await resp.json()) as T;
}

const enc = encodeURIComponent;

export const api = {
  createWSTicket(): Promise<WSTicketResponse> {
    return apiFetch<WSTicketResponse>('/api/ws-ticket', { method: 'POST', json: {} });
  },

  bootstrap(): Promise<BootstrapResponse> {
    return apiFetch<BootstrapResponse>('/api/bootstrap');
  },

  async listAllTasks(opts?: {
    limit?: number;
    excludeProject?: string;
    projectId?: string;
    q?: string;
    archived?: 'active' | 'only' | 'all';
    sort?: 'archived_at' | 'updated_at' | 'title';
    topLevelOnly?: boolean;
  }): Promise<TaskSummary[]> {
    const res = await api.listTasksWithMeta(opts);
    return res.tasks ?? [];
  },

  async listTasksWithMeta(opts?: {
    limit?: number;
    excludeProject?: string;
    projectId?: string;
    q?: string;
    archived?: 'active' | 'only' | 'all';
    sort?: 'archived_at' | 'updated_at' | 'title';
    topLevelOnly?: boolean;
  }): Promise<{ tasks: TaskSummary[]; projectCounts?: Record<string, number>; totalCount?: number }> {
    const q = new URLSearchParams();
    if (opts?.limit) q.set('limit', String(opts.limit));
    if (opts?.excludeProject) q.set('excludeProject', opts.excludeProject);
    if (opts?.projectId) q.set('projectId', opts.projectId);
    if (opts?.q && opts.q.trim()) q.set('q', opts.q.trim());
    if (opts?.archived && opts.archived !== 'active') q.set('archived', opts.archived);
    if (opts?.sort) q.set('sort', opts.sort);
    if (opts?.topLevelOnly) q.set('topLevelOnly', '1');
    const qs = q.toString();
    const res = await apiFetch<{
      tasks?: TaskSummary[];
      projectCounts?: Record<string, number>;
      totalCount?: number;
    }>(`/api/tasks${qs ? `?${qs}` : ''}`);
    return {
      tasks: res.tasks ?? [],
      projectCounts: res.projectCounts,
      totalCount: res.totalCount,
    };
  },

  async listProjectTasks(projectId: string, archived?: 'active' | 'only' | 'all'): Promise<TaskSummary[]> {
    const qs = archived && archived !== 'active' ? `?archived=${enc(archived)}` : '';
    const res = await apiFetch<{ tasks: TaskSummary[] }>(`/api/projects/${enc(projectId)}/tasks${qs}`);
    return res.tasks ?? [];
  },

  archiveTask(taskId: string, cancelActive = false): Promise<ArchiveTaskResponse> {
    return apiFetch<ArchiveTaskResponse>(`/api/tasks/${enc(taskId)}/archive`, {
      method: 'POST',
      json: cancelActive ? { cancelActive: true } : {},
    });
  },

  unarchiveTask(taskId: string): Promise<ArchiveTaskResponse> {
    return apiFetch<ArchiveTaskResponse>(`/api/tasks/${enc(taskId)}/unarchive`, {
      method: 'POST',
      json: {},
    });
  },

  deleteTask(taskId: string): Promise<DeleteTaskResponse> {
    return apiFetch<DeleteTaskResponse>(`/api/tasks/${enc(taskId)}`, {
      method: 'DELETE',
    });
  },

  createTask(projectId: string, req: CreateTaskRequest): Promise<CreateTaskResponse> {
    const key = req.idempotencyKey || makeIdempotencyKey();
    return apiFetch<CreateTaskResponse>(`/api/projects/${enc(projectId)}/tasks`, {
      method: 'POST',
      json: { ...req, idempotencyKey: key },
      idempotencyKey: key,
    });
  },

  bindProject(taskId: string, projectId: string): Promise<BindScratchTaskResponse> {
    return apiFetch<BindScratchTaskResponse>(`/api/tasks/${enc(taskId)}/bind-project`, {
      method: 'POST',
      json: { projectId },
    });
  },

  getTaskTimeline(taskId: string): Promise<TaskTimelineResponse> {
    return apiFetch<TaskTimelineResponse>(`/api/tasks/${enc(taskId)}/timeline?includeRaw=0`);
  },

  listChildren(taskId: string, includeArchived = false): Promise<ListChildrenResponse> {
    const qs = includeArchived ? '?includeArchived=1' : '';
    return apiFetch<ListChildrenResponse>(`/api/tasks/${enc(taskId)}/children${qs}`);
  },

  spawnChild(taskId: string, req: CreateChildTaskRequest): Promise<CreateTaskResponse> {
    const key = req.idempotencyKey || makeIdempotencyKey();
    return apiFetch<CreateTaskResponse>(`/api/tasks/${enc(taskId)}/children`, {
      method: 'POST',
      json: { ...req, idempotencyKey: key },
      idempotencyKey: key,
    });
  },

  followup(taskId: string, req: FollowupTaskRequest): Promise<FollowupTaskResponse> {
    return apiFetch<FollowupTaskResponse>(`/api/tasks/${enc(taskId)}/followup`, {
      method: 'POST',
      json: req,
      idempotencyKey: makeIdempotencyKey(),
    });
  },

  cancelTask(taskId: string, reason?: string): Promise<CancelTaskResponse> {
    return apiFetch<CancelTaskResponse>(`/api/tasks/${enc(taskId)}/cancel`, {
      method: 'POST',
      json: { reason: reason ?? '' },
    });
  },

  retryTask(taskId: string, req: RetryTaskRequest): Promise<RetryTaskResponse> {
    return apiFetch<RetryTaskResponse>(`/api/tasks/${enc(taskId)}/retry`, {
      method: 'POST',
      json: req,
      idempotencyKey: makeIdempotencyKey(),
    });
  },

  updateQueueInput(taskId: string, inputId: string, body: string): Promise<PendingInput> {
    return apiFetch<PendingInput>(`/api/tasks/${enc(taskId)}/queue/${enc(inputId)}`, {
      method: 'PATCH',
      json: { body },
    });
  },

  deleteQueueInput(taskId: string, inputId: string): Promise<{ deleted: boolean; inputId: string }> {
    return apiFetch(`/api/tasks/${enc(taskId)}/queue/${enc(inputId)}`, { method: 'DELETE' });
  },

  steerQueueInput(taskId: string, inputId: string): Promise<PendingInput> {
    return apiFetch<PendingInput>(`/api/tasks/${enc(taskId)}/queue/${enc(inputId)}/steer`, {
      method: 'POST',
    });
  },

  async reorderQueue(taskId: string, inputIds: string[]): Promise<PendingInput[]> {
    const res = await apiFetch<{ items: PendingInput[] }>(`/api/tasks/${enc(taskId)}/queue/reorder`, {
      method: 'POST',
      json: { inputIds },
    });
    return res.items ?? [];
  },

  dispatchAnnotations(taskId: string, req: DispatchAnnotationsRequest): Promise<DispatchAnnotationsResponse> {
    return apiFetch<DispatchAnnotationsResponse>(`/api/tasks/${enc(taskId)}/dispatch-annotations`, {
      method: 'POST',
      json: req,
      idempotencyKey: makeIdempotencyKey(),
    });
  },

  getBoard(taskId: string, opts?: { beforeSeq?: number; limit?: number }): Promise<TaskBoardResponse> {
    const params = new URLSearchParams();
    if (opts?.beforeSeq && opts.beforeSeq > 0) params.set('beforeSeq', String(opts.beforeSeq));
    if (opts?.limit && opts.limit > 0) params.set('limit', String(opts.limit));
    const q = params.toString();
    return apiFetch<TaskBoardResponse>(`/api/tasks/${enc(taskId)}/board${q ? `?${q}` : ''}`);
  },

  postBoardMessage(taskId: string, req: PostBoardMessageRequest): Promise<BoardMessage> {
    return apiFetch<BoardMessage>(`/api/tasks/${enc(taskId)}/board/messages`, {
      method: 'POST',
      json: req,
      idempotencyKey: makeIdempotencyKey(),
    });
  },

  getWorkerStream(taskId: string, workerName: string): Promise<WorkerStreamResponse> {
    return apiFetch<WorkerStreamResponse>(`/api/tasks/${enc(taskId)}/workers/${enc(workerName)}/stream`);
  },

  addSource(taskId: string, input: CreateTaskSourceInput): Promise<TaskSource> {
    return apiFetch<TaskSource>(`/api/tasks/${enc(taskId)}/sources`, {
      method: 'POST',
      json: input,
      idempotencyKey: makeIdempotencyKey(),
    });
  },

  deleteSource(taskId: string, sourceId: string): Promise<{ deleted: boolean }> {
    return apiFetch(`/api/tasks/${enc(taskId)}/sources/${enc(sourceId)}`, { method: 'DELETE' });
  },

  getAttemptRaw(attemptId: string, afterSeq: number, limit = 500): Promise<AttemptRawEventsResponse> {
    return apiFetch<AttemptRawEventsResponse>(
      `/api/attempts/${enc(attemptId)}/raw?afterSeq=${afterSeq}&limit=${limit}`,
    );
  },

  async listApprovals(state?: ApprovalState): Promise<ApprovalView[]> {
    const q = state ? `?state=${enc(state)}` : '';
    const res = await apiFetch<{ approvals: ApprovalView[] }>(`/api/approvals${q}`);
    return res.approvals ?? [];
  },

  decideApproval(approvalId: string, req: DecideApprovalRequest): Promise<DecideApprovalResponse> {
    return apiFetch<DecideApprovalResponse>(`/api/approvals/${enc(approvalId)}/decide`, {
      method: 'POST',
      json: req,
      idempotencyKey: makeIdempotencyKey(),
    });
  },

  paBrief(req: PABriefRequest = {}): Promise<PABriefResponse> {
    return apiFetch<PABriefResponse>('/api/pa/brief', { method: 'POST', json: req });
  },

  listPAThreads(): Promise<{ threads: PAThread[]; paState: PAStateView }> {
    return apiFetch('/api/pa/threads');
  },

  createPAThread(title?: string): Promise<{ thread: PAThread; paState: PAStateView }> {
    return apiFetch('/api/pa/threads', { method: 'POST', json: { title: title ?? '' } });
  },

  resetPASession(): Promise<{ thread: PAThread; paState: PAStateView }> {
    return apiFetch('/api/pa/reset-session', { method: 'POST', json: {} });
  },

  getPAThread(threadId: string): Promise<PAThreadDetailResponse> {
    return apiFetch<PAThreadDetailResponse>(`/api/pa/threads/${enc(threadId)}`);
  },

  paChat(req: PAChatRequest): Promise<PAChatResponse> {
    return apiFetch<PAChatResponse>('/api/pa/chat', {
      method: 'POST',
      json: req,
      idempotencyKey: makeIdempotencyKey(),
    });
  },

  getHebi8Main(): Promise<Hebi8MainResponse> {
    return apiFetch<Hebi8MainResponse>('/api/hebi8/main');
  },

  foldHebi8(req: Hebi8FoldRequest = {}): Promise<Hebi8FoldResponse> {
    return apiFetch<Hebi8FoldResponse>('/api/hebi8/fold', {
      method: 'POST',
      json: req,
    });
  },

  getHebi8Line(): Promise<Hebi8LineResponse> {
    return apiFetch<Hebi8LineResponse>('/api/hebi8/line');
  },

  expandHebi8Node(nodeId: string, opts?: { offset?: number; limit?: number }): Promise<Hebi8ExpandResponse> {
    const q = new URLSearchParams();
    if (opts?.offset !== undefined) q.set('offset', String(opts.offset));
    if (opts?.limit !== undefined) q.set('limit', String(opts.limit));
    const qs = q.toString();
    return apiFetch<Hebi8ExpandResponse>(`/api/hebi8/line/${enc(nodeId)}/expand${qs ? `?${qs}` : ''}`);
  },

  pinHebi8Node(nodeId: string, pinned: boolean): Promise<Hebi8PinResponse> {
    return apiFetch<Hebi8PinResponse>(`/api/hebi8/line/${enc(nodeId)}/pin`, {
      method: 'POST',
      json: { pinned },
    });
  },

  getDelivery(deliveryId: string): Promise<DeliveryDetailResponse> {
    return apiFetch<DeliveryDetailResponse>(`/api/deliveries/${enc(deliveryId)}`);
  },

  getDeliveryDiff(deliveryId: string, file?: string): Promise<DeliveryDiffResponse> {
    const q = file ? `?file=${enc(file)}` : '';
    return apiFetch<DeliveryDiffResponse>(`/api/deliveries/${enc(deliveryId)}/diff${q}`);
  },

  createAnnotation(
    deliveryId: string,
    payload: CreateAnnotationRequest,
    idempotencyKey?: string,
  ): Promise<CreateAnnotationResponse> {
    return apiFetch<CreateAnnotationResponse>(`/api/deliveries/${enc(deliveryId)}/annotations`, {
      method: 'POST',
      json: payload,
      idempotencyKey: idempotencyKey ?? makeIdempotencyKey(),
    });
  },

  syncDeliveryRyugi(deliveryId: string): Promise<RyugiSyncResponse> {
    return apiFetch<RyugiSyncResponse>(`/api/deliveries/${enc(deliveryId)}/ryugi/sync`, {
      method: 'POST',
    });
  },

  openDeliveryRyugi(deliveryId: string, bindingId?: string): Promise<RyugiOpenResponse> {
    const q = bindingId ? `?bindingId=${enc(bindingId)}` : '';
    return apiFetch<RyugiOpenResponse>(`/api/deliveries/${enc(deliveryId)}/ryugi/open${q}`, {
      method: 'POST',
      json: {},
    });
  },

  getRyugiArtifactContent(deliveryId: string, bindingId: string): Promise<RyugiArtifactContentResponse> {
    return apiFetch<RyugiArtifactContentResponse>(
      `/api/deliveries/${enc(deliveryId)}/ryugi/${enc(bindingId)}/content`,
    );
  },

  completeDeliveryReview(deliveryId: string): Promise<CompleteReviewResponse> {
    return apiFetch<CompleteReviewResponse>(`/api/deliveries/${enc(deliveryId)}/complete-review`, {
      method: 'POST',
    });
  },

  reopenDeliveryReview(deliveryId: string): Promise<CompleteReviewResponse> {
    return apiFetch<CompleteReviewResponse>(`/api/deliveries/${enc(deliveryId)}/reopen-review`, {
      method: 'POST',
    });
  },

  deleteAnnotation(annotationId: string): Promise<DeleteAnnotationResponse> {
    return apiFetch<DeleteAnnotationResponse>(`/api/annotations/${enc(annotationId)}`, {
      method: 'DELETE',
    });
  },

  async listGlobalArtifacts(): Promise<GlobalArtifactView[]> {
    const res = await apiFetch<ListArtifactsResponse>('/api/artifacts');
    return res.artifacts ?? [];
  },

  blobUrl(sha256: string): string {
    const devToken = getStoredDevToken();
    const base = `/api/blobs/${enc(sha256)}`;
    return devToken ? `${base}?token=${enc(devToken)}` : base;
  },

  async fetchBlob(sha256: string): Promise<Blob> {
    const headers = new Headers();
    const clientToken = getStoredClientToken();
    const devToken = getStoredDevToken();
    if (clientToken) {
      headers.set('Authorization', `Bearer ${clientToken}`);
    } else if (devToken) {
      headers.set('X-Orbit-Dev-Token', devToken);
    }
    const resp = await fetch(`/api/blobs/${enc(sha256)}`, {
      headers,
      credentials: 'omit',
    });
    if (!resp.ok) {
      throw new ApiError(resp.status, '', `读取文件失败 (HTTP ${resp.status})`);
    }
    return resp.blob();
  },

  async fetchBlobText(sha256: string): Promise<string> {
    const blob = await api.fetchBlob(sha256);
    return blob.text();
  },

  async downloadBlob(sha256: string, filename: string): Promise<void> {
    const blob = await api.fetchBlob(sha256);
    const objUrl = URL.createObjectURL(blob);
    try {
      const a = document.createElement('a');
      a.href = objUrl;
      a.download = filename || sha256;
      document.body.appendChild(a);
      a.click();
      a.remove();
    } finally {
      window.setTimeout(() => URL.revokeObjectURL(objUrl), 10_000);
    }
  },

  claimAuthHandoff(code: string): Promise<ClaimAuthHandoffResponse> {
    return apiFetch<ClaimAuthHandoffResponse>('/api/auth/handoff/claim', {
      method: 'POST',
      json: { code },
    });
  },

  pairClient(code: string, deviceName?: string): Promise<PairClientResponse> {
    return apiFetch<PairClientResponse>('/api/auth/pair-client', {
      method: 'POST',
      json: { code, deviceName: deviceName?.trim() || undefined },
    });
  },

  async listClients(): Promise<ClientDeviceView[]> {
    const res = await apiFetch<ListClientsResponse>('/api/clients');
    return res.clients ?? [];
  },

  revokeClient(clientId: string): Promise<RevokeClientResponse> {
    return apiFetch<RevokeClientResponse>(`/api/clients/${enc(clientId)}`, {
      method: 'DELETE',
    });
  },

  logoutClient(): Promise<LogoutClientResponse> {
    return apiFetch<LogoutClientResponse>('/api/auth/logout', {
      method: 'POST',
      json: {},
    });
  },

  completeOnboarding(req: CompleteOnboardingRequest = {}): Promise<CompleteOnboardingResponse> {
    return apiFetch<CompleteOnboardingResponse>('/api/onboarding/complete', {
      method: 'POST',
      json: req,
    });
  },

  createPairingCode(req: CreatePairingCodeRequest = {}): Promise<CreatePairingCodeResponse> {
    return apiFetch<CreatePairingCodeResponse>('/api/pairing/codes', {
      method: 'POST',
      json: req,
    });
  },

  getPairingCode(pairingId: string): Promise<PairingCodeStatusResponse> {
    return apiFetch<PairingCodeStatusResponse>(`/api/pairing/codes/${enc(pairingId)}`);
  },

  revokePairingCode(pairingId: string): Promise<PairingCodeStatusResponse> {
    return apiFetch<PairingCodeStatusResponse>(`/api/pairing/codes/${enc(pairingId)}`, {
      method: 'DELETE',
    });
  },

  async listMachines(): Promise<ListMachinesResponse['machines']> {
    const res = await apiFetch<ListMachinesResponse>('/api/machines');
    return res.machines ?? [];
  },

  updateMachine(machineId: string, req: UpdateMachineRequest): Promise<ListMachinesResponse> {
    return apiFetch<ListMachinesResponse>(`/api/machines/${enc(machineId)}`, {
      method: 'PATCH',
      json: req,
    });
  },

  deleteMachine(machineId: string): Promise<{ removed: boolean; machineId: string }> {
    return apiFetch(`/api/machines/${enc(machineId)}`, { method: 'DELETE' });
  },

  probeMachineAgents(machineId: string): Promise<MachineAgentActionResponse> {
    return apiFetch<MachineAgentActionResponse>(`/api/machines/${enc(machineId)}/agents/probe`, {
      method: 'POST',
      json: {},
    });
  },

  refreshMachineQuotas(machineId: string): Promise<MachineQuotasResponse> {
    return apiFetch<MachineQuotasResponse>(`/api/machines/${enc(machineId)}/quotas/refresh`, {
      method: 'POST',
      json: {},
    });
  },

  testMachineAgent(machineId: string, adapter: string): Promise<MachineAgentActionResponse> {
    return apiFetch<MachineAgentActionResponse>(
      `/api/machines/${enc(machineId)}/agents/${enc(adapter)}/test`,
      { method: 'POST', json: {} },
    );
  },

  installMachineAgent(machineId: string, adapter: string): Promise<MachineAgentActionResponse> {
    return apiFetch<MachineAgentActionResponse>(
      `/api/machines/${enc(machineId)}/agents/${enc(adapter)}/install`,
      { method: 'POST', json: {} },
    );
  },

  updateMachineAgent(
    machineId: string,
    adapter: string,
    req: UpdateMachineAgentRequest,
  ): Promise<MachineAgentActionResponse> {
    return apiFetch<MachineAgentActionResponse>(
      `/api/machines/${enc(machineId)}/agents/${enc(adapter)}`,
      { method: 'PATCH', json: req },
    );
  },

  listMachineDirs(machineId: string, path?: string): Promise<ListDirsResponse> {
    const q = path ? `?path=${enc(path)}` : '';
    return apiFetch<ListDirsResponse>(`/api/machines/${enc(machineId)}/dirs${q}`);
  },

  async getMachineSSHCandidates(
    machine: Pick<MachineView, 'machineId' | 'displayName' | 'displayAlias' | 'tailscaleHost' | 'sshAlias'>,
  ): Promise<SSHCandidatesResponse> {
    const helperPort = getDesktopHelperPort();
    if (helperPort) {
      try {
        const q = new URLSearchParams();
        if (machine.tailscaleHost) q.set('host', machine.tailscaleHost);
        const name = machine.displayAlias || machine.displayName;
        if (name) q.set('name', name);
        if (machine.machineId) q.set('machineId', machine.machineId);
        if (machine.sshAlias) q.set('sshAlias', machine.sshAlias);
        const resp = await fetch(`http://127.0.0.1:${helperPort}/api/local-ssh-candidates?${q.toString()}`, {
          credentials: 'omit',
        });
        if (resp.ok) {
          const data = (await resp.json()) as SSHCandidatesResponse;
          return {
            localConfigAvailable: Boolean(data.localConfigAvailable),
            recommended: data.recommended,
            candidates: data.candidates ?? [],
            allHosts: data.allHosts ?? [],
          };
        }
      } catch {
        // Fall through to Hub endpoint if desktop helper is unreachable.
      }
    }
    const res = await apiFetch<SSHCandidatesResponse>(`/api/machines/${enc(machine.machineId)}/ssh-candidates`);
    return {
      localConfigAvailable: Boolean(res.localConfigAvailable),
      recommended: res.recommended,
      candidates: res.candidates ?? [],
      allHosts: res.allHosts ?? [],
    };
  },

  createProject(req: CreateProjectRequest): Promise<CreateProjectResponse> {
    return apiFetch<CreateProjectResponse>('/api/projects', {
      method: 'POST',
      json: req,
    });
  },

  updateProjectSettings(projectId: string, req: UpdateProjectSettingsRequest): Promise<CreateProjectResponse> {
    return apiFetch<CreateProjectResponse>(`/api/projects/${enc(projectId)}`, {
      method: 'PATCH',
      json: req,
    });
  },

  deleteProject(projectId: string): Promise<{ removed: boolean; projectId: string }> {
    return apiFetch(`/api/projects/${enc(projectId)}`, { method: 'DELETE' });
  },

  createProjectPart(
    projectId: string,
    req: CreateProjectPartRequest,
  ): Promise<{ project: ProjectView; part: ProjectPartView }> {
    return apiFetch(`/api/projects/${enc(projectId)}/parts`, {
      method: 'POST',
      json: req,
    });
  },

  updateProjectPart(
    projectId: string,
    partId: string,
    req: UpdateProjectPartRequest,
  ): Promise<{ project: ProjectView; part: ProjectPartView }> {
    return apiFetch(`/api/projects/${enc(projectId)}/parts/${enc(partId)}`, {
      method: 'PATCH',
      json: req,
    });
  },

  deleteProjectPart(projectId: string, partId: string): Promise<{ project: ProjectView }> {
    return apiFetch(`/api/projects/${enc(projectId)}/parts/${enc(partId)}`, { method: 'DELETE' });
  },

  createProjectLocation(
    projectId: string,
    partId: string,
    req: CreateProjectLocationRequest,
  ): Promise<{ project: ProjectView; location: ProjectLocationView }> {
    return apiFetch(`/api/projects/${enc(projectId)}/parts/${enc(partId)}/locations`, {
      method: 'POST',
      json: req,
    });
  },

  updateProjectLocation(
    projectId: string,
    partId: string,
    locationId: string,
    req: UpdateProjectLocationRequest,
  ): Promise<{ project: ProjectView }> {
    return apiFetch(`/api/projects/${enc(projectId)}/parts/${enc(partId)}/locations/${enc(locationId)}`, {
      method: 'PATCH',
      json: req,
    });
  },

  deleteProjectLocation(projectId: string, partId: string, locationId: string): Promise<{ project: ProjectView }> {
    return apiFetch(`/api/projects/${enc(projectId)}/parts/${enc(partId)}/locations/${enc(locationId)}`, {
      method: 'DELETE',
    });
  },

  refreshProjectLocations(projectId: string): Promise<{ project: ProjectView }> {
    return apiFetch(`/api/projects/${enc(projectId)}/locations/refresh`, {
      method: 'POST',
      json: {},
    });
  },

  getProjectSuggestions(projectId: string): Promise<ProjectSuggestionsResponse> {
    return apiFetch<ProjectSuggestionsResponse>(`/api/projects/${enc(projectId)}/suggestions`);
  },

  dismissProjectSuggestion(projectId: string, partId: string, req: DismissSuggestionRequest): Promise<{ dismissed: boolean }> {
    return apiFetch(`/api/projects/${enc(projectId)}/parts/${enc(partId)}/suggestions/dismiss`, {
      method: 'POST',
      json: req,
    });
  },

  async downloadTaskFolderFile(taskId: string, relPath: string, attemptId?: string): Promise<void> {
    const headers = new Headers();
    const clientToken = getStoredClientToken();
    const devToken = getStoredDevToken();
    if (clientToken) {
      headers.set('Authorization', `Bearer ${clientToken}`);
    } else if (devToken) {
      headers.set('X-Orbit-Dev-Token', devToken);
    }
    const q = new URLSearchParams({ path: relPath });
    if (attemptId) q.set('attemptId', attemptId);
    const resp = await fetch(`/api/tasks/${enc(taskId)}/folder-file?${q.toString()}`, {
      headers,
      credentials: 'omit',
    });
    if (!resp.ok) {
      throw new ApiError(resp.status, '', `下载文件失败 (HTTP ${resp.status})`);
    }
    const blob = await resp.blob();
    const objUrl = URL.createObjectURL(blob);
    try {
      const a = document.createElement('a');
      a.href = objUrl;
      a.download = relPath.split('/').pop() || relPath;
      document.body.appendChild(a);
      a.click();
      a.remove();
    } finally {
      window.setTimeout(() => URL.revokeObjectURL(objUrl), 10_000);
    }
  },

  promoteFolderChanges(taskId: string, attemptId?: string): Promise<PromoteFolderChangesResponse> {
    return apiFetch<PromoteFolderChangesResponse>(`/api/tasks/${enc(taskId)}/folder-changes/promote`, {
      method: 'POST',
      json: attemptId ? { attemptId } : {},
    });
  },

  getNetworkSettings(): Promise<NetworkSettingsView> {
    return apiFetch<NetworkSettingsView>('/api/network/settings');
  },

  updateNetworkSettings(req: UpdateNetworkSettingsRequest): Promise<NetworkSettingsView> {
    return apiFetch<NetworkSettingsView>('/api/network/settings', {
      method: 'PATCH',
      json: req,
    });
  },

  enableTailscaleShare(req: TailscaleEnableRequest = {}): Promise<TailscaleEnableResponse> {
    return apiFetch<TailscaleEnableResponse>('/api/network/tailscale-enable', {
      method: 'POST',
      json: req,
    });
  },

  disableTailscaleShare(): Promise<TailscaleDisableResponse> {
    return apiFetch<TailscaleDisableResponse>('/api/network/tailscale-disable', {
      method: 'POST',
      json: {},
    });
  },

  discoverNetworks(): Promise<NetworkDiscoverResponse> {
    return apiFetch<NetworkDiscoverResponse>('/api/network/discover', {
      method: 'POST',
      json: {},
    });
  },
};

export function makeIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return `k_${Date.now()}_${Math.random().toString(36).slice(2, 10)}`;
}

export function errMsg(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
