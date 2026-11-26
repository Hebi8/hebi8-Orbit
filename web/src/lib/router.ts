import { useSyncExternalStore } from 'react';

export type NewMode = 'pa' | 'chat' | 'project';
export type ProjectTab = 'threads' | 'deliveries' | 'settings';

export type Route =
  | { name: 'home' }
  | { name: 'pa'; threadId?: string; childId?: string }
  | { name: 'artifacts' }
  | { name: 'archived' }
  | { name: 'new'; mode?: NewMode; projectId?: string; machineId?: string; adapter?: string }
  | { name: 'project'; projectId: string; tab: ProjectTab }
  | { name: 'session'; taskId: string; worker?: string; childId?: string }
  | { name: 'projects' }
  | { name: 'onboarding' }
  | { name: 'settings'; machineId?: string }
  | { name: 'auth'; code: string }
  | { name: 'join'; code: string; scope?: string };

export function parseHash(hash: string): Route {
  const raw = hash.replace(/^#/, '');
  const [pathPart, queryPart = ''] = raw.split('?');
  const segs = pathPart.split('/').filter(Boolean).map((s) => {
    try {
      return decodeURIComponent(s);
    } catch {
      return s;
    }
  });
  const q = new URLSearchParams(queryPart);
  switch (segs[0]) {
    case undefined:
    case 'home':
      return { name: 'home' };
    case 'auth':
      if (segs[1]) return { name: 'auth', code: segs[1] };
      return { name: 'home' };
    case 'join':
      if (segs[1]) return { name: 'join', code: segs[1], scope: q.get('scope') ?? undefined };
      return { name: 'home' };
    case 'onboarding':
      return { name: 'onboarding' };
    case 'settings':
      return { name: 'settings', machineId: q.get('machine') ?? undefined };
    case 'pa':
    case 'hebi8':
      return { name: 'pa', threadId: segs[1], childId: q.get('child') ?? undefined };
    case 'artifacts':
      return { name: 'artifacts' };
    case 'archived':
      return { name: 'archived' };
    case 'new': {
      const m = q.get('mode');
      const mode: NewMode | undefined = m === 'pa' || m === 'chat' || m === 'project' ? m : undefined;
      return {
        name: 'new',
        mode,
        projectId: q.get('project') ?? undefined,
        machineId: q.get('machine') ?? undefined,
        adapter: q.get('adapter') ?? undefined,
      };
    }
    case 'p':
      if (segs[1]) {
        const t = q.get('tab');
        const tab: ProjectTab =
          t === 'settings' ? 'settings' : t === 'deliveries' || t === 'artifacts' ? 'deliveries' : 'threads';
        return { name: 'project', projectId: segs[1], tab };
      }
      return { name: 'projects' };
    case 'projects':
      return { name: 'projects' };
    case 't':
      if (segs[1]) {
        return {
          name: 'session',
          taskId: segs[1],
          worker: segs[2] === 'w' && segs[3] ? segs[3] : undefined,
          childId: q.get('child') ?? undefined,
        };
      }
      return { name: 'home' };
    default:
      return { name: 'home' };
  }
}

const e = encodeURIComponent;

export const paths = {
  home: () => '#/',
  onboarding: () => '#/onboarding',
  settings: (machineId?: string) => (machineId ? `#/settings?machine=${e(machineId)}` : '#/settings'),
  hebi8: (childId?: string) => (childId ? `#/hebi8?child=${e(childId)}` : '#/hebi8'),
  pa: (threadId?: string, childId?: string) => {
    const base = threadId ? `#/pa/${e(threadId)}` : '#/hebi8';
    return childId ? `${base}?child=${e(childId)}` : base;
  },
  artifacts: () => '#/artifacts',
  archived: () => '#/archived',
  newThread: (mode?: NewMode, projectId?: string, opts?: { machineId?: string; adapter?: string }) => {
    const q = new URLSearchParams();
    if (mode) q.set('mode', mode);
    if (projectId) q.set('project', projectId);
    if (opts?.machineId) q.set('machine', opts.machineId);
    if (opts?.adapter) q.set('adapter', opts.adapter);
    const s = q.toString();
    return s ? `#/new?${s}` : '#/new';
  },
  project: (projectId: string, tab: ProjectTab = 'threads') =>
    tab === 'threads' ? `#/p/${e(projectId)}` : `#/p/${e(projectId)}?tab=${tab}`,
  projects: () => '#/projects',
  session: (taskId: string, worker?: string, childId?: string) => {
    const base = worker ? `#/t/${e(taskId)}/w/${e(worker)}` : `#/t/${e(taskId)}`;
    return childId ? `${base}?child=${e(childId)}` : base;
  },
};

export function navigate(hash: string) {
  if (window.location.hash === hash) return;
  window.location.hash = hash;
}

export function replaceHash(hash: string) {
  window.history.replaceState(null, '', hash);
  window.dispatchEvent(new Event('hashchange'));
}

function subscribe(cb: () => void) {
  window.addEventListener('hashchange', cb);
  return () => window.removeEventListener('hashchange', cb);
}

function getSnapshot() {
  return window.location.hash;
}

export function useRoute(): Route {
  const hash = useSyncExternalStore(subscribe, getSnapshot, getSnapshot);
  return parseHash(hash);
}
