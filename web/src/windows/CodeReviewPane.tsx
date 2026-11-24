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
import type { AnnotationView, BootstrapResponse, DiffFileStat, TaskTimelineResponse } from '../generated/protocol';
import { useDelivery } from '../hooks/queries';
import { api, errMsg, makeIdempotencyKey } from '../lib/api';
import { shortSha } from '../lib/format';
import { buildIDEJump, launchIDE } from '../lib/ide';
import { tokenizeHunksWithShiki } from '../lib/shikiDiff';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

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

export function CodeReviewPane({
  taskId,
  deliveryId,
  onSelectDelivery,
  tl,
  boot,
  onClose,
}: {
  taskId: string;
  deliveryId: string;
  onSelectDelivery: (id: string) => void;
  tl?: TaskTimelineResponse;
  boot: BootstrapResponse;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const theme = useWorkbenchStore((s) => s.theme);
  const localMachineId = useWorkbenchStore((s) => s.localMachineId);
  const deselectedArr = useWorkbenchStore((s) => s.deselectedAnnotations[taskId]);
  const toggleAnnSelected = useWorkbenchStore((s) => s.toggleAnnotationSelected);
  const clearDeselected = useWorkbenchStore((s) => s.clearDeselectedAnnotations);
  const deselected = useMemo(() => new Set(deselectedArr ?? []), [deselectedArr]);
  const [file, setFile] = useState('');
  const [viewType, setViewType] = useState<ViewType>('unified');
  const [draft, setDraft] = useState<LineDraft | null>(null);
  const [body, setBody] = useState('');
  const [tokens, setTokens] = useState<HunkTokens | null>(null);
  const [verifiedSite, setVerifiedSite] = useState(false);
  const [lastLine, setLastLine] = useState<{ path: string; line?: number } | null>(null);

  useEffect(() => {
    setFile('');
    setDraft(null);
    setBody('');
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
  const lastAtt = tl?.attempts?.[tl.attempts.length - 1];
  const needsVerifyAttemptId = !tl?.task.activeAttemptId && lastAtt?.state === 'needs_verification' ? lastAtt.attemptId : undefined;
  useEffect(() => setVerifiedSite(false), [needsVerifyAttemptId]);

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

  const annotations = (detail?.annotations ?? []).filter(isDiffAnnotation);
  const openAnns = annotations.filter((a) => a.status === 'open');
  const localSelectedIds = openAnns.filter((a) => !deselected.has(a.annotationId)).map((a) => a.annotationId);
  const { selDiffIds, selRyugiIds, combinedSelectedIds } = useMemo(() => {
    const byId = new Map<string, AnnotationView>();
    for (const a of tl?.annotations ?? []) byId.set(a.annotationId, a);
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
    };
  }, [tl?.annotations, detail?.annotations, deselected]);
  const fileAnns = annotations.filter((a) => a.anchor.path === activeFile);

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['delivery'] });
    void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] });
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
      api.dispatchAnnotations(taskId, {
        annotationIds: ids,
        mode: 'queue',
        verifiedSite: needsVerifyAttemptId ? verifiedSite : undefined,
        verifiedAttemptId: needsVerifyAttemptId,
      }),
    onSuccess: (res) => {
      toast(res.pendingInput ? `已送入队列（${res.queuedCount} 条批注）` : `已发给会话（${res.queuedCount} 条批注）`);
      clearDeselected(taskId);
      setVerifiedSite(false);
      refresh();
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const complete = useMutation({
    mutationFn: () => api.completeDeliveryReview(effectiveDeliveryId),
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
          <span className="stt">{a.status === 'open' ? '未发' : a.status === 'queued' ? '已入队' : a.status}</span>
        </div>,
      );
    }
    if (draft && draft.deliveryId === effectiveDeliveryId && draft.path === activeFile) {
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
              保存
            </button>
          </div>
        </div>,
      );
    }
    const out: Record<string, ReactNode> = {};
    for (const [k, nodes] of Object.entries(map)) out[k] = <div className="cmrow">{nodes}</div>;
    return out;
  }, [parsed, fileAnns, draft, body, effectiveDeliveryId, activeFile, create]);

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
  const d = detail?.delivery;
  const annCount = (p: string) => annotations.filter((a) => a.anchor.path === p).length;

  return (
    <>
      <div className="vtabs">
        {versions.map((v, i) => (
          <button
            type="button"
            key={v.deliveryId}
            className={v.deliveryId === effectiveDeliveryId ? 'on' : ''}
            onClick={() => onSelectDelivery(v.deliveryId)}
          >
            v{i + 1}
            <span className="mono faint">{shortSha(v.headSha)}</span>
            {v.lateArrival && <span className="faint"> (迟到)</span>}
          </button>
        ))}
        {d && (
          <span className="mono faint" style={{ marginLeft: 4 }}>
            {d.manifest.filesChanged} 文件 <span className="add">+{d.manifest.insertions}</span>{' '}
            <span className="del">−{d.manifest.deletions}</span>
          </span>
        )}
        <span className="sp" />
        {ide && (
          <button type="button" className="btn ghost" onClick={() => launchIDE(ide.uri)} title={ide.resolvedPath}>
            <ArrowUpRight size={13} strokeWidth={1.6} /> Cursor
          </button>
        )}
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
                  {annCount(f.path) > 0 && <span className="cc">●</span>}
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
              <div className="fdir">待发送行评</div>
              {openAnns.map((a) => (
                <label className="pend-i" key={a.annotationId}>
                  <input
                    type="checkbox"
                    checked={!deselected.has(a.annotationId)}
                    onChange={(e) => toggleAnnSelected(taskId, a.annotationId, e.target.checked)}
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
          {dq.isError ? (
            <div className="empty bad-t">
              载入交付失败：{errMsg(dq.error)}{' '}
              <button type="button" className="btn ghost" onClick={onClose}>
                关闭
              </button>
            </div>
          ) : files.length === 0 && !dq.isLoading ? (
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

      <div className="wf">
        <span className="l">
          {combinedSelectedIds.length > 0
            ? `已选 ${combinedSelectedIds.length} 条批注`
            : '点行号留行评，攒好后一次送进右侧会话'}
        </span>
        <span className="r">
          {needsVerifyAttemptId && (
            <label className="chk" style={{ color: 'var(--warn)', marginRight: 4 }}>
              <input type="checkbox" checked={verifiedSite} onChange={(e) => setVerifiedSite(e.target.checked)} />
              已核对现场
            </label>
          )}
          <button type="button" className="btn" disabled={complete.isPending || !d} onClick={() => complete.mutate()}>
            审阅完成
          </button>
          {selRyugiIds.length > 0 && localSelectedIds.length > 0 && (
            <button
              type="button"
              className="btn ghost"
              disabled={dispatch.isPending || (!!needsVerifyAttemptId && !verifiedSite)}
              onClick={() => dispatch.mutate(localSelectedIds)}
            >
              仅发代码 ({localSelectedIds.length})
            </button>
          )}
          <button
            type="button"
            className="btn pri"
            disabled={combinedSelectedIds.length === 0 || dispatch.isPending || (!!needsVerifyAttemptId && !verifiedSite)}
            onClick={() => dispatch.mutate(combinedSelectedIds)}
          >
            {selDiffIds.length > 0 && selRyugiIds.length > 0
              ? `合并发送 ${combinedSelectedIds.length} 条批注`
              : `发送 ${combinedSelectedIds.length} 条批注`}
          </button>
        </span>
      </div>
    </>
  );
}
