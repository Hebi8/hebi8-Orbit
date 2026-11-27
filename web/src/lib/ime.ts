import { useCallback, useMemo, useRef, type KeyboardEvent as ReactKeyboardEvent } from 'react';

/**
 * Grace window (ms) after `compositionend` during which a trailing `keydown(Enter)`
 * is still treated as IME confirmation.
 *
 * On macOS WKWebView (Orbit desktop shell & Safari), pressing Enter to commit a CJK
 * Pinyin candidate fires `compositionend` *before* `keydown(Enter)`, causing a naive
 * `e.nativeEvent.isComposing` check to read `false` and prematurely submit messages.
 */
export const IME_COMPOSITION_GRACE_MS = 120;

export function isImeKeyEvent(
  e: ReactKeyboardEvent | KeyboardEvent,
  composing: boolean,
  lastCompositionEndAt: number,
): boolean {
  const native = 'nativeEvent' in e ? e.nativeEvent : e;
  const reactKeyCode = 'keyCode' in e ? e.keyCode : 0;
  const nativeKeyCode = native.keyCode ?? 0;

  if (native.isComposing || reactKeyCode === 229 || nativeKeyCode === 229 || native.key === 'Process') {
    return true;
  }
  if (composing) {
    return true;
  }
  if (lastCompositionEndAt > 0 && performance.now() - lastCompositionEndAt < IME_COMPOSITION_GRACE_MS) {
    return true;
  }
  return false;
}

/**
 * Shared IME composition guard for inputs & textareas across macOS WKWebView and all browsers.
 * Spread `compositionProps` onto `<input>` / `<textarea>`, and check `isImeEnter(e)` before
 * handling Enter / Escape submission shortcuts.
 */
export function useImeGuard() {
  const composingRef = useRef(false);
  const lastCompositionEndAtRef = useRef(0);

  const onCompositionStart = useCallback(() => {
    composingRef.current = true;
  }, []);

  const onCompositionEnd = useCallback(() => {
    composingRef.current = false;
    lastCompositionEndAtRef.current = performance.now();
  }, []);

  const compositionProps = useMemo(
    () => ({
      onCompositionStart,
      onCompositionEnd,
    }),
    [onCompositionStart, onCompositionEnd],
  );

  const isImeEnter = useCallback((e: ReactKeyboardEvent | KeyboardEvent): boolean => {
    return isImeKeyEvent(e, composingRef.current, lastCompositionEndAtRef.current);
  }, []);

  return {
    composingRef,
    lastCompositionEndAtRef,
    onCompositionStart,
    onCompositionEnd,
    compositionProps,
    isImeEnter,
  };
}
