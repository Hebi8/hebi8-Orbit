import type { ReactNode } from 'react';
import { PanelLeftOpen, Search } from 'lucide-react';
import { useWorkbenchStore } from '../store/useWorkbenchStore';

/** 46px hairline top bar shared by all first-level pages (expand-sidebar affordance when collapsed). */
export function PageHeader({ children, right }: { children?: ReactNode; right?: ReactNode }) {
  const collapsed = useWorkbenchStore((s) => s.sidebarCollapsed);
  const toggleSidebar = useWorkbenchStore((s) => s.toggleSidebar);
  const setCmdk = useWorkbenchStore((s) => s.setCmdkOpen);
  return (
    <header className="thead">
      {collapsed && (
        <button type="button" className="ib no-mobile" title="展开侧栏 (⌘\)" onClick={toggleSidebar}>
          <PanelLeftOpen size={14} strokeWidth={1.6} />
        </button>
      )}
      {children}
      <div className="hact">
        {right}
        <button type="button" className="hb" title="全局快唤 (⌘K)" onClick={() => setCmdk(true)}>
          <Search size={13} strokeWidth={1.6} className="only-mobile" />
          <span className="kbd no-mobile" style={{ border: 0 }}>
            ⌘K
          </span>
        </button>
      </div>
    </header>
  );
}
