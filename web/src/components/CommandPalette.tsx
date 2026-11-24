import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { ArrowUpRight, FolderGit2, LayoutGrid, MessageSquare, Plus, SunMoon, Zap } from 'lucide-react';
import type { BootstrapResponse, PABriefResponse, PAChatResponse } from '../generated/protocol';
import { useManyProjectTasks } from '../hooks/queries';
import {
  SCRATCH_PROJECT_ID,
  displayMarkRank,
  indexInboxByTask,
  relTime,
  rootSessionMark,
  taskTitle,
} from '../lib/format';
import { navigate, paths } from '../lib/router';
import { useWorkbenchStore } from '../store/useWorkbenchStore';
import { PAIcon } from './Icons';
import { Md } from './Md';
import { ActionCards, BriefView, usePABrief, usePAChat } from './PAViews';
import { StatusMark } from './StatusMark';

interface Item {
  key: string;
  group: string;
  icon?: ReactNode;
  label: string;
  trail?: ReactNode;
  run: () => void;
}

type Out = { kind: 'chat'; res: PAChatResponse } | { kind: 'brief'; res: PABriefResponse } | null;

export function CommandPalette({ boot }: { boot: BootstrapResponse }) {
  const setOpen = useWorkbenchStore((s) => s.setCmdkOpen);
  const toggleTheme = useWorkbenchStore((s) => s.toggleTheme);
  const [q, setQ] = useState('');
  const [act, setAct] = useState(0);
  const [out, setOut] = useState<Out>(null);
  const inputRef = useRef<HTMLInputElement | null>(null);
  const tasksBy = useManyProjectTasks(boot.projects);
  const chat = usePAChat((res) => setOut({ kind: 'chat', res }));
  const brief = usePABrief((res) => setOut({ kind: 'brief', res }));
  const inboxByTask = useMemo(() => indexInboxByTask(boot.openInboxItems), [boot.openInboxItems]);

  useEffect(() => inputRef.current?.focus(), []);

  const go = (hash: string) => {
    setOpen(false);
    navigate(hash);
  };

  const items = useMemo<Item[]>(() => {
    const k = q.trim().toLowerCase();
    const match = (s: string) => !k || s.toLowerCase().includes(k);
    const list: Item[] = [];

    if (k) {
      list.push({
        key: 'ask',
        group: 'PA',
        icon: <PAIcon />,
        label: `问 PA：${q.trim()}`,
        trail: <span className="d">↵</span>,
        run: () => chat.mutate({ message: q.trim(), threadId: boot.paState.activePaThreadId || undefined }),
      });
    }

    // Recent threads first (so opening ⌘K jumps straight to active/recent sessions)
    const allTasks = Object.values(tasksBy).flat();
    const threads = allTasks
      .filter((t) => !t.parentTaskId)
      .filter((t) => (k ? match(`${t.title} ${t.goal}`) : true))
      .map((t) => ({ t, mark: rootSessionMark(t, allTasks, inboxByTask) }))
      .sort((a, b) => displayMarkRank(a.mark) - displayMarkRank(b.mark) || b.t.updatedAt - a.t.updatedAt)
      .slice(0, k ? 10 : 6);

    for (const { t, mark } of threads) {
      const pn =
        t.projectId === SCRATCH_PROJECT_ID
          ? '自由 Chat'
          : boot.projects.find((p) => p.projectId === t.projectId)?.displayName ?? t.projectId;
      list.push({
        key: `t:${t.taskId}`,
        group: k ? '线程' : '最近线程',
        label: `${pn} / ${taskTitle(t)}`,
        trail: mark ? <StatusMark mark={mark} /> : <span className="d">{relTime(t.updatedAt)}</span>,
        run: () => go(paths.session(t.taskId)),
      });
    }

    for (const p of boot.projects) {
      if (p.projectId === SCRATCH_PROJECT_ID || p.archived) continue;
      if (!match(`${p.displayName} ${p.repoIdentity}`)) continue;
      list.push({
        key: `p:${p.projectId}`,
        group: '项目',
        icon: <FolderGit2 size={14} strokeWidth={1.6} />,
        label: p.displayName,
        run: () => go(paths.project(p.projectId)),
      });
    }

    const actions: Item[] = [
      { key: 'brief', group: '动作', icon: <Zap size={14} strokeWidth={1.6} />, label: '今日摘要', run: () => brief.mutate() },
      { key: 'new', group: '动作', icon: <Plus size={14} strokeWidth={1.6} />, label: '新建线程', trail: <span className="d">⌘N</span>, run: () => go(paths.newThread()) },
      { key: 'home', group: '动作', icon: <LayoutGrid size={14} strokeWidth={1.6} />, label: '总览', run: () => go(paths.home()) },
      { key: 'pa', group: '动作', icon: <PAIcon />, label: 'PA', run: () => go(paths.pa()) },
      { key: 'theme', group: '动作', icon: <SunMoon size={14} strokeWidth={1.6} />, label: '切换深 / 浅色', run: () => toggleTheme() },
    ];
    list.push(...actions.filter((a) => match(a.label)));

    return list;
    // chat/brief mutate fns are stable per hook instance
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [q, boot, tasksBy, inboxByTask]);

  useEffect(() => setAct(0), [q]);
  const groups: string[] = [];
  for (const it of items) if (!groups.includes(it.group)) groups.push(it.group);
  const threadIdForPA = out?.kind === 'chat' ? out.res.thread.paThreadId : boot.paState.activePaThreadId;

  return (
    <>
      <div className="scrim" onClick={() => setOpen(false)} />
      <section className="cmdk" role="dialog" aria-label="全局快唤">
        <div className="ck-in">
          <PAIcon />
          <input
            ref={inputRef}
            value={q}
            placeholder="跳线程、搜项目，或问 PA…"
            onChange={(e) => setQ(e.target.value)}
            onKeyDown={(e) => {
              if (e.nativeEvent.isComposing) return;
              if (e.key === 'ArrowDown') {
                e.preventDefault();
                setAct((a) => Math.min(items.length - 1, a + 1));
              } else if (e.key === 'ArrowUp') {
                e.preventDefault();
                setAct((a) => Math.max(0, a - 1));
              } else if (e.key === 'Enter') {
                e.preventDefault();
                items[act]?.run();
              }
            }}
            aria-label="搜索或提问"
          />
          <span className="kbd">esc</span>
        </div>
        {(out || chat.isPending || brief.isPending) && (
          <div className="ck-out">
            {chat.isPending || brief.isPending ? (
              <div className="running">
                <span className="spin" />
                <span className="shim">{brief.isPending ? '汇总中…' : '思考中…'}</span>
              </div>
            ) : out?.kind === 'chat' ? (
              <>
                <Md text={out.res.assistantMessage.content} />
                <ActionCards actions={out.res.actions ?? out.res.assistantMessage.actions} />
              </>
            ) : out?.kind === 'brief' ? (
              <BriefView brief={out.res} projects={boot.projects} />
            ) : null}
            {out && (
              <div style={{ marginTop: 10 }}>
                <button type="button" className="btn" onClick={() => go(paths.pa(threadIdForPA || undefined))}>
                  在 PA 页继续 <ArrowUpRight size={12} strokeWidth={1.6} />
                </button>
              </div>
            )}
          </div>
        )}
        <div className="ck-list">
          {groups.map((g) => (
            <div key={g}>
              <div className="ck-g">{g}</div>
              {items
                .map((it, idx) => ({ it, idx }))
                .filter(({ it }) => it.group === g)
                .map(({ it, idx }) => (
                  <button
                    type="button"
                    key={it.key}
                    className={`ck-i ${idx === act ? 'act' : ''}`}
                    onMouseEnter={() => setAct(idx)}
                    onClick={() => it.run()}
                  >
                    {it.icon}
                    <span className="ell">{it.label}</span>
                    {it.trail && <span className="ck-trail">{it.trail}</span>}
                  </button>
                ))}
            </div>
          ))}
          {items.length === 0 && (
            <div className="empty">
              <MessageSquare size={14} strokeWidth={1.6} /> 没有匹配项
            </div>
          )}
        </div>
      </section>
    </>
  );
}
