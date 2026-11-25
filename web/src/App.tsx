import { useEffect, useState } from 'react';
import { AddProjectModal } from './components/AddProjectModal';
import { CommandPalette } from './components/CommandPalette';
import { MachinesPopover } from './components/MachinesPopover';
import { MobileTabs } from './components/MobileTabs';
import { Sidebar } from './components/Sidebar';
import { useBootstrap } from './hooks/queries';
import { useRealtimeSync } from './hooks/useRealtimeSync';
import {
  ApiError,
  api,
  detectAndRememberDesktopShell,
  errMsg,
  isDesktopShell,
  openDesktopSetup,
  setStoredClientToken,
  setStoredDevToken,
} from './lib/api';
import { hasUrgentSession } from './lib/format';
import { navigate, parseHash, paths, replaceHash, useRoute } from './lib/router';
import { ArtifactsPage } from './pages/ArtifactsPage';
import { HomePage } from './pages/HomePage';
import { NewThreadPage } from './pages/NewThreadPage';
import { OnboardingPage } from './pages/OnboardingPage';
import { PAPage } from './pages/PAPage';
import { ProjectPage } from './pages/ProjectPage';
import { ProjectsListPage } from './pages/ProjectsListPage';
import { SettingsPage } from './pages/SettingsPage';
import { SessionPage } from './session/SessionPage';
import { applyThemeToDocument, toast, useWorkbenchStore } from './store/useWorkbenchStore';
import { ReviewOverlay } from './windows/ReviewOverlay';

function Toast() {
  const t = useWorkbenchStore((s) => s.toast);
  if (!t) return null;
  return (
    <div className={`toast ${t.tone === 'err' ? 'err' : ''}`} role="status" key={t.id}>
      {t.text}
    </div>
  );
}

/** Esc walks back one layer at a time: ⌘K → popover → review overlay → worker drill → board → mobile dock. */
function useGlobalKeys() {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const s = useWorkbenchStore.getState();
      const route = parseHash(window.location.hash);
      const mod = e.metaKey || e.ctrlKey;
      if (mod && !e.shiftKey && !e.altKey && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        s.setCmdkOpen(!s.cmdkOpen);
        return;
      }
      if (mod && !e.shiftKey && !e.altKey && e.key.toLowerCase() === 'n') {
        e.preventDefault();
        s.setCmdkOpen(false);
        navigate(paths.newThread());
        return;
      }
      if (mod && e.key === '\\') {
        e.preventDefault();
        s.toggleSidebar();
        return;
      }
      if (e.key !== 'Escape' || e.isComposing) return;
      if (s.cmdkOpen) {
        s.setCmdkOpen(false);
      } else if (s.machinesOpen) {
        s.setMachinesOpen(false);
      } else if (s.review) {
        s.closeReview();
      } else if (route.name === 'session' && route.worker) {
        navigate(paths.session(route.taskId));
      } else if (s.boardOpen && route.name === 'session') {
        s.setBoardOpen(false);
        s.setMobileDockOpen(false);
      } else if (s.mobileDockOpen) {
        s.setMobileDockOpen(false);
      } else {
        return;
      }
      e.preventDefault();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);
}

