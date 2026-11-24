import { useMemo, useState } from 'react';
import { FileSpreadsheet, FileText, LayoutTemplate, MessageSquare, Paperclip, Presentation } from 'lucide-react';
import type { GlobalArtifactView } from '../generated/protocol';
import { PageHeader } from '../components/PageHeader';
import { useGlobalArtifacts } from '../hooks/queries';
import { artifactFilterLabel, artifactShortLabel, relTime } from '../lib/format';
import { useWorkbenchStore } from '../store/useWorkbenchStore';

const FILTER_KINDS = ['all', 'design', 'markdown', 'pptx', 'docx', 'spreadsheet', 'file'] as const;

export function ArtifactKindIcon({ kind, size = 14 }: { kind: string; size?: number }) {
  switch (kind) {
    case 'design':
      return <LayoutTemplate size={size} strokeWidth={1.6} />;
    case 'pptx':
      return <Presentation size={size} strokeWidth={1.6} />;
    case 'spreadsheet':
      return <FileSpreadsheet size={size} strokeWidth={1.6} />;
    case 'markdown':
    case 'docx':
      return <FileText size={size} strokeWidth={1.6} />;
    default:
      return <Paperclip size={size} strokeWidth={1.6} />;
  }
}

export function ArtifactsPage() {
  const q = useGlobalArtifacts(true);
  const openReview = useWorkbenchStore((s) => s.openReview);
  const [filter, setFilter] = useState<string>('all');

  const items = useMemo(() => q.data ?? [], [q.data]);
  const presentKinds = useMemo(() => {
    const set = new Set(items.map((a) => a.kind));
    return FILTER_KINDS.filter((k) => k === 'all' || set.has(k));
  }, [items]);

  const visible = useMemo(
    () => items.filter((a) => filter === 'all' || a.kind === filter),
    [items, filter],
  );

  const openItem = (a: GlobalArtifactView) => {
    openReview('art', a.taskId, a.deliveryId, a.artifactId);
  };

  return (
    <main className="main">
      <PageHeader>
        <span className="ttl">Artifacts</span>
      </PageHeader>
      <div className="page-scroll">
        <div className="page art-page">
          <div className="home-hd">
            <div>
              <h1 className="home-title">Artifacts</h1>
            </div>
          </div>

          {presentKinds.length > 1 && (
            <div className="art-filters" role="tablist" aria-label="类型筛选">
              {presentKinds.map((k) => (
                <button
                  key={k}
                  type="button"
                  role="tab"
                  aria-selected={filter === k}
                  className={`art-filter-btn ${filter === k ? 'active' : ''}`}
                  onClick={() => setFilter(k)}
                >
                  {artifactFilterLabel(k)}
                </button>
              ))}
            </div>
          )}

          {q.isLoading ? (
            <div className="empty">载入 Artifacts…</div>
          ) : visible.length === 0 ? (
            <div className="quiet-alone">还没有归档的 Artifacts</div>
          ) : (
            <div className="art-lib-list">
              {visible.map((a) => (
                <button
                  type="button"
                  key={a.artifactId}
                  className="dlv-card"
                  onClick={() => openItem(a)}
                  title={`${a.title} · ${a.locator}`}
                >
                  <span className="dlv-ico">
                    <ArtifactKindIcon kind={a.kind} size={14} />
                  </span>
                  <div className="dlv-info">
                    <div className="dlv-title">{a.title}</div>
                    <div className="dlv-meta">
                      {a.projectName} · {a.taskTitle} · {relTime(a.updatedAt)}
                    </div>
                  </div>
                  {a.openAnnotations > 0 ? (
                    <span className="comment-count-badge">
                      <MessageSquare size={11} strokeWidth={1.7} />
                      {a.openAnnotations}
                    </span>
                  ) : (
                    <span className="dlv-kind-tag">{artifactShortLabel(a.kind)}</span>
                  )}
                </button>
              ))}
            </div>
          )}
        </div>
      </div>
    </main>
  );
}
