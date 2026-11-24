import { useEffect, useMemo, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { FolderGit2, MessageCircle, Plus, Settings2, X } from 'lucide-react';
import type { BootstrapResponse, CreateTaskRequest, CreateTaskSourceInput, SessionAdapter, TaskSourceKind } from '../generated/protocol';
import { SessionAdapterValues } from '../generated/protocol';
import { PAIcon } from '../components/Icons';
import { PageHeader } from '../components/PageHeader';
import { api, errMsg } from '../lib/api';
import { SCRATCH_PROJECT_ID } from '../lib/format';
import { navigate, paths, type NewMode } from '../lib/router';
import { toast } from '../store/useWorkbenchStore';

interface SourceDraft extends CreateTaskSourceInput {
  key: number;
}

let srcKey = 0;

export function NewThreadPage({ boot, mode, projectId }: { boot: BootstrapResponse; mode?: NewMode; projectId?: string }) {
  const qc = useQueryClient();
  const projects = useMemo(() => boot.projects.filter((p) => p.projectId !== SCRATCH_PROJECT_ID && !p.archived), [boot.projects]);
  const [m, setM] = useState<NewMode>(mode ?? (projects.length ? 'project' : 'chat'));
  const [pid, setPid] = useState<string>(projectId ?? projects[0]?.projectId ?? '');
  const [text, setText] = useState('');
  const [paTarget, setPaTarget] = useState<'continue' | 'new'>('continue');
  const [adapter, setAdapter] = useState<string>('');
  const [model, setModel] = useState('');
  const [advOpen, setAdvOpen] = useState(false);
  const [capability, setCapability] = useState('');
  const [importDirty, setImportDirty] = useState(false);
  const [sources, setSources] = useState<SourceDraft[]>([]);

  useEffect(() => {
    if (mode) setM(mode);
  }, [mode]);
  useEffect(() => {
    if (projectId) {
      setPid(projectId);
      setM('project');
    }
  }, [projectId]);

  const project = projects.find((p) => p.projectId === pid);
  const availableAdapters = useMemo(() => {
    const list: SessionAdapter[] = [];
    const seen = new Set<string>();
    for (const mach of boot.machines) {
      for (const adp of mach.adapters ?? []) {
        if ((SessionAdapterValues as readonly string[]).includes(adp) && !seen.has(adp)) {
          seen.add(adp);
          list.push(adp as SessionAdapter);
        }
      }
    }
    if (list.length === 0) list.push('agy');
    return list;
  }, [boot.machines]);

  useEffect(() => {
    setCapability('');
    setAdapter('');
  }, [pid]);

  const submit = useMutation({
    mutationFn: async () => {
      const goal = text.trim();
      if (m === 'pa') {
        let threadId = boot.paState.activePaThreadId || undefined;
        if (paTarget === 'new' || !threadId) {
          const created = await api.createPAThread();
          threadId = created.thread.paThreadId;
        }
        const res = await api.paChat({ threadId, message: goal });
        return { kind: 'pa' as const, id: res.thread.paThreadId };
      }
      const req: CreateTaskRequest = { goal };
      if (adapter) req.adapter = adapter as SessionAdapter;
      if (model.trim()) req.config = { model: model.trim() };
      if (m === 'chat') {
        const res = await api.createTask(SCRATCH_PROJECT_ID, req);
        return { kind: 'task' as const, id: res.task.taskId, pid: SCRATCH_PROJECT_ID };
      }
      if (!pid) throw new Error('请选择项目');
      if (capability) req.requiredCapability = capability;
      if (importDirty) req.importDirtyWorkspace = true;
      const srcs = sources
        .filter((s) => s.uriOrRef.trim())
        .map(({ kind, label, uriOrRef }) => ({ kind, label: label.trim() || uriOrRef.trim(), uriOrRef: uriOrRef.trim() }));
      if (srcs.length) req.sources = srcs;
      const res = await api.createTask(pid, req);
      return { kind: 'task' as const, id: res.task.taskId, pid };
    },
    onSuccess: (r) => {
      setText('');
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
      if (r.kind === 'pa') {
        void qc.invalidateQueries({ queryKey: ['pa-threads'] });
        navigate(paths.pa(r.id));
      } else {
        void qc.invalidateQueries({ queryKey: ['project-tasks', r.pid] });
        navigate(paths.session(r.id));
      }
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const canSend = !!text.trim() && !submit.isPending && (m !== 'project' || !!pid);
  const send = () => canSend && submit.mutate();
  const placeholder =
    m === 'pa'
      ? '问 PA：今天的安排、项目进展，或让它帮你起草任务…'
      : m === 'chat'
        ? '随便聊聊或调研点什么…'
        : '描述你的目标，或直接粘贴需求…';

  return (
    <main className="main">
      <PageHeader>
        <span className="ttl">新建线程</span>
      </PageHeader>
      <div className="page-scroll">
        <div className="nt-wrap">
          <h2>新建线程</h2>
          {/* Fixed labels & geometry: switching tabs never shifts button positions. */}
          <div className="pills" role="tablist" aria-label="线程类型">
            <button type="button" role="tab" aria-selected={m === 'pa'} className={`pill ${m === 'pa' ? 'on' : ''}`} onClick={() => setM('pa')}>
              <PAIcon size={13} /> 和 PA 对话
            </button>
            <button type="button" role="tab" aria-selected={m === 'chat'} className={`pill ${m === 'chat' ? 'on' : ''}`} onClick={() => setM('chat')}>
              <MessageCircle size={13} strokeWidth={1.6} /> 自由 Chat
            </button>
            <button
              type="button"
              role="tab"
              aria-selected={m === 'project'}
              className={`pill ${m === 'project' ? 'on' : ''}`}
              onClick={() => setM('project')}
              disabled={projects.length === 0}
            >
              <FolderGit2 size={13} strokeWidth={1.6} /> 项目线程
            </button>
          </div>
          <div className="canvas">
            <textarea
              value={text}
              autoFocus
              placeholder={placeholder}
              onChange={(e) => setText(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                  e.preventDefault();
                  send();
                }
              }}
              aria-label="线程目标"
            />
            <div className="cv-foot">
              {m === 'pa' && (
                <div className="seg">
                  <button type="button" className={paTarget === 'continue' ? 'on' : ''} onClick={() => setPaTarget('continue')}>
                    续聊当前 PA
                  </button>
                  <button type="button" className={paTarget === 'new' ? 'on' : ''} onClick={() => setPaTarget('new')}>
                    新开 PA 线
                  </button>
                </div>
              )}
              {m === 'project' && (
                <select className="field" value={pid} onChange={(e) => setPid(e.target.value)} aria-label="项目">
                  {projects.map((p) => (
                    <option key={p.projectId} value={p.projectId}>
                      {p.displayName}
                    </option>
                  ))}
                </select>
              )}
              {m !== 'pa' && (
                <>
                  <select className="field" value={adapter} onChange={(e) => setAdapter(e.target.value)} aria-label="Agent">
                    <option value="">{m === 'project' && project ? `默认 · ${project.defaultAdapter}` : `默认 · ${availableAdapters[0]}`}</option>
                    {availableAdapters.map((a) => (
                      <option key={a} value={a}>
                        {a}
                      </option>
                    ))}
                  </select>
                  <input className="field" value={model} onChange={(e) => setModel(e.target.value)} placeholder="模型（默认）" aria-label="模型" />
                </>
              )}
              {m === 'project' && (
                <button type="button" className={`btn ghost ${advOpen ? 'on' : ''}`} onClick={() => setAdvOpen(!advOpen)}>
                  <Settings2 size={13} strokeWidth={1.6} /> 高级
                </button>
              )}
              <span className="sp">
                <button type="button" className="cbtn pri" disabled={!canSend} onClick={send}>
                  {submit.isPending ? <span className="spin" /> : null}
                  {m === 'pa' ? '发送' : '开始'} <span className="kb">↵</span>
                </button>
              </span>
            </div>
            {m === 'project' && advOpen && project && (
              <div className="adv">
                <span>运行环境</span>
                <select className="field" value={capability} onChange={(e) => setCapability(e.target.value)}>
                  <option value="">默认 · {project.defaultCapability}</option>
                  {Object.entries(project.anchors).map(([cap, a]) => (
                    <option key={cap} value={cap}>
                      {cap} · {a.machine}
                    </option>
                  ))}
                </select>
                <span>工作区</span>
                <label className="chk">
                  <input type="checkbox" checked={importDirty} onChange={(e) => setImportDirty(e.target.checked)} />
                  带入当前未提交改动
                </label>
                <span style={{ alignSelf: 'start', paddingTop: 6 }}>参考 Sources</span>
                <div className="srcs">
                  {sources.map((s) => (
                    <div className="r" key={s.key}>
                      <select
                        className="field"
                        value={s.kind}
                        onChange={(e) => setSources((xs) => xs.map((x) => (x.key === s.key ? { ...x, kind: e.target.value as TaskSourceKind } : x)))}
                      >
                        <option value="link">链接</option>
                        <option value="file">文件</option>
                        <option value="nmem">nmem</option>
                      </select>
                      <input
                        className="field lbl-in"
                        placeholder="标题"
                        value={s.label}
                        onChange={(e) => setSources((xs) => xs.map((x) => (x.key === s.key ? { ...x, label: e.target.value } : x)))}
                      />
                      <input
                        className="field"
                        placeholder={s.kind === 'link' ? 'https://…' : s.kind === 'file' ? '仓库内路径' : 'nmem 引用'}
                        value={s.uriOrRef}
                        onChange={(e) => setSources((xs) => xs.map((x) => (x.key === s.key ? { ...x, uriOrRef: e.target.value } : x)))}
                      />
                      <button type="button" className="ib" title="移除" onClick={() => setSources((xs) => xs.filter((x) => x.key !== s.key))}>
                        <X size={13} strokeWidth={1.6} />
                      </button>
                    </div>
                  ))}
                  <button
                    type="button"
                    className="btn ghost"
                    style={{ alignSelf: 'flex-start' }}
                    onClick={() => setSources((xs) => [...xs, { key: ++srcKey, kind: 'link', label: '', uriOrRef: '' }])}
                  >
                    <Plus size={13} strokeWidth={1.6} /> 添加 Source
                  </button>
                </div>
              </div>
            )}
          </div>
        </div>
      </div>
    </main>
  );
}
