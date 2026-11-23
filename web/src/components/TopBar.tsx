import { useState } from 'react';
import {
  Command,
  Laptop,
  Moon,
  Orbit,
  SlidersHorizontal,
  Sun,
  Monitor,
  Wifi,
  WifiOff,
} from 'lucide-react';
import {
  IDESchemeValues,
  type IDEScheme,
  type MachineView,
  type ProjectView,
} from '../generated/protocol';
import { useWorkbenchStore, type ThemeMode } from '../store/useWorkbenchStore';

interface TopBarProps {
  activeProject?: ProjectView;
  machines: MachineView[];
  openInboxCount: number;
}

export function TopBar({ activeProject, machines, openInboxCount }: TopBarProps) {
  const {
    theme,
    setTheme,
    localMachineId,
    setLocalMachineId,
    preferredIDE,
    setPreferredIDE,
    setShowPAOverlay,
    wsStatus,
    lastSeq,
  } = useWorkbenchStore();

  const [showPrefs, setShowPrefs] = useState(false);

  const cycleTheme = () => {
    const order: ThemeMode[] = ['dark', 'light', 'system'];
    const next = order[(order.indexOf(theme) + 1) % order.length];
    setTheme(next);
  };

  return (
    <header className="relative z-20 flex h-12 shrink-0 items-center justify-between border-b border-zinc-200 bg-white px-3 dark:border-zinc-800/90 dark:bg-zinc-950">
      <div className="flex items-center gap-3 min-w-0">
        <div className="flex items-center gap-2 text-zinc-900 dark:text-zinc-100">
          <Orbit className="h-4 w-4 text-sky-500" />
          <span className="text-sm font-semibold tracking-tight">hebi8-Orbit</span>
        </div>

        {activeProject && (
          <>
            <span className="text-zinc-300 dark:text-zinc-700">/</span>
            <span className="truncate text-xs font-medium text-zinc-700 dark:text-zinc-300">
              {activeProject.displayName}
            </span>
            <span className="hidden rounded border border-zinc-200 bg-zinc-50 px-1.5 py-0.5 font-mono text-[11px] text-zinc-500 sm:inline dark:border-zinc-800 dark:bg-zinc-900 dark:text-zinc-400">
              {activeProject.defaultBranch}
            </span>
          </>
        )}
      </div>

      <div className="flex items-center gap-1.5">
        <div
          title={`WebSocket: ${wsStatus} (seq=${lastSeq})`}
          className="hidden items-center gap-1.5 rounded-md border border-zinc-200 px-2 py-1 text-[11px] text-zinc-500 sm:flex dark:border-zinc-800 dark:text-zinc-400"
        >
          {wsStatus === 'live' ? (
            <Wifi className="h-3 w-3 text-emerald-500" />
          ) : (
            <WifiOff className="h-3 w-3 text-amber-500" />
          )}
          <span className="font-mono">#{lastSeq}</span>
        </div>

        {/* Device + IDE selector popover */}
        <div className="relative">
          <button
            type="button"
            onClick={() => setShowPrefs((v) => !v)}
            className="flex items-center gap-1.5 rounded-md border border-zinc-200 px-2.5 py-1 text-xs text-zinc-600 transition hover:bg-zinc-100 dark:border-zinc-800 dark:text-zinc-300 dark:hover:bg-zinc-900"
            title="本机设备标识与首选 IDE"
          >
            <SlidersHorizontal className="h-3.5 w-3.5" />
            <span className="hidden md:inline font-mono text-[11px]">
              {localMachineId || 'local'} · {preferredIDE}
            </span>
          </button>

          {showPrefs && (
            <>
              <div
                className="fixed inset-0 z-20"
                onClick={() => setShowPrefs(false)}
              />
              <div className="absolute right-0 top-10 z-30 w-72 rounded-lg border border-zinc-200 bg-white p-3 shadow-xl dark:border-zinc-800 dark:bg-zinc-900">
                <div className="mb-2 flex items-center gap-1.5 text-xs font-semibold text-zinc-800 dark:text-zinc-200">
                  <Laptop className="h-3.5 w-3.5 text-sky-500" />
                  本机环境与 IDE 跳转偏好
                </div>

                <label className="mb-1 block text-[11px] text-zinc-500 dark:text-zinc-400">
                  当前所在设备（用于判断直开本地路径还是 SSH Remote）
                </label>
                <select
                  value={localMachineId}
                  onChange={(e) => setLocalMachineId(e.target.value)}
                  className="mb-3 w-full rounded-md border border-zinc-200 bg-zinc-50 px-2.5 py-1.5 text-base sm:text-xs text-zinc-900 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100"
                >
                  <option value="external-client">外部客户端（全部走 SSH Remote）</option>
                  {machines.map((m) => (
                    <option key={m.machineId} value={m.machineId}>
                      {m.machineId} ({m.displayName})
                    </option>
                  ))}
                </select>

                <label className="mb-1 block text-[11px] text-zinc-500 dark:text-zinc-400">
                  首选 IDE 协议
                </label>
                <div className="grid grid-cols-2 gap-1.5">
                  {IDESchemeValues.map((ide: IDEScheme) => (
                    <button
                      key={ide}
                      type="button"
                      onClick={() => setPreferredIDE(ide)}
                      className={`rounded-md border px-2 py-1.5 text-xs font-mono transition ${
                        preferredIDE === ide
                          ? 'border-sky-500 bg-sky-500/10 text-sky-600 dark:text-sky-400'
                          : 'border-zinc-200 text-zinc-600 hover:bg-zinc-100 dark:border-zinc-800 dark:text-zinc-400 dark:hover:bg-zinc-800'
                      }`}
                    >
                      {ide}
                    </button>
                  ))}
                </div>
              </div>
            </>
          )}
        </div>

        <button
          type="button"
          onClick={cycleTheme}
          title={`主题模式: ${theme}`}
          className="flex h-7 w-7 items-center justify-center rounded-md border border-zinc-200 text-zinc-600 transition hover:bg-zinc-100 dark:border-zinc-800 dark:text-zinc-400 dark:hover:bg-zinc-900"
        >
          {theme === 'dark' ? (
            <Moon className="h-3.5 w-3.5" />
          ) : theme === 'light' ? (
            <Sun className="h-3.5 w-3.5" />
          ) : (
            <Monitor className="h-3.5 w-3.5" />
          )}
        </button>

        <button
          type="button"
          onClick={() => setShowPAOverlay(true)}
          className="flex items-center gap-1.5 rounded-md border border-zinc-200 bg-zinc-50 px-2.5 py-1 text-xs font-medium text-zinc-700 transition hover:border-sky-500/50 hover:text-zinc-900 dark:border-zinc-800 dark:bg-zinc-900 dark:text-zinc-300 dark:hover:text-white"
        >
          <Command className="h-3 w-3 text-zinc-400" />
          <span>全局 PA</span>
          {openInboxCount > 0 && (
            <span className="h-1.5 w-1.5 rounded-full bg-sky-500" />
          )}
        </button>
      </div>
    </header>
  );
}
