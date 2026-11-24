import { useEffect, useState } from 'react';
import { CommandPalette } from './components/CommandPalette';
import { MachinesPopover } from './components/MachinesPopover';
import { MobileTabs } from './components/MobileTabs';
import { Sidebar } from './components/Sidebar';
import { useBootstrap } from './hooks/queries';
import { useRealtimeSync } from './hooks/useRealtimeSync';
import { errMsg } from './lib/api';
import { hasUrgentSession } from './lib/format';
import { navigate, parseHash, paths, useRoute } from './lib/router';
import { HomePage } from './pages/HomePage';
import { NewThreadPage } from './pages/NewThreadPage';
import { PAPage } from './pages/PAPage';
import { ProjectPage } from './pages/ProjectPage';
import { ProjectsListPage } from './pages/ProjectsListPage';
import { SessionPage } from './session/SessionPage';
import { applyThemeToDocument, useWorkbenchStore } from './store/useWorkbenchStore';
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

function BootError({ message, onRetry }: { message: string; onRetry: () => void }) {
  const [token, setToken] = useState('');
  return (
    <div className="empty" style={{ maxWidth: 440, margin: '18vh auto 0' }}>
      <div className="brand" style={{ justifyContent: 'center' }}>
        <span className="brand-mark" />
        <b>hebi8-Orbit</b>
      </div>
      <p>连接 Hub 失败：{message}</p>
      <p className="faint">本机开发可填入 Hub 的 dev token（只存在本浏览器 localStorage）。</p>
      <div style={{ display: 'flex', gap: 6 }}>
        <input className="field" value={token} onChange={(e) => setToken(e.target.value)} placeholder="X-Orbit-Dev-Token" />
        <button
          type="button"
          className="btn pri"
          onClick={() => {
            try {
              localStorage.setItem('orbit.devToken', token.trim());
            } catch {
              // storage unavailable; retry will surface the same error
            }
            onRetry();
          }}
        >
          重试
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
  const bootQ = useBootstrap();
  const route = useRoute();
  useRealtimeSync(bootQ.data?.snapshotSeq);
  useGlobalKeys();

  useEffect(() => applyThemeToDocument(theme), [theme]);

  const boot = bootQ.data;
  if (!boot) {
    if (bootQ.isError) return <BootError message={errMsg(bootQ.error)} onRetry={() => void bootQ.refetch()} />;
    return <div className="empty" style={{ marginTop: '30vh' }}>连接 Hub…</div>;
  }

  let page;
  switch (route.name) {
    case 'pa':
      page = <PAPage boot={boot} threadId={route.threadId} />;
      break;
    case 'new':
      page = <NewThreadPage boot={boot} mode={route.mode} projectId={route.projectId} />;
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
        <Sidebar boot={boot} route={route} />
        {page}
        <MobileTabs route={route} hasUrgent={hasUrgentSession(boot.openInboxItems, boot.activeTasks)} />
      </div>
      <ReviewOverlay boot={boot} />
      {cmdkOpen && <CommandPalette boot={boot} />}
      {machinesOpen && <MachinesPopover machines={boot.machines} />}
      <Toast />
    </>
  );
}
