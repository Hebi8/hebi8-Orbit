import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Check, ChevronDown, ChevronRight, Copy, Lock, RefreshCw, XCircle } from 'lucide-react';
import type { AgentProbe, MachineView } from '../generated/protocol';
import { api, errMsg } from '../lib/api';
import { toast } from '../store/useWorkbenchStore';

export function agentDisplayName(adapter: string): string {
  switch (adapter) {
    case 'agy':
      return 'Antigravity';
    case 'claude':
      return 'Claude Code';
    case 'codex':
      return 'Codex';
    default:
      return adapter;
  }
}

function loginCommandFor(adapter: string): string {
  switch (adapter) {
    case 'claude':
      return 'claude';
    case 'codex':
      return 'codex login';
    default:
      return 'agy login';
  }
}

function installCommandFor(adapter: string): string {
  switch (adapter) {
    case 'claude':
      return 'npm install -g @anthropic-ai/claude-code';
    case 'codex':
      return 'npm install -g @openai/codex';
    default:
      return 'curl -fsSL https://antigravity.dev/install.sh | bash';
  }
}

async function copyText(text: string, label: string) {
  try {
    await navigator.clipboard.writeText(text);
    toast(`已复制${label}`);
  } catch {
    toast(text);
  }
}

interface AgentRowProps {
  machine: MachineView;
  probe: AgentProbe;
  canControl: boolean;
  showAdvanced?: boolean;
  onRefreshDone: () => void;
}

