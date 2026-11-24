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
  type GutterOptions,
  type HunkData,
  type HunkTokens,
  type RenderToken,
  type ViewType,
} from 'react-diff-view';
import { ArrowUpRight } from 'lucide-react';
import type { AnnotationView, BootstrapResponse, DiffFileStat } from '../generated/protocol';
import { DiffIcon } from '../components/Icons';
import { useDelivery, useTimeline } from '../hooks/queries';
import { api, errMsg, makeIdempotencyKey } from '../lib/api';
import { shortSha } from '../lib/format';
import { buildIDEJump, launchIDE } from '../lib/ide';
import { tokenizeHunksWithShiki } from '../lib/shikiDiff';
import { toast, useWorkbenchStore, type WinState } from '../store/useWorkbenchStore';
import { FloatingWindow } from './FloatingWindow';

const CODE_KINDS = new Set(['code_diff', 'mixed', 'workspace_snapshot']);

interface LineDraft {
  deliveryId: string;
  changeKey: string;
  path: string;
  side: 'old' | 'new';
  line: number;
  contextBefore: string;
  contextAfter: string;
  idempotencyKey: string;
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

function CodeReviewInner({ w, boot }: { w: WinState; boot: BootstrapResponse }) {
  const qc = useQueryClient();
  const theme = useWorkbenchStore((s) => s.theme);
  const localMachineId = useWorkbenchStore((s) => s.localMachineId);
  const close = useWorkbenchStore((s) => s.closeWindow);
  const deselectedArr = useWorkbenchStore((s) => s.deselectedAnnotations[w.taskId]);
  const toggleAnnSelected = useWorkbenchStore((s) => s.toggleAnnotationSelected);
  const clearDeselected = useWorkbenchStore((s) => s.clearDeselectedAnnotations);
  const deselected = useMemo(() => new Set(deselectedArr ?? []), [deselectedArr]);
  const [deliveryId, setDeliveryId] = useState(w.deliveryId);
  const [file, setFile] = useState('');
  const [viewType, setViewType] = useState<ViewType>('unified');
  const [draft, setDraft] = useState<LineDraft | null>(null);
  const [body, setBody] = useState('');
  const [tokens, setTokens] = useState<HunkTokens | null>(null);
  const [verifiedSite, setVerifiedSite] = useState(false);
  const [lastLine, setLastLine] = useState<{ path: string; line?: number } | null>(null);

  useEffect(() => setDeliveryId(w.deliveryId), [w.deliveryId]);
  useEffect(() => {
    setFile('');
    setDraft(null);
    setBody('');
  }, [deliveryId]);

  const tl = useTimeline(w.taskId);
  const versions = (tl.data?.deliveries ?? []).filter((d) => CODE_KINDS.has(d.kind));
  const dq = useDelivery(deliveryId);
  const detail = dq.data;
  const files = detail?.diffFiles ?? [];
  const activeFile = (file && files.some((f) => f.path === file) ? file : files[0]?.path) || '';
  const lastAtt = tl.data?.attempts?.[tl.data.attempts.length - 1];
  const needsVerifyAttemptId = !tl.data?.task.activeAttemptId && lastAtt?.state === 'needs_verification' ? lastAtt.attemptId : undefined;
  useEffect(() => setVerifiedSite(false), [needsVerifyAttemptId]);

  const diffQ = useQuery({
    queryKey: ['delivery-diff', deliveryId, activeFile],
    queryFn: () => api.getDeliveryDiff(deliveryId, activeFile || undefined),
    enabled: !!deliveryId && !!activeFile,
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

  const annotations = (detail?.annotations ?? []).filter(isDiffAnnotation);
  const openAnns = annotations.filter((a) => a.status === 'open');
  const localSelectedIds = openAnns.filter((a) => !deselected.has(a.annotationId)).map((a) => a.annotationId);
  const { selDiffIds, selRyugiIds, combinedSelectedIds, allOpenDiffCount, allOpenRyugiCount } = useMemo(() => {
    const byId = new Map<string, AnnotationView>();
    for (const a of tl.data?.annotations ?? []) byId.set(a.annotationId, a);
    for (const a of detail?.annotations ?? []) byId.set(a.annotationId, a);
    const allOpen = [...byId.values()].filter((a) => a.status === 'open');
    const openDiff = allOpen.filter(isDiffAnnotation);
    const openRyugi = allOpen.filter((a) => a.source === 'ryugi');
    const diffIds = openDiff.filter((a) => !deselected.has(a.annotationId)).map((a) => a.annotationId);
    const ryugiIds = openRyugi.filter((a) => !deselected.has(a.annotationId)).map((a) => a.annotationId);
    return {
      selDiffIds: diffIds,
      selRyugiIds: ryugiIds,
      combinedSelectedIds: [...diffIds, ...ryugiIds],
      allOpenDiffCount: openDiff.length,
      allOpenRyugiCount: openRyugi.length,
    };
  }, [tl.data?.annotations, detail?.annotations, deselected]);
  const fileAnns = annotations.filter((a) => a.anchor.path === activeFile);

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['delivery'] });
    void qc.invalidateQueries({ queryKey: ['task-timeline', w.taskId] });
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
            startLine: v.d.line,
            endLine: v.d.line,
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
      refresh();
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const dispatch = useMutation({
    mutationFn: (ids: string[]) =>
      api.dispatchAnnotations(w.taskId, {
        annotationIds: ids,
        mode: 'queue',
        verifiedSite: needsVerifyAttemptId ? verifiedSite : undefined,
        verifiedAttemptId: needsVerifyAttemptId,
      }),
    onSuccess: (res) => {
      toast(
        res.pendingInput
          ? `已打包 ${res.queuedCount} 条批注 → 待消费队列`
          : res.attempt
            ? `已打包 ${res.queuedCount} 条批注 → 会话空闲，直接开始新一轮`
            : `已打包 ${res.queuedCount} 条批注`,
      );
      clearDeselected(w.taskId);
      setVerifiedSite(false);
      refresh();
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const complete = useMutation({
    mutationFn: () => api.completeDeliveryReview(deliveryId),
    onSuccess: () => {
      toast('已标记审阅完成');
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
      refresh();
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const openComposer = (change: ChangeData, clicked: 'old' | 'new') => {
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
    const ctx = sameHunkSideContext(parsed.hunks, change, side);
    setLastLine({ path: activeFile, line: newLn > 0 ? newLn : undefined });
    setDraft({
      deliveryId: detail.delivery.deliveryId,
      changeKey: getChangeKey(change),
      path: activeFile,
      side,
      line,
      ...ctx,
      idempotencyKey: makeIdempotencyKey(),
    });
    setBody('');
  };

  const widgets = useMemo<Record<string, ReactNode>>(() => {
    if (!parsed) return {};
    const keyBySideLine = new Map<string, string>();
    for (const h of parsed.hunks) {
      for (const ch of h.changes) {
        const ck = getChangeKey(ch);
        const n = computeNewLineNumber(ch);
        const o = computeOldLineNumber(ch);
        if (n > 0) keyBySideLine.set(`new:${n}`, ck);
        if (o > 0) keyBySideLine.set(`old:${o}`, ck);
      }
    }
    const map: Record<string, ReactNode[]> = {};
    for (const a of fileAnns) {
      const side = a.anchor.side === 'old' ? 'old' : 'new';
      const ck = keyBySideLine.get(`${side}:${a.anchor.startLine ?? 0}`);
      if (!ck) continue;
      (map[ck] ??= []).push(
        <div className="cmsaved" key={a.annotationId}>
          <span className="who">行评</span>
          <span style={{ whiteSpace: 'pre-wrap', minWidth: 0 }}>{a.body}</span>
          <span className="stt">{a.status === 'open' ? '未决' : a.status === 'queued' ? '已入队' : a.status}</span>
        </div>,
      );
    }
    if (draft && draft.deliveryId === deliveryId && draft.path === activeFile) {
      (map[draft.changeKey] ??= []).push(
        <div className="cmbox" key="__draft">
          <div className="loc mono">
            {draft.path}:{draft.line} ({draft.side === 'old' ? '旧' : '新'})
          </div>
          <textarea
            autoFocus
            value={body}
            placeholder="写下针对这一行的修改意见…"
            onChange={(e) => {
              setBody(e.target.value);
              setDraft((d) => (d ? { ...d, idempotencyKey: makeIdempotencyKey() } : d));
            }}
            onKeyDown={(e) => {
              if (e.key === 'Escape') {
                e.stopPropagation();
                setDraft(null);
              }
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && body.trim()) {
                e.preventDefault();
                create.mutate({ d: draft, body: body.trim() });
              }
            }}
          />
          <div className="row">
            <span className="faint" style={{ fontSize: 11, marginRight: 'auto' }}>
              ⌘↵ 保存
            </span>
            <button type="button" className="btn ghost" onClick={() => setDraft(null)}>
              取消
            </button>
            <button type="button" className="btn pri" disabled={!body.trim() || create.isPending} onClick={() => create.mutate({ d: draft, body: body.trim() })}>
              保存行评
            </button>
          </div>
        </div>,
      );
    }
    const out: Record<string, ReactNode> = {};
    for (const [k, nodes] of Object.entries(map)) out[k] = <div className="cmrow">{nodes}</div>;
    return out;
  }, [parsed, fileAnns, draft, body, deliveryId, activeFile, create]);

  const renderGutter = ({ change, side, renderDefault }: GutterOptions) => {
    const ln = side === 'old' ? computeOldLineNumber(change) : computeNewLineNumber(change);
    if (ln <= 0) return renderDefault();
    return (
      <button
        type="button"
        className="gbtn"
        title={`在第 ${ln} 行留行评`}
        onClick={(e) => {
          e.stopPropagation();
          openComposer(change, side);
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
  const ide =
    detail && project
      ? buildIDEJump({
          scheme: project.ideScheme,
          localMachineId,
          delivery: detail.delivery,
          project,
          machines: boot.machines,
          file: lastLine?.path ?? activeFile,
          line: lastLine?.line,
        })
      : null;
  const vIdx = versions.findIndex((v) => v.deliveryId === deliveryId);
  const d = detail?.delivery;
  const annCount = (p: string) => annotations.filter((a) => a.anchor.path === p).length;

  return (
    <FloatingWindow
      kind="cr"
      icon={<DiffIcon />}
      title="Code Review"
      width="min(920px, calc(100vw - 340px))"
      height="min(640px, calc(100vh - 90px))"
      subtitle={
        d ? (
          <span className="mono">
            {shortSha(d.baseSha)} → {shortSha(d.headSha)} · {d.manifest.filesChanged} 文件 <span className="add">+{d.manifest.insertions}</span>{' '}
            <span className="del">−{d.manifest.deletions}</span>
          </span>
        ) : (
          '载入中…'
        )
      }
      actions={
        ide ? (
          <button type="button" className="btn ghost" onClick={() => launchIDE(ide.uri)} title={ide.resolvedPath}>
            <ArrowUpRight size={13} strokeWidth={1.6} /> 在 Cursor 打开
          </button>
        ) : null
      }
      footer={
        <>
          <span className="l">
            {allOpenDiffCount + allOpenRyugiCount > 0
              ? `任务已选 ${combinedSelectedIds.length} 条未决批注（Diff ${selDiffIds.length}/${allOpenDiffCount}${allOpenRyugiCount > 0 ? ` · Ryugi ${selRyugiIds.length}/${allOpenRyugiCount}` : ''}）`
              : '点代码行号留行评；攒好后可与 Ryugi 画布批注合并送进会话'}
          </span>
          <span className="r">
            {needsVerifyAttemptId && (
              <label style={{ display: 'inline-flex', alignItems: 'center', gap: 5, fontSize: 12, color: 'var(--warn)', marginRight: 4, cursor: 'pointer' }}>
                <input type="checkbox" checked={verifiedSite} onChange={(e) => setVerifiedSite(e.target.checked)} />
                <span>已核对现场</span>
              </label>
            )}
            <button type="button" className="btn" disabled={complete.isPending || !d} onClick={() => complete.mutate()}>
              标记审阅完成
            </button>
            {selRyugiIds.length > 0 && localSelectedIds.length > 0 && (
              <button
                type="button"
                className="btn ghost"
                disabled={dispatch.isPending || (!!needsVerifyAttemptId && !verifiedSite)}
                onClick={() => dispatch.mutate(localSelectedIds)}
              >
                仅发本窗 ({localSelectedIds.length})
              </button>
            )}
            <button
              type="button"
              className="btn pri"
              disabled={combinedSelectedIds.length === 0 || dispatch.isPending || (!!needsVerifyAttemptId && !verifiedSite)}
              onClick={() => dispatch.mutate(combinedSelectedIds)}
            >
              {selDiffIds.length > 0 && selRyugiIds.length > 0
                ? `合并打包 ${combinedSelectedIds.length} 条（Diff ${selDiffIds.length} + Ryugi ${selRyugiIds.length}）→ 入队`
                : `打包 ${combinedSelectedIds.length} 条批注 → 送入待消费队列`}
            </button>
          </span>
        </>
      }
    >
      <div className="vtabs">
        {versions.map((v, i) => (
          <button type="button" key={v.deliveryId} className={v.deliveryId === deliveryId ? 'on' : ''} onClick={() => setDeliveryId(v.deliveryId)}>
            v{i + 1}
            <span className="mono faint">{shortSha(v.headSha)}</span>
            {v.lateArrival && <span className="faint"> (迟到档案)</span>}
          </button>
        ))}
        {vIdx < 0 && d && (
          <button type="button" className="on">
            {d.kind} <span className="mono faint">{shortSha(d.headSha)}</span>
          </button>
        )}
        <span className="sp" />
        <div className="seg">
          <button type="button" className={viewType === 'unified' ? 'on' : ''} onClick={() => setViewType('unified')}>
            Unified
          </button>
          <button type="button" className={viewType === 'split' ? 'on' : ''} onClick={() => setViewType('split')}>
            Split
          </button>
        </div>
      </div>
      <div className="cr-body">
        <div className="ftree">
          {dq.isLoading && <div className="fdir">载入文件…</div>}
          {groupByDir(files).map(([dir, fs]) => (
            <div key={dir || '/'}>
              {dir && <div className="fdir mono">{dir}/</div>}
              {fs.map((f) => (
                <button
                  type="button"
                  key={f.path}
                  className={`ff ${f.path === activeFile ? 'on' : ''}`}
                  onClick={() => {
                    setFile(f.path);
                    setDraft(null);
                  }}
                  title={f.path}
                >
                  <span className="fn">{f.path.split('/').pop()}</span>
                  {annCount(f.path) > 0 && <span className="cc">●{annCount(f.path)}</span>}
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
              ))}
            </div>
          ))}
          {openAnns.length > 0 && (
            <div className="pend">
              <div className="fdir">未决行评 · 勾选后打包</div>
              {openAnns.map((a) => (
                <label className="pend-i" key={a.annotationId}>
                  <input
                    type="checkbox"
                    checked={!deselected.has(a.annotationId)}
                    onChange={(e) => toggleAnnSelected(w.taskId, a.annotationId, e.target.checked)}
                  />
                  <span style={{ minWidth: 0 }}>
                    <span className="loc mono">
                      {a.anchor.path}:{a.anchor.startLine}
                    </span>
                    <span className="txt">{a.body}</span>
                  </span>
                </label>
              ))}
            </div>
          )}
        </div>
        <div className="diffpane orbit-diff">
          {files.length === 0 && !dq.isLoading ? (
            <div className="empty">这次交付相对基线没有文本差异</div>
          ) : (
            <>
              <div className="dhead">
                <span className="p mono">{activeFile}</span>
                <span className="r faint">点行号留行评</span>
              </div>
              {diffQ.isLoading ? (
                <div className="empty">载入 Diff…</div>
              ) : !parsed ? (
                <div className="empty">该文件为二进制或无文本差异</div>
              ) : (
                <Diff
                  viewType={viewType}
                  diffType={parsed.type}
                  hunks={parsed.hunks}
                  tokens={tokens}
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
      {dq.isError && (
        <div className="empty bad-t">
          载入交付失败：{errMsg(dq.error)}{' '}
          <button type="button" className="btn ghost" onClick={() => close('cr')}>
            关闭
          </button>
        </div>
      )}
    </FloatingWindow>
  );
}

export function CodeReviewWindow({ boot }: { boot: BootstrapResponse }) {
  const w = useWorkbenchStore((s) => s.windows.cr);
  if (!w) return null;
  return <CodeReviewInner w={w} boot={boot} />;
}
