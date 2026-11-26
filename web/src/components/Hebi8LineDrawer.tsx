import { useEffect, useMemo, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ChevronDown, ChevronRight, Layers, Pin, PinOff, X } from 'lucide-react';
import type { Hebi8LineNode, Hebi8LineSummary } from '../generated/protocol';
import { useHebi8Expand } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import { toast } from '../store/useWorkbenchStore';
import { Md } from './Md';

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

function NodeTreeRow({
  node,
  nodeMap,
  depth,
  selectedId,
  expandedTreeIds,
  onToggleTree,
  onSelect,
}: {
  node: Hebi8LineNode;
  nodeMap: ReadonlyMap<string, Hebi8LineNode>;
  depth: number;
  selectedId: string | null;
  expandedTreeIds: ReadonlySet<string>;
  onToggleTree: (id: string) => void;
  onSelect: (id: string) => void;
}) {
  const isFold = node.kind === 'fold';
  const children = isFold
    ? (node.covers ?? []).map((cid) => nodeMap.get(cid)).filter((x): x is Hebi8LineNode => !!x)
    : [];
  const isOpen = expandedTreeIds.has(node.id);
  const isSelected = selectedId === node.id;

  return (
    <div className="h8-tree-branch">
      <div
        className={`h8-tree-row ${isSelected ? 'on' : ''}`}
        style={{ paddingLeft: `${8 + depth * 14}px` }}
      >
        {isFold && children.length > 0 ? (
          <button
            type="button"
            className="h8-tree-caret"
            aria-label={isOpen ? '收起子节点' : '展开子节点'}
            onClick={(e) => {
              e.stopPropagation();
              onToggleTree(node.id);
            }}
          >
            {isOpen ? <ChevronDown size={12} strokeWidth={1.8} /> : <ChevronRight size={12} strokeWidth={1.8} />}
          </button>
        ) : (
          <span className="h8-tree-caret-ph" />
        )}
        <button
          type="button"
          className="h8-tree-main"
          onClick={() => onSelect(node.id)}
          title={`${node.id} · ${node.title}`}
        >
          <div className="h8-tree-top">
            <span className={`h8-kind-tag ${isFold ? 'fold' : 'seg'}`}>
              {isFold ? `L${node.level} 折叠` : '分段'}
            </span>
            <span className="mono h8-node-id">{node.id}</span>
            {node.pinned && (
              <span className="h8-pin-pill" title="已钉住，不会被自动折叠">
                <Pin size={10} strokeWidth={2} />
                Pin
              </span>
            )}
            <span className="h8-node-count">
              {isFold ? `${node.covers?.length ?? 0} 段` : `${node.attemptRange?.turnCount ?? 0} 轮`}
            </span>
          </div>
          <div className="h8-tree-title ell">{node.title}</div>
          <div className="h8-tree-sub mono">{fmtShortRange(node.coversFrom, node.coversTo, node.coversFromMs, node.coversToMs)}</div>
        </button>
      </div>
      {isFold && isOpen && children.length > 0 && (
        <div className="h8-tree-children">
          {children.map((ch) => (
            <NodeTreeRow
              key={ch.id}
              node={ch}
              nodeMap={nodeMap}
              depth={depth + 1}
              selectedId={selectedId}
              expandedTreeIds={expandedTreeIds}
              onToggleTree={onToggleTree}
              onSelect={onSelect}
            />
          ))}
        </div>
      )}
    </div>
  );
}

