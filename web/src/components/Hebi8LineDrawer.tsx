import { useEffect, useMemo, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ChevronDown, ChevronRight, Eye, FoldVertical, GitBranch, Pin, PinOff, X } from 'lucide-react';
import type { Hebi8LineNode, Hebi8LineSummary } from '../generated/protocol';
import { useHebi8Expand } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import { toast } from '../store/useWorkbenchStore';
import { Md } from './Md';

export const HEBI8_HEAD_NODE_ID = '__head__';

function fmtShortRange(fromRFC?: string, toRFC?: string, fromMs?: number, toMs?: number): string {
  const f = fromMs ? new Date(fromMs) : fromRFC ? new Date(fromRFC) : null;
  const t = toMs ? new Date(toMs) : toRFC ? new Date(toRFC) : null;
  if (!f || Number.isNaN(f.getTime())) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  const fd = `${pad(f.getMonth() + 1)}-${pad(f.getDate())} ${pad(f.getHours())}:${pad(f.getMinutes())}`;
  if (!t || Number.isNaN(t.getTime())) return fd;
  const sameDay = f.getFullYear() === t.getFullYear() && f.getMonth() === t.getMonth() && f.getDate() === t.getDate();
  const td = sameDay
    ? `${pad(t.getHours())}:${pad(t.getMinutes())}`
    : `${pad(t.getMonth() + 1)}-${pad(t.getDate())} ${pad(t.getHours())}:${pad(t.getMinutes())}`;
  return `${fd}~${td}`;
}

function BranchChildItem({
  node,
  nodeMap,
  selectedId,
  expandedFoldIds,
  onToggleFold,
  onSelect,
}: {
  node: Hebi8LineNode;
  nodeMap: ReadonlyMap<string, Hebi8LineNode>;
  selectedId: string | null;
  expandedFoldIds: ReadonlySet<string>;
  onToggleFold: (id: string) => void;
  onSelect: (id: string) => void;
}) {
  const isFold = node.kind === 'fold';
  const subChildren = useMemo(
    () =>
      isFold
        ? (node.covers ?? [])
            .map((cid) => nodeMap.get(cid))
            .filter((x): x is Hebi8LineNode => !!x)
            .sort((a, b) => b.seq - a.seq)
        : [],
    [isFold, node.covers, nodeMap],
  );
  const isOpen = expandedFoldIds.has(node.id);
  const isOn = selectedId === node.id;
  const rangeStr = fmtShortRange(node.coversFrom, node.coversTo, node.coversFromMs, node.coversToMs);
  const countStr = isFold ? `${subChildren.length} 段` : `${node.attemptRange?.turnCount ?? 0} 轮`;

  return (
    <div className="h8-git-branch-node">
      <div className="h8-git-branch-row-wrap">
        <button
          type="button"
          className={`h8-git-branch-row ${isOn ? 'on' : ''}`}
          onClick={() => onSelect(node.id)}
          title={node.title}
        >
          <div className="h8-git-card-top">
            <span className={`h8-kind-tag ${isFold ? 'fold' : 'seg'}`}>
              {isFold ? `L${node.level} 合并` : '片段'}
            </span>
            <span className="h8-node-count mono">{countStr}</span>
          </div>
          <div className="h8-git-card-ttl">{node.title}</div>
          {rangeStr && <div className="h8-git-card-sub mono">{rangeStr}</div>}
        </button>
        {isFold && subChildren.length > 0 && (
          <button
            type="button"
            className={`h8-git-exp-btn ${isOpen ? 'open' : ''}`}
            title={isOpen ? '收起下层原始片段' : `展开下层 ${subChildren.length} 个原始片段`}
            aria-label={isOpen ? '收起折叠子层' : '展开折叠子层'}
            aria-expanded={isOpen}
            onClick={(e) => {
              e.stopPropagation();
              onToggleFold(node.id);
            }}
          >
            {isOpen ? <ChevronDown size={13} strokeWidth={1.8} /> : <ChevronRight size={13} strokeWidth={1.8} />}
          </button>
        )}
      </div>
      {isFold && isOpen && subChildren.length > 0 && (
        <div className="h8-git-branch-list is-nested">
          {subChildren.map((ch) => (
            <BranchChildItem
              key={ch.id}
              node={ch}
              nodeMap={nodeMap}
              selectedId={selectedId}
              expandedFoldIds={expandedFoldIds}
              onToggleFold={onToggleFold}
              onSelect={onSelect}
            />
          ))}
        </div>
      )}
    </div>
  );
}