function AgentRow({ machine, probe, canControl, showAdvanced, onRefreshDone }: AgentRowProps) {
  const [busy, setBusy] = useState<'test' | 'install' | 'save' | null>(null);
  const [expanded, setExpanded] = useState(false);
  const [binDraft, setBinDraft] = useState(probe.bridgeBin ?? '');
  const [modelDraft, setModelDraft] = useState(probe.defaultModel ?? '');
  const [workersDraft, setWorkersDraft] = useState(probe.maxLiveWorkers ? String(probe.maxLiveWorkers) : '');

  const online = machine.status === 'online';
  const locked = new Set(probe.lockedFields ?? []);
  const name = agentDisplayName(probe.adapter);

  const runTest = async () => {
    setBusy('test');
    try {
      const res = await api.testMachineAgent(machine.machineId, probe.adapter);
      if (res.probe?.lastTest?.ok) {
        toast(`${name} 连通正常`);
      } else {
        toast(res.probe?.detail || `${name} 连通性检查未通过`, 'err');
      }
      onRefreshDone();
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setBusy(null);
    }
  };

  const runInstall = async () => {
    setBusy('install');
    try {
      await api.installMachineAgent(machine.machineId, probe.adapter);
      toast(`${name} 已配置就绪`);
      onRefreshDone();
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setBusy(null);
    }
  };

  const toggleEnabled = async (next: boolean) => {
    setBusy('save');
    try {
      await api.updateMachineAgent(machine.machineId, probe.adapter, { enabled: next });
      toast(`${name} 已${next ? '启用' : '停用'}`);
      onRefreshDone();
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setBusy(null);
    }
  };

  const saveAdvanced = async () => {
    setBusy('save');
    try {
      const req: { bin?: string; defaultModel?: string; maxLiveWorkers?: number } = {
        defaultModel: modelDraft.trim(),
      };
      if (!locked.has('bin')) {
        req.bin = binDraft.trim();
      }
      if (!locked.has('maxLiveWorkers')) {
        const w = Number(workersDraft.trim());
        req.maxLiveWorkers = Number.isFinite(w) && w > 0 ? Math.round(w) : 0;
      }
      await api.updateMachineAgent(machine.machineId, probe.adapter, req);
      toast(`已保存 ${name} 配置`);
      onRefreshDone();
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setBusy(null);
    }
  };

  // Clean v3.2 status copy: version only when ready (ZERO ms latency clutter).
  let statusNote = '';
  if (busy === 'install' || probe.state === 'installing') {
    statusNote = '正在配置…';
  } else if (busy === 'test' || probe.state === 'checking') {
    statusNote = '检查中…';
  } else if (probe.state === 'ready') {
    statusNote = probe.bridgeVersion || '';
  } else if (probe.state === 'missing_bridge') {
    statusNote = probe.hintCode === 'missing_npm' ? '需先安装 Node.js' : '未就绪';
  } else if (probe.state === 'needs_login') {
    statusNote = '未登录';
  } else if (probe.state === 'disabled') {
    statusNote = '已停用';
  } else if (probe.state === 'error') {
    statusNote = probe.detail || '检查未通过';
  } else {
    statusNote = '未安装';
  }

  return (
    <div className={`ag-row ${probe.state === 'disabled' ? 'is-off' : ''}`}>
      <div className="ag-main">
        <div className="ag-left">
          {showAdvanced ? (
            <button
              type="button"
              className="ag-expand"
              onClick={() => setExpanded((v) => !v)}
              aria-label={`展开 ${name} 配置`}
            >
              {expanded ? <ChevronDown size={13} strokeWidth={1.7} /> : <ChevronRight size={13} strokeWidth={1.7} />}
            </button>
          ) : null}
          <span className="ag-name">{name}</span>
          {statusNote && (
            <span
              className={`ag-note mono ell ${probe.state === 'error' ? 'bad-t' : ''}`}
              title={probe.detail || statusNote}
            >
              {statusNote}
            </span>
          )}
        </div>

        <div className="ag-right">
          {probe.state === 'missing_bridge' && probe.hintCode !== 'missing_npm' && online && canControl && (
            <button type="button" className="btn pri sm" disabled={busy !== null} onClick={() => void runInstall()}>
              {busy === 'install' ? '配置中…' : '一键配置'}
            </button>
          )}
          {probe.state === 'needs_login' && (
            <button
              type="button"
              className="btn ghost sm"
              onClick={() => void copyText(loginCommandFor(probe.adapter), '登录命令')}
            >
              <Copy size={11} strokeWidth={1.7} /> 复制登录命令
            </button>
          )}
          {probe.state === 'not_installed' && (
            <button
              type="button"
              className="btn ghost sm"
              onClick={() => void copyText(installCommandFor(probe.adapter), '安装命令')}
            >
              <Copy size={11} strokeWidth={1.7} /> 复制安装命令
            </button>
          )}
          {probe.state === 'error' && online && canControl && (
            <button type="button" className="btn ghost sm" disabled={busy !== null} onClick={() => void runTest()}>
              {busy === 'test' ? '检查中…' : '重试'}
            </button>
          )}

          <span className="ag-mark" title={statusNote || probe.state}>
            {probe.state === 'ready' ? (
              <Check size={14} strokeWidth={2.2} className="ok-t" />
            ) : probe.state === 'error' || probe.state === 'needs_login' ? (
              <XCircle size={13.5} strokeWidth={2} className="bad-t" />
            ) : probe.state === 'missing_bridge' ? (
              <span className="ag-dot warn" />
            ) : null}
          </span>
        </div>
      </div>

      {showAdvanced && expanded && (
        <div className="ag-adv">
          <div className="ag-adv-row">
            <span className="ag-adv-lbl">启用</span>
            <label className="chk">
              <input
                type="checkbox"
                checked={probe.enabled}
                disabled={busy !== null}
                onChange={(e) => void toggleEnabled(e.target.checked)}
              />
              <span>{probe.enabled ? '允许调度到这台设备' : '已停用'}</span>
            </label>
          </div>
          <div className="ag-adv-row">
            <span className="ag-adv-lbl">
              可执行路径
              {locked.has('bin') && (
                <span className="ag-lock" title="在配置文件中设定">
                  <Lock size={11} />
                </span>
              )}
            </span>
            <input
              className="field mono"
              value={binDraft}
              disabled={locked.has('bin') || busy !== null}
              placeholder="自动探测"
              onChange={(e) => setBinDraft(e.target.value)}
            />
          </div>
          <div className="ag-adv-row">
            <span className="ag-adv-lbl">默认模型</span>
            <input
              className="field"
              value={modelDraft}
              disabled={busy !== null}
              placeholder="跟随系统默认"
              onChange={(e) => setModelDraft(e.target.value)}
            />
          </div>
          <div className="ag-adv-row">
            <span className="ag-adv-lbl">
              并发上限
              {locked.has('maxLiveWorkers') && (
                <span className="ag-lock" title="在配置文件中设定">
                  <Lock size={11} />
                </span>
              )}
            </span>
            <input
              className="field mono"
              style={{ maxWidth: 96 }}
              value={workersDraft}
              disabled={locked.has('maxLiveWorkers') || busy !== null}
              placeholder="4"
              onChange={(e) => setWorkersDraft(e.target.value)}
            />
          </div>
          <div className="ag-adv-foot">
            {online && canControl && (
              <button
                type="button"
                className="btn ghost sm"
                disabled={busy !== null}
                onClick={() => void runTest()}
              >
                {busy === 'test' ? '检查中…' : '验证连通性'}
              </button>
            )}
            <button type="button" className="btn pri sm" disabled={busy !== null} onClick={() => void saveAdvanced()}>
              保存
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

export function AgentList({
  machine,
  showAdvanced = false,
}: {
  machine: MachineView;
  showAdvanced?: boolean;
}) {
  const qc = useQueryClient();
  const [probing, setProbing] = useState(false);

  const canControl = (machine.runnerCapabilities ?? []).includes('agents.v1');
  const online = machine.status === 'online';
  const agents = machine.agents ?? [];

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['bootstrap'] });
    void qc.invalidateQueries({ queryKey: ['machines'] });
  };

  const runProbeAll = async () => {
    setProbing(true);
    try {
      await api.probeMachineAgents(machine.machineId);
      refresh();
      toast('已刷新 Agent 状态');
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setProbing(false);
    }
  };

  return (
    <div className="ag-block">
      <div className="ag-block-hd">
        <span className="ag-block-lbl">Agent</span>
        {online && canControl && (
          <button
            type="button"
            className="ib"
            title="刷新 Agent 状态"
            aria-label="刷新 Agent 状态"
            disabled={probing}
            onClick={() => void runProbeAll()}
          >
            <RefreshCw size={12.5} strokeWidth={1.7} className={probing ? 'spin-ico' : ''} />
          </button>
        )}
      </div>
      <div className="ag-list">
        {agents.map((probe) => (
          <AgentRow
            key={probe.adapter}
            machine={machine}
            probe={probe}
            canControl={canControl}
            showAdvanced={showAdvanced}
            onRefreshDone={refresh}
          />
        ))}
      </div>
    </div>
  );
}