export function Hebi8LineDrawer({
  nodes,
  pins,
  summary,
  selectedNodeId,
  onSelectNode,
  onClose,
}: {
  nodes: Hebi8LineNode[];
  pins: string[];
  summary: Hebi8LineSummary;
  selectedNodeId: string | null;
  onSelectNode: (nodeId: string | null) => void;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [expandedTreeIds, setExpandedTreeIds] = useState<Set<string>>(() => new Set());
  const [turnOffset, setTurnOffset] = useState(0);
  const [showRawTurns, setShowRawTurns] = useState(false);

  const nodeMap = useMemo(() => {
    const m = new Map<string, Hebi8LineNode>();
    for (const n of nodes) m.set(n.id, n);
    return m;
  }, [nodes]);

  const roots = useMemo(() => {
    // Top-level nodes: not folded into another node, newest first for quick browsing
    return [...nodes].filter((n) => !n.foldedInto).sort((a, b) => b.seq - a.seq);
  }, [nodes]);

  // Auto-expand ancestor fold nodes whenever selectedNodeId changes (e.g. clicked from timeline divider)
  useEffect(() => {
    setTurnOffset(0);
    setShowRawTurns(false);
    if (!selectedNodeId) return;
    const ancestors: string[] = [];
    let cur = nodeMap.get(selectedNodeId);
    let guard = 0;
    while (cur?.foldedInto && guard < 16) {
      ancestors.push(cur.foldedInto);
      cur = nodeMap.get(cur.foldedInto);
      guard++;
    }
    if (ancestors.length > 0) {
      setExpandedTreeIds((prev) => {
        const next = new Set(prev);
        for (const id of ancestors) next.add(id);
        return next;
      });
    }
  }, [selectedNodeId, nodeMap]);

  const activeId = selectedNodeId && nodeMap.has(selectedNodeId) ? selectedNodeId : (roots[0]?.id ?? null);
  const expandQ = useHebi8Expand(activeId ?? undefined, {
    offset: turnOffset,
    limit: 10,
    enabled: !!activeId,
  });

  const pinMut = useMutation({
    mutationFn: ({ nodeId, pinned }: { nodeId: string; pinned: boolean }) => api.pinHebi8Node(nodeId, pinned),
    onSuccess: (res) => {
      toast(res.pinned ? `已钉住 ${res.nodeId}` : `已取消钉住 ${res.nodeId}`);
      void qc.invalidateQueries({ queryKey: ['hebi8-main'] });
      void qc.invalidateQueries({ queryKey: ['hebi8-line'] });
      void qc.invalidateQueries({ queryKey: ['hebi8-expand', res.nodeId] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const toggleTree = (id: string) => {
    setExpandedTreeIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const detail = expandQ.data;
  const detailNode = detail?.node ?? (activeId ? nodeMap.get(activeId) : undefined);

  return (
    <aside className="h8-drawer" aria-label="Hebi8 上下文折叠树">
      <div className="h8-drawer-hd">
        <div className="h8-drawer-title">
          <Layers size={13.5} strokeWidth={1.7} />
          <span>上下文折叠树</span>
          <span className="h8-drawer-meta mono">
            {summary.totalNodes} 节点 · Pin {pins.length}/8
          </span>
        </div>
        <button type="button" className="ib" title="关闭面板" aria-label="关闭面板" onClick={onClose}>
          <X size={14} strokeWidth={1.7} />
        </button>
      </div>

      {nodes.length === 0 ? (
        <div className="h8-drawer-empty">
          <p>还没有折叠节点。</p>
          <p className="muted" style={{ fontSize: 12, lineHeight: 1.55 }}>
            对话进行若干轮后，点击顶栏「折叠换代」即可将当前段落封装为不可变摘要节点并开启全新底层会话；同层未钉住节点达到 8 个时会自动将最旧 5 个合并为高层节点。
          </p>
        </div>
      ) : (
        <div className="h8-drawer-body">
          <div className="h8-tree-list" role="tree" aria-label="折叠节点树">
            {roots.map((n) => (
              <NodeTreeRow
                key={n.id}
                node={n}
                nodeMap={nodeMap}
                depth={0}
                selectedId={activeId}
                expandedTreeIds={expandedTreeIds}
                onToggleTree={toggleTree}
                onSelect={(id) => onSelectNode(id)}
              />
            ))}
          </div>

          {detailNode && (
            <div className="h8-detail">
              <div className="h8-detail-hd">
                <div className="h8-detail-idrow">
                  <span className={`h8-kind-tag ${detailNode.kind === 'fold' ? 'fold' : 'seg'}`}>
                    {detailNode.kind === 'fold' ? `L${detailNode.level} 折叠` : '分段'}
                  </span>
                  <span className="mono h8-node-id">{detailNode.id}</span>
                  <span className="faint" style={{ fontSize: 11 }}>
                    {detailNode.summaryQuality === 'written' ? '精写摘要' : '自动摘要'}
                  </span>
                </div>
                <button
                  type="button"
                  className={`btn ghost sm ${detailNode.pinned ? 'h8-pin-active' : ''}`}
                  disabled={pinMut.isPending}
                  onClick={() => pinMut.mutate({ nodeId: detailNode.id, pinned: !detailNode.pinned })}
                  title={detailNode.pinned ? '取消钉住' : '钉住此节点（不会被自动折叠，上限 8 个）'}
                >
                  {detailNode.pinned ? (
                    <>
                      <PinOff size={12} strokeWidth={1.7} /> 取消 Pin
                    </>
                  ) : (
                    <>
                      <Pin size={12} strokeWidth={1.7} /> Pin
                    </>
                  )}
                </button>
              </div>

              <div className="h8-detail-title">{detailNode.title}</div>
              <div className="h8-detail-meta mono">
                {fmtShortRange(detailNode.coversFrom, detailNode.coversTo, detailNode.coversFromMs, detailNode.coversToMs)}
                {detailNode.attemptRange && ` · #${detailNode.attemptRange.startAttemptNo}–#${detailNode.attemptRange.endAttemptNo} (${detailNode.attemptRange.turnCount} 轮)`}
                {detailNode.kind === 'fold' && ` · 覆盖 ${detailNode.covers?.length ?? 0} 个下层节点`}
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
                <div className="muted" style={{ padding: '12px 0', fontSize: 12 }}>
                  载入节点摘要…
                </div>
              ) : expandQ.isError ? (
                <div className="bad-t" style={{ padding: '12px 0', fontSize: 12 }}>
                  读取失败：{errMsg(expandQ.error)}
                </div>
              ) : detail ? (
                <>
                  <div className="h8-detail-md">
                    <Md text={detail.summary || '（无摘要正文）'} />
                  </div>

                  {detail.node.kind === 'fold' && detail.children && detail.children.length > 0 && (
                    <div className="h8-subsec">
                      <div className="h8-subsec-hd">包含的下层节点（点击展开）</div>
                      <div className="h8-child-cards">
                        {detail.children.map((ch) => (
                          <button
                            key={ch.id}
                            type="button"
                            className="h8-child-card"
                            onClick={() => onSelectNode(ch.id)}
                          >
                            <div className="h8-tree-top">
                              <span className={`h8-kind-tag ${ch.kind === 'fold' ? 'fold' : 'seg'}`}>
                                {ch.kind === 'fold' ? `L${ch.level}` : '分段'}
                              </span>
                              <span className="mono h8-node-id">{ch.id}</span>
                              {ch.pinned && <span className="h8-pin-pill">Pin</span>}
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
                        <span>原文对话（{detail.totalTurns ?? 0} 轮）</span>
                        <button
                          type="button"
                          className="lk"
                          style={{ fontSize: 11.5 }}
                          onClick={() => setShowRawTurns((v) => !v)}
                        >
                          {showRawTurns ? '收起原文' : '预览原文'}
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
      )}
    </aside>
  );
}
