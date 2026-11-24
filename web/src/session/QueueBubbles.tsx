import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ArrowDown, ArrowUp, Pencil, Undo2 } from 'lucide-react';
import type { PendingInput } from '../generated/protocol';
import { SteerIcon } from '../components/Icons';
import { api, errMsg } from '../lib/api';
import { sourceLabel } from '../lib/format';
import { toast } from '../store/useWorkbenchStore';

/** Human -> Agent pending inputs rendered as calm dashed bubbles at the tail of the conversation. */
export function QueueBubbles({
  taskId,
  items,
  busy,
  editingId,
  onEdit,
}: {
  taskId: string;
  items: PendingInput[];
  busy: boolean;
  editingId: string | null;
  onEdit: (item: PendingInput) => void;
}) {
  const qc = useQueryClient();
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['task-timeline', taskId] });
    void qc.invalidateQueries({ queryKey: ['bootstrap'] });
  };
  const act = useMutation({
    mutationFn: async (v: { op: 'steer' | 'delete' | 'up' | 'down'; item: PendingInput }) => {
      if (v.op === 'steer') return api.steerQueueInput(taskId, v.item.inputId);
      if (v.op === 'delete') return api.deleteQueueInput(taskId, v.item.inputId);
      const ids = items.map((i) => i.inputId);
      const idx = ids.indexOf(v.item.inputId);
      const to = v.op === 'up' ? idx - 1 : idx + 1;
      if (idx < 0 || to < 0 || to >= ids.length) return null;
      [ids[idx], ids[to]] = [ids[to], ids[idx]];
      return api.reorderQueue(taskId, ids);
    },
    onSuccess: (_r, v) => {
      if (v.op === 'steer') toast(busy ? '已打断当前轮，优先执行' : '已立即发送');
      if (v.op === 'delete') toast('已撤回');
      refresh();
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  if (items.length === 0) return null;
  const multi = items.length >= 2;

  return (
    <div className="queue it" aria-label="待发送消息">
      {items.map((it, i) => {
        const showSource = it.source !== 'human';
        return (
          <div key={it.inputId} className={`qb ${editingId === it.inputId ? 'editing' : ''}`}>
            {showSource && <div className="src">{sourceLabel(it.source)}</div>}
            <div className="bub">{editingId === it.inputId ? '正在下方编辑…' : it.body}</div>
            <div className="qb-foot">
              <span className="qb-note">下一轮发出</span>
              <div className="qacts">
                <button
                  type="button"
                  className="qa steer"
                  disabled={act.isPending}
                  onClick={() => act.mutate({ op: 'steer', item: it })}
                  title="打断当前轮，优先执行"
                >
                  <SteerIcon size={12} /> 立即发
                </button>
                {it.kind === 'followup' && (
                  <button
                    type="button"
                    className="qa"
                    disabled={act.isPending || editingId !== null}
                    onClick={() => onEdit(it)}
                    title="编辑"
                  >
                    <Pencil size={12} strokeWidth={1.6} /> 编辑
                  </button>
                )}
                {multi && (
                  <>
                    <button
                      type="button"
                      className="qa"
                      disabled={act.isPending || i === 0}
                      onClick={() => act.mutate({ op: 'up', item: it })}
                      title="上移"
                    >
                      <ArrowUp size={12} strokeWidth={1.6} />
                    </button>
                    <button
                      type="button"
                      className="qa"
                      disabled={act.isPending || i === items.length - 1}
                      onClick={() => act.mutate({ op: 'down', item: it })}
                      title="下移"
                    >
                      <ArrowDown size={12} strokeWidth={1.6} />
                    </button>
                  </>
                )}
                <button
                  type="button"
                  className="qa danger"
                  disabled={act.isPending || editingId === it.inputId}
                  onClick={() => act.mutate({ op: 'delete', item: it })}
                  title="撤回"
                >
                  <Undo2 size={12} strokeWidth={1.6} /> 撤回
                </button>
              </div>
            </div>
          </div>
        );
      })}
    </div>
  );
}
