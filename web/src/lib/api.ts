import type {
  ApprovalState,
  ApprovalView,
  AttemptRawEventsResponse,
  BindScratchTaskResponse,
  BoardMessage,
  BootstrapResponse,
  CancelTaskResponse,
  CompleteReviewResponse,
  CreateAnnotationRequest,
  CreateAnnotationResponse,
  CreateTaskRequest,
  CreateTaskResponse,
  CreateTaskSourceInput,
  DecideApprovalRequest,
  DecideApprovalResponse,
  DeleteAnnotationResponse,
  DeliveryDetailResponse,
  DeliveryDiffResponse,
  DispatchAnnotationsRequest,
  DispatchAnnotationsResponse,
  FollowupTaskRequest,
  FollowupTaskResponse,
  GlobalArtifactView,
  ListArtifactsResponse,
  PABriefRequest,
  PABriefResponse,
  PAChatRequest,
  PAChatResponse,
  PAStateView,
  PAThread,
  PAThreadDetailResponse,
  PendingInput,
  PostBoardMessageRequest,
  RetryTaskRequest,
  RetryTaskResponse,
  RyugiSyncResponse,
  TaskBoardResponse,
  TaskSource,
  TaskSummary,
  TaskTimelineResponse,
  WSTicketResponse,
  WorkerStreamResponse,
} from '../generated/protocol';

const DEV_TOKEN_KEY = 'orbit.devToken';

export function getStoredDevToken(): string {
  try {
    return localStorage.getItem(DEV_TOKEN_KEY) ?? '';
  } catch {
    return '';
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
  const devToken = getStoredDevToken();
  if (devToken) {
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

  async listProjectTasks(projectId: string): Promise<TaskSummary[]> {
    const res = await apiFetch<{ tasks: TaskSummary[] }>(`/api/projects/${enc(projectId)}/tasks`);
    return res.tasks ?? [];
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

  completeDeliveryReview(deliveryId: string): Promise<CompleteReviewResponse> {
    return apiFetch<CompleteReviewResponse>(`/api/deliveries/${enc(deliveryId)}/complete-review`, {
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
