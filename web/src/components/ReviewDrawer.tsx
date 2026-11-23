import { useRef, useState } from 'react';
import { Code2, ExternalLink, FileDiff, Layers, X } from 'lucide-react';
import type {
  DeliveryDetailResponse,
  DeliveryView,
  MachineView,
} from '../generated/protocol';
import { buildIDEJump } from '../lib/ide';
import { useWorkbenchStore } from '../store/useWorkbenchStore';
import { DiffReviewTab } from './DiffReviewTab';
import { RyugiCanvasTab } from './RyugiCanvasTab';

interface ReviewDrawerProps {
  detail?: DeliveryDetailResponse;
  isLoading: boolean;
  allTaskDeliveries: DeliveryView[];
  machines: MachineView[];
  isDark: boolean;
}

export function ReviewDrawer({
  detail,
  isLoading,
  allTaskDeliveries,
  machines,
  isDark,
}: ReviewDrawerProps) {
  const {
    selectedDeliveryId,
    setSelectedDelivery,
    drawerTab,
    setDrawerTab,
    drawerWidth,
    setDrawerWidth,
    localMachineId,
    preferredIDE,
  } = useWorkbenchStore();

  const [selectedFile, setSelectedFile] = useState('');
  const [selectedLine, setSelectedLine] = useState<number | undefined>(undefined);
  const draggingRef = useRef(false);

  if (!selectedDeliveryId) {
    return null;
  }

  const startResize = (e: React.MouseEvent) => {
    e.preventDefault();
    draggingRef.current = true;
    const onMove = (ev: MouseEvent) => {
      if (!draggingRef.current) return;
      const newWidth = window.innerWidth - ev.clientX;
      setDrawerWidth(newWidth);
    };
    const onUp = () => {
      draggingRef.current = false;
      window.removeEventListener('mousemove', onMove);
      window.removeEventListener('mouseup', onUp);
    };
    window.addEventListener('mousemove', onMove);
    window.addEventListener('mouseup', onUp);
  };

  const activeDiffFile =
    detail && selectedFile && detail.diffFiles.some((f) => f.path === selectedFile)
      ? selectedFile
      : detail?.diffFiles[0]?.path;

  const ideJump = detail
    ? buildIDEJump({
        scheme: preferredIDE || detail.project.ideScheme,
        localMachineId,
        delivery: detail.delivery,
        project: detail.project,
        machines,
        file: drawerTab === 'diff' ? activeDiffFile : undefined,
        line: drawerTab === 'diff' ? selectedLine : undefined,
      })
    : null;

  return (
    <aside
      style={{ width: drawerWidth }}
      className="relative flex shrink-0 flex-col border-l border-zinc-200 bg-white dark:border-zinc-800/90 dark:bg-zinc-950"
    >
      {/* Drag Resize Handle */}
      <div
        onMouseDown={startResize}
        title="拖拽调整抽屉宽度"
        className="absolute inset-y-0 -left-1 z-20 w-2 cursor-col-resize hover:bg-sky-500/30"
      />

      {/* Drawer Top Header: Version Switcher + IDE Jump + Close */}
      <div className="flex items-center justify-between gap-2 border-b border-zinc-200 px-3 py-2 dark:border-zinc-800">
        <div className="flex items-center gap-2 min-w-0">
          <select
            value={selectedDeliveryId}
            onChange={(e) => {
              setSelectedFile('');
              setSelectedLine(undefined);
              setSelectedDelivery(e.target.value);
            }}
            className="max-w-[190px] rounded-md border border-zinc-200 bg-zinc-50 px-2 py-1 font-mono text-xs text-zinc-800 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-200"
          >
            {allTaskDeliveries.map((d, idx) => (
              <option key={d.deliveryId} value={d.deliveryId}>
                v{idx + 1} · {d.headSha.slice(0, 7)}
              </option>
            ))}
          </select>

          {ideJump && (
            <a
              href={ideJump.uri}
              title={`${ideJump.mode === 'ssh' ? 'SSH Remote' : '本机直开'} (${ideJump.targetLabel}): ${ideJump.resolvedPath}${ideJump.line ? `:${ideJump.line}` : ''}`}
              className="flex shrink-0 items-center gap-1 rounded-md border border-zinc-200 bg-zinc-50 px-2 py-1 text-xs text-zinc-700 transition hover:border-sky-500/50 dark:border-zinc-800 dark:bg-zinc-900 dark:text-zinc-300"
            >
              <Code2 className="h-3.5 w-3.5 text-sky-500" />
              <span className="hidden sm:inline">在 IDE 中打开</span>
              {ideJump.line && (
                <span className="font-mono text-[10px] text-sky-500">:{ideJump.line}</span>
              )}
              <span className="font-mono text-[10px] text-zinc-400">
                {ideJump.mode === 'ssh' ? 'SSH' : '本地'}
              </span>
              <ExternalLink className="h-3 w-3 opacity-60" />
            </a>
          )}
        </div>

        <button
          type="button"
          onClick={() => setSelectedDelivery(null)}
          className="rounded p-1 text-zinc-400 hover:bg-zinc-100 hover:text-zinc-800 dark:hover:bg-zinc-900 dark:hover:text-zinc-100"
        >
          <X className="h-4 w-4" />
        </button>
      </div>

      {/* Dual Review Tabs */}
      <div className="flex border-b border-zinc-200 px-3 dark:border-zinc-800">
        <button
          type="button"
          onClick={() => setDrawerTab('ryugi')}
          className={`flex items-center gap-1.5 border-b-2 px-3 py-2 text-xs font-medium transition ${
            drawerTab === 'ryugi'
              ? 'border-sky-500 text-sky-600 dark:text-sky-400'
              : 'border-transparent text-zinc-500 hover:text-zinc-800 dark:text-zinc-400 dark:hover:text-zinc-200'
          }`}
        >
          <Layers className="h-3.5 w-3.5" />
          <span>Ryugi 画布</span>
          {detail && detail.ryugiBindings.length > 0 && (
            <span className="rounded bg-zinc-100 px-1 font-mono text-[10px] dark:bg-zinc-800">
              {detail.ryugiBindings.length}
            </span>
          )}
        </button>

        <button
          type="button"
          onClick={() => setDrawerTab('diff')}
          className={`flex items-center gap-1.5 border-b-2 px-3 py-2 text-xs font-medium transition ${
            drawerTab === 'diff'
              ? 'border-sky-500 text-sky-600 dark:text-sky-400'
              : 'border-transparent text-zinc-500 hover:text-zinc-800 dark:text-zinc-400 dark:hover:text-zinc-200'
          }`}
        >
          <FileDiff className="h-3.5 w-3.5" />
          <span>轻量 Diff + 行级批注</span>
          {detail && detail.diffFiles.length > 0 && (
            <span className="rounded bg-zinc-100 px-1 font-mono text-[10px] dark:bg-zinc-800">
              {detail.diffFiles.length}
            </span>
          )}
        </button>
      </div>

      {/* Tab Body */}
      {isLoading || !detail ? (
        <div className="flex flex-1 items-center justify-center text-xs text-zinc-500">
          加载交付详情...
        </div>
      ) : drawerTab === 'ryugi' ? (
        <RyugiCanvasTab detail={detail} />
      ) : (
        <DiffReviewTab
          detail={detail}
          isDark={isDark}
          selectedFile={selectedFile}
          onSelectFile={(f) => {
            setSelectedFile(f);
            setSelectedLine(undefined);
          }}
          onSelectLine={(f, ln) => {
            setSelectedFile(f);
            setSelectedLine(ln);
          }}
        />
      )}
    </aside>
  );
}
