import { useState } from 'react';
import {
  AlertTriangle,
  FolderGit2,
  Inbox,
  PackageCheck,
  ShieldAlert,
} from 'lucide-react';
import type {
  InboxItemView,
  MachineView,
  ProjectView,
} from '../generated/protocol';
import { useWorkbenchStore } from '../store/useWorkbenchStore';

interface LeftSidebarProps {
  projects: ProjectView[];
  machines: MachineView[];
  inboxItems: InboxItemView[];
  onSelectInboxItem: (item: InboxItemView) => void;
}

export function LeftSidebar({
  projects,
  machines,
  inboxItems,
  onSelectInboxItem,
}: LeftSidebarProps) {
  const { selectedProjectId, setSelectedProject } = useWorkbenchStore();
  const [hoveredMachine, setHoveredMachine] = useState<MachineView | null>(null);

  return (
    <aside className="flex w-[232px] shrink-0 flex-col border-r border-zinc-200 bg-zinc-50/70 dark:border-zinc-800/90 dark:bg-zinc-950/80">
      {/* Low-noise Inbox (ONLY numeric badge in the entire workbench) */}
      <div className="border-b border-zinc-200 p-2.5 dark:border-zinc-800/80">
        <div className="mb-1.5 flex items-center justify-between px-1.5">
          <div className="flex items-center gap-1.5 text-[11px] font-semibold uppercase tracking-wider text-zinc-500 dark:text-zinc-400">
            <Inbox className="h-3.5 w-3.5" />
            <span>待拍板收件箱</span>
          </div>
          {inboxItems.length > 0 && (
            <span className="rounded-full bg-sky-500/15 px-1.5 py-0.5 font-mono text-[11px] font-semibold text-sky-600 dark:text-sky-400">
              {inboxItems.length}
            </span>
          )}
        </div>

        {inboxItems.length === 0 ? (
          <div className="rounded-md px-2 py-2 text-xs text-zinc-400 dark:text-zinc-500">
            当前无待拍板事项
          </div>
        ) : (
          <div className="max-h-56 space-y-1 overflow-y-auto pr-0.5">
            {inboxItems.map((item) => (
              <button
                key={item.inboxId}
                type="button"
                onClick={() => onSelectInboxItem(item)}
                className="group w-full rounded-md border border-transparent bg-white px-2 py-1.5 text-left transition hover:border-zinc-300 dark:bg-zinc-900/80 dark:hover:border-zinc-700"
              >
                <div className="flex items-center gap-1.5">
                  {item.category === 'approval_pending' ? (
                    <ShieldAlert className="h-3.5 w-3.5 shrink-0 text-amber-500" />
                  ) : item.category === 'task_blocked' ? (
                    <AlertTriangle className="h-3.5 w-3.5 shrink-0 text-rose-500" />
                  ) : (
                    <PackageCheck className="h-3.5 w-3.5 shrink-0 text-sky-500" />
                  )}
                  <span className="truncate text-xs font-medium text-zinc-800 dark:text-zinc-200">
                    {item.title}
                  </span>
                </div>
                <p className="mt-0.5 truncate pl-5 text-[11px] text-zinc-500 dark:text-zinc-400">
                  {item.summary}
                </p>
              </button>
            ))}
          </div>
        )}
      </div>

      {/* Logical Projects (muted status dots ONLY, never numeric badges) */}
      <div className="flex-1 overflow-y-auto p-2.5">
        <div className="mb-1.5 flex items-center gap-1.5 px-1.5 text-[11px] font-semibold uppercase tracking-wider text-zinc-500 dark:text-zinc-400">
          <FolderGit2 className="h-3.5 w-3.5" />
          <span>逻辑项目</span>
        </div>

        {projects.length === 0 ? (
          <div className="px-2 py-3 text-xs text-zinc-400 dark:text-zinc-500">
            未在 projects.yaml 中配置项目
          </div>
        ) : (
          <nav className="space-y-0.5">
            {projects.map((p) => {
              const selected = selectedProjectId === p.projectId;
              const dotClass =
                p.blockedTaskCount > 0
                  ? 'bg-amber-500'
                  : p.activeTaskCount > 0
                    ? 'bg-sky-500'
                    : 'bg-zinc-300 dark:bg-zinc-700';
              return (
                <button
                  key={p.projectId}
                  type="button"
                  onClick={() => setSelectedProject(p.projectId)}
                  className={`flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-xs transition ${
                    selected
                      ? 'bg-zinc-200/80 font-medium text-zinc-950 dark:bg-zinc-800/90 dark:text-zinc-50'
                      : 'text-zinc-600 hover:bg-zinc-200/40 dark:text-zinc-400 dark:hover:bg-zinc-900'
                  }`}
                >
                  <span className={`h-1.5 w-1.5 shrink-0 rounded-full ${dotClass}`} />
                  <span className="truncate">{p.displayName}</span>
                </button>
              );
            })}
          </nav>
        )}
      </div>

      {/* Compact Machine Status Micro-Strip */}
      <div className="relative border-t border-zinc-200 px-3 py-2 dark:border-zinc-800/80">
        {hoveredMachine && (
          <div className="absolute bottom-11 left-2 right-2 z-20 rounded-md border border-zinc-200 bg-white p-2 text-[11px] shadow-lg dark:border-zinc-700 dark:bg-zinc-900">
            <div className="flex items-center justify-between font-medium text-zinc-800 dark:text-zinc-100">
              <span>{hoveredMachine.displayName}</span>
              <span className="font-mono text-[10px] text-zinc-500">{hoveredMachine.status}</span>
            </div>
            <div className="mt-1 font-mono text-[10px] text-zinc-500 dark:text-zinc-400">
              {hoveredMachine.transportMode} · {hoveredMachine.capabilities.join(', ') || 'no caps'}
            </div>
          </div>
        )}
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
          {machines.map((m) => (
            <button
              key={m.machineId}
              type="button"
              onMouseEnter={() => setHoveredMachine(m)}
              onMouseLeave={() => setHoveredMachine(null)}
              onFocus={() => setHoveredMachine(m)}
              onBlur={() => setHoveredMachine(null)}
              className="flex items-center gap-1.5 text-[11px] text-zinc-500 hover:text-zinc-900 dark:text-zinc-400 dark:hover:text-zinc-100"
            >
              <span
                className={`h-1.5 w-1.5 rounded-full ${
                  m.status === 'online'
                    ? 'bg-emerald-500'
                    : m.status === 'degraded'
                      ? 'bg-amber-500'
                      : 'bg-zinc-400 dark:bg-zinc-600'
                }`}
              />
              <span className="font-mono">{m.machineId}</span>
            </button>
          ))}
        </div>
      </div>
    </aside>
  );
}
