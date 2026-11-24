import { useEffect, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ChevronDown, Plus, RotateCcw, ShieldCheck, Zap } from 'lucide-react';
import type { BootstrapResponse, PAFocusPointer, PAMessage } from '../generated/protocol';
import { CardIcon } from '../components/Icons';
import { Md } from '../components/Md';
import { PageHeader } from '../components/PageHeader';
import { ActionCards } from '../components/PAViews';
import { usePAThread, usePAThreads } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import { hhmm, relTime } from '../lib/format';
import { navigate, paths } from '../lib/router';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';
import { useAutoGrow } from './HomePage';

function focusTarget(p: PAFocusPointer): string {
  const k = p.kind.toLowerCase();
  if (k.includes('project')) return paths.project(p.id);
  if (k.includes('task') || k.includes('thread') || k.includes('session')) return paths.session(p.id);
  return '';
}

function Message({ m }: { m: PAMessage }) {
  if (m.role === 'user') {
    return (
      <div className="it u">
        <div className="ub">{m.content}</div>
        <div className="umeta">{hhmm(m.createdAt)}</div>
      </div>
    );
  }
  if (m.role === 'system') {
    return (
      <div className="it">
        <div className="done-line">{m.content}</div>
      </div>
    );
  }
  return (
    <div className="it pa-a">
      <span className="pa-av" />
      <div style={{ minWidth: 0 }}>
        <Md text={m.content} />
        <ActionCards actions={m.actions} />
        {(m.deltaToSeq ?? 0) > 0 && (
          <div className="pa-delta mono">
            旁路增量 #{(m.deltaFromSeq ?? 0) + 1}..#{m.deltaToSeq} · {hhmm(m.createdAt)}
          </div>
        )}
      </div>
    </div>
  );
}

