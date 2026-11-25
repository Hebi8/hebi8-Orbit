import { useQueries, useQuery, useQueryClient } from '@tanstack/react-query';
import type { EventRecord, ProjectView, TaskSummary } from '../generated/protocol';
import { api } from '../lib/api';

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

export function useGlobalArtifacts(enabled = true) {
  return useQuery({
    queryKey: ['global-artifacts'],
    queryFn: api.listGlobalArtifacts,
    enabled,
    staleTime: 10_000,
  });
}

export function useProjectTasks(projectId: string | undefined, enabled = true) {
  return useQuery({
    queryKey: ['project-tasks', projectId],
    queryFn: () => api.listProjectTasks(projectId!),
    enabled: !!projectId && enabled,
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

/**
 * Incremental raw ACP stream for one attempt. The hub WS deliberately excludes raw_acp frames,
 * so the live turn tails `GET /api/attempts/:id/raw?afterSeq=` and merges pages into the cache
 * (seq-ordered, de-duplicated). Historical turns load once on demand.
 */
export function useAttemptRaw(attemptId: string | undefined, opts: { enabled: boolean; live: boolean }) {
  const qc = useQueryClient();
  return useQuery({
    queryKey: ['attempt-raw', attemptId],
    enabled: !!attemptId && opts.enabled,
    queryFn: async () => {
      const prev = qc.getQueryData<EventRecord[]>(['attempt-raw', attemptId]) ?? [];
      const out = [...prev];
      let after = out.length ? out[out.length - 1].seq : 0;
      for (let page = 0; page < 20; page++) {
        const res = await api.getAttemptRaw(attemptId!, after, 500);
        const evs = res.events ?? [];
        for (const ev of evs) {
          if (ev.seq > after) {
            out.push(ev);
            after = ev.seq;
          }
        }
        if (evs.length < 500) break;
      }
      return out;
    },
    refetchInterval: opts.live ? 2000 : false,
    staleTime: opts.live ? 0 : 60_000,
  });
}
