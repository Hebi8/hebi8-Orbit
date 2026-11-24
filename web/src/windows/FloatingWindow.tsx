import { useRef, type PointerEvent as RPointerEvent, type ReactNode } from 'react';
import { X } from 'lucide-react';
import { useWorkbenchStore, type WinKind } from '../store/useWorkbenchStore';

/** Independent, draggable floating window (Code Review / Artifacts can be open side by side). */
export function FloatingWindow({
  kind,
  icon,
  title,
  subtitle,
  actions,
  footer,
  width,
  height,
  children,
}: {
  kind: WinKind;
  icon: ReactNode;
  title: string;
  subtitle?: ReactNode;
  actions?: ReactNode;
  footer?: ReactNode;
  width: string;
  height: string;
  children: ReactNode;
}) {
  const win = useWorkbenchStore((s) => s.windows[kind]);
  const order = useWorkbenchStore((s) => s.winOrder);
  const focus = useWorkbenchStore((s) => s.focusWindow);
  const move = useWorkbenchStore((s) => s.moveWindow);
  const close = useWorkbenchStore((s) => s.closeWindow);
  const drag = useRef<{ dx: number; dy: number; id: number } | null>(null);
  if (!win) return null;
  const z = 40 + Math.max(0, order.indexOf(kind));

  const onPointerDown = (e: RPointerEvent<HTMLDivElement>) => {
    if ((e.target as HTMLElement).closest('button, a, input, select, textarea')) return;
    if (window.matchMedia('(max-width: 767px)').matches) return;
    focus(kind);
    drag.current = { dx: e.clientX - win.x, dy: e.clientY - win.y, id: e.pointerId };
    e.currentTarget.setPointerCapture(e.pointerId);
  };
  const onPointerMove = (e: RPointerEvent<HTMLDivElement>) => {
    const d = drag.current;
    if (!d || d.id !== e.pointerId) return;
    const x = Math.min(Math.max(e.clientX - d.dx, 0), window.innerWidth - 120);
    const y = Math.min(Math.max(e.clientY - d.dy, 0), window.innerHeight - 48);
    move(kind, x, y);
  };
  const onPointerUp = (e: RPointerEvent<HTMLDivElement>) => {
    if (drag.current?.id === e.pointerId) drag.current = null;
  };

  return (
    <section
      className="win"
      role="dialog"
      aria-label={title}
      data-win={kind}
      style={{ left: win.x, top: win.y, width, height, zIndex: z }}
      onPointerDownCapture={() => focus(kind)}
    >
      <div className="wh" onPointerDown={onPointerDown} onPointerMove={onPointerMove} onPointerUp={onPointerUp} onPointerCancel={onPointerUp}>
        {icon}
        <span className="t">{title}</span>
        {subtitle && <span className="s">{subtitle}</span>}
        <div className="acts">
          {actions}
          <button type="button" className="ib" title="关闭 (Esc)" onClick={() => close(kind)}>
            <X size={14} strokeWidth={1.6} />
          </button>
        </div>
      </div>
      {children}
      {footer && <div className="wf">{footer}</div>}
    </section>
  );
}
