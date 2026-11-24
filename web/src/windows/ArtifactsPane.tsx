import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { ArrowUpRight, Download, FileText, MessageSquare, Plus, RefreshCw, Trash2 } from 'lucide-react';
import type { TaskTimelineResponse } from '../generated/protocol';
import { ArtIcon } from '../components/Icons';
import { Md } from '../components/Md';
import { useDelivery } from '../hooks/queries';
import { api, errMsg, getStoredDevToken, makeIdempotencyKey } from '../lib/api';
import type { SessionArtifactItem } from '../lib/artifacts';
import { artifactShortLabel, basename } from '../lib/format';
import { toast } from '../store/useWorkbenchStore';

function openExternal(url: string) {
  window.open(url, '_blank', 'noopener,noreferrer');
}

function isTextRenderable(path: string): boolean {
  const lower = path.toLowerCase();
  return (
    lower.endsWith('.md') ||
    lower.endsWith('.markdown') ||
    lower.endsWith('.txt') ||
    lower.endsWith('.csv') ||
    lower.endsWith('.tsv') ||
    lower.endsWith('.json') ||
    lower.endsWith('.yaml') ||
    lower.endsWith('.yml')
  );
}

function formatBytes(n?: number): string {
  if (!n || n <= 0) return '';
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

async function fetchBlobText(sha256: string): Promise<string> {
  const headers: Record<string, string> = {};
  const tok = getStoredDevToken();
  if (tok) headers['X-Orbit-Dev-Token'] = tok;
  const res = await fetch(api.blobUrl(sha256), { headers });
  if (!res.ok) throw new Error(`读取失败 (${res.status})`);
  return res.text();
}

/**
 * Full-height Artifacts viewer (zero bottom tray):
 * Supports both Ryugi canvases and document blobs (Markdown/CSV/Text/Word/PPT/PDF).
 * Pending comments render inline directly underneath the artifact and sync with the right-hand Session composer.
 */
export function ArtifactsPane({
  taskId,
  deliveryId,
  artifactId,
  artifacts,
  tl,
  titleSwitcher,
  headerEnd,
}: {
  taskId: string;
  deliveryId: string;
  artifactId?: string;
  artifacts: SessionArtifactItem[];
  tl?: TaskTimelineResponse;
  titleSwitcher?: ReactNode;
  headerEnd?: ReactNode;
}) {
  const qc = useQueryClient();
  const lastAutoSyncRef = useRef<number>(0);
  const [addingNote, setAddingNote] = useState(false);
  const [noteBody, setNoteBody] = useState('');

  const activeArt = useMemo(() => {
    if (artifactId) {
      const found = artifacts.find((a) => a.artifactId === artifactId);
      if (found) return found;
    }
    return artifacts.find((a) => a.deliveryId === deliveryId) ?? artifacts[0];
  }, [artifacts, artifactId, deliveryId]);

  const effectiveDeliveryId = activeArt?.deliveryId ?? deliveryId;
  const dq = useDelivery(effectiveDeliveryId);
  const detail = dq.data;
  const bindings = detail?.ryugiBindings ?? [];

  const isRyugi = activeArt?.source === 'ryugi' || (!activeArt && bindings.length > 0);
  const isTextBlob = activeArt?.source === 'blob' && !!activeArt.blobSha256 && isTextRenderable(activeArt.locator);
  const isMarkdown = activeArt?.locator.toLowerCase().endsWith('.md') || activeArt?.locator.toLowerCase().endsWith('.markdown');

  const blobQ = useQuery({
    queryKey: ['blob-text', activeArt?.blobSha256],
    queryFn: () => fetchBlobText(activeArt!.blobSha256!),
    enabled: isTextBlob,
  });

  const notes = useMemo(() => {
    const all = detail?.annotations ?? tl?.annotations ?? [];
    if (!activeArt) return all.filter((a) => a.source === 'ryugi');
    if (activeArt.source === 'ryugi') {
      return all.filter((a) => a.deliveryId === effectiveDeliveryId && a.source === 'ryugi');
    }
    return all.filter(
      (a) =>
        a.deliveryId === effectiveDeliveryId &&
        (a.anchor.artifact === activeArt.locator || a.anchor.path === activeArt.locator),
    );
  }, [detail?.annotations, tl?.annotations, activeArt, effectiveDeliveryId]);

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['delivery'] });
    void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] });
    void qc.invalidateQueries({ queryKey: ['global-artifacts'] });
    void qc.invalidateQueries({ queryKey: ['bootstrap'] });
  };

  const sync = useMutation({
    mutationFn: (silent: boolean) => api.syncDeliveryRyugi(effectiveDeliveryId).then((res) => ({ res, silent })),
    onSuccess: ({ res, silent }) => {
      refresh();
      if (!silent) {
        const errs = res.errors ?? [];
        toast(
          errs.length ? `同步部分失败：${errs[0]}` : res.newAnnotations > 0 ? `同步了 ${res.newAnnotations} 条新批注` : '已是最新',
          errs.length && !res.bindingsSynced ? 'err' : 'info',
        );
      }
    },
    onError: (e, silent) => {
      if (!silent) toast(errMsg(e), 'err');
    },
  });

  const addNote = useMutation({
    mutationFn: (text: string) =>
      api.createAnnotation(
        effectiveDeliveryId,
        {
          source: 'diff_drawer',
          anchor: {
            artifact: activeArt?.locator || activeArt?.title || 'artifact',
            path: activeArt?.locator,
          },
          body: text,
        },
        makeIdempotencyKey(),
      ),
    onSuccess: () => {
      setNoteBody('');
      setAddingNote(false);
      refresh();
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const removeAnn = useMutation({
    mutationFn: (annotationId: string) => api.deleteAnnotation(annotationId),
    onSuccess: () => refresh(),
    onError: (e) => toast(errMsg(e), 'err'),
  });

  // Auto-sync silently on window focus after returning from Ryugi :8443
  useEffect(() => {
    if (!effectiveDeliveryId || !isRyugi || bindings.length === 0) return;
    const onFocus = () => {
      if (Date.now() - lastAutoSyncRef.current < 4000 || sync.isPending) return;
      lastAutoSyncRef.current = Date.now();
      sync.mutate(true);
    };
    window.addEventListener('focus', onFocus);
    return () => window.removeEventListener('focus', onFocus);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [effectiveDeliveryId, isRyugi, bindings.length]);

  useEffect(() => {
    setAddingNote(false);
    setNoteBody('');
  }, [activeArt?.artifactId]);

  const activeBinding = useMemo(() => {
    if (!isRyugi) return undefined;
    if (activeArt?.artifactId.startsWith('ryugi:')) {
      const id = activeArt.artifactId.slice('ryugi:'.length);
      return bindings.find((b) => b.bindingId === id) ?? bindings[0];
    }
    return bindings[0];
  }, [isRyugi, activeArt, bindings]);

  return (
    <>
      <div className="rev-h">
        {titleSwitcher}
        {activeArt && <span className="dlv-kind-tag no-mobile">{artifactShortLabel(activeArt.kind)}</span>}
        <div className="rev-h-acts">
          {isRyugi && effectiveDeliveryId && (
            <button
              type="button"
              className="ib"
              title="同步画布批注"
              disabled={sync.isPending}
              onClick={() => sync.mutate(false)}
            >
              <RefreshCw size={13} strokeWidth={1.6} className={sync.isPending ? 'spinning' : ''} />
            </button>
          )}
          {!isRyugi && activeArt?.blobSha256 && (
            <>
              <button
                type="button"
                className="btn ghost"
                onClick={() => setAddingNote((v) => !v)}
              >
                <Plus size={12} strokeWidth={1.8} /> 批注
              </button>
              <a
                className="ib"
                href={api.blobUrl(activeArt.blobSha256)}
                download={activeArt.title}
                title="下载文件"
              >
                <Download size={14} strokeWidth={1.6} />
              </a>
            </>
          )}
          {headerEnd}
        </div>
      </div>

      <div className="art-single">
        {dq.isLoading && <div className="empty">载入中…</div>}
        {!dq.isLoading && !activeArt && bindings.length === 0 && <div className="empty">没有可预览的 Artifact</div>}

        {/* Ryugi interactive design canvas */}
        {isRyugi && activeBinding && (
          <div className="art-list">
            <div className="art-row" title={activeBinding.artifactLocator}>
              <ArtIcon size={15} />
              <div style={{ flex: 1, minWidth: 0 }}>
                <div className="n ell">{basename(activeBinding.artifactLocator) || activeBinding.artifactLocator}</div>
                <div className="mono faint" style={{ fontSize: 11, marginTop: 2 }}>
                  {activeBinding.artifactLocator}
                </div>
              </div>
              <button
                type="button"
                className="btn pri"
                disabled={!activeBinding.openUrl}
                onClick={() => openExternal(activeBinding.openUrl)}
              >
                打开画布 <ArrowUpRight size={12} strokeWidth={1.7} />
              </button>
            </div>
          </div>
        )}

        {/* Document blob viewer (Markdown / Text / Binary Card) */}
        {!isRyugi && activeArt && (
          <div className="doc-reader">
            {isTextBlob ? (
              blobQ.isLoading ? (
                <div className="empty">读取文档…</div>
              ) : blobQ.isError ? (
                <div className="empty bad-t">{errMsg(blobQ.error)}</div>
              ) : isMarkdown ? (
                <article className="doc-paper">
                  <Md text={blobQ.data ?? ''} />
                </article>
              ) : (
                <pre className="doc-code mono">{blobQ.data ?? ''}</pre>
              )
            ) : (
              <div className="art-row">
                <FileText size={16} strokeWidth={1.6} />
                <div style={{ flex: 1, minWidth: 0 }}>
                  <div className="n ell">{activeArt.title}</div>
                  <div className="mono faint" style={{ fontSize: 11, marginTop: 2 }}>
                    {[activeArt.locator, formatBytes(activeArt.sizeBytes)].filter(Boolean).join(' · ')}
                  </div>
                </div>
                {activeArt.blobSha256 && (
                  <a className="btn pri" href={api.blobUrl(activeArt.blobSha256)} download={activeArt.title}>
                    <Download size={12} strokeWidth={1.7} /> 下载
                  </a>
                )}
              </div>
            )}
          </div>
        )}

        {/* Inline comment composer for document artifacts */}
        {addingNote && !isRyugi && activeArt && (
          <div className="gh-comment-wrap gh-comment-draft" style={{ margin: '12px 0 0' }}>
            <div className="gh-comment-hd">
              <span className="gh-comment-anchor">{activeArt.title}</span>
            </div>
            <div className="gh-draft-inner">
              <textarea
                autoFocus
                value={noteBody}
                placeholder="写下针对这份 Artifact 的修改建议…"
                onChange={(e) => setNoteBody(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Escape') {
                    e.stopPropagation();
                    setAddingNote(false);
                  }
                  if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && noteBody.trim()) {
                    e.preventDefault();
                    addNote.mutate(noteBody.trim());
                  }
                }}
              />
              <div className="gh-draft-foot">
                <span className="faint" style={{ fontSize: 11, marginRight: 'auto' }}>
                  ⌘↵ 添加到待发批注
                </span>
                <button type="button" className="btn ghost" onClick={() => setAddingNote(false)}>
                  取消
                </button>
                <button
                  type="button"
                  className="btn pri"
                  disabled={!noteBody.trim() || addNote.isPending}
                  onClick={() => addNote.mutate(noteBody.trim())}
                >
                  添加评论
                </button>
              </div>
            </div>
          </div>
        )}

        {/* Inline comment cards directly anchored below the artifact */}
        {notes.length > 0 && (
          <div className="art-notes-inline">
            <div className="art-notes-hd">
              <MessageSquare size={12} strokeWidth={1.7} />
              <span>批注 · {notes.length}</span>
            </div>
            {notes.map((a) => {
              const loc = [basename(a.anchor.artifact), a.anchor.section || a.anchor.elementId || a.anchor.page]
                .filter(Boolean)
                .join(' · ');
              return (
                <div className="gh-comment-wrap" key={a.annotationId} style={{ margin: 0 }}>
                  <div className="gh-comment-hd">
                    <span className="gh-comment-anchor">{loc || activeArt?.title || '批注'}</span>
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
                </div>
              );
            })}
          </div>
        )}
      </div>
    </>
  );
}