function TrunkGitNode({
  node,
  isLast,
  nodeMap,
  selectedId,
  expandedFoldIds,
  onToggleFold,
  onSelect,
}: {
  node: Hebi8LineNode;
  isLast: boolean;
  nodeMap: ReadonlyMap<string, Hebi8LineNode>;
  selectedId: string | null;
  expandedFoldIds: ReadonlySet<string>;
  onToggleFold: (id: string) => void;
  onSelect: (id: string) => void;
}) {
  const isFold = node.kind === 'fold';
  const children = useMemo(
    () =>
      isFold
        ? (node.covers ?? [])
            .map((cid) => nodeMap.get(cid))
            .filter((x): x is Hebi8LineNode => !!x)
            .sort((a, b) => b.seq - a.seq)
        : [],
    [isFold, node.covers, nodeMap],
  );
  const isOpen = expandedFoldIds.has(node.id);
  const isOn = selectedId === node.id;
  const hasVisibleBranch = isFold && isOpen && children.length > 0;
  const rangeStr = fmtShortRange(node.coversFrom, node.coversTo, node.coversFromMs, node.coversToMs);
  const countStr = isFold ? `${children.length} 段` : `${node.attemptRange?.turnCount ?? 0} 轮`;

  return (
    <div className="h8-git-group" role="treeitem" aria-expanded={isFold ? isOpen : undefined} aria-selected={isOn}>
      <div className="h8-git-item" data-h8-node-id={node.id}>
        <div className="h8-git-rail" aria-hidden="true">
          <span className="h8-git-line-top" />
          <span className={`h8-git-dot ${isFold ? 'is-fold' : ''}`} />
          <span className={`h8-git-line-bot ${isLast && !hasVisibleBranch ? 'is-none' : ''}`} />
        </div>
        <div className="h8-git-card-wrap">
          <button
            type="button"
            className={`h8-git-card ${isOn ? 'on' : ''}`}
            onClick={() => onSelect(node.id)}
            title={node.title}
          >
            <div className="h8-git-card-top">
              <span className={`h8-kind-tag ${isFold ? 'fold' : 'seg'}`}>
                {isFold ? `L${node.level} 合并` : '片段'}
              </span>
              <span className="h8-node-count mono">{countStr}</span>
            </div>
            <div className="h8-git-card-ttl">{node.title}</div>
            {rangeStr && <div className="h8-git-card-sub mono">{rangeStr}</div>}
          </button>
          {isFold && children.length > 0 && (
            <button
              type="button"
              className={`h8-git-exp-btn ${isOpen ? 'open' : ''}`}
              title={isOpen ? '收起下层原始片段' : `展开下层 ${children.length} 个原始片段`}
              aria-label={isOpen ? '收起折叠子层' : '展开折叠子层'}
              aria-expanded={isOpen}
              onClick={(e) => {
                e.stopPropagation();
                onToggleFold(node.id);
              }}
            >
              {isOpen ? <ChevronDown size={13} strokeWidth={1.8} /> : <ChevronRight size={13} strokeWidth={1.8} />}
            </button>
          )}
        </div>
      </div>

      {hasVisibleBranch && (
        <div className="h8-git-branch" role="group">
          <div className={`h8-git-branch-trunk ${isLast ? 'is-last' : ''}`} aria-hidden="true" />
          <div className="h8-git-branch-list">
            {children.map((ch) => (
              <BranchChildItem
                key={ch.id}
                node={ch}
                nodeMap={nodeMap}
                selectedId={selectedId}
                expandedFoldIds={expandedFoldIds}
                onToggleFold={onToggleFold}
                onSelect={onSelect}
              />
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

/**
 * Vertical Git-Tree context node drawer for Hebi8 (v4):
 * - Standard `git log --graph` order: `HEAD · 当前活跃段` fixed at top, older nodes descending below.
 * - All merged (fold) nodes collapsed by default.
 * - Minimalist node rows: no Pin badges, no internal node IDs, no footer hint copy.
 * - Clear interaction separation: expand chevron toggles child branch (stopPropagation); node body opens dismissible detail sheet.
 * - Manual fold button sits in drawer header; HEAD turn count resets to 0 immediately after fold.
 */
export function Hebi8LineDrawer({
  nodes,
  summary,
  pendingTurns,
  busy = false,
  folding = false,
  selectedNodeId,
  onSelectNode,
  onFold,
  onClose,
}: {
  nodes: Hebi8LineNode[];
  summary: Hebi8LineSummary;
  pendingTurns: number;
  busy?: boolean;
  folding?: boolean;
  selectedNodeId: string | null;
  onSelectNode: (nodeId: string | null) => void;
  onFold: () => void;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  // All merged fold nodes start collapsed by default
  const [expandedFoldIds, setExpandedFoldIds] = useState<Set<string>>(() => new Set());
  const [turnOffset, setTurnOffset] = useState(0);
  const [showRawTurns, setShowRawTurns] = useState(false);

  const nodeMap = useMemo(() => {
    const m = new Map<string, Hebi8LineNode>();
    for (const n of nodes) m.set(n.id, n);
    return m;
  }, [nodes]);

  const roots = useMemo(
    () => [...nodes].filter((n) => !n.foldedInto).sort((a, b) => b.seq - a.seq),
    [nodes],
  );

  // When a specific nested node is selected externally (e.g. from timeline fold divider),
  // expand only its ancestor fold nodes so it becomes visible.
  useEffect(() => {
    setTurnOffset(0);
    setShowRawTurns(false);
    if (!selectedNodeId || selectedNodeId === HEBI8_HEAD_NODE_ID) return;
    const ancestors: string[] = [];
    let cur = nodeMap.get(selectedNodeId);
    let guard = 0;
    while (cur?.foldedInto && guard < 16) {
      ancestors.push(cur.foldedInto);
      cur = nodeMap.get(cur.foldedInto);
      guard++;
    }
    if (ancestors.length > 0) {
      setExpandedFoldIds((prev) => {
        let changed = false;
        for (const id of ancestors) {
          if (!prev.has(id)) {
            changed = true;
            break;
          }
        }
        if (!changed) return prev;
        const next = new Set(prev);
        for (const id of ancestors) next.add(id);
        return next;
      });
    }
  }, [selectedNodeId, nodeMap]);

  const isHeadSelected = selectedNodeId === HEBI8_HEAD_NODE_ID;
  const activeRealNodeId = !isHeadSelected && selectedNodeId && nodeMap.has(selectedNodeId) ? selectedNodeId : null;

  const expandQ = useHebi8Expand(activeRealNodeId ?? undefined, {
    offset: turnOffset,
    limit: 10,
    enabled: !!activeRealNodeId,
  });

  const pinMut = useMutation({
    mutationFn: ({ nodeId, pinned }: { nodeId: string; pinned: boolean }) => api.pinHebi8Node(nodeId, pinned),
    onSuccess: (res) => {
      toast(res.pinned ? '已钉住该节点' : '已取消钉住');
      void qc.invalidateQueries({ queryKey: ['hebi8-main'] });
      void qc.invalidateQueries({ queryKey: ['hebi8-line'] });
      void qc.invalidateQueries({ queryKey: ['hebi8-expand', res.nodeId] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const toggleFold = (id: string) => {
    setExpandedFoldIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const handleSelectNode = (id: string) => {
    onSelectNode(selectedNodeId === id ? null : id);
  };

  const canFoldHead = pendingTurns > 0 && !busy && !folding;
  const foldTitle =
    pendingTurns <= 0
      ? '当前活跃段暂无新增轮次可折叠'
      : busy
        ? '请等待当前轮回复完成后再折叠换代'
        : `将当前活跃段 ${pendingTurns} 轮手动折叠归档为新节点`;

  const detail = expandQ.data;
  const detailNode = detail?.node ?? (activeRealNodeId ? nodeMap.get(activeRealNodeId) : undefined);

  return (
    <aside className="h8-drawer" aria-label="Hebi8 上下文节点树">
      <div className="h8-drawer-hd">
        <div className="h8-drawer-title">
          <GitBranch size={13.5} strokeWidth={1.7} />
          <span>上下文节点树</span>
          <span className="h8-drawer-meta mono">{summary.totalNodes || nodes.length} 节点</span>
        </div>
        <div className="h8-drawer-hd-acts">
          <button
            type="button"
            className="btn ghost sm h8-drawer-fold-btn"
            disabled={!canFoldHead}
            title={foldTitle}
            onClick={onFold}
          >
            <FoldVertical size={12} strokeWidth={1.7} />
            <span>{folding ? '折叠中…' : '折叠当前段'}</span>
          </button>
          <button type="button" className="ib" title="关闭节点树" aria-label="关闭节点树" onClick={onClose}>
            <X size={14} strokeWidth={1.7} />
          </button>
        </div>
      </div>

      <div className="h8-drawer-body">
        <div className="h8-git-scroll" role="tree" aria-label="上下文历史节点">
          {/* HEAD · 当前活跃段 pinned at top */}
          <div className="h8-git-group" role="treeitem" aria-selected={isHeadSelected}>
            <div className="h8-git-item" data-h8-node-id={HEBI8_HEAD_NODE_ID}>
              <div className="h8-git-rail" aria-hidden="true">
                <span className="h8-git-line-top is-none" />
                <span className="h8-git-dot is-head" />
                <span className={`h8-git-line-bot ${roots.length > 0 ? 'is-active' : 'is-none'}`} />
              </div>
              <div className="h8-git-card-wrap">
                <button
                  type="button"
                  className={`h8-git-card is-head ${isHeadSelected ? 'on' : ''}`}
                  onClick={() => handleSelectNode(HEBI8_HEAD_NODE_ID)}
                >
                  <div className="h8-git-card-top">
                    <span className="h8-head-badge">HEAD</span>
                    <span className="h8-head-lbl">当前活跃段</span>
                    <span className="h8-node-count mono">{pendingTurns} 轮</span>
                  </div>
                  <div className="h8-git-card-sub">
                    {pendingTurns > 0
                      ? '最新对话实时演进中 · 点击查看或归档折叠'
                      : '暂无新增可折叠轮次'}
                  </div>
                </button>
              </div>
            </div>
          </div>

          {roots.length === 0 ? (
            <div className="h8-git-empty">暂无已折叠的历史节点</div>
          ) : (
            roots.map((n, idx) => (
              <TrunkGitNode
                key={n.id}
                node={n}
                isLast={idx === roots.length - 1}
                nodeMap={nodeMap}
                selectedId={activeRealNodeId}
                expandedFoldIds={expandedFoldIds}
                onToggleFold={toggleFold}
                onSelect={handleSelectNode}
              />
            ))
          )}
        </div>

        {/* On-demand dismissible detail sheet (ONLY rendered when user selects a node or HEAD) */}
        {isHeadSelected && (
          <div className="h8-detail-sheet" role="region" aria-label="当前活跃段详情">
            <div className="h8-detail-hd">
              <div className="h8-detail-idrow">
                <span className="h8-kind-tag fold">HEAD 活跃段</span>
                <span className="faint mono" style={{ fontSize: 11 }}>
                  {pendingTurns} 轮
                </span>
              </div>
              <div className="h8-detail-hd-acts">
                <button
                  type="button"
                  className="btn ghost sm"
                  disabled={!canFoldHead}
                  title={foldTitle}
                  onClick={onFold}
                >
                  <FoldVertical size={12} strokeWidth={1.7} />
                  <span>{folding ? '折叠中…' : '折叠当前段'}</span>
                </button>
                <button
                  type="button"
                  className="ib"
                  title="收起详情"
                  aria-label="收起详情"
                  onClick={() => onSelectNode(null)}
                >
                  <X size={13.5} strokeWidth={1.7} />
                </button>
              </div>
            </div>
            <div className="h8-detail-title">当前活跃段（正在进行）</div>
            <div className="h8-detail-md">
              {pendingTurns > 0
                ? `当前活跃段已累积 ${pendingTurns} 轮对话。点击「折叠当前段」可将其封装为不可变摘要节点并开启全新底层会话上下文。`
                : '刚刚完成折叠换代，当前活跃段暂无新增轮次；继续发起新对话后可再次折叠归档。'}
            </div>
          </div>
        )}

        {!isHeadSelected && detailNode && (
          <div className="h8-detail-sheet" role="region" aria-label="节点详情">
            <div className="h8-detail-hd">
              <div className="h8-detail-idrow">
                <span className={`h8-kind-tag ${detailNode.kind === 'fold' ? 'fold' : 'seg'}`}>
                  {detailNode.kind === 'fold' ? `L${detailNode.level} 合并摘要` : '历史片段'}
                </span>
                <span className="faint" style={{ fontSize: 11 }}>
                  {detailNode.summaryQuality === 'written' ? '精写摘要' : '自动摘要'}
                </span>
              </div>
              <div className="h8-detail-hd-acts">
                <button
                  type="button"
                  className={`ib ${detailNode.pinned ? 'h8-pin-active' : ''}`}
                  disabled={pinMut.isPending}
                  onClick={() => pinMut.mutate({ nodeId: detailNode.id, pinned: !detailNode.pinned })}
                  title={detailNode.pinned ? '取消钉住' : '钉住此节点（不会被自动合并，上限 8 个）'}
                  aria-label={detailNode.pinned ? '取消钉住' : '钉住此节点'}
                >
                  {detailNode.pinned ? <PinOff size={13} strokeWidth={1.7} /> : <Pin size={13} strokeWidth={1.7} />}
                </button>
                <button
                  type="button"
                  className="ib"
                  title="收起详情"
                  aria-label="收起详情"
                  onClick={() => onSelectNode(null)}
                >
                  <X size={13.5} strokeWidth={1.7} />
                </button>
              </div>
            </div>

            <div className="h8-detail-title">{detailNode.title}</div>
            <div className="h8-detail-meta mono">
              {fmtShortRange(detailNode.coversFrom, detailNode.coversTo, detailNode.coversFromMs, detailNode.coversToMs)}
              {detailNode.attemptRange &&
                ` · #${detailNode.attemptRange.startAttemptNo}–#${detailNode.attemptRange.endAttemptNo} (${detailNode.attemptRange.turnCount} 轮)`}
              {detailNode.kind === 'fold' && ` · 覆盖 ${detailNode.covers?.length ?? 0} 段`}
            </div>
            {detailNode.tags && detailNode.tags.length > 0 && (
              <div className="h8-tags">
                {detailNode.tags.map((tg) => (
                  <span className="h8-tag" key={tg}>
                    #{tg}
                  </span>
                ))}
              </div>
            )}

            {expandQ.isLoading ? (
              <div className="muted" style={{ padding: '10px 0', fontSize: 12 }}>
                载入节点摘要…
              </div>
            ) : expandQ.isError ? (
              <div className="bad-t" style={{ padding: '10px 0', fontSize: 12 }}>
                读取失败：{errMsg(expandQ.error)}
              </div>
            ) : detail ? (
              <>
                <div className="h8-detail-md">
                  <Md text={detail.summary || '（无摘要正文）'} />
                </div>

                {detail.node.kind === 'fold' && detail.children && detail.children.length > 0 && (
                  <div className="h8-subsec">
                    <div className="h8-subsec-hd">包含的下层片段</div>
                    <div className="h8-child-cards">
                      {detail.children.map((ch) => (
                        <button
                          key={ch.id}
                          type="button"
                          className="h8-child-card"
                          onClick={() => onSelectNode(ch.id)}
                        >
                          <div className="h8-git-card-top">
                            <span className={`h8-kind-tag ${ch.kind === 'fold' ? 'fold' : 'seg'}`}>
                              {ch.kind === 'fold' ? `L${ch.level} 合并` : '片段'}
                            </span>
                            <span className="h8-node-count mono">
                              {ch.kind === 'fold'
                                ? `${ch.covers?.length ?? 0} 段`
                                : `${ch.attemptRange?.turnCount ?? 0} 轮`}
                            </span>
                          </div>
                          <div className="h8-child-ttl ell">{ch.title}</div>
                        </button>
                      ))}
                    </div>
                  </div>
                )}

                {detail.node.kind === 'segment' && (
                  <div className="h8-subsec">
                    <div className="h8-subsec-hd">
                      <button
                        type="button"
                        className="btn ghost sm"
                        onClick={() => setShowRawTurns((v) => !v)}
                      >
                        <Eye size={12} strokeWidth={1.7} />
                        <span>{showRawTurns ? '收起原文' : `预览原文 (${detail.totalTurns ?? 0} 轮)`}</span>
                      </button>
                    </div>
                    {showRawTurns && (
                      <div className="h8-raw-turns">
                        {(detail.turns ?? []).map((t) => (
                          <div className="h8-raw-turn" key={t.attemptId}>
                            <div className="h8-raw-turn-hd mono">第 #{t.attemptNo} 轮</div>
                            {t.userPrompt && (
                              <div className="h8-raw-u">
                                <b>你：</b>
                                <span>{t.userPrompt}</span>
                              </div>
                            )}
                            {t.assistantReply && (
                              <div className="h8-raw-a">
                                <b>Hebi8：</b>
                                <span>{t.assistantReply}</span>
                              </div>
                            )}
                          </div>
                        ))}
                        {(detail.totalTurns ?? 0) > (detail.limit ?? 10) && (
                          <div className="h8-pager">
                            <button
                              type="button"
                              className="btn ghost sm"
                              disabled={turnOffset === 0}
                              onClick={() => setTurnOffset((o) => Math.max(0, o - 10))}
                            >
                              上一页
                            </button>
                            <span className="faint mono" style={{ fontSize: 11 }}>
                              {turnOffset + 1}–{Math.min(detail.totalTurns ?? 0, turnOffset + 10)} / {detail.totalTurns}
                            </span>
                            <button
                              type="button"
                              className="btn ghost sm"
                              disabled={turnOffset + 10 >= (detail.totalTurns ?? 0)}
                              onClick={() => setTurnOffset((o) => o + 10)}
                            >
                              下一页
                            </button>
                          </div>
                        )}
                      </div>
                    )}
                  </div>
                )}
              </>
            ) : null}
          </div>
        )}
      </div>
    </aside>
  );
}
