import { Fragment, type ReactNode } from 'react';
import { PanelLeftOpen, Search } from 'lucide-react';
import { useWorkbenchStore } from '../store/useWorkbenchStore';

export interface PageHeaderCrumb {
  label: string;
  title?: string;
  onClick?: () => void;
}

/**
 * Single-line merged breadcrumb for PageHeader first row (e.g. `父会话 / 子会话`).
 * Replaces stacked `.parent-crumb-bar` on full child-session pages.
 */
export function HeaderBreadcrumb({
  items,
  trail,
}: {
  items: readonly PageHeaderCrumb[];
  trail?: ReactNode;
}) {
  if (items.length === 0) return null;
  return (
    <nav className="page-hd-crumb" aria-label="页面路径">
      {items.map((item, idx) => {
        const isLast = idx === items.length - 1;
        return (
          <Fragment key={`${idx}:${item.label}`}>
            {idx > 0 && (
              <span className="topbar-crumb-sep" aria-hidden="true">
                /
              </span>
            )}
            {isLast || !item.onClick ? (
              <span
                className={isLast ? 'topbar-title' : 'topbar-crumb-parent'}
                title={item.title ?? item.label}
              >
                {item.label}
              </span>
            ) : (
              <button
                type="button"
                className="topbar-crumb-parent"
                title={item.title ?? item.label}
                onClick={item.onClick}
              >
                {item.label}
              </button>
            )}
          </Fragment>
        );
      })}
      {trail}
    </nav>
  );
}

export interface PageHeaderProps {
  children?: ReactNode;
  /** Optional first-row single-line breadcrumb (used e.g. for `parent / child` on full child-session pages). */
  breadcrumb?: readonly PageHeaderCrumb[];
  /** Optional inline trailing node rendered directly after the final breadcrumb segment. */
  titleTrail?: ReactNode;
  /** Optional second-line metadata rendered beneath the breadcrumb inside `.topbar-left`. */
  subtitle?: ReactNode;
  right?: ReactNode;
  /** When true, omits the default top-right ⌘K/Search button (shortcut remains active globally). */
  hideCmdK?: boolean;
  className?: string;
}

/** Top hairline header shared by pages (expand-sidebar affordance when collapsed + optional single-line breadcrumb). */
export function PageHeader({
  children,
  breadcrumb,
  titleTrail,
  subtitle,
  right,
  hideCmdK = false,
  className,
}: PageHeaderProps) {
  const collapsed = useWorkbenchStore((s) => s.sidebarCollapsed);
  const toggleSidebar = useWorkbenchStore((s) => s.toggleSidebar);
  const setCmdk = useWorkbenchStore((s) => s.setCmdkOpen);

  const hasBreadcrumb = !!breadcrumb && breadcrumb.length > 0;
  const headerClass = `thead ${subtitle ? 'thead-session' : ''} ${className ?? ''}`.trim();

  return (
    <header className={headerClass}>
      {collapsed && (
        <button type="button" className="ib no-mobile" title="展开侧栏 (⌘\)" aria-label="展开侧栏" onClick={toggleSidebar}>
          <PanelLeftOpen size={14} strokeWidth={1.6} />
        </button>
      )}
      {hasBreadcrumb ? (
        subtitle ? (
          <div className="topbar-left">
            <div className="topbar-title-row">
              <HeaderBreadcrumb items={breadcrumb} trail={titleTrail} />
            </div>
            <div className="topbar-meta">{subtitle}</div>
          </div>
        ) : (
          <HeaderBreadcrumb items={breadcrumb} trail={titleTrail} />
        )
      ) : (
        children
      )}
      <div className="hact">
        {right}
        {!hideCmdK && (
          <button type="button" className="hb" title="全局快唤 (⌘K)" aria-label="全局快唤" onClick={() => setCmdk(true)}>
            <Search size={13} strokeWidth={1.6} className="only-mobile" />
            <span className="kbd no-mobile" style={{ border: 0 }}>
              ⌘K
            </span>
          </button>
        )}
      </div>
    </header>
  );
}