function BootError({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  const isUnauth = error instanceof ApiError && error.status === 401;
  const [pairCode, setPairCode] = useState('');
  const [deviceName, setDeviceName] = useState('');
  const [pairing, setPairing] = useState(false);
  const [pairErr, setPairErr] = useState('');
  const [showManual, setShowManual] = useState(!isUnauth);
  const [token, setToken] = useState('');
  const inDesktop = isDesktopShell();

  const submitPair = async () => {
    const trimmed = pairCode.trim();
    if (!trimmed || pairing) return;
    setPairing(true);
    setPairErr('');
    try {
      const res = await api.pairClient(trimmed, deviceName);
      setStoredClientToken(res.clientToken);
      setStoredDevToken('');
      toast(`已配对并登录到 ${res.workspaceName || 'Orbit'}`);
      onRetry();
    } catch (e) {
      setPairErr(errMsg(e));
    } finally {
      setPairing(false);
    }
  };

  return (
    <div className="auth-gate">
      <div className="auth-gate-card">
        <div className="brand" style={{ padding: 0, marginBottom: 14 }}>
          <span className="brand-mark" />
          <b>hebi8-Orbit</b>
        </div>

        {isUnauth ? (
          <>
            <h1 className="auth-gate-title">配对访问此网络</h1>
            <p className="muted" style={{ fontSize: 12.5, margin: '0 0 14px', lineHeight: 1.55 }}>
              输入目标网络的 8 位配对码即可保留登录态访问。配对后本浏览器仅作为已登录客户端设备，不会注册为执行主机。
            </p>
            <div className="auth-gate-form">
              <input
                type="text"
                className="field mono auth-code-in"
                value={pairCode}
                onChange={(e) => setPairCode(e.target.value.toUpperCase())}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') void submitPair();
                }}
                placeholder="输入配对码（如 XXXX-XXXX）"
                autoComplete="off"
                spellCheck={false}
                autoFocus
              />
              <input
                type="text"
                className="field"
                value={deviceName}
                onChange={(e) => setDeviceName(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') void submitPair();
                }}
                placeholder="设备名称（可选，如 MacBook Chrome）"
                autoComplete="off"
              />
              {pairErr && <div className="bad-t" style={{ fontSize: 12 }}>{pairErr}</div>}
              <div style={{ display: 'flex', gap: 8, alignItems: 'center', marginTop: 4 }}>
                <button
                  type="button"
                  className="btn pri"
                  style={{ flex: 1 }}
                  disabled={!pairCode.trim() || pairing}
                  onClick={() => void submitPair()}
                >
                  {pairing ? '验证配对码…' : '配对并进入'}
                </button>
                {inDesktop && (
                  <button type="button" className="btn ghost" onClick={() => openDesktopSetup()}>
                    切换网络
                  </button>
                )}
              </div>
            </div>
            <div className="auth-gate-tip">
              在目标主机运行 <code className="mono">orbit pair --client</code> 或在「设置 → 机器与 Agent → 添加机器」获取配对码。
            </div>
            <div style={{ marginTop: 12 }}>
              <button type="button" className="lk faint" style={{ fontSize: 11.5 }} onClick={() => setShowManual((v) => !v)}>
                {showManual ? '收起手动令牌' : '已有访问令牌？手动输入'}
              </button>
            </div>
          </>
        ) : (
          <>
            <p style={{ margin: '0 0 8px' }}>连接失败：{errMsg(error)}</p>
            <div style={{ display: 'flex', gap: 8, marginBottom: 12 }}>
              <button type="button" className="btn pri sm" onClick={onRetry}>重试</button>
              {inDesktop && (
                <button type="button" className="btn ghost sm" onClick={() => openDesktopSetup()}>
                  切换网络
                </button>
              )}
            </div>
          </>
        )}

        {showManual && (
          <div style={{ display: 'flex', gap: 6, marginTop: 10 }}>
            <input
              type="password"
              className="field"
              value={token}
              onChange={(e) => setToken(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && token.trim()) {
                  const trimmed = token.trim();
                  if (trimmed.startsWith('ock_')) {
                    setStoredClientToken(trimmed);
                    setStoredDevToken('');
                  } else {
                    setStoredClientToken('');
                    setStoredDevToken(trimmed);
                  }
                  onRetry();
                }
              }}
              placeholder="输入 ock_ 或 devToken"
              autoComplete="off"
            />
            <button
              type="button"
              className="btn ghost"
              onClick={() => {
                const trimmed = token.trim();
                if (trimmed.startsWith('ock_')) {
                  setStoredClientToken(trimmed);
                  setStoredDevToken('');
                } else if (trimmed) {
                  setStoredClientToken('');
                  setStoredDevToken(trimmed);
                }
                onRetry();
              }}
            >
              登录
            </button>
          </div>
        )}
      </div>
    </div>
  );
}

const handoffClaims = new Map<string, ReturnType<typeof api.claimAuthHandoff>>();

function AuthHandoffScreen({ code }: { code: string }) {
  const [error, setError] = useState('');
  const setLocalMachineId = useWorkbenchStore((s) => s.setLocalMachineId);

  useEffect(() => {
    let cancelled = false;
    let p = handoffClaims.get(code);
    if (!p) {
      p = api.claimAuthHandoff(code);
      handoffClaims.set(code, p);
    }
    void p
      .then((res) => {
        if (cancelled) return;
        setStoredClientToken(res.clientToken);
        if (res.machineId) {
          setLocalMachineId(res.machineId);
        }
        replaceHash(paths.home());
      })
      .catch((e) => {
        handoffClaims.delete(code);
        if (!cancelled) setError(errMsg(e));
      });
    return () => {
      cancelled = true;
    };
  }, [code, setLocalMachineId]);

  if (error) {
    return (
      <div className="empty" style={{ maxWidth: 420, margin: '20vh auto 0' }}>
        <p className="bad-t">登录链接已失效：{error}</p>
        <button type="button" className="btn pri" onClick={() => replaceHash(paths.home())}>
          返回首页
        </button>
      </div>
    );
  }
  return <div className="empty" style={{ marginTop: '30vh' }}>正在进入工作台…</div>;
}

function JoinHintScreen({ code, scope }: { code: string; scope?: string }) {
  const flag = scope === 'client' ? ' --no-runner' : scope === 'runner' ? ' --runner-only' : '';
  const cmd = `orbit join ${window.location.origin} ${code}${flag}`;
  return (
    <div className="empty" style={{ maxWidth: 480, margin: '18vh auto 0', textAlign: 'left' }}>
      <div className="brand" style={{ padding: 0, marginBottom: 14 }}>
        <span className="brand-mark" />
        <b>hebi8-Orbit</b>
      </div>
      <p style={{ marginBottom: 10 }}>在要加入的机器终端运行这条命令，或粘贴到桌面端「连到已有的 Orbit」：</p>
      <div className="pair-cmd-box">
        <code className="mono ell">{cmd}</code>
        <button
          type="button"
          className="btn ghost sm"
          onClick={() => {
            void navigator.clipboard.writeText(cmd).then(() => toast('已复制命令')).catch(() => {});
          }}
        >
          复制
        </button>
      </div>
    </div>
  );
}

