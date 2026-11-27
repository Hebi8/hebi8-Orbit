import { useEffect } from 'react';
import { useQueries, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import type { EventRecord, MachineView, ProjectView, TaskSummary } from '../generated/protocol';
import { api } from '../lib/api';
import { attemptWatchRegistry, mergeAttemptRawEvents } from '../lib/attemptStream';
import { useWorkbenchStore } from '../store/useWorkbenchStore';

export function useBootstrap(enabled = true) {
  return useQuery({ queryKey: ['bootstrap'], queryFn: api.bootstrap, enabled, staleTime: 10_000 });
}

export function useNetworkSettings(enabled = true) {
  return useQuery({
    queryKey: ['network-settings'],
    queryFn: api.getNetworkSettings,
    enabled,
    staleTime: 15_000,
  });
}

export function useClientDevices(enabled = true) {
  return useQuery({
    queryKey: ['client-devices'],
    queryFn: api.listClients,
    enabled,
    staleTime: 15_000,
  });
}

export function useAllTasks(opts?: {
  limit?: number;
  excludeProject?: string;
  projectId?: string;
  q?: string;
  archived?: 'active' | 'only' | 'all';
  enabled?: boolean;
}) {
  const trimmedQ = opts?.q?.trim() ?? '';
  const archived = opts?.archived ?? 'active';
  return useQuery({
    queryKey: ['all-tasks', opts?.excludeProject ?? '', opts?.projectId ?? '', archived, opts?.limit ?? 80, trimmedQ],
    queryFn: () =>
      api.listAllTasks({
        limit: opts?.limit,
        excludeProject: opts?.excludeProject,
        projectId: opts?.projectId,
        q: trimmedQ || undefined,
        archived,
      }),
    enabled: opts?.enabled ?? true,
    staleTime: 10_000,
  });
}

export function useArchivedTasks(opts?: {
  projectId?: string;
  q?: string;
  sort?: 'archived_at' | 'updated_at' | 'title';
  limit?: number;
  enabled?: boolean;
}) {
  const trimmedQ = opts?.q?.trim() ?? '';
  const sort = opts?.sort ?? 'archived_at';
  return useQuery({
    queryKey: ['archived-tasks', opts?.projectId ?? '', sort, opts?.limit ?? 200, trimmedQ],
    queryFn: () =>
      api.listTasksWithMeta({
        limit: opts?.limit ?? 200,
        projectId: opts?.projectId || undefined,
        q: trimmedQ || undefined,
        archived: 'only',
        sort,
        topLevelOnly: true,
      }),
    enabled: opts?.enabled ?? true,
    staleTime: 10_000,
    placeholderData: (prev) => prev,
  });
}

export function useGlobalArtifacts(enabled = true) {
  return useQuery({
    queryKey: ['global-artifacts'],
    queryFn: api.listGlobalArtifacts,
    enabled,
    staleTime: 10_000,
  });
}

export function useProjectTasks(
  projectId: string | undefined,
  enabled = true,
  archived: 'active' | 'only' | 'all' = 'active',
) {
  return useQuery({
    queryKey: ['project-tasks', projectId, archived],
    queryFn: () => api.listProjectTasks(projectId!, archived),
    enabled: !!projectId && enabled,
  });
}

export function useMachineSSHCandidates(machine?: MachineView | null, enabled = true) {
  return useQuery({
    queryKey: [
      'ssh-candidates',
      machine?.machineId ?? '',
      machine?.tailscaleHost ?? '',
      machine?.sshAlias ?? '',
      machine?.displayAlias || machine?.displayName || '',
    ],
    queryFn: () => api.getMachineSSHCandidates(machine!),
    enabled: !!machine && enabled,
    staleTime: 60_000,
    retry: false,
  });
}

export function useRyugiArtifactContent(
  deliveryId: string | undefined,
  bindingId: string | undefined,
  enabled = true,
) {
  return useQuery({
    queryKey: ['ryugi-content', deliveryId, bindingId],
    queryFn: () => api.getRyugiArtifactContent(deliveryId!, bindingId!),
    enabled: !!deliveryId && !!bindingId && enabled,
    staleTime: 60_000,
    retry: false,
  });
}

export function useProjectSuggestions(projectId: string | undefined, enabled = true) {
  return useQuery({
    queryKey: ['project-suggestions', projectId],
    queryFn: () => api.getProjectSuggestions(projectId!),
    enabled: !!projectId && enabled,
    staleTime: 30_000,
  });
}

/** Fetches task lists for many projects at once (sidebar tree, ⌘K search, "全部" session filter). */
export function useManyProjectTasks(projects: ProjectView[], enabled = true) {
  const results = useQueries({
    queries: projects.map((p) => ({
      queryKey: ['project-tasks', p.projectId],
      queryFn: () => api.listProjectTasks(p.projectId),
      enabled,
    })),
  });
  const byProject: Record<string, TaskSummary[]> = {};
  projects.forEach((p, i) => {
    byProject[p.projectId] = results[i]?.data ?? [];
  });
  return byProject;
}

export function useTimeline(taskId: string | undefined) {
  return useQuery({
    queryKey: ['task-timeline', taskId],
    queryFn: () => api.getTaskTimeline(taskId!),
    enabled: !!taskId,
  });
}

export function useTaskChildren(taskId: string | undefined, enabled = true, includeArchived = false) {
  return useQuery({
    queryKey: ['task-children', taskId, includeArchived ? 'all' : 'active'],
    queryFn: () => api.listChildren(taskId!, includeArchived),
    enabled: !!taskId && enabled,
    staleTime: 5_000,
    refetchInterval: (query) => {
      const children = query.state.data?.children ?? [];
      const hasLive = children.some(
        (c) =>
          c.state === 'running' ||
          c.state === 'queued' ||
          c.state === 'waiting_approval' ||
          c.state === 'waiting_resource' ||
          c.openApprovalCount > 0,
      );
      return hasLive ? 4_000 : false;
    },
  });
}

export function useBoard(taskId: string | undefined, enabled = true) {
  return useQuery({
    queryKey: ['task-board', taskId],
    queryFn: () => api.getBoard(taskId!),
    enabled: !!taskId && enabled,
  });
}

export function useWorkerStream(taskId: string | undefined, workerName: string | undefined, live: boolean) {
  return useQuery({
    queryKey: ['worker-stream', taskId, workerName],
    queryFn: () => api.getWorkerStream(taskId!, workerName!),
    enabled: !!taskId && !!workerName,
    // Raw ACP frames never travel over the client WS, so a working worker is tailed by light polling.
    refetchInterval: live ? 3000 : false,
  });
}

export function useDelivery(deliveryId: string | undefined) {
  return useQuery({
    queryKey: ['delivery', deliveryId],
    queryFn: () => api.getDelivery(deliveryId!),
    enabled: !!deliveryId,
  });
}

/** Recent approvals (any state) so inbox rows can tell "still pending" from "already decided / expired". */
export function useRecentApprovals(enabled = true) {
  return useQuery({
    queryKey: ['approvals', 'recent'],
    queryFn: () => api.listApprovals(),
    enabled,
  });
}

export function usePAThreads() {
  return useQuery({ queryKey: ['pa-threads'], queryFn: api.listPAThreads });
}

export function usePAThread(threadId: string | undefined) {
  return useQuery({
    queryKey: ['pa-thread', threadId],
    queryFn: () => api.getPAThread(threadId!),
    enabled: !!threadId,
  });
}

export function useHebi8Main(enabled = true) {
  return useQuery({
    queryKey: ['hebi8-main'],
    queryFn: api.getHebi8Main,
    enabled,
    staleTime: 5_000,
  });
}

export function useHebi8Line(enabled = true) {
  return useQuery({
    queryKey: ['hebi8-line'],
    queryFn: api.getHebi8Line,
    enabled,
    staleTime: 10_000,
  });
}

export function useHebi8Expand(nodeId: string | undefined, opts?: { offset?: number; limit?: number; enabled?: boolean }) {
  const offset = opts?.offset ?? 0;
  const limit = opts?.limit ?? 20;
  return useQuery({
    queryKey: ['hebi8-expand', nodeId, offset, limit],
    queryFn: () => api.expandHebi8Node(nodeId!, { offset, limit }),
    enabled: !!nodeId && (opts?.enabled ?? true),
    staleTime: 30_000,
  });
}

const hydratedAttemptsByClient = new WeakMap<QueryClient, Set<string>>();

function getHydratedSet(qc: QueryClient): Set<string> {
  let set = hydratedAttemptsByClient.get(qc);
  if (!set) {
    set = new Set<string>();
    hydratedAttemptsByClient.set(qc, set);
  }
  return set;
}

/**
 * Real-time raw ACP stream for one attempt:
 * - Initial load fetches historical raw events via `GET /api/attempts/:id/raw?afterSeq=`.
 * - While `opts.live` is true, registers `attemptId` in `attemptWatchRegistry` so `/ws/client`
 *   pushes live `raw_acp` chunks directly into `['attempt-raw', attemptId]` in-place (zero 2s polling).
 * - Automatically falls back to 2s HTTP polling only while WS is disconnected/reconnecting.
 */
export function useAttemptRaw(attemptId: string | undefined, opts: { enabled: boolean; live: boolean }) {
  const qc = useQueryClient();
  const wsStatus = useWorkbenchStore((s) => s.wsStatus);
  const watching = !!attemptId && opts.enabled && opts.live;

  useEffect(() => {
    if (!watching || !attemptId) return;
    return attemptWatchRegistry.retain(attemptId);
  }, [watching, attemptId]);

  return useQuery({
    queryKey: ['attempt-raw', attemptId],
    enabled: !!attemptId && opts.enabled,
    queryFn: async () => {
      const id = attemptId!;
      const hydrated = getHydratedSet(qc);
      const prev = qc.getQueryData<EventRecord[]>(['attempt-raw', id]) ?? [];
      // Always start at 0 until at least one HTTP hydration has succeeded, even if live WS frames arrived first.
      let after = hydrated.has(id) && prev.length ? prev[prev.length - 1].seq : 0;
      const fetched: EventRecord[] = [];
      for (let page = 0; page < 20; page++) {
        const res = await api.getAttemptRaw(id, after, 500);
        const evs = res.events ?? [];
        for (const ev of evs) {
          if (ev.seq > after) {
            fetched.push(ev);
            after = ev.seq;
          }
        }
        if (evs.length < 500) break;
      }
      hydrated.add(id);
      if (hydrated.size > 500) {
        const oldest = hydrated.values().next().value;
        if (oldest) hydrated.delete(oldest);
      }
      // Re-read cache after async fetch so live WS chunks that landed in-flight are never overwritten.
      const latest = qc.getQueryData<EventRecord[]>(['attempt-raw', id]) ?? prev;
      return mergeAttemptRawEvents(latest, fetched);
    },
    refetchInterval: watching && wsStatus !== 'live' ? 2000 : false,
    staleTime: opts.live ? 0 : 60_000,
  });
}
