import { useState } from 'react';
import { ChevronDown, ChevronRight, Download, Folder } from 'lucide-react';
import type { FolderChangesSummary } from '../generated/protocol';
import { api, errMsg } from '../lib/api';
import { toast } from '../store/useWorkbenchStore';

/**
 * Full-width (100%), airy plain-folder in-place change strip (no shadow Git, no gray fill, no "收进 Artifacts"):
 * - Header: `[chevron] [folder] 已修改 N 个文件       +A −D`
 * - Expandable 27px file rows with status letter (`A`/`M`/`D`), line delta, and direct download icon.
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
  const [open, setOpen] = useState(true);
  const [downloadingPath, setDownloadingPath] = useState('');

  const files = changes.files ?? [];
  if (files.length === 0) return null;

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
        <button
          type="button"
          className="fc-hd"
          onClick={() => setOpen((v) => !v)}
          aria-expanded={open}
        >
          {open ? <ChevronDown size={13} strokeWidth={1.8} className="muted" /> : <ChevronRight size={13} strokeWidth={1.8} className="muted" />}
          <Folder size={12.5} strokeWidth={1.7} className="muted" />
          <span className="fc-title">已修改 {files.length} 个文件</span>
          {(changes.totalAdds > 0 || changes.totalDels > 0) && (
            <span className="fc-delta mono">
              {changes.totalAdds > 0 && <span className="add">+{changes.totalAdds}</span>}
              {changes.totalDels > 0 && <span className="del">−{changes.totalDels}</span>}
            </span>
          )}
        </button>

        {open && (
          <div className="fc-list">
            {(changes.concurrentSharedDir || changes.concurrentNotice) && (
              <div className="fc-row" style={{ color: 'var(--warn)', fontSize: 11.5 }}>
                <span className="ell">{changes.concurrentNotice || '共享目录存在并发运行，以下变更可能包含同期其他会话的修改'}</span>
              </div>
            )}
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