export function App() {
  const theme = useWorkbenchStore((s) => s.theme);
  const collapsed = useWorkbenchStore((s) => s.sidebarCollapsed);
  const cmdkOpen = useWorkbenchStore((s) => s.cmdkOpen);
  const machinesOpen = useWorkbenchStore((s) => s.machinesOpen);
  const route = useRoute();
  const [addProjectOpen, setAddProjectOpen] = useState(false);

  const isPublicRoute = route.name === 'auth' || route.name === 'join';
  const bootQ = useBootstrap(!isPublicRoute);
  useRealtimeSync(isPublicRoute ? undefined : bootQ.data?.snapshotSeq);
  useGlobalKeys();

  useEffect(() => {
    detectAndRememberDesktopShell();
  }, []);

  useEffect(() => applyThemeToDocument(theme), [theme]);

  const boot = bootQ.data;
  const storedLocalMachineId = useWorkbenchStore((s) => s.localMachineId);
  const setLocalMachineId = useWorkbenchStore((s) => s.setLocalMachineId);

  // Auto-hydrate current client device identity from server-authenticated bootstrap
  // (fixes Mac mini desktop client mistakenly displaying hub's local_ws machine as "本机").
  useEffect(() => {
    const serverCurrent = boot?.setup?.currentMachineId;
    if (serverCurrent && serverCurrent !== storedLocalMachineId) {
      setLocalMachineId(serverCurrent);
    }
  }, [boot?.setup?.currentMachineId, storedLocalMachineId, setLocalMachineId]);

  // Auto-guide first-time workspaces through local agent discovery & setup onboarding.
  useEffect(() => {
    if (boot?.setup?.needsOnboarding && route.name === 'home') {
      replaceHash(paths.onboarding());
    }
  }, [boot?.setup?.needsOnboarding, route.name]);

  if (route.name === 'auth') {
    return <AuthHandoffScreen code={route.code} />;
  }
  if (route.name === 'join') {
    return <JoinHintScreen code={route.code} scope={route.scope} />;
  }

  if (!boot) {
    if (bootQ.isError) return <BootError error={bootQ.error} onRetry={() => void bootQ.refetch()} />;
    return <div className="empty" style={{ marginTop: '30vh' }}>连接中…</div>;
  }

  if (route.name === 'onboarding') {
    return (
      <>
        <OnboardingPage boot={boot} />
        <Toast />
      </>
    );
  }

  let page;
  switch (route.name) {
    case 'settings':
      page = <SettingsPage boot={boot} />;
      break;
    case 'pa':
      page = <PAPage boot={boot} threadId={route.threadId} />;
      break;
    case 'artifacts':
      page = <ArtifactsPage />;
      break;
    case 'new':
      page = (
        <NewThreadPage
          boot={boot}
          mode={route.mode}
          projectId={route.projectId}
          initialMachineId={route.machineId}
          initialAdapter={route.adapter}
        />
      );
      break;
    case 'project':
      page = <ProjectPage key={route.projectId} boot={boot} projectId={route.projectId} tab={route.tab} />;
      break;
    case 'projects':
      page = <ProjectsListPage boot={boot} />;
      break;
    case 'session':
      page = <SessionPage key={route.taskId} boot={boot} taskId={route.taskId} worker={route.worker} />;
      break;
    default:
      page = <HomePage boot={boot} />;
  }

  return (
    <>
      <div className={`app ${collapsed ? 'side-collapsed' : ''}`}>
        <Sidebar boot={boot} route={route} onAddProject={() => setAddProjectOpen(true)} />
        {page}
        <MobileTabs route={route} hasUrgent={hasUrgentSession(boot.openInboxItems, boot.activeTasks)} />
      </div>
      <ReviewOverlay boot={boot} />
      {cmdkOpen && <CommandPalette boot={boot} />}
      {machinesOpen && (
        <MachinesPopover
          machines={boot.machines}
          defaultWorkspaceName={boot.workspaceName}
          activeDutyMachineId={boot.setup?.activeDutyMachineId}
          currentMachineId={boot.setup?.currentMachineId}
        />
      )}
      <AddProjectModal
        open={addProjectOpen}
        machines={boot.machines}
        defaultMachineId={boot.setup?.currentMachineId}
        activeDutyMachineId={boot.setup?.activeDutyMachineId}
        currentMachineId={boot.setup?.currentMachineId}
        onClose={() => setAddProjectOpen(false)}
      />
      <Toast />
    </>
  );
}
