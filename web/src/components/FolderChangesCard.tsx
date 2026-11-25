import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Check, ChevronDown, ChevronRight, Download, Layers } from 'lucide-react';
import type { FolderChangesSummary } from '../generated/protocol';
import { api, errMsg } from '../lib/api';
import { toast } from '../store/useWorkbenchStore';

/**
 * Compact plain-folder in-place change card (no shadow Git):
 * - Header: `变更 N 个文件   +A −D   [收进 Artifacts]`
 * - Expandable 27px file list with status letter (`A`/`M`/`D`), line delta, and direct download.
 */
export function FolderChangesCard({
  taskId,
  attemptId,
  changes,
}: {
  taskId: string;
  attemptId: string;
  changes: FolderChangesSummary;
}) {
  const qc = useQueryClient();
  const [open, setOpen] = useState(true);
  const [downloadingPath, setDownloadingPath] = useState('');

  const promote = useMutation({
    mutationFn: () => api.promoteFolderChanges(taskId, attemptId),
    onSuccess: () => {
      toast('已收进 Artifacts');
      void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] });
      void qc.invalidateQueries({ queryKey: ['global-artifacts'] });
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const files = changes.files ?? [];
  if (files.length === 0) return null;

  const hasDownloadable = files.some((f) => f.kind !== 'del');

  const handleDownload = async (relPath: string) => {
    setDownloadingPath(relPath);
    try {
      await api.downloadTaskFolderFile(taskId, relPath, attemptId);
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setDownloadingPath('');
    }
  };

  return (
    <div className="it">
      <div className="fc-card">
        <div className="fc-hd">
          <button
            type="button"
            className="fc-toggle"
            onClick={() => setOpen((v) => !v)}
            aria-expanded={open}
          >
            {open ? <ChevronDown size={13} strokeWidth={1.8} /> : <ChevronRight size={13} strokeWidth={1.8} />}
            <span className="fc-title">变更 {files.length} 个文件</span>
            {(changes.totalAdds > 0 || changes.totalDels > 0) && (
              <span className="fc-delta mono">
                {changes.totalAdds > 0 && <span className="add">+{changes.totalAdds}</span>}
                {changes.totalDels > 0 && <span className="del">−{changes.totalDels}</span>}
              </span>
            )}
          </button>

          {hasDownloadable && (
            changes.promoted ? (
              <span className="fc-promoted">
                <Check size={11.5} strokeWidth={2.2} />
                已收进 Artifacts
              </span>
            ) : (
              <button
                type="button"
                className="fc-promote-btn"
                disabled={promote.isPending}
                onClick={() => promote.mutate()}
              >
                <Layers size={11.5} strokeWidth={1.7} />
                {promote.isPending ? '归档中…' : '收进 Artifacts'}
              </button>
            )
          )}
        </div>

        {open && (
          <div className="fc-list">
            {files.map((f) => {
              const tag = f.kind === 'add' ? 'A' : f.kind === 'del' ? 'D' : 'M';
              return (
                <div className="fc-row" key={f.path}>
                  <span className={`fc-k mono k-${f.kind}`}>{tag}</span>
                  <span className="fc-path mono ell" title={f.path}>
                    {f.path}
                  </span>
                  <span className="fc-stat mono">
                    {f.insertions > 0 && <span className="add">+{f.insertions}</span>}
                    {f.deletions > 0 && <span className="del">−{f.deletions}</span>}
                  </span>
                  {f.kind !== 'del' ? (
                    <button
                      type="button"
                      className="fc-dl"
                      title={`下载 ${f.path}`}
                      aria-label={`下载 ${f.path}`}
                      disabled={downloadingPath === f.path}
                      onClick={() => void handleDownload(f.path)}
                    >
                      <Download size={12} strokeWidth={1.7} />
                    </button>
                  ) : (
                    <span className="fc-dl-ph" />
                  )}
                </div>
              );
            })}
          </div>
        )}
      </div>
    </div>
  );
}
