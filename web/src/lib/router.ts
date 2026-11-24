import { useSyncExternalStore } from 'react';

export type NewMode = 'pa' | 'chat' | 'project';
export type ProjectTab = 'threads' | 'deliveries';

export type Route =
  | { name: 'home' }
  | { name: 'pa'; threadId?: string }
  | { name: 'new'; mode?: NewMode; projectId?: string }
  | { name: 'project'; projectId: string; tab: ProjectTab }
  | { name: 'session'; taskId: string; worker?: string }
  | { name: 'projects' };

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
    case 'pa':
      return { name: 'pa', threadId: segs[1] };
    case 'new': {
      const m = q.get('mode');
      const mode: NewMode | undefined = m === 'pa' || m === 'chat' || m === 'project' ? m : undefined;
      return { name: 'new', mode, projectId: q.get('project') ?? undefined };
    }
    case 'p':
      if (segs[1]) {
        return { name: 'project', projectId: segs[1], tab: q.get('tab') === 'deliveries' ? 'deliveries' : 'threads' };
      }
      return { name: 'projects' };
    case 'projects':
      return { name: 'projects' };
    case 't':
      if (segs[1]) {
        return { name: 'session', taskId: segs[1], worker: segs[2] === 'w' && segs[3] ? segs[3] : undefined };
      }
      return { name: 'home' };
    default:
      return { name: 'home' };
  }
}

const e = encodeURIComponent;

export const paths = {
  home: () => '#/',
  pa: (threadId?: string) => (threadId ? `#/pa/${e(threadId)}` : '#/pa'),
  newThread: (mode?: NewMode, projectId?: string) => {
    const q = new URLSearchParams();
    if (mode) q.set('mode', mode);
    if (projectId) q.set('project', projectId);
    const s = q.toString();
    return s ? `#/new?${s}` : '#/new';
  },
  project: (projectId: string, tab: ProjectTab = 'threads') =>
    tab === 'threads' ? `#/p/${e(projectId)}` : `#/p/${e(projectId)}?tab=${tab}`,
  projects: () => '#/projects',
  session: (taskId: string, worker?: string) =>
    worker ? `#/t/${e(taskId)}/w/${e(worker)}` : `#/t/${e(taskId)}`,
};

export function navigate(hash: string) {
  if (window.location.hash === hash) return;
  window.location.hash = hash;
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
