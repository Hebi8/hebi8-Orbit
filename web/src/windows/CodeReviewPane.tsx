import { useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react';
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
  type GutterOptions,
  type HunkData,
  type HunkTokens,
  type RenderToken,
  type ViewType,
} from 'react-diff-view';
import { Check, ChevronUp, MessageSquare, PanelLeft, Trash2 } from 'lucide-react';
import type { AnnotationView, BootstrapResponse, DiffFileStat, TaskTimelineResponse } from '../generated/protocol';
import { IdeOpenButton } from '../components/IdeOpenButton';
import { useDelivery } from '../hooks/queries';
import { ApiError, api, errMsg, makeIdempotencyKey } from '../lib/api';
import { shortSha } from '../lib/format';
import { tokenizeHunksWithShiki } from '../lib/shikiDiff';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

const CODE_KINDS = new Set(['code_diff', 'mixed', 'workspace_snapshot']);

interface LineDraft {
  deliveryId: string;
  anchorChangeKey: string;
  endChangeKey: string;
  path: string;
  side: 'old' | 'new';
  startLine: number;
  endLine: number;
  quote?: string;
  contextBefore: string;
  contextAfter: string;
  idempotencyKey: string;
}

interface TextSelPop {
  x: number;
  y: number;
  quote: string;
  side: 'old' | 'new';
  line: number;
  change: ChangeData;
}

/** Context lines strictly within the same hunk and the same side, so anchors never leak across hunks. */
function sameHunkSideContext(hunks: HunkData[], target: ChangeData, side: 'old' | 'new') {
  const hunk = hunks.find((h) => h.changes.includes(target));
  if (!hunk) return { contextBefore: '', contextAfter: '' };
  const lineFn = side === 'old' ? computeOldLineNumber : computeNewLineNumber;
  const same = hunk.changes.filter((ch) => lineFn(ch) > 0);
  const idx = same.indexOf(target);
  if (idx < 0) return { contextBefore: '', contextAfter: '' };
  return {
    contextBefore: idx > 0 ? same[idx - 1].content.slice(0, 120) : '',
    contextAfter: idx + 1 < same.length ? same[idx + 1].content.slice(0, 120) : '',
  };
}

function groupByDir(files: DiffFileStat[]): [string, DiffFileStat[]][] {
  const m = new Map<string, DiffFileStat[]>();
  for (const f of files) {
    const i = f.path.lastIndexOf('/');
    const dir = i >= 0 ? f.path.slice(0, i) : '';
    if (!m.has(dir)) m.set(dir, []);
    m.get(dir)!.push(f);
  }
  return [...m.entries()].sort((a, b) => a[0].localeCompare(b[0]));
}

function isDiffAnnotation(a: AnnotationView) {
  return a.source !== 'ryugi';
}

function formatLineAnchor(a: AnnotationView): string {
  const path = a.anchor.path?.split('/').pop() || a.anchor.path || '';
  const sideLabel = a.anchor.side === 'old' ? '左侧' : '右侧';
  const s = a.anchor.startLine ?? 0;
  const e = a.anchor.endLine ?? s;
  if (s <= 0) return path;
  return e > s ? `${path} · ${sideLabel} L${s}–L${e}` : `${path} · ${sideLabel} L${s}`;
}

