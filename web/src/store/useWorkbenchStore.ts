import { create } from 'zustand';
import type { IDEScheme } from '../generated/protocol';

export type ThemeMode = 'dark' | 'light';
export type WSConnectionStatus = 'connecting' | 'live' | 'reconnecting';
export type ReviewTab = 'code' | 'art';

export interface ReviewState {
  taskId: string;
  deliveryId: string;
  tab: ReviewTab;
  /** Optional specific artifact locator/id (e.g. 'ryugi:<bindingId>' or 'blob:<sha>:<path>') when tab === 'art'. */
  artifactId?: string;
}

export interface ToastAction {
  label: string;
  onClick: () => void;
}

export interface ToastState {
  id: number;
  text: string;
  tone: 'info' | 'err';
  action?: ToastAction;
}

function readLS(key: string, fallback: string): string {
  try {
    return localStorage.getItem(key) ?? fallback;
  } catch {
    return fallback;
  }
}

function writeLS(key: string, val: string) {
  try {
    localStorage.setItem(key, val);
  } catch {
    // storage may be unavailable (private mode); UI state simply won't persist
  }
}

function readJSON<T>(key: string, fallback: T): T {
  try {
    const raw = localStorage.getItem(key);
    return raw ? (JSON.parse(raw) as T) : fallback;
  } catch {
    return fallback;
  }
}

const PROMPT_CACHE_KEY = 'orbit.promptCache';
const PROMPT_CACHE_MAX = 400;
const REVIEW_RATIO_MIN = 36;
const REVIEW_RATIO_MAX = 78;

function readRatio(key: string, fallback: number): number {
  const n = Number(readLS(key, String(fallback)));
  return Number.isFinite(n) ? Math.min(REVIEW_RATIO_MAX, Math.max(REVIEW_RATIO_MIN, n)) : fallback;
}

export function applyThemeToDocument(mode: ThemeMode) {
  document.documentElement.setAttribute('data-mode', mode);
}

const initialTheme: ThemeMode = readLS('orbit.theme', 'dark') === 'light' ? 'light' : 'dark';

interface WorkbenchState {
  theme: ThemeMode;
  sidebarCollapsed: boolean;
  rightCardOpen: boolean;
  boardOpen: boolean;
  mobileDockOpen: boolean;
  cmdkOpen: boolean;
  machinesOpen: boolean;
  review: ReviewState | null;
  reviewRatios: Record<ReviewTab, number>;
  reviewSessionHidden: boolean;
  wsStatus: WSConnectionStatus;
  lastSeq: number;
  localMachineId: string;
  preferredIDE: IDEScheme;
  sshAliasRev: number;
  workspaceNameOverride: string;
  expandedProjects: Record<string, boolean>;
  expandedParents: Record<string, boolean>;
  failoverBannerDismissedKey: string | null;
  promptCache: Record<string, string>;
  deselectedAnnotations: Record<string, string[]>;
  toast: ToastState | null;

  toggleTheme: () => void;
  toggleSidebar: () => void;
  setRightCardOpen: (open: boolean) => void;
  setBoardOpen: (open: boolean) => void;
  setMobileDockOpen: (open: boolean) => void;
  setCmdkOpen: (open: boolean) => void;
  setMachinesOpen: (open: boolean) => void;
  openReview: (tab: ReviewTab, taskId: string, deliveryId: string, artifactId?: string) => void;
  closeReview: () => void;
  setReviewTab: (tab: ReviewTab, artifactId?: string) => void;
  setReviewDelivery: (deliveryId: string, artifactId?: string) => void;
  setReviewRatio: (tab: ReviewTab, ratio: number) => void;
  setReviewSessionHidden: (hidden: boolean) => void;
  setWSStatus: (status: WSConnectionStatus) => void;
  setLastSeq: (seq: number) => void;
  setLocalMachineId: (machineId: string) => void;
  setPreferredIDE: (scheme: IDEScheme) => void;
  bumpSshAliasRev: () => void;
  setWorkspaceNameOverride: (name: string) => void;
  setProjectExpanded: (projectId: string, open: boolean) => void;
  setParentExpanded: (parentTaskId: string, open: boolean) => void;
  toggleParentExpanded: (parentTaskId: string) => void;
  ensureParentExpanded: (parentTaskId: string) => void;
  dismissFailoverBanner: (key: string) => void;
  rememberPrompt: (key: string, body: string) => void;
  toggleAnnotationSelected: (taskId: string, annotationId: string, selected: boolean) => void;
  clearDeselectedAnnotations: (taskId: string) => void;
  showToast: (text: string, tone?: 'info' | 'err', action?: ToastAction) => void;
  clearToast: (id: number) => void;
}

let toastSeq = 0;

