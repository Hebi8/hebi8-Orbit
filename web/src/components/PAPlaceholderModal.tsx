import { Command, X } from 'lucide-react';
import type { BootstrapResponse } from '../generated/protocol';
import { useWorkbenchStore } from '../store/useWorkbenchStore';

export function PAPlaceholderModal({ bootstrap }: { bootstrap?: BootstrapResponse }) {
  const { showPAOverlay, setShowPAOverlay } = useWorkbenchStore();
  if (!showPAOverlay) return null;

  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center bg-black/50 p-4 pt-20 backdrop-blur-[2px]">
      <div className="w-full max-w-lg rounded-xl border border-zinc-200 bg-white shadow-2xl dark:border-zinc-800 dark:bg-zinc-900">
        <div className="flex items-center justify-between border-b border-zinc-200 px-4 py-3 dark:border-zinc-800">
          <div className="flex items-center gap-2 text-sm font-semibold">
            <Command className="h-4 w-4 text-sky-500" />
            <span>全局 PA 控制口（Phase 3 占位）</span>
          </div>
          <button
            type="button"
            onClick={() => setShowPAOverlay(false)}
            className="rounded p-1 text-zinc-400 hover:text-zinc-700 dark:hover:text-zinc-200"
          >
            <X className="h-4 w-4" />
          </button>
        </div>

        <div className="p-4 space-y-3 text-xs text-zinc-600 dark:text-zinc-400">
          <p className="leading-relaxed">
            当前为 Phase 0 不可变交付与双审阅抽屉里程碑。Phase 3 接入全局 PA Runtime 后，将通过统一事件日志固定上界水位与 8 项确定性控制指令实现随时可达的全网调度。
          </p>
          {bootstrap && (
            <div className="grid grid-cols-3 gap-2 rounded-lg border border-zinc-200 bg-zinc-50 p-3 font-mono text-[11px] dark:border-zinc-800 dark:bg-zinc-950">
              <div>
                <div className="text-zinc-400">Projects</div>
                <div className="text-sm font-semibold text-zinc-900 dark:text-zinc-100">
                  {bootstrap.projects.length}
                </div>
              </div>
              <div>
                <div className="text-zinc-400">Open Inbox</div>
                <div className="text-sm font-semibold text-sky-500">
                  {bootstrap.openInboxItems.length}
                </div>
              </div>
              <div>
                <div className="text-zinc-400">Snapshot Seq</div>
                <div className="text-sm font-semibold text-zinc-900 dark:text-zinc-100">
                  #{bootstrap.snapshotSeq}
                </div>
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
