import { useEffect, useLayoutEffect, useState, type CSSProperties, type RefObject } from 'react';

/**
 * Computes viewport-clamped `position: fixed` coordinates for a portal popover
 * anchored to `anchorRef`, preventing clipping by parent `overflow: hidden`,
 * narrow split drawers, sidebars, or mobile viewports.
 */
export function useAnchoredPopover({
  open,
  anchorRef,
  popRef,
  width,
  align = 'left',
  placement = 'up',
  gap = 8,
  minSpaceUp = 190,
  onClose,
}: {
  open: boolean;
  anchorRef: RefObject<HTMLElement | null>;
  popRef: RefObject<HTMLElement | null>;
  width: number;
  align?: 'left' | 'right';
  placement?: 'up' | 'down';
  gap?: number;
  minSpaceUp?: number;
  onClose: () => void;
}): CSSProperties | null {
  const [style, setStyle] = useState<CSSProperties | null>(null);

  useLayoutEffect(() => {
    if (!open) {
      setStyle(null);
      return;
    }
    const update = () => {
      const el = anchorRef.current;
      if (!el) return;
      const rect = el.getBoundingClientRect();
      const vw = window.innerWidth;
      const vh = window.innerHeight;
      const margin = 8;
      const effWidth = Math.min(width, Math.max(180, vw - margin * 2));
      const rawLeft = align === 'right' ? rect.right - effWidth : rect.left;
      const left = Math.max(margin, Math.min(rawLeft, vw - effWidth - margin));

      const preferUp = placement === 'up';
      const openUp = preferUp ? rect.top >= minSpaceUp || rect.top >= vh - rect.bottom : false;

      if (openUp) {
        setStyle({
          position: 'fixed',
          left,
          bottom: Math.max(margin, vh - rect.top + gap),
          width: effWidth,
          maxHeight: Math.max(140, rect.top - gap - margin),
        });
      } else {
        const top = Math.min(vh - margin - 120, rect.bottom + gap);
        setStyle({
          position: 'fixed',
          left,
          top: Math.max(margin, top),
          width: effWidth,
          maxHeight: Math.max(140, vh - top - margin),
        });
      }
    };

    update();
    window.addEventListener('resize', update);
    window.addEventListener('scroll', update, true);
    return () => {
      window.removeEventListener('resize', update);
      window.removeEventListener('scroll', update, true);
    };
  }, [open, anchorRef, width, align, placement, gap, minSpaceUp]);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      const target = e.target as Node | null;
      if (!target) return;
      if (anchorRef.current?.contains(target)) return;
      if (popRef.current?.contains(target)) return;
      onClose();
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !e.isComposing) {
        e.preventDefault();
        e.stopImmediatePropagation();
        e.stopPropagation();
        onClose();
      }
    };
    document.addEventListener('mousedown', onDown);
    window.addEventListener('keydown', onKey, true);
    return () => {
      document.removeEventListener('mousedown', onDown);
      window.removeEventListener('keydown', onKey, true);
    };
  }, [open, anchorRef, popRef, onClose]);

  return style;
}
