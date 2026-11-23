import type {
  BootstrapResponse,
  CreateAnnotationRequest,
  CreateAnnotationResponse,
  DeliveryDetailResponse,
  DeliveryDiffResponse,
  RyugiSyncResponse,
  TaskSummary,
  TaskTimelineResponse,
} from '../generated/protocol';

const DEV_TOKEN_KEY = 'orbit.devToken';

export function getStoredDevToken(): string {
  try {
    return localStorage.getItem(DEV_TOKEN_KEY) ?? '';
  } catch {
    return '';
  }
}

async function apiFetch<T>(
  path: string,
  options: RequestInit & { idempotencyKey?: string } = {},
): Promise<T> {
  const headers = new Headers(options.headers);
  const method = (options.method ?? 'GET').toUpperCase();
  const isWrite = method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE';

  if (isWrite) {
    headers.set('X-Orbit-Request', '1');
    if (options.body && !headers.has('Content-Type')) {
      headers.set('Content-Type', 'application/json');
    }
  }
  if (options.idempotencyKey) {
    headers.set('Idempotency-Key', options.idempotencyKey);
  }
  const devToken = getStoredDevToken();
  if (devToken) {
    headers.set('X-Orbit-Dev-Token', devToken);
  }

  const resp = await fetch(path, {
    ...options,
    headers,
    credentials: 'omit', // Cookie-less auth discipline (RFC 6265 §8.5)
  });

  if (!resp.ok) {
    let message = `HTTP ${resp.status}`;
    try {
      const errBody = (await resp.json()) as { code?: string; message?: string };
      if (errBody.message) {
        message = errBody.code ? `${errBody.code}: ${errBody.message}` : errBody.message;
      }
    } catch {
      // ignore non-json error bodies
    }
    throw new Error(message);
  }

  return (await resp.json()) as T;
}

export const api = {
  bootstrap(): Promise<BootstrapResponse> {
    return apiFetch<BootstrapResponse>('/api/bootstrap');
  },

  async listProjectTasks(projectId: string): Promise<TaskSummary[]> {
    const res = await apiFetch<{ tasks: TaskSummary[] }>(
      `/api/projects/${encodeURIComponent(projectId)}/tasks`,
    );
    return res.tasks ?? [];
  },

  getTaskTimeline(taskId: string, includeRaw = false): Promise<TaskTimelineResponse> {
    const q = includeRaw ? '?includeRaw=1' : '?includeRaw=0';
    return apiFetch<TaskTimelineResponse>(
      `/api/tasks/${encodeURIComponent(taskId)}/timeline${q}`,
    );
  },

  getDelivery(deliveryId: string): Promise<DeliveryDetailResponse> {
    return apiFetch<DeliveryDetailResponse>(
      `/api/deliveries/${encodeURIComponent(deliveryId)}`,
    );
  },

  getDeliveryDiff(deliveryId: string, file?: string): Promise<DeliveryDiffResponse> {
    const q = file ? `?file=${encodeURIComponent(file)}` : '';
    return apiFetch<DeliveryDiffResponse>(
      `/api/deliveries/${encodeURIComponent(deliveryId)}/diff${q}`,
    );
  },

  createAnnotation(
    deliveryId: string,
    payload: CreateAnnotationRequest,
    idempotencyKey?: string,
  ): Promise<CreateAnnotationResponse> {
    return apiFetch<CreateAnnotationResponse>(
      `/api/deliveries/${encodeURIComponent(deliveryId)}/annotations`,
      {
        method: 'POST',
        body: JSON.stringify(payload),
        idempotencyKey: idempotencyKey ?? makeIdempotencyKey(),
      },
    );
  },

  syncDeliveryRyugi(deliveryId: string): Promise<RyugiSyncResponse> {
    return apiFetch<RyugiSyncResponse>(
      `/api/deliveries/${encodeURIComponent(deliveryId)}/ryugi/sync`,
      { method: 'POST' },
    );
  },
};

export function makeIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return `k_${Date.now()}_${Math.random().toString(36).slice(2, 10)}`;
}