export function CodeReviewPane({
  taskId,
  deliveryId,
  onSelectDelivery,
  tl,
  boot,
  onClose,
  titleSwitcher,
  headerEnd,
}: {
  taskId: string;
  deliveryId: string;
  onSelectDelivery: (id: string) => void;
  tl?: TaskTimelineResponse;
  boot: BootstrapResponse;
  onClose: () => void;
  titleSwitcher?: ReactNode;
  headerEnd?: ReactNode;
}) {
  const qc = useQueryClient();
  const theme = useWorkbenchStore((s) => s.theme);
  const [file, setFile] = useState('');
  const [viewType, setViewType] = useState<ViewType>('unified');
  const [treeOverride, setTreeOverride] = useState<boolean | null>(null);
  const [draft, setDraft] = useState<LineDraft | null>(null);
  const [body, setBody] = useState('');
  const [tokens, setTokens] = useState<HunkTokens | null>(null);
  const [lastLine, setLastLine] = useState<{ path: string; line?: number } | null>(null);
  const [railPopOpen, setRailPopOpen] = useState(false);
  const [textSel, setTextSel] = useState<TextSelPop | null>(null);
  const diffPaneRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    setFile('');
    setDraft(null);
    setBody('');
    setTreeOverride(null);
    setTextSel(null);
    setRailPopOpen(false);
  }, [deliveryId]);

  const versions = (tl?.deliveries ?? []).filter((d) => CODE_KINDS.has(d.kind));
  const effectiveDeliveryId =
    versions.some((v) => v.deliveryId === deliveryId)
      ? deliveryId
      : versions[versions.length - 1]?.deliveryId ?? deliveryId;

  const dq = useDelivery(effectiveDeliveryId);
  const detail = dq.data;
  const files = detail?.diffFiles ?? [];
  const activeFile = (file && files.some((f) => f.path === file) ? file : files[0]?.path) || '';
  const isNarrow = typeof window !== 'undefined' && window.innerWidth < 768;
  const defaultTreeOpen = !isNarrow && files.length > 1;
  const treeOpen = treeOverride ?? defaultTreeOpen;
  const effectiveViewType: ViewType = viewType;

  const diffQ = useQuery({
    queryKey: ['delivery-diff', effectiveDeliveryId, activeFile],
    queryFn: () => api.getDeliveryDiff(effectiveDeliveryId, activeFile || undefined),
    enabled: !!effectiveDeliveryId && !!activeFile,
  });
  const parsed = useMemo<FileData | null>(() => {
    const text = diffQ.data?.diffText ?? '';
    if (!text.trim()) return null;
    try {
      return parseDiff(text, { nearbySequences: 'zip' })[0] ?? null;
    } catch {
      return null;
    }
  }, [diffQ.data?.diffText]);

  useEffect(() => {
    let cancelled = false;
    setTokens(null);
    if (!parsed || parsed.hunks.length === 0) return;
    void tokenizeHunksWithShiki(parsed.hunks, activeFile || parsed.newPath || parsed.oldPath, theme === 'dark').then((res) => {
      if (!cancelled) setTokens(res);
    });
    return () => {
      cancelled = true;
    };
  }, [parsed, activeFile, theme]);

  const annotations = useMemo(() => (detail?.annotations ?? []).filter(isDiffAnnotation), [detail?.annotations]);
  const openAnnotations = useMemo(() => annotations.filter((a) => a.status === 'open'), [annotations]);
  const fileAnns = useMemo(() => annotations.filter((a) => a.anchor.path === activeFile), [annotations, activeFile]);

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['delivery'] });
    void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] });
    void qc.invalidateQueries({ queryKey: ['project-tasks'] });
    void qc.invalidateQueries({ queryKey: ['all-tasks'] });
    void qc.invalidateQueries({ queryKey: ['bootstrap'] });
  };

  const create = useMutation({
    mutationFn: (v: { d: LineDraft; body: string }) =>
      api.createAnnotation(
        v.d.deliveryId,
        {
          source: 'diff_drawer',
          anchor: {
            path: v.d.path,
            side: v.d.side,
            startLine: v.d.startLine,
            endLine: v.d.endLine,
            quote: v.d.quote,
            contextBefore: v.d.contextBefore,
            contextAfter: v.d.contextAfter,
            baseSha: detail?.delivery.baseSha,
            headSha: detail?.delivery.headSha,
          },
          body: v.body,
        },
        v.d.idempotencyKey,
      ),
    onSuccess: () => {
      setDraft(null);
      setBody('');
      setTextSel(null);
      refresh();
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const removeAnn = useMutation({
    mutationFn: (annotationId: string) => api.deleteAnnotation(annotationId),
    onSuccess: () => {
      refresh();
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const checkpoint = useMutation({
    mutationFn: () => api.completeDeliveryReview(effectiveDeliveryId),
    onSuccess: () => {
      toast('已标记 Checkpoint（发送消息前可再次点击撤销）');
      refresh();
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const undoCheckpoint = useMutation({
    mutationFn: () => api.reopenDeliveryReview(effectiveDeliveryId),
    onSuccess: () => {
      toast('已撤销 Checkpoint 标记');
      refresh();
    },
    onError: (e) => {
      if (e instanceof ApiError && e.code === 'ERR_CHECKPOINT_LOCKED') {
        toast('已发送新消息，该 Checkpoint 已锁定不可撤销', 'err');
        refresh();
      } else {
        toast(errMsg(e), 'err');
      }
    },
  });

  // Lookup maps for mapping line numbers <-> change keys
  const lineIndex = useMemo(() => {
    const keyBySideLine = new Map<string, string>();
    const changeBySideLine = new Map<string, ChangeData>();
    if (!parsed) return { keyBySideLine, changeBySideLine };
    for (const h of parsed.hunks) {
      for (const ch of h.changes) {
        const ck = getChangeKey(ch);
        const n = computeNewLineNumber(ch);
        const o = computeOldLineNumber(ch);
        if (n > 0) {
          keyBySideLine.set(`new:${n}`, ck);
          changeBySideLine.set(`new:${n}`, ch);
        }
        if (o > 0) {
          keyBySideLine.set(`old:${o}`, ck);
          changeBySideLine.set(`old:${o}`, ch);
        }
      }
    }
    return { keyBySideLine, changeBySideLine };
  }, [parsed]);

  const openComposer = (change: ChangeData, clicked: 'old' | 'new', extendRange: boolean, quote?: string) => {
    if (!parsed || !detail) return;
    const oldLn = computeOldLineNumber(change);
    const newLn = computeNewLineNumber(change);
    let side = clicked;
    let line = side === 'old' ? oldLn : newLn;
    if (line <= 0) {
      side = side === 'old' ? 'new' : 'old';
      line = side === 'old' ? oldLn : newLn;
    }
    if (line <= 0) return;

    setLastLine({ path: activeFile, line: newLn > 0 ? newLn : undefined });
    setTextSel(null);

    // Shift+click extends current draft line range on the same file & side
    if (extendRange && draft && draft.path === activeFile && draft.side === side && draft.deliveryId === detail.delivery.deliveryId) {
      const startLine = Math.min(draft.startLine, line);
      const endLine = Math.max(draft.startLine, line);
      const endChange = lineIndex.changeBySideLine.get(`${side}:${endLine}`) ?? change;
      const endKey = lineIndex.keyBySideLine.get(`${side}:${endLine}`) ?? getChangeKey(change);
      const ctx = sameHunkSideContext(parsed.hunks, endChange, side);
      setDraft({
        ...draft,
        startLine,
        endLine,
        endChangeKey: endKey,
        quote: undefined,
        ...ctx,
        idempotencyKey: makeIdempotencyKey(),
      });
      return;
    }

    const ctx = sameHunkSideContext(parsed.hunks, change, side);
    const ck = getChangeKey(change);
    setDraft({
      deliveryId: detail.delivery.deliveryId,
      anchorChangeKey: ck,
      endChangeKey: ck,
      path: activeFile,
      side,
      startLine: line,
      endLine: line,
      quote,
      ...ctx,
      idempotencyKey: makeIdempotencyKey(),
    });
    setBody('');
  };

  // Highlight selected line range for active draft AND for open range comments (works across Unified and Split views)
  const selectedChanges = useMemo<string[]>(() => {
    if (!parsed) return [];
    const keys = new Set<string>();
    const addRange = (side: 'old' | 'new', start: number, end: number) => {
      for (let ln = start; ln <= end; ln++) {
        const ck = lineIndex.keyBySideLine.get(`${side}:${ln}`);
        if (ck) keys.add(ck);
      }
    };
    if (draft && draft.deliveryId === effectiveDeliveryId && draft.path === activeFile) {
      addRange(draft.side, draft.startLine, draft.endLine);
    }
    for (const a of fileAnns) {
      if (a.status !== 'open') continue;
      const side = a.anchor.side === 'old' ? 'old' : 'new';
      const s = a.anchor.startLine ?? 0;
      const e = a.anchor.endLine ?? s;
      if (s > 0) addRange(side, s, Math.max(s, e));
    }
    return [...keys];
  }, [parsed, draft, effectiveDeliveryId, activeFile, fileAnns, lineIndex]);

  // Handle mouseup text selection inside code cells for inline quote commenting (Split & Unified aware)
  const handleCodeMouseUp = () => {
    const sel = window.getSelection();
    if (!sel || sel.isCollapsed || !diffPaneRef.current || !parsed) {
      setTextSel(null);
      return;
    }
    const rawQuote = sel.toString().trim();
    if (!rawQuote || rawQuote.length > 240) {
      setTextSel(null);
      return;
    }
    const anchorEl =
      sel.anchorNode instanceof HTMLElement ? sel.anchorNode : sel.anchorNode?.parentElement ?? null;
    const rowEl = anchorEl?.closest('tr.diff-line') as HTMLElement | null;
    const codeCell = anchorEl?.closest('td.diff-code') as HTMLElement | null;
    // In split mode, each row has two code cells; check the immediately preceding gutter cell first.
    const adjacentGutter = codeCell?.previousElementSibling?.querySelector('button[data-diff-line]') as HTMLButtonElement | null;
    const gutterBtn = adjacentGutter ?? (rowEl?.querySelector('button[data-diff-line]') as HTMLButtonElement | null);
    if (!rowEl || !gutterBtn) {
      setTextSel(null);
      return;
    }
    const side = (gutterBtn.dataset.diffSide === 'old' ? 'old' : 'new') as 'old' | 'new';
    const line = Number(gutterBtn.dataset.diffLine || 0);
    const change = lineIndex.changeBySideLine.get(`${side}:${line}`);
    if (!change || line <= 0) {
      setTextSel(null);
      return;
    }
    const rangeRect = sel.getRangeAt(0).getBoundingClientRect();
    const paneRect = diffPaneRef.current.getBoundingClientRect();
    setTextSel({
      x: Math.max(16, Math.min(rangeRect.left - paneRect.left + diffPaneRef.current.scrollLeft, paneRect.width - 150)),
      y: Math.max(40, rangeRect.top - paneRect.top + diffPaneRef.current.scrollTop - 34),
      quote: rawQuote,
      side,
      line,
      change,
    });
  };

  const widgets = useMemo<Record<string, ReactNode>>(() => {
    if (!parsed) return {};
    const map: Record<string, ReactNode[]> = {};
    for (const a of fileAnns) {
      const side = a.anchor.side === 'old' ? 'old' : 'new';
      const anchorLine = a.anchor.endLine && a.anchor.endLine > 0 ? a.anchor.endLine : a.anchor.startLine ?? 0;
      const ck =
        lineIndex.keyBySideLine.get(`${side}:${anchorLine}`) ??
        lineIndex.keyBySideLine.get(`${side}:${a.anchor.startLine ?? 0}`);
      if (!ck) continue;
      (map[ck] ??= []).push(
        <div className="gh-comment-wrap" key={a.annotationId}>
          <div className="gh-comment-hd">
            <span className="gh-comment-anchor">{formatLineAnchor(a)}</span>
            <span className="gh-comment-hd-right">
              {a.status === 'open' ? (
                <span className="gh-pending-tag">Pending</span>
              ) : (
                <span className="stt">{a.status === 'queued' ? '待发出' : a.status}</span>
              )}
              {a.status === 'open' && (
                <button
                  type="button"
                  className="gh-del-btn"
                  title="删除这条批注"
                  aria-label="删除批注"
                  disabled={removeAnn.isPending}
                  onClick={() => removeAnn.mutate(a.annotationId)}
                >
                  <Trash2 size={12} strokeWidth={1.7} />
                </button>
              )}
            </span>
          </div>
          {a.anchor.quote && <div className="gh-comment-quote">“{a.anchor.quote}”</div>}
          <div className="gh-comment-body">{a.body}</div>
        </div>,
      );
    }

    if (draft && draft.deliveryId === effectiveDeliveryId && draft.path === activeFile) {
      const sideLabel = draft.side === 'old' ? '左侧' : '右侧';
      const locLabel =
        draft.endLine > draft.startLine
          ? `${draft.path.split('/').pop()} · ${sideLabel} L${draft.startLine}–L${draft.endLine}`
          : `${draft.path.split('/').pop()} · ${sideLabel} L${draft.startLine}`;
      (map[draft.endChangeKey] ??= []).push(
        <div className="gh-comment-wrap gh-comment-draft" key="__draft">
          <div className="gh-comment-hd">
            <span className="gh-comment-anchor">{locLabel}</span>
            <span className="faint" style={{ fontSize: 10.5 }}>
              Shift+点击行号可扩展行段
            </span>
          </div>
          {draft.quote && <div className="gh-comment-quote">“{draft.quote}”</div>}
          <div className="gh-draft-inner">
            <textarea
              autoFocus
              value={body}
              placeholder="写下修改意见…"
              onChange={(e) => {
                setBody(e.target.value);
                setDraft((d) => (d ? { ...d, idempotencyKey: makeIdempotencyKey() } : d));
              }}
              onKeyDown={(e) => {
                if (e.key === 'Escape') {
                  e.preventDefault();
                  e.stopPropagation();
                  setDraft(null);
                }
                if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && body.trim()) {
                  e.preventDefault();
                  create.mutate({ d: draft, body: body.trim() });
                }
              }}
            />
            <div className="gh-draft-foot">
              <span className="faint" style={{ fontSize: 11, marginRight: 'auto' }}>
                ⌘↵ 添加到待发批注
              </span>
              <button type="button" className="btn ghost" onClick={() => setDraft(null)}>
                取消
              </button>
              <button
                type="button"
                className="btn pri"
                disabled={!body.trim() || create.isPending}
                onClick={() => create.mutate({ d: draft, body: body.trim() })}
              >
                添加评论
              </button>
            </div>
          </div>
        </div>,
      );
    }

    const out: Record<string, ReactNode> = {};
    if (effectiveViewType === 'split') {
      // In split mode, merge paired delete+insert rows into ONE shared React element assigned to BOTH keys
      // so react-diff-view's SplitWidget natively emits a single full-width `<td colSpan={4}>`.
      const handled = new Set<string>();
      for (const hunk of parsed.hunks) {
        const changes = hunk.changes;
        for (let i = 0; i < changes.length; i++) {
          const cur = changes[i];
          if (cur.type === 'delete' && i + 1 < changes.length && changes[i + 1].type === 'insert') {
            const delKey = getChangeKey(cur);
            const insKey = getChangeKey(changes[i + 1]);
            const combined = [...(map[delKey] ?? []), ...(map[insKey] ?? [])];
            if (combined.length > 0) {
              const sharedEl = <div className="cmrow">{combined}</div>;
              out[delKey] = sharedEl;
              out[insKey] = sharedEl;
            }
            handled.add(delKey);
            handled.add(insKey);
            i += 1;
          }
        }
      }
      for (const [k, nodes] of Object.entries(map)) {
        if (!handled.has(k)) out[k] = <div className="cmrow">{nodes}</div>;
      }
    } else {
      for (const [k, nodes] of Object.entries(map)) out[k] = <div className="cmrow">{nodes}</div>;
    }
    return out;
  }, [parsed, fileAnns, draft, body, effectiveDeliveryId, activeFile, create, removeAnn, lineIndex, effectiveViewType]);

  // For pure-insert / pure-delete lines in Split Diff mode, react-diff-view renders two colSpan=2 cells
  // (one empty, one with widget content). Promote the populated cell to full-width colSpan=4 without
  // mutating React's DOM tree structure (empty sibling is merely display:none).
  useLayoutEffect(() => {
    const pane = diffPaneRef.current;
    if (!pane || effectiveViewType !== 'split') return;
    const rows = pane.querySelectorAll<HTMLTableRowElement>('table.diff-split tr.diff-widget');
    for (const row of rows) {
      const cells = Array.from(row.children).filter(
        (el): el is HTMLTableCellElement => el.tagName === 'TD' && el.classList.contains('diff-widget-content'),
      );
      if (cells.length === 2) {
        const [left, right] = cells;
        const leftHas = left.childNodes.length > 0;
        const rightHas = right.childNodes.length > 0;
        if (leftHas && !rightHas) {
          left.colSpan = 4;
          left.style.display = '';
          right.style.display = 'none';
        } else if (rightHas && !leftHas) {
          right.colSpan = 4;
          right.style.display = '';
          left.style.display = 'none';
        } else {
          left.colSpan = 2;
          right.colSpan = 2;
          left.style.display = '';
          right.style.display = '';
        }
      }
    }
  }, [effectiveViewType, widgets]);

  const renderGutter = ({ change, side, renderDefault }: GutterOptions) => {
    const ln = side === 'old' ? computeOldLineNumber(change) : computeNewLineNumber(change);
    if (ln <= 0) return renderDefault();
    return (
      <button
        type="button"
        className="gbtn"
        data-diff-line={ln}
        data-diff-side={side}
        title={`点击第 ${ln} 行留批注（Shift+点击多行选区）`}
        onClick={(e) => {
          e.stopPropagation();
          openComposer(change, side, e.shiftKey);
        }}
      >
        {renderDefault()}
      </button>
    );
  };

  const renderToken: RenderToken = (token, defaultRender, i) => {
    if (token.type === 'shiki') {
      return (
        <span key={i} style={{ color: token.color as string | undefined }}>
          {token.value as string}
        </span>
      );
    }
    return defaultRender(token, i);
  };

  const project = boot.projects.find((p) => p.projectId === detail?.delivery.projectId) ?? detail?.project;
  const d = detail?.delivery;
  const openCountForFile = (p: string) => openAnnotations.filter((a) => a.anchor.path === p).length;

  const isCheckpointed = detail?.reviewState === 'resolved';
  const canUndoCheckpoint = isCheckpointed && !!detail?.canUndoReview;
  const isCheckpointLocked = isCheckpointed && !detail?.canUndoReview;
  const isReviewUnavailable = !detail?.reviewState || detail.reviewState === 'dismissed';
  const checkpointBusy = checkpoint.isPending || undoCheckpoint.isPending;

  return (
    <>
      <div className="rev-h">
        {files.length > 0 && (
          <button
            type="button"
            className={`ib ${treeOpen ? 'on' : ''}`}
            title={treeOpen ? '收起文件树' : '展开文件树'}
            onClick={() => setTreeOverride(!treeOpen)}
          >
            <PanelLeft size={14} strokeWidth={1.6} />
          </button>
        )}
        {titleSwitcher}
        {versions.length >= 2 && (
          <select
            className="rev-ver mono"
            value={effectiveDeliveryId}
            onChange={(e) => onSelectDelivery(e.target.value)}
            aria-label="版本"
          >
            {versions.map((v, i) => (
              <option key={v.deliveryId} value={v.deliveryId}>
                v{i + 1} · {shortSha(v.headSha)}
              </option>
            ))}
          </select>
        )}
        {d && (
          <span className="rev-stat mono no-mobile">
            <span className="add">+{d.manifest.insertions}</span> <span className="del">−{d.manifest.deletions}</span>
          </span>
        )}
        <div className="rev-h-acts">
          <div className="seg rev-view-seg" role="group" aria-label="Diff 视图模式">
            <button
              type="button"
              className={viewType === 'unified' ? 'on' : ''}
              onClick={() => setViewType('unified')}
            >
              统一
            </button>
            <button
              type="button"
              className={viewType === 'split' ? 'on' : ''}
              onClick={() => setViewType('split')}
            >
              分栏
            </button>
          </div>
          {project && d && (
            <div className="rev-ide-slot">
              <IdeOpenButton
                project={project}
                delivery={d}
                machines={boot.machines}
                file={lastLine?.path ?? activeFile}
                line={lastLine?.line}
              />
            </div>
          )}
          {d && (
            <button
              type="button"
              className={`btn-checkpoint ${isCheckpointed ? 'is-checked' : ''} ${isCheckpointLocked || isReviewUnavailable ? 'is-locked' : ''}`}
              disabled={checkpointBusy || isCheckpointLocked || isReviewUnavailable}
              title={
                detail?.reviewState === 'dismissed'
                  ? '该交付审阅已关闭（任务已归档或已忽略）'
                  : !detail?.reviewState
                    ? '当前交付无可审阅状态'
                    : canUndoCheckpoint
                      ? '已标记 Checkpoint（发送消息前再次点击可撤销）'
                      : isCheckpointLocked
                        ? '已发送新消息，该 Checkpoint 已确认锁定不可撤销'
                        : '确认无误并记为 Checkpoint（发送消息前可再次点击撤销）'
              }
              onClick={() => {
                if (isReviewUnavailable) return;
                if (canUndoCheckpoint) undoCheckpoint.mutate();
                else if (!isCheckpointed) checkpoint.mutate();
              }}
            >
              <Check size={12} strokeWidth={2.2} />
              <span>
                {detail?.reviewState === 'dismissed'
                  ? '已关闭'
                  : canUndoCheckpoint
                    ? '已 Check · 撤销'
                    : isCheckpointLocked
                      ? '已 Checkpoint'
                      : 'Checkpoint'}
              </span>
            </button>
          )}
          {headerEnd}
        </div>
      </div>

      <div className={`cr-body ${treeOpen ? '' : 'tree-collapsed'}`}>
        {treeOpen && (
          <div className="ftree">
            <div className="ftree-scroll">
              {dq.isLoading && <div className="fdir">载入文件…</div>}
              {groupByDir(files).map(([dir, fs]) => (
                <div key={dir || '/'}>
                  {dir && <div className="fdir mono">{dir}/</div>}
                  {fs.map((f) => {
                    const cnt = openCountForFile(f.path);
                    return (
                      <button
                        type="button"
                        key={f.path}
                        className={`ff ${f.path === activeFile ? 'on' : ''}`}
                        onClick={() => {
                          setFile(f.path);
                          setDraft(null);
                          setTextSel(null);
                          if (isNarrow) setTreeOverride(false);
                        }}
                        title={f.path}
                      >
                        <span className="fn">{f.path.split('/').pop()}</span>
                        {cnt > 0 && (
                          <span className="comment-count-badge">
                            <MessageSquare size={10} strokeWidth={1.8} />
                            {cnt}
                          </span>
                        )}
                        <span className="fs mono">
                          {f.binary ? (
                            <span className="faint">bin</span>
                          ) : (
                            <>
                              <span className="add">+{f.insertions}</span> <span className="del">−{f.deletions}</span>
                            </>
                          )}
                        </span>
                      </button>
                    );
                  })}
                </div>
              ))}
            </div>

            {/* Quiet bottom-left collapsed jump drawer for pending comments (zero bottom bar!) */}
            {openAnnotations.length > 0 && (
              <div className="rail-cmt-foot">
                {railPopOpen && (
                  <div className="rail-cmt-pop">
                    {openAnnotations.map((a) => (
                      <button
                        type="button"
                        key={a.annotationId}
                        className="rail-cmt-item"
                        onClick={() => {
                          if (a.anchor.path) setFile(a.anchor.path);
                          setRailPopOpen(false);
                        }}
                      >
                        <div className="gh-comment-anchor">{formatLineAnchor(a)}</div>
                        <div className="ell" style={{ color: 'var(--fg2)', marginTop: 2 }}>
                          {a.body}
                        </div>
                      </button>
                    ))}
                  </div>
                )}
                <button
                  type="button"
                  className="rail-cmt-toggle"
                  onClick={() => setRailPopOpen((v) => !v)}
                >
                  <MessageSquare size={12} strokeWidth={1.7} />
                  <span>{openAnnotations.length} 条待发评论</span>
                  <ChevronUp size={12} strokeWidth={1.7} style={{ marginLeft: 'auto', transform: railPopOpen ? 'rotate(180deg)' : undefined }} />
                </button>
              </div>
            )}
          </div>
        )}

        <div className="diffpane orbit-diff" ref={diffPaneRef} onMouseUp={handleCodeMouseUp}>
          {textSel && (
            <button
              type="button"
              className="sel-pop"
              style={{ left: textSel.x, top: textSel.y }}
              onMouseDown={(e) => e.preventDefault()}
              onClick={() => openComposer(textSel.change, textSel.side, false, textSel.quote)}
            >
              <MessageSquare size={11} strokeWidth={1.9} />
              评论选中内容
            </button>
          )}

          {dq.isError ? (
            <div className="empty bad-t">
              载入交付失败：{errMsg(dq.error)}{' '}
              <button type="button" className="btn ghost" onClick={onClose}>
                关闭
              </button>
            </div>
          ) : files.length === 0 && !dq.isLoading ? (
            <div className="empty">没有文本差异</div>
          ) : (
            <>
              <div className="dhead">
                {!treeOpen && files.length > 1 ? (
                  <select
                    className="dhead-sel mono"
                    value={activeFile}
                    onChange={(e) => {
                      setFile(e.target.value);
                      setDraft(null);
                      setTextSel(null);
                    }}
                    aria-label="切换文件"
                  >
                    {files.map((f) => (
                      <option key={f.path} value={f.path}>
                        {f.path} (+{f.insertions} −{f.deletions})
                      </option>
                    ))}
                  </select>
                ) : (
                  <span className="p mono">{activeFile}</span>
                )}
              </div>
              {diffQ.isLoading ? (
                <div className="empty">载入 Diff…</div>
              ) : !parsed ? (
                <div className="empty">该文件为二进制或无文本差异</div>
              ) : (
                <Diff
                  viewType={effectiveViewType}
                  diffType={parsed.type}
                  hunks={parsed.hunks}
                  tokens={tokens}
                  selectedChanges={selectedChanges}
                  renderToken={renderToken}
                  renderGutter={renderGutter}
                  widgets={widgets}
                >
                  {(hunks) => hunks.map((h) => <Hunk key={h.content} hunk={h} />)}
                </Diff>
              )}
            </>
          )}
        </div>
      </div>
    </>
  );
}
