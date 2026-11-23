import { create } from 'zustand';
import type { IDEScheme } from '../generated/protocol';

export type DrawerTab = 'ryugi' | 'diff';
export type MobileTab = 'inbox' | 'projects' | 'pa';
export type ThemeMode = 'system' | 'dark' | 'light';
export type WSConnectionStatus = 'connecting' | 'live' | 'reconnecting';

interface WorkbenchState {
  selectedProjectId: string | null;
  selectedTaskId: string | null;
  selectedDeliveryId: string | null;
  drawerTab: DrawerTab;
  drawerWidth: number;
  mobileTab: MobileTab;
  theme: ThemeMode;
  localMachineId: string;
  preferredIDE: IDEScheme;
  showPAOverlay: boolean;
  wsStatus: WSConnectionStatus;
  lastSeq: number;

  setSelectedProject: (projectId: string | null) => void;
  setSelectedTask: (taskId: string | null) => void;
  setSelectedDelivery: (deliveryId: string | null, tab?: DrawerTab) => void;
  setDrawerTab: (tab: DrawerTab) => void;
  setDrawerWidth: (width: number) => void;
  setMobileTab: (tab: MobileTab) => void;
  setTheme: (theme: ThemeMode) => void;
  setLocalMachineId: (machineId: string) => void;
  setPreferredIDE: (ide: IDEScheme) => void;
  setShowPAOverlay: (open: boolean) => void;
  setWSStatus: (status: WSConnectionStatus) => void;
  setLastSeq: (seq: number) => void;
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
    // ignore storage errors
  }
}

export const useWorkbenchStore = create<WorkbenchState>((set) => ({
  selectedProjectId: null,
  selectedTaskId: null,
  selectedDeliveryId: null,
  drawerTab: 'diff',
  drawerWidth: Math.min(760, Math.max(420, Number(readLS('orbit.drawerWidth', '560')) || 560)),
  mobileTab: 'inbox',
  theme: (readLS('orbit.theme', 'dark') as ThemeMode) || 'dark',
  localMachineId: readLS('orbit.localMachineId', 'hub-node'),
  preferredIDE: (readLS('orbit.preferredIDE', 'cursor') as IDEScheme) || 'cursor',
  showPAOverlay: false,
  wsStatus: 'connecting',
  lastSeq: 0,

  setSelectedProject: (projectId) =>
    set({ selectedProjectId: projectId, selectedTaskId: null, selectedDeliveryId: null }),
  setSelectedTask: (taskId) => set({ selectedTaskId: taskId, selectedDeliveryId: null }),
  setSelectedDelivery: (deliveryId, tab) =>
    set((s) => ({ selectedDeliveryId: deliveryId, drawerTab: tab ?? s.drawerTab })),
  setDrawerTab: (tab) => set({ drawerTab: tab }),
  setDrawerWidth: (width) => {
    const clamped = Math.min(760, Math.max(400, Math.round(width)));
    writeLS('orbit.drawerWidth', String(clamped));
    set({ drawerWidth: clamped });
  },
  setMobileTab: (tab) => set({ mobileTab: tab }),
  setTheme: (theme) => {
    writeLS('orbit.theme', theme);
    set({ theme });
  },
  setLocalMachineId: (machineId) => {
    writeLS('orbit.localMachineId', machineId);
    set({ localMachineId: machineId });
  },
  setPreferredIDE: (ide) => {
    writeLS('orbit.preferredIDE', ide);
    set({ preferredIDE: ide });
  },
  setShowPAOverlay: (open) => set({ showPAOverlay: open }),
  setWSStatus: (wsStatus) => set({ wsStatus }),
  setLastSeq: (lastSeq) => set({ lastSeq }),
}));
