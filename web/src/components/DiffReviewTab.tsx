import { useEffect, useMemo, useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  Diff,
  Hunk,
  computeNewLineNumber,
  computeOldLineNumber,
  getChangeKey,
  parseDiff,
  type ChangeData,
  type FileData,
  type HunkTokens,
  type RenderToken,
  type ViewType,
} from 'react-diff-view';
import { Columns2, FileCode2, MessageSquarePlus, Rows2, Send, X } from 'lucide-react';
import type {
  AnnotationView,
  DeliveryDetailResponse,
  DiffFileStat,
} from '../generated/protocol';
import { api } from '../lib/api';
import { tokenizeHunksWithShiki } from '../lib/shikiDiff';

interface DiffReviewTabProps {
  detail: DeliveryDetailResponse;
  isDark: boolean;
  selectedFile: string;
  onSelectFile: (file: string) => void;
}

interface PendingComposer {
  changeKey: string;
  path: string;
  side: 'old' | 'new';
  line: number;
  contextBefore: string;
  contextAfter: string;
}

export function DiffReviewTab({
  detail,
  isDark,
  selectedFile,
  onSelectFile,
}: DiffReviewTabProps) {
  const queryClient = useQueryClient();
  const [viewType, setViewType] = useState<ViewType>('unified');
  const [composer, setComposer] = useState<PendingComposer | null>(null);
  const [draftBody, setDraftBody] = useState('');
  const [tokens, setTokens] = useState<HunkTokens | null>(null);

  const files = detail.diffFiles;
  const activeFile =
    (selectedFile && files.some((f) => f.path === selectedFile) ? selectedFile : files[0]?.path) ||
    '';

  const diffQuery = useQuery({
    queryKey: ['delivery-diff', detail.delivery.deliveryId, activeFile],
    queryFn: () => api.getDeliveryDiff(detail.delivery.deliveryId, activeFile || undefined),
    enabled: !!detail.delivery.deliveryId && (files.length === 0 || !!activeFile),
  });

  const parsedFiles = useMemo<FileData[]>(() => {
    const text = diffQuery.data?.diffText ?? '';
    if (!text.trim()) return [];
    try {
      return parseDiff(text, { nearbySequences: 'zip' });
    } catch {
      return [];
    }
  }, [diffQuery.data?.diffText]);

  const currentDiffFile = parsedFiles[0] ?? null;

  // Shiki syntax highlighting for the active file's hunks
  useEffect(() => {
    let cancelled = false;
    setTokens(null);
    if (!currentDiffFile || currentDiffFile.hunks.length === 0) return;

    void tokenizeHunksWithShiki(
      currentDiffFile.hunks,
      activeFile || currentDiffFile.newPath || currentDiffFile.oldPath,
      isDark,
    ).then((res) => {
      if (!cancelled) {
        setTokens(res);
      }
    });
    return () => {
      cancelled = true;
    };
  }, [currentDiffFile, activeFile, isDark]);

  const createAnnotationMutation = useMutation({
    mutationFn: (vars: { composer: PendingComposer; body: string }) =>
      api.createAnnotation(detail.delivery.deliveryId, {
        source: 'diff_drawer',
        anchor: {
          path: vars.composer.path,
          side: vars.composer.side,
          startLine: vars.composer.line,
          endLine: vars.composer.line,
          contextBefore: vars.composer.contextBefore,
          contextAfter: vars.composer.contextAfter,
          baseSha: detail.delivery.baseSha,
          headSha: detail.delivery.headSha,
        },
        body: vars.body,
      }),
    onSuccess: () => {
      setComposer(null);
      setDraftBody('');
      void queryClient.invalidateQueries({ queryKey: ['delivery', detail.delivery.deliveryId] });
      void queryClient.invalidateQueries({ queryKey: ['task-timeline', detail.delivery.taskId] });
    },
  });

  // Map existing annotations + active inline composer to react-diff-view `widgets`
  const fileAnnotations = useMemo(() => {
    return detail.annotations.filter(
      (a) => a.source !== 'ryugi' && a.anchor.path === activeFile,
    );
  }, [detail.annotations, activeFile]);

  const widgets = useMemo<Record<string, ReactNode>>(() => {
    if (!currentDiffFile) return {};
    const map: Record<string, ReactNode[]> = {};

    // Index changes by (side, lineNumber) so saved annotations pin cleanly to their exact lines
    const keyBySideLine = new Map<string, string>();
    for (const hunk of currentDiffFile.hunks) {
      for (const ch of hunk.changes) {
        const ck = getChangeKey(ch);
        const newLn = computeNewLineNumber(ch);
        const oldLn = computeOldLineNumber(ch);
        if (newLn > 0) keyBySideLine.set(`new:${newLn}`, ck);
        if (oldLn > 0) keyBySideLine.set(`old:${oldLn}`, ck);
      }
    }

    for (const ann of fileAnnotations) {
      const side = ann.anchor.side === 'old' ? 'old' : 'new';
      const ln = ann.anchor.startLine ?? 0;
      const ck = keyBySideLine.get(`${side}:${ln}`);
      if (!ck) continue;
      if (!map[ck]) map[ck] = [];
      map[ck].push(<InlineAnnotationCard key={ann.annotationId} annotation={ann} />);
    }

    if (composer && composer.path === activeFile) {
      const ck = composer.changeKey;
      if (!map[ck]) map[ck] = [];
      map[ck].push(
        <div
          key="__composer"
          className="border-y border-sky-500/40 bg-sky-500/[0.06] p-3 font-sans dark:bg-sky-950/30"
        >
          <div className="mb-1.5 flex items-center justify-between text-[11px] font-medium text-sky-700 dark:text-sky-300">
            <span className="font-mono">
              批注 {composer.path}:{composer.line} ({composer.side})
            </span>
            <button
              type="button"
              onClick={() => setComposer(null)}
              className="rounded p-0.5 text-zinc-400 hover:text-zinc-700 dark:hover:text-zinc-200"
            >
              <X className="h-3.5 w-3.5" />
            </button>
          </div>
          <textarea
            value={draftBody}
            onChange={(e) => setDraftBody(e.target.value)}
            rows={2}
            placeholder="写下针对该行的具体改动意见..."
            className="w-full rounded-md border border-zinc-300 bg-white px-2.5 py-1.5 text-base sm:text-xs text-zinc-900 focus:border-sky-500 focus:outline-none dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100"
          />
          <div className="mt-2 flex items-center justify-end gap-2">
            <button
              type="button"
              onClick={() => setComposer(null)}
              className="rounded-md px-2.5 py-1 text-xs text-zinc-500 hover:text-zinc-800 dark:hover:text-zinc-200"
            >
              取消
            </button>
            <button
              type="button"
              disabled={!draftBody.trim() || createAnnotationMutation.isPending}
              onClick={() =>
                createAnnotationMutation.mutate({
                  composer,
                  body: draftBody.trim(),
                })
              }
              className="flex items-center gap-1 rounded-md bg-sky-600 px-3 py-1 text-xs font-medium text-white transition hover:bg-sky-500 disabled:opacity-50"
            >
              <Send className="h-3 w-3" />
              <span>{createAnnotationMutation.isPending ? '保存中...' : '保存批注'}</span>
            </button>
          </div>
          {createAnnotationMutation.isError && (
            <p className="mt-1 text-[11px] text-rose-500">
              {(createAnnotationMutation.error as Error).message}
            </p>
          )}
        </div>,
      );
    }

    const combined: Record<string, ReactNode> = {};
    for (const [k, nodes] of Object.entries(map)) {
      combined[k] = <div>{nodes}</div>;
    }
    return combined;
  }, [currentDiffFile, fileAnnotations, composer, activeFile, draftBody, createAnnotationMutation]);

  const handleOpenComposer = (change: ChangeData | null, preferredSide?: 'old' | 'new') => {
    if (!change || !currentDiffFile) return;
    const newLn = computeNewLineNumber(change);
    const oldLn = computeOldLineNumber(change);
    const side: 'old' | 'new' =
      preferredSide === 'old' && oldLn > 0
        ? 'old'
        : newLn > 0
          ? 'new'
          : 'old';
    const line = side === 'new' ? newLn : oldLn;
    if (line <= 0) return;

    // Extract nearby context lines for stable anchoring
    const allChanges = currentDiffFile.hunks.flatMap((h) => h.changes);
    const idx = allChanges.indexOf(change);
    const contextBefore = idx > 0 ? allChanges[idx - 1].content.slice(0, 120) : '';
    const contextAfter =
      idx >= 0 && idx + 1 < allChanges.length ? allChanges[idx + 1].content.slice(0, 120) : '';

    setComposer({
      changeKey: getChangeKey(change),
      path: activeFile,
      side,
      line,
      contextBefore,
      contextAfter,
    });
    setDraftBody('');
  };

  const renderShikiToken: RenderToken = (token, defaultRender, index) => {
    if (token.type === 'shiki') {
      return (
        <span key={index} style={{ color: token.color as string | undefined }}>
          {token.value as string}
        </span>
      );
    }
    return defaultRender(token, index);
  };

  if (files.length === 0) {
    return (
      <div className="flex flex-1 items-center justify-center p-8 text-xs text-zinc-500">
        本次交付相对于审阅基线无代码文本差异（baseSha == headSha）。
      </div>
    );
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      {/* Sub-toolbar: File strip + Unified/Split switch */}
      <div className="flex items-center justify-between gap-2 border-b border-zinc-200 px-3 py-2 dark:border-zinc-800">
        <div className="flex items-center gap-1.5 text-[11px] text-zinc-500">
          <MessageSquarePlus className="h-3.5 w-3.5 text-sky-500" />
          <span>点击任意行号即可原地添加行级批注</span>
        </div>

        <div className="flex items-center rounded-md border border-zinc-200 p-0.5 dark:border-zinc-800">
          <button
            type="button"
            onClick={() => setViewType('unified')}
            className={`flex items-center gap-1 rounded px-2 py-0.5 text-[11px] transition ${
              viewType === 'unified'
                ? 'bg-zinc-200 font-medium text-zinc-900 dark:bg-zinc-800 dark:text-zinc-100'
                : 'text-zinc-500 hover:text-zinc-800 dark:hover:text-zinc-200'
            }`}
          >
            <Rows2 className="h-3 w-3" />
            <span>Unified</span>
          </button>
          <button
            type="button"
            onClick={() => setViewType('split')}
            className={`flex items-center gap-1 rounded px-2 py-0.5 text-[11px] transition ${
              viewType === 'split'
                ? 'bg-zinc-200 font-medium text-zinc-900 dark:bg-zinc-800 dark:text-zinc-100'
                : 'text-zinc-500 hover:text-zinc-800 dark:hover:text-zinc-200'
            }`}
          >
            <Columns2 className="h-3 w-3" />
            <span>Split</span>
          </button>
        </div>
      </div>

      {/* File Tree + Diff Viewer */}
      <div className="flex min-h-0 flex-1 flex-col md:flex-row">
        {/* Compact Changed Files List */}
        <div className="max-h-36 shrink-0 overflow-y-auto border-b border-zinc-200 bg-zinc-50/70 p-1.5 md:max-h-none md:w-44 md:border-b-0 md:border-r dark:border-zinc-800 dark:bg-zinc-950/60">
          {files.map((f: DiffFileStat) => {
            const active = f.path === activeFile;
            const annCount = detail.annotations.filter(
              (a) => a.source !== 'ryugi' && a.anchor.path === f.path,
            ).length;
            return (
              <button
                key={f.path}
                type="button"
                onClick={() => {
                  onSelectFile(f.path);
                  setComposer(null);
                }}
                className={`mb-0.5 flex w-full flex-col rounded px-2 py-1.5 text-left transition ${
                  active
                    ? 'bg-sky-500/12 text-zinc-950 dark:text-zinc-50'
                    : 'text-zinc-600 hover:bg-zinc-200/50 dark:text-zinc-400 dark:hover:bg-zinc-900'
                }`}
              >
                <div className="flex items-center gap-1.5 min-w-0">
                  <FileCode2 className="h-3 w-3 shrink-0 opacity-70" />
                  <span className="truncate font-mono text-[11px]" title={f.path}>
                    {f.path.split('/').pop()}
                  </span>
                </div>
                <div className="mt-0.5 flex items-center justify-between pl-4 font-mono text-[10px]">
                  <span className="text-zinc-400 truncate max-w-[85px]">{f.status}</span>
                  <span>
                    <span className="text-emerald-600 dark:text-emerald-400">+{f.insertions}</span>{' '}
                    <span className="text-rose-600 dark:text-rose-400">-{f.deletions}</span>
                    {annCount > 0 && (
                      <span className="ml-1 text-amber-500">·{annCount}</span>
                    )}
                  </span>
                </div>
              </button>
            );
          })}
        </div>

        {/* Highlighted Diff Content */}
        <div className="orbit-diff min-w-0 flex-1 overflow-auto bg-white dark:bg-[#0c0c0f]">
          {diffQuery.isLoading ? (
            <div className="p-6 text-center text-xs text-zinc-500">加载 Diff...</div>
          ) : !currentDiffFile ? (
            <div className="p-6 text-center text-xs text-zinc-500">
              该文件为二进制或无文本差异。
            </div>
          ) : (
            <div>
              <div className="sticky top-0 z-10 border-b border-zinc-200 bg-zinc-50 px-3 py-1.5 font-mono text-[11px] text-zinc-600 dark:border-zinc-800 dark:bg-zinc-900 dark:text-zinc-300">
                {activeFile}
              </div>
              <Diff
                viewType={viewType}
                diffType={currentDiffFile.type}
                hunks={currentDiffFile.hunks}
                tokens={tokens}
                renderToken={renderShikiToken}
                widgets={widgets}
                gutterEvents={{
                  onClick: ({ change, side }) => handleOpenComposer(change, side),
                }}
              >
                {(hunks) => hunks.map((hunk) => <Hunk key={hunk.content} hunk={hunk} />)}
              </Diff>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

function InlineAnnotationCard({ annotation }: { annotation: AnnotationView }) {
  return (
    <div className="border-y border-amber-500/30 bg-amber-500/[0.06] px-3 py-2 font-sans text-xs dark:bg-amber-500/[0.08]">
      <div className="flex items-center justify-between font-mono text-[10px] text-amber-700 dark:text-amber-300">
        <span>
          行评 · {annotation.anchor.path}:{annotation.anchor.startLine} ({annotation.anchor.side})
        </span>
        <span className="rounded bg-amber-500/15 px-1.5 py-0.5">{annotation.status}</span>
      </div>
      <p className="mt-1 whitespace-pre-wrap text-zinc-800 dark:text-zinc-100">
        {annotation.body}
      </p>
    </div>
  );
}
