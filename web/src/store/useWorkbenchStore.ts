import { create } from 'zustand';
import type { IDEScheme } from '../generated/protocol';

export type ThemeMode = 'dark' | 'light';
export type WSConnectionStatus = 'connecting' | 'live' | 'reconnecting';
export type WinKind = 'cr' | 'art';

export interface WinState {
  taskId: string;
  deliveryId: string;
  x: number;
  y: number;
}

export interface ToastState {
  id: number;
  text: string;
  tone: 'info' | 'err';
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
  windows: Partial<Record<WinKind, WinState>>;
  winOrder: WinKind[];
  wsStatus: WSConnectionStatus;
  lastSeq: number;
  localMachineId: string;
  preferredIDE: IDEScheme;
  expandedProjects: Record<string, boolean>;
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
  openWindow: (kind: WinKind, taskId: string, deliveryId: string) => void;
  closeWindow: (kind: WinKind) => void;
  focusWindow: (kind: WinKind) => void;
  moveWindow: (kind: WinKind, x: number, y: number) => void;
  setWSStatus: (status: WSConnectionStatus) => void;
  setLastSeq: (seq: number) => void;
  setLocalMachineId: (machineId: string) => void;
  setProjectExpanded: (projectId: string, open: boolean) => void;
  rememberPrompt: (key: string, body: string) => void;
  toggleAnnotationSelected: (taskId: string, annotationId: string, selected: boolean) => void;
  clearDeselectedAnnotations: (taskId: string) => void;
  showToast: (text: string, tone?: 'info' | 'err') => void;
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
  windows: {},
  winOrder: [],
  wsStatus: 'connecting',
  lastSeq: 0,
  localMachineId: readLS('orbit.localMachineId', ''),
  preferredIDE: (readLS('orbit.preferredIDE', 'cursor') as IDEScheme) || 'cursor',
  expandedProjects: readJSON<Record<string, boolean>>('orbit.expandedProjects', {}),
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
  openWindow: (kind, taskId, deliveryId) =>
    set((s) => {
      const prev = s.windows[kind];
      // Default to the left so the right-hand floating card stays reachable; the second window cascades.
      const other = kind === 'cr' ? s.windows.art : s.windows.cr;
      const base = kind === 'cr' ? { x: 24, y: 56 } : { x: 64, y: 96 };
      const defaults = other && other.x === base.x && other.y === base.y ? { x: base.x + 40, y: base.y + 40 } : base;
      return {
        windows: { ...s.windows, [kind]: { taskId, deliveryId, x: prev?.x ?? defaults.x, y: prev?.y ?? defaults.y } },
        winOrder: [...s.winOrder.filter((k) => k !== kind), kind],
      };
    }),
  closeWindow: (kind) =>
    set((s) => {
      const next = { ...s.windows };
      delete next[kind];
      return { windows: next, winOrder: s.winOrder.filter((k) => k !== kind) };
    }),
  focusWindow: (kind) =>
    set((s) => (s.winOrder[s.winOrder.length - 1] === kind ? s : { winOrder: [...s.winOrder.filter((k) => k !== kind), kind] })),
  moveWindow: (kind, x, y) =>
    set((s) => {
      const w = s.windows[kind];
      if (!w) return s;
      return { windows: { ...s.windows, [kind]: { ...w, x, y } } };
    }),
  setWSStatus: (wsStatus) => set({ wsStatus }),
  setLastSeq: (lastSeq) => set({ lastSeq }),
  setLocalMachineId: (machineId) => {
    writeLS('orbit.localMachineId', machineId);
    set({ localMachineId: machineId });
  },
  setProjectExpanded: (projectId, open) => {
    const next = { ...get().expandedProjects, [projectId]: open };
    writeLS('orbit.expandedProjects', JSON.stringify(next));
    set({ expandedProjects: next });
  },
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
  showToast: (text, tone = 'info') => {
    toastSeq += 1;
    const id = toastSeq;
    set({ toast: { id, text, tone } });
    window.setTimeout(() => get().clearToast(id), tone === 'err' ? 4200 : 2400);
  },
  clearToast: (id) => set((s) => (s.toast?.id === id ? { toast: null } : s)),
}));

export function toast(text: string, tone: 'info' | 'err' = 'info') {
  useWorkbenchStore.getState().showToast(text, tone);
}
