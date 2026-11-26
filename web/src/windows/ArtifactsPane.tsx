import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { ArrowUpRight, Code2, Download, Eye, FileText, MessageSquare, Plus, RefreshCw, Trash2 } from 'lucide-react';
import type { RyugiArtifactContentResponse, TaskTimelineResponse } from '../generated/protocol';
import { ArtIcon } from '../components/Icons';
import { Md } from '../components/Md';
import { useDelivery, useRyugiArtifactContent } from '../hooks/queries';
import { api, errMsg, makeIdempotencyKey } from '../lib/api';
import type { SessionArtifactItem } from '../lib/artifacts';
import { artifactShortLabel, basename } from '../lib/format';
import { toast } from '../store/useWorkbenchStore';

function openExternal(url: string) {
  window.open(url, '_blank', 'noopener,noreferrer');
}

function isHtmlPath(path: string): boolean {
  const lower = path.toLowerCase();
  return lower.endsWith('.html') || lower.endsWith('.htm');
}

function isTextRenderable(path: string): boolean {
  const lower = path.toLowerCase();
  return (
    lower.endsWith('.md') ||
    lower.endsWith('.markdown') ||
    lower.endsWith('.html') ||
    lower.endsWith('.htm') ||
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

function fetchBlobText(sha256: string): Promise<string> {
  return api.fetchBlobText(sha256);
}

// =====================================================================
// Built-in read-only Ryugi previewers (HTML, Document XML, Excalidraw SVG)
// =====================================================================

interface ExcalidrawElem {
  id?: string;
  type: string;
  x?: number;
  y?: number;
  width?: number;
  height?: number;
  isDeleted?: boolean;
  strokeColor?: string;
  backgroundColor?: string;
  strokeWidth?: number;
  strokeStyle?: string;
  opacity?: number;
  roundness?: unknown;
  text?: string;
  fontSize?: number;
  points?: [number, number][];
  name?: string;
}

function ExcalidrawSvgPreview({ rawJson }: { rawJson: string }) {
  const parsed = useMemo(() => {
    try {
      const obj = JSON.parse(rawJson) as {
        elements?: ExcalidrawElem[];
        appState?: { viewBackgroundColor?: string };
        boards?: unknown[];
      };
      if (!Array.isArray(obj.elements)) return null;
      const elems = obj.elements.filter((e) => e && !e.isDeleted);
      if (elems.length === 0) return { empty: true as const };

      let minX = Infinity;
      let minY = Infinity;
      let maxX = -Infinity;
      let maxY = -Infinity;

      const expand = (x: number, y: number) => {
        if (!Number.isFinite(x) || !Number.isFinite(y)) return;
        if (x < minX) minX = x;
        if (y < minY) minY = y;
        if (x > maxX) maxX = x;
        if (y > maxY) maxY = y;
      };

      for (const el of elems) {
        const x = el.x ?? 0;
        const y = el.y ?? 0;
        if (Array.isArray(el.points) && el.points.length > 0) {
          for (const pt of el.points) {
            expand(x + (pt[0] ?? 0), y + (pt[1] ?? 0));
          }
        } else {
          const w = el.width ?? (el.type === 'text' ? Math.max(40, (el.text?.length ?? 4) * ((el.fontSize ?? 16) * 0.6)) : 40);
          const h = el.height ?? (el.type === 'text' ? (el.fontSize ?? 16) * 1.4 : 40);
          expand(x, y);
          expand(x + w, y + h);
        }
      }

      if (!Number.isFinite(minX)) return { empty: true as const };
      const pad = 32;
      const vw = Math.max(120, maxX - minX + pad * 2);
      const vh = Math.max(90, maxY - minY + pad * 2);
      return {
        empty: false as const,
        elems,
        viewBox: `${minX - pad} ${minY - pad} ${vw} ${vh}`,
        bg: obj.appState?.viewBackgroundColor || '#ffffff',
      };
    } catch {
      return null;
    }
  }, [rawJson]);

  if (!parsed) {
    return <pre className="doc-code mono">{rawJson.slice(0, 8000)}</pre>;
  }
  if (parsed.empty) {
    return <div className="ryugi-preview-empty">空白画板（暂无元素）</div>;
  }

  return (
    <div className="ryugi-svg-wrap" style={{ background: parsed.bg }}>
      <svg className="ryugi-svg-canvas" viewBox={parsed.viewBox} preserveAspectRatio="xMidYMid meet">
        <defs>
          <marker id="ryugi-arrow" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
            <path d="M 0 1.5 L 10 5 L 0 8.5 z" fill="context-stroke" />
          </marker>
        </defs>
        {parsed.elems.map((el, idx) => {
          const key = el.id || `${el.type}-${idx}`;
          const x = el.x ?? 0;
          const y = el.y ?? 0;
          const w = el.width ?? 0;
          const h = el.height ?? 0;
          const stroke = el.strokeColor && el.strokeColor !== 'transparent' ? el.strokeColor : '#1e1e1e';
          const fill = el.backgroundColor && el.backgroundColor !== 'transparent' ? el.backgroundColor : 'none';
          const sw = el.strokeWidth ?? 1.5;
          const dash = el.strokeStyle === 'dashed' ? '8 5' : el.strokeStyle === 'dotted' ? '2 4' : undefined;
          const opacity = typeof el.opacity === 'number' ? Math.max(0.05, Math.min(1, el.opacity / 100)) : 1;

          switch (el.type) {
            case 'rectangle':
              return (
                <rect
                  key={key}
                  x={x}
                  y={y}
                  width={Math.max(1, w)}
                  height={Math.max(1, h)}
                  rx={el.roundness ? 8 : 0}
                  fill={fill}
                  stroke={stroke}
                  strokeWidth={sw}
                  strokeDasharray={dash}
                  opacity={opacity}
                />
              );
            case 'frame':
              return (
                <g key={key} opacity={opacity}>
                  <rect
                    x={x}
                    y={y}
                    width={Math.max(1, w)}
                    height={Math.max(1, h)}
                    rx={6}
                    fill="none"
                    stroke="#8a8a93"
                    strokeWidth={1.2}
                    strokeDasharray="6 4"
                  />
                  {el.name && (
                    <text x={x + 6} y={y - 6} fontSize={12} fill="#6e6e77">
                      {el.name}
                    </text>
                  )}
                </g>
              );
            case 'ellipse':
              return (
                <ellipse
                  key={key}
                  cx={x + w / 2}
                  cy={y + h / 2}
                  rx={Math.max(1, Math.abs(w) / 2)}
                  ry={Math.max(1, Math.abs(h) / 2)}
                  fill={fill}
                  stroke={stroke}
                  strokeWidth={sw}
                  strokeDasharray={dash}
                  opacity={opacity}
                />
              );
            case 'diamond': {
              const pts = [
                `${x + w / 2},${y}`,
                `${x + w},${y + h / 2}`,
                `${x + w / 2},${y + h}`,
                `${x},${y + h / 2}`,
              ].join(' ');
              return (
                <polygon
                  key={key}
                  points={pts}
                  fill={fill}
                  stroke={stroke}
                  strokeWidth={sw}
                  strokeDasharray={dash}
                  opacity={opacity}
                />
              );
            }
            case 'arrow':
            case 'line':
            case 'freedraw': {
              const pts = (el.points ?? [])
                .map((pt) => `${x + (pt[0] ?? 0)},${y + (pt[1] ?? 0)}`)
                .join(' ');
              if (!pts) return null;
              return (
                <polyline
                  key={key}
                  points={pts}
                  fill="none"
                  stroke={stroke}
                  strokeWidth={sw}
                  strokeDasharray={dash}
                  strokeLinecap="round"
                  strokeLinejoin="round"
                  markerEnd={el.type === 'arrow' ? 'url(#ryugi-arrow)' : undefined}
                  opacity={opacity}
                />
              );
            }
            case 'text': {
              const fs = el.fontSize ?? 16;
              const lines = (el.text ?? '').split('\n');
              return (
                <text
                  key={key}
                  x={x}
                  y={y + fs}
                  fontSize={fs}
                  fill={stroke}
                  opacity={opacity}
                  fontFamily="Inter, -apple-system, sans-serif"
                >
                  {lines.map((ln, li) => (
                    <tspan key={li} x={x} dy={li === 0 ? 0 : fs * 1.25}>
                      {ln || ' '}
                    </tspan>
                  ))}
                </text>
              );
            }
            default:
              return null;
          }
        })}
      </svg>
    </div>
  );
}

function renderXmlNode(node: Element, key: string): ReactNode {
  const tag = node.tagName.toLowerCase();
  const childElems = Array.from(node.children);
  const directText = (node.textContent ?? '').trim();

  switch (tag) {
    case 'document':
    case 'doc':
    case 'body':
    case 'slides':
    case 'slide':
    case 'section': {
      const titleAttr = node.getAttribute('title') || node.getAttribute('heading');
      return (
        <section key={key} className={tag === 'section' || tag === 'slide' ? 'ryugi-doc-sec' : undefined}>
          {titleAttr && <h3 className="ryugi-doc-h">{titleAttr}</h3>}
          {childElems.length > 0
            ? childElems.map((ch, i) => renderXmlNode(ch, `${key}-${i}`))
            : directText && <p className="ryugi-doc-p">{directText}</p>}
        </section>
      );
    }
    case 'heading':
    case 'title':
    case 'h1':
    case 'h2':
    case 'h3':
    case 'h4': {
      const level = Number(node.getAttribute('level') || (tag.startsWith('h') ? tag.slice(1) : '2')) || 2;
      return (
        <div key={key} className={`ryugi-doc-h ryugi-doc-h${Math.min(4, Math.max(1, level))}`}>
          {directText}
        </div>
      );
    }
    case 'p':
    case 'paragraph':
    case 'text':
      return directText ? (
        <p key={key} className="ryugi-doc-p">
          {directText}
        </p>
      ) : null;
    case 'list':
    case 'ul':
    case 'ol': {
      const items = childElems.length > 0 ? childElems : [];
      return (
        <ul key={key} className="ryugi-doc-list">
          {items.map((it, i) => (
            <li key={`${key}-li-${i}`}>{(it.textContent ?? '').trim()}</li>
          ))}
        </ul>
      );
    }
    case 'item':
    case 'li':
      return <p key={key} className="ryugi-doc-p">• {directText}</p>;
    case 'ask': {
      const q =
        node.getAttribute('question') ||
        node.getAttribute('prompt') ||
        node.querySelector('prompt, question')?.textContent?.trim() ||
        '';
      const opts = Array.from(node.querySelectorAll('option, choice'))
        .map((o) => (o.getAttribute('label') || o.textContent || '').trim())
        .filter(Boolean);
      const fallback = !q && opts.length === 0 ? directText : '';
      return (
        <div key={key} className="ryugi-doc-ask">
          <div className="ryugi-doc-ask-tag">待确认问题</div>
          {(q || fallback) && <div className="ryugi-doc-ask-q">{q || fallback}</div>}
          {opts.length > 0 && (
            <div className="ryugi-doc-ask-opts">
              {opts.map((o, i) => (
                <span key={i} className="ryugi-doc-ask-opt">
                  {o}
                </span>
              ))}
            </div>
          )}
        </div>
      );
    }
    case 'code':
    case 'pre':
      return (
        <pre key={key} className="doc-code mono">
          {node.textContent ?? ''}
        </pre>
      );
    default:
      if (childElems.length > 0) {
        return <div key={key}>{childElems.map((ch, i) => renderXmlNode(ch, `${key}-${i}`))}</div>;
      }
      return directText ? (
        <p key={key} className="ryugi-doc-p">
          {directText}
        </p>
      ) : null;
  }
}

function RyugiXmlDocPreview({ xmlText }: { xmlText: string }) {
  const parsed = useMemo(() => {
    try {
      const parser = new DOMParser();
      const doc = parser.parseFromString(xmlText, 'application/xml');
      if (doc.querySelector('parsererror')) return null;
      return doc.documentElement;
    } catch {
      return null;
    }
  }, [xmlText]);

  if (!parsed) {
    return <pre className="doc-code mono">{xmlText}</pre>;
  }
  return <article className="doc-paper ryugi-doc-paper">{renderXmlNode(parsed, 'root')}</article>;
}

function RyugiInlinePreview({
  content,
  onOpenFull,
  opening,
}: {
  content: RyugiArtifactContentResponse;
  onOpenFull: () => void;
  opening: boolean;
}) {
  const loc = content.locator.toLowerCase();
  const ct = content.contentType.toLowerCase();

  let body: ReactNode;
  if (loc.startsWith('html/') || ct === 'text/html') {
    body = (
      <iframe
        className="ryugi-html-frame"
        sandbox="allow-scripts"
        srcDoc={content.text}
        title={content.filename || content.locator}
      />
    );
  } else if (ct === 'image/svg+xml') {
    const svgDoc = `<!doctype html><html><body style="margin:0;display:flex;align-items:center;justify-content:center;min-height:100vh;background:#fff;">${content.text}</body></html>`;
    body = (
      <iframe
        className="ryugi-html-frame"
        sandbox=""
        srcDoc={svgDoc}
        title={content.filename || content.locator}
      />
    );
  } else if (loc.startsWith('document/') || loc.startsWith('slides/') || ct === 'application/xml') {
    if (ct === 'text/markdown') {
      body = (
        <article className="doc-paper">
          <Md text={content.text} />
        </article>
      );
    } else {
      body = <RyugiXmlDocPreview xmlText={content.text} />;
    }
  } else if (loc.startsWith('whiteboard/') || loc.startsWith('artboard/') || ct === 'application/json') {
    body = <ExcalidrawSvgPreview rawJson={content.text} />;
  } else {
    body = <pre className="doc-code mono">{content.text}</pre>;
  }

  return (
    <div className="ryugi-preview-box">
      <div className="ryugi-preview-hd">
        <span className="faint mono">{content.filename}</span>
        <span className="faint">内置只读预览</span>
      </div>
      <div className="ryugi-preview-body">{body}</div>
      <div className="ryugi-preview-ft">
        <button type="button" className="btn ghost sm" disabled={opening} onClick={onOpenFull}>
          {opening ? '正在启动画布…' : '打开完整可编辑画布'} <ArrowUpRight size={12} strokeWidth={1.7} />
        </button>
      </div>
    </div>
  );
}

/**
 * Full-height Artifacts viewer (zero bottom tray):
 * Supports both Ryugi canvases (with live self-healing open + built-in read-only preview fallback)
 * and document blobs (Markdown/HTML/CSV/Text/Word/PPT/PDF).
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
  const [showRawHtml, setShowRawHtml] = useState(false);

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
  const isHtmlBlob = activeArt?.source === 'blob' && !!activeArt.locator && isHtmlPath(activeArt.locator);

  const blobQ = useQuery({
    queryKey: ['blob-text', activeArt?.blobSha256],
    queryFn: () => fetchBlobText(activeArt!.blobSha256!),
    enabled: isTextBlob,
  });

  const activeBinding = useMemo(() => {
    if (!isRyugi) return undefined;
    if (activeArt?.artifactId.startsWith('ryugi:')) {
      const id = activeArt.artifactId.slice('ryugi:'.length);
      return bindings.find((b) => b.bindingId === id) ?? bindings[0];
    }
    return bindings[0];
  }, [isRyugi, activeArt, bindings]);

  const ryugiContentQ = useRyugiArtifactContent(
    effectiveDeliveryId,
    activeBinding?.bindingId,
    isRyugi && !!activeBinding,
  );

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

  const openCanvas = useMutation({
    mutationFn: async (bindingId: string) => {
      const res = await api.openDeliveryRyugi(effectiveDeliveryId, bindingId);
      return res;
    },
    onSuccess: (res) => {
      refresh();
      const targetUrl = res.openUrl || activeBinding?.openUrl || '';
      if (targetUrl) {
        openExternal(targetUrl);
      } else {
        toast('未获取到有效的画布地址', 'err');
      }
    },
    onError: (e) => {
      if (activeBinding?.openUrl) {
        toast(`刷新画布失败，尝试打开旧链接：${errMsg(e)}`, 'err');
        openExternal(activeBinding.openUrl);
      } else {
        toast(`打开画布失败：${errMsg(e)}`, 'err');
      }
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

  // Auto-sync silently on window focus after returning from external Ryugi canvas
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
    setShowRawHtml(false);
  }, [activeArt?.artifactId]);

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
          {!isRyugi && isHtmlBlob && (
            <button
              type="button"
              className="btn ghost"
              onClick={() => setShowRawHtml((v) => !v)}
              title={showRawHtml ? '切换到预览' : '查看 HTML 源码'}
            >
              {showRawHtml ? <Eye size={12} strokeWidth={1.7} /> : <Code2 size={12} strokeWidth={1.7} />}
              {showRawHtml ? '预览' : '源码'}
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
              <button
                type="button"
                className="ib"
                title="下载文件"
                aria-label="下载文件"
                onClick={() => void api.downloadBlob(activeArt.blobSha256!, activeArt.title).catch((e) => toast(errMsg(e), 'err'))}
              >
                <Download size={14} strokeWidth={1.6} />
              </button>
            </>
          )}
          {headerEnd}
        </div>
      </div>

      <div className="art-single">
        {dq.isLoading && <div className="empty">载入中…</div>}
        {!dq.isLoading && !activeArt && bindings.length === 0 && <div className="empty">没有可预览的 Artifact</div>}

        {/* Ryugi interactive design canvas + built-in read-only preview fallback */}
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
                disabled={openCanvas.isPending}
                onClick={() => openCanvas.mutate(activeBinding.bindingId)}
              >
                {openCanvas.isPending ? '打开中…' : '打开完整画布'} <ArrowUpRight size={12} strokeWidth={1.7} />
              </button>
            </div>

            {ryugiContentQ.isLoading && <div className="quiet">载入内置预览…</div>}
            {ryugiContentQ.data && (
              <RyugiInlinePreview
                content={ryugiContentQ.data}
                opening={openCanvas.isPending}
                onOpenFull={() => openCanvas.mutate(activeBinding.bindingId)}
              />
            )}
          </div>
        )}

        {/* Document blob viewer (Markdown / HTML sandbox / Text / Binary Card) */}
        {!isRyugi && activeArt && (
          <div className="doc-reader">
            {isTextBlob ? (
              blobQ.isLoading ? (
                <div className="empty">读取文档…</div>
              ) : blobQ.isError ? (
                <div className="empty bad-t">{errMsg(blobQ.error)}</div>
              ) : isHtmlBlob && !showRawHtml ? (
                <iframe
                  className="ryugi-html-frame"
                  sandbox="allow-scripts"
                  srcDoc={blobQ.data ?? ''}
                  title={activeArt.title}
                />
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
                  <button
                    type="button"
                    className="btn pri"
                    onClick={() => void api.downloadBlob(activeArt.blobSha256!, activeArt.title).catch((e) => toast(errMsg(e), 'err'))}
                  >
                    <Download size={12} strokeWidth={1.7} /> 下载
                  </button>
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
