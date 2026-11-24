import { useEffect, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { ChevronDown, Plus, RotateCcw, Zap } from 'lucide-react';
import type { BootstrapResponse, PAMessage } from '../generated/protocol';
import { Md } from '../components/Md';
import { PageHeader } from '../components/PageHeader';
import { ActionCards } from '../components/PAViews';
import { usePAThread, usePAThreads } from '../hooks/queries';
import { api, errMsg } from '../lib/api';
import { hhmm, relTime } from '../lib/format';
import { navigate, paths } from '../lib/router';
import { toast } from '../store/useWorkbenchStore';
import { useAutoGrow } from './HomePage';

function Message({ m }: { m: PAMessage }) {
  if (m.role === 'user') {
    return (
      <div className="it u">
        <div className="ub">{m.content}</div>
        {m.createdAt > 0 && <div className="umeta hover-meta">{hhmm(m.createdAt)}</div>}
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
        {m.createdAt > 0 && <div className="pa-time hover-meta">{hhmm(m.createdAt)}</div>}
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
  const [menuOpen, setMenuOpen] = useState(false);
  const [text, setText] = useState('');
  const { ref, grow } = useAutoGrow();
  const stageRef = useRef<HTMLDivElement | null>(null);

  const messages = detail.data?.messages ?? [];
  const current = detail.data?.thread ?? threads.find((t) => t.paThreadId === currentId);

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
      setMenuOpen(false);
      toast(cold ? '已重置并开启新对话' : '已新建对话');
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

  return (
    <main className="main">
      <PageHeader
        right={
          <button
            type="button"
            className="ib"
            title="今日摘要"
            aria-label="今日摘要"
            disabled={chat.isPending}
            onClick={() => chat.mutate({ message: '今日摘要', brief: true })}
          >
            <Zap size={14} strokeWidth={1.6} />
          </button>
        }
      >
        <div className="relpos" style={{ minWidth: 0 }}>
          <button type="button" className="crumb" onClick={() => setMenuOpen(!menuOpen)} title="切换历史对话">
            <span className="c0">PA</span>
            <span className="sep">/</span>
            <span className="c1">{current?.title || '新对话'}</span>
            <ChevronDown size={13} strokeWidth={1.6} className="faint" />
          </button>
          {menuOpen && (
            <>
              <div className="scrim" style={{ background: 'transparent', backdropFilter: 'none', zIndex: 49 }} onClick={() => setMenuOpen(false)} />
              <div className="menu" style={{ top: 32, left: 0 }}>
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
                    <span className="ell" style={{ flex: 1 }}>
                      {t.title || '未命名对话'}
                    </span>
                    <span className="faint" style={{ fontSize: 11 }}>
                      {relTime(t.updatedAt)}
                    </span>
                  </button>
                ))}
                {threads.length === 0 && <div className="mh">还没有历史对话</div>}
                <div style={{ borderTop: '1px solid var(--line)', marginTop: 4, paddingTop: 4 }}>
                  <button type="button" disabled={create.isPending} onClick={() => create.mutate(false)}>
                    <Plus size={12} strokeWidth={1.6} />
                    <span>新对话</span>
                  </button>
                  <button type="button" className="faint" disabled={create.isPending} onClick={() => create.mutate(true)}>
                    <RotateCcw size={12} strokeWidth={1.6} />
                    <span>重置上下文并新开</span>
                  </button>
                </div>
              </div>
            </>
          )}
        </div>
      </PageHeader>
      <div className="sess-body" style={{ flex: 1 }}>
        <div className="stage" ref={stageRef}>
          <div className="pa-msgs">
            {!currentId && <div className="empty">直接在下方输入即可开始。</div>}
            {detail.isLoading && <div className="empty">载入对话…</div>}
            {currentId && !detail.isLoading && messages.length === 0 && (
              <div className="empty">问问今天的安排或项目进展。</div>
            )}
            {messages.map((m) => (
              <Message key={m.messageId} m={m} />
            ))}
            {chat.isPending && (
              <div className="it running">
                <span className="spin" />
                <span className="shim">思考中…</span>
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
              placeholder="和 PA 聊聊…"
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
              <button type="button" className="cbtn pri" disabled={!text.trim() || chat.isPending} onClick={send}>
                发送 <span className="kb">↵</span>
              </button>
            </div>
          </div>
        </div>
      </div>
    </main>
  );
}