export const useWorkbenchStore = create<WorkbenchState>((set, get) => ({
  theme: initialTheme,
  sidebarCollapsed: readLS('orbit.sidebarCollapsed', '0') === '1',
  rightCardOpen: readLS('orbit.rightCard', '1') !== '0',
  boardOpen: false,
  mobileDockOpen: false,
  cmdkOpen: false,
  machinesOpen: false,
  review: null,
  reviewRatios: {
    code: readRatio('orbit.reviewRatio.code', 72),
    art: readRatio('orbit.reviewRatio.art', 60),
  },
  reviewSessionHidden: false,
  wsStatus: 'connecting',
  lastSeq: 0,
  localMachineId: readLS('orbit.localMachineId', ''),
  preferredIDE: (readLS('orbit.preferredIDE', 'cursor') as IDEScheme) || 'cursor',
  sshAliasRev: 0,
  workspaceNameOverride: readLS('orbit.workspaceName', ''),
  expandedProjects: readJSON<Record<string, boolean>>('orbit.expandedProjects', {}),
  expandedParents: readJSON<Record<string, boolean>>('orbit.expandedParents', {}),
  failoverBannerDismissedKey: null,
  promptCache: readJSON<Record<string, string>>(PROMPT_CACHE_KEY, {}),
  deselectedAnnotations: {},
  toast: null,

  toggleTheme: () => {
    const next: ThemeMode = get().theme === 'dark' ? 'light' : 'dark';
    writeLS('orbit.theme', next);
    applyThemeToDocument(next);
    set({ theme: next });
  },
  toggleSidebar: () => {
    const next = !get().sidebarCollapsed;
    writeLS('orbit.sidebarCollapsed', next ? '1' : '0');
    set({ sidebarCollapsed: next });
  },
  setRightCardOpen: (open) => {
    writeLS('orbit.rightCard', open ? '1' : '0');
    set({ rightCardOpen: open });
  },
  setBoardOpen: (open) => set({ boardOpen: open }),
  setMobileDockOpen: (open) => set({ mobileDockOpen: open }),
  setCmdkOpen: (open) => set({ cmdkOpen: open, machinesOpen: false }),
  setMachinesOpen: (open) => set({ machinesOpen: open }),
  openReview: (tab, taskId, deliveryId, artifactId) =>
    set({
      review: { taskId, deliveryId, tab, artifactId },
      cmdkOpen: false,
      machinesOpen: false,
    }),
  closeReview: () => set({ review: null }),
  setReviewTab: (tab, artifactId) =>
    set((s) => (s.review ? { review: { ...s.review, tab, artifactId } } : s)),
  setReviewDelivery: (deliveryId, artifactId) =>
    set((s) => (s.review ? { review: { ...s.review, deliveryId, artifactId: artifactId ?? s.review.artifactId } } : s)),
  setReviewRatio: (tab, ratio) => {
    const clamped = Math.min(REVIEW_RATIO_MAX, Math.max(REVIEW_RATIO_MIN, Math.round(ratio)));
    writeLS(`orbit.reviewRatio.${tab}`, String(clamped));
    set((s) => ({ reviewRatios: { ...s.reviewRatios, [tab]: clamped } }));
  },
  setReviewSessionHidden: (hidden) => set({ reviewSessionHidden: hidden }),
  setWSStatus: (wsStatus) => set({ wsStatus }),
  setLastSeq: (lastSeq) => set({ lastSeq }),
  setLocalMachineId: (machineId) => {
    writeLS('orbit.localMachineId', machineId);
    set({ localMachineId: machineId });
  },
  setPreferredIDE: (scheme) => {
    writeLS('orbit.preferredIDE', scheme);
    set({ preferredIDE: scheme });
  },
  bumpSshAliasRev: () => set((s) => ({ sshAliasRev: s.sshAliasRev + 1 })),
  setWorkspaceNameOverride: (name) => {
    const trimmed = name.trim();
    writeLS('orbit.workspaceName', trimmed);
    set({ workspaceNameOverride: trimmed });
  },
  setProjectExpanded: (projectId, open) => {
    const next = { ...get().expandedProjects, [projectId]: open };
    writeLS('orbit.expandedProjects', JSON.stringify(next));
    set({ expandedProjects: next });
  },
  setParentExpanded: (parentTaskId, open) => {
    if (!parentTaskId) return;
    const cur = get().expandedParents;
    if (Boolean(cur[parentTaskId]) === open) return;
    const next = { ...cur };
    if (open) next[parentTaskId] = true;
    else delete next[parentTaskId];
    writeLS('orbit.expandedParents', JSON.stringify(next));
    set({ expandedParents: next });
  },
  toggleParentExpanded: (parentTaskId) => {
    if (!parentTaskId) return;
    const open = !get().expandedParents[parentTaskId];
    get().setParentExpanded(parentTaskId, open);
  },
  ensureParentExpanded: (parentTaskId) => {
    if (!parentTaskId || get().expandedParents[parentTaskId]) return;
    get().setParentExpanded(parentTaskId, true);
  },
  dismissFailoverBanner: (key) => set({ failoverBannerDismissedKey: key }),
  rememberPrompt: (key, body) => {
    if (!key || !body) return;
    const cur = get().promptCache;
    if (cur[key] === body) return;
    const next: Record<string, string> = { ...cur, [key]: body };
    const keys = Object.keys(next);
    if (keys.length > PROMPT_CACHE_MAX) {
      for (const k of keys.slice(0, keys.length - PROMPT_CACHE_MAX)) delete next[k];
    }
    writeLS(PROMPT_CACHE_KEY, JSON.stringify(next));
    set({ promptCache: next });
  },
  toggleAnnotationSelected: (taskId, annotationId, selected) =>
    set((s) => {
      const prev = new Set(s.deselectedAnnotations[taskId] ?? []);
      if (selected) prev.delete(annotationId);
      else prev.add(annotationId);
      return { deselectedAnnotations: { ...s.deselectedAnnotations, [taskId]: [...prev] } };
    }),
  clearDeselectedAnnotations: (taskId) =>
    set((s) => {
      if (!s.deselectedAnnotations[taskId]) return s;
      const next = { ...s.deselectedAnnotations };
      delete next[taskId];
      return { deselectedAnnotations: next };
    }),
  showToast: (text, tone = 'info', action) => {
    toastSeq += 1;
    const id = toastSeq;
    set({ toast: { id, text, tone, action } });
    const ms = action ? 5500 : tone === 'err' ? 4200 : 2400;
    window.setTimeout(() => get().clearToast(id), ms);
  },
  clearToast: (id) => set((s) => (s.toast?.id === id ? { toast: null } : s)),
}));

export function toast(text: string, tone: 'info' | 'err' = 'info', action?: ToastAction) {
  useWorkbenchStore.getState().showToast(text, tone, action);
}