export function PAPage({ boot, threadId }: { boot: BootstrapResponse; threadId?: string }) {
  const qc = useQueryClient();
  const threadsQ = usePAThreads();
  const threads = threadsQ.data?.threads ?? boot.paThreads;
  const paState = threadsQ.data?.paState ?? boot.paState;
  const currentId = threadId || paState.activePaThreadId || threads[0]?.paThreadId;
  const detail = usePAThread(currentId);
  const cardOpen = useWorkbenchStore((s) => s.rightCardOpen);
  const setCardOpen = useWorkbenchStore((s) => s.setRightCardOpen);
  const mobileDock = useWorkbenchStore((s) => s.mobileDockOpen);
  const setMobileDock = useWorkbenchStore((s) => s.setMobileDockOpen);
  const [menuOpen, setMenuOpen] = useState(false);
  const [text, setText] = useState('');
  const { ref, grow } = useAutoGrow();
  const stageRef = useRef<HTMLDivElement | null>(null);

  const messages = detail.data?.messages ?? [];
  const current = detail.data?.thread ?? threads.find((t) => t.paThreadId === currentId);
  const state = detail.data?.paState ?? paState;

  useEffect(() => {
    const el = stageRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [messages.length, currentId]);

  const afterTurn = (tid: string) => {
    void qc.invalidateQueries({ queryKey: ['pa-thread', tid] });
    void qc.invalidateQueries({ queryKey: ['pa-threads'] });
    void qc.invalidateQueries({ queryKey: ['bootstrap'] });
  };

  const chat = useMutation({
    mutationFn: (v: { message: string; brief?: boolean }) =>
      api.paChat({ threadId: currentId, message: v.message, action: v.brief ? { action: 'brief' } : undefined }),
    onSuccess: (res) => {
      setText('');
      requestAnimationFrame(grow);
      afterTurn(res.thread.paThreadId);
      if (res.thread.paThreadId !== currentId) navigate(paths.pa(res.thread.paThreadId));
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });
  const create = useMutation({
    mutationFn: (cold: boolean) => (cold ? api.resetPASession() : api.createPAThread()),
    onSuccess: (res, cold) => {
      toast(cold ? '已冷启动新 PA 会话（保留注意力指针）' : '已新建 PA 对话');
      afterTurn(res.thread.paThreadId);
      navigate(paths.pa(res.thread.paThreadId));
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const send = () => {
    const msg = text.trim();
    if (!msg || chat.isPending) return;
    chat.mutate({ message: msg });
  };

  const card = (
    <aside className="fcard" aria-label="PA 状态">
      <div className="fsec">
        <div className="fsec-h">
          Focus
          <span className="r mono">{state.focusPointers.length}/20</span>
        </div>
        {state.focusPointers.length === 0 ? (
          <div className="fsub">PA 暂未跟踪任何项目 / 任务指针</div>
        ) : (
          state.focusPointers.map((p) => {
            const target = focusTarget(p);
            return (
              <button
                type="button"
                className="frow"
                key={`${p.kind}:${p.id}`}
                disabled={!target}
                onClick={() => target && navigate(target)}
                title={`${p.kind} · ${p.id}`}
              >
                <span className="sd rev" />
                <span className="ell">{p.label || p.id}</span>
                <span className="fx">{p.kind}</span>
              </button>
            );
          })
        )}
      </div>
      <div className="fsec">
        <div className="fsec-h">Sync</div>
        <div className="fsub mono" style={{ color: 'var(--fg2)' }}>
          已同步至 event #{state.lastReadEventSeq}
        </div>
        <div className="fsub">旁路读统一事件日志 · 零失忆</div>
      </div>
      <div className="fsec">
        <div className="fsec-h">Guard</div>
        <div className="guard">
          <ShieldCheck size={13} strokeWidth={1.6} />
          代码只读 · 零直接部署权
        </div>
        <div className="fsub">可派活与起草审批；审批与部署只认人类决定</div>
      </div>
    </aside>
  );

  return (
    <main className="main">
      <PageHeader
        right={
          <>
            <button type="button" className="hb" disabled={chat.isPending} onClick={() => chat.mutate({ message: '⚡ Brief Me', brief: true })}>
              <Zap size={13} strokeWidth={1.6} />
              <span className="no-mobile">Brief Me</span>
            </button>
            <button type="button" className="hb" disabled={create.isPending} onClick={() => create.mutate(false)} title="新建 PA 对话">
              <Plus size={13} strokeWidth={1.6} />
              <span className="no-mobile">新建 PA 对话</span>
            </button>
            <button type="button" className="ib" disabled={create.isPending} onClick={() => create.mutate(true)} title="冷启动：新会话只注入注意力指针与当前态势">
              <RotateCcw size={13} strokeWidth={1.6} />
            </button>
            <div className="hsep no-mobile" />
            <button
              type="button"
              className={`ib ${cardOpen ? 'on' : ''} no-mobile`}
              title="右侧浮卡"
              onClick={() => setCardOpen(!cardOpen)}
            >
              <CardIcon />
            </button>
            <button type="button" className="ib only-mobile" title="PA 状态" onClick={() => setMobileDock(true)}>
              <CardIcon />
            </button>
          </>
        }
      >
        <div className="relpos" style={{ minWidth: 0 }}>
          <button type="button" className="crumb" onClick={() => setMenuOpen(!menuOpen)} title="切换 PA 历史对话">
            <span className="c0">PA</span>
            <span className="sep">/</span>
            <span className="c1">{current?.title || '新对话'}</span>
            <ChevronDown size={13} strokeWidth={1.6} className="faint" />
          </button>
          {menuOpen && (
            <>
              <div className="scrim" style={{ background: 'transparent', backdropFilter: 'none', zIndex: 49 }} onClick={() => setMenuOpen(false)} />
              <div className="menu" style={{ top: 32, left: 0 }}>
                <div className="mh">历史 PA 对话</div>
                {threads.map((t) => (
                  <button
                    type="button"
                    key={t.paThreadId}
                    className={t.paThreadId === currentId ? 'on' : ''}
                    onClick={() => {
                      setMenuOpen(false);
                      navigate(paths.pa(t.paThreadId));
                    }}
                  >
                    <span className={`sd ${t.paThreadId === paState.activePaThreadId ? 'on' : ''}`} />
                    <span className="ell" style={{ flex: 1 }}>
                      {t.title || t.paThreadId}
                    </span>
                    <span className="faint" style={{ fontSize: 11 }}>
                      {relTime(t.updatedAt)}
                    </span>
                  </button>
                ))}
                {threads.length === 0 && <div className="mh">还没有 PA 对话</div>}
              </div>
            </>
          )}
        </div>
      </PageHeader>
      <div className="sess">
        <div className="sess-body">
          <div className="stage" ref={stageRef}>
            <div className="pa-msgs">
              {!currentId && <div className="empty">还没有 PA 对话。直接在下方输入，或点「新建 PA 对话」。</div>}
              {detail.isLoading && <div className="empty">载入对话…</div>}
              {currentId && !detail.isLoading && messages.length === 0 && (
                <div className="empty">这是一个新的 PA 对话。问问今天的态势、卡住的线程，或让它派一个活。</div>
              )}
              {messages.map((m) => (
                <Message key={m.messageId} m={m} />
              ))}
              {chat.isPending && (
                <div className="it running">
                  <span className="spin" />
                  <span className="shim">PA 正在读取增量事件…</span>
                </div>
              )}
            </div>
          </div>
          <div className="cwrap">
            <div className="composer">
              <textarea
                ref={ref}
                value={text}
                rows={2}
                placeholder="和 PA 聊：跨项目统筹、问态势、派活或起草审批…"
                onChange={(e) => {
                  setText(e.target.value);
                  grow();
                }}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                    e.preventDefault();
                    send();
                  }
                }}
                aria-label="发给 PA"
              />
              <div className="cfoot">
                <span className="ctl">
                  <ShieldCheck size={12} strokeWidth={1.6} /> 只读 · 可派活 · 不能审批 / 部署
                </span>
                <span className="hint no-mobile">↵ 发送 · ⇧↵ 换行</span>
                <button type="button" className="cbtn pri" disabled={!text.trim() || chat.isPending} onClick={send}>
                  发送 <span className="kb">↵</span>
                </button>
              </div>
            </div>
          </div>
        </div>
        {(cardOpen || mobileDock) && (
          <div className={`dock has-card ${mobileDock ? 'mobile-open' : ''}`} onClick={(e) => e.target === e.currentTarget && setMobileDock(false)}>
            {card}
          </div>
        )}
      </div>
    </main>
  );
}

