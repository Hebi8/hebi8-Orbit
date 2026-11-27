import {
  useCallback,
  useMemo,
  useRef,
  type FormEvent as ReactFormEvent,
  type KeyboardEvent as ReactKeyboardEvent,
} from 'react';

/**
 * Tight synchronous window (ms) after `compositionend` during which at most ONE
 * trailing `keydown(Enter)` is treated as the IME candidate confirmation key.
 *
 * On macOS WKWebView (Orbit desktop shell & Safari), pressing Enter to commit a CJK
 * Pinyin candidate fires `compositionend` *before* `keydown(Enter)`, causing a naive
 * `e.nativeEvent.isComposing` check to read `false` and prematurely submit messages.
 */
export const IME_TRAILING_ENTER_WINDOW_MS = 50;

export interface ImeKeyLikeEvent {
  key: string;
  keyCode?: number;
  isComposing?: boolean;
  metaKey?: boolean;
  ctrlKey?: boolean;
  altKey?: boolean;
  nativeEvent?: {
    key?: string;
    keyCode?: number;
    isComposing?: boolean;
  };
}

export interface ImeInputLikeEvent {
  isComposing?: boolean;
  inputType?: string;
  nativeEvent?: {
    isComposing?: boolean;
    inputType?: string;
  };
}

export interface ImeGuardController {
  readonly isComposing: () => boolean;
  onCompositionStart: () => void;
  onCompositionEnd: () => void;
  onKeyUp: () => void;
  onInput: (e?: ImeInputLikeEvent | ReactFormEvent | Event) => void;
  clearPendingTrailingEnter: () => void;
  isImeKeyEvent: (e: ImeKeyLikeEvent | ReactKeyboardEvent | KeyboardEvent) => boolean;
}

function isCompositionCommitInput(e?: ImeInputLikeEvent | ReactFormEvent | Event): boolean {
  if (!e) return false;
  const raw = ('nativeEvent' in e && e.nativeEvent ? e.nativeEvent : e) as {
    isComposing?: boolean;
    inputType?: string;
  };
  if (raw.isComposing) return true;
  const t = raw.inputType ?? '';
  return t === 'insertCompositionText' || t === 'insertFromComposition';
}

function defaultNow(): number {
  return typeof performance !== 'undefined' ? performance.now() : Date.now();
}

/**
 * Pure stateful IME guard controller with one-shot trailing Enter suppression.
 * - During active composition (`composing || isComposing || keyCode === 229 || key === 'Process'`),
 *   ALL keys (Enter, ArrowUp/Down, Escape, Tab, Backspace) are treated as IME events.
 * - On `compositionend`, arms a ONE-SHOT flag valid only for the immediately following `Enter`
 *   within `IME_TRAILING_ENTER_WINDOW_MS` (50ms).
 * - Immediately clears the one-shot flag after consuming that trailing `Enter`, on `keyup`,
 *   on any subsequent non-composition `input`, or on any non-Enter keydown.
 * - NEVER treats post-`compositionend` `ArrowUp` / `ArrowDown` / `Escape` / `Tab` / `Backspace`
 *   as IME events.
 */
export function createImeGuardController(nowFn: () => number = defaultNow): ImeGuardController {
  let composing = false;
  let pendingTrailingEnter = false;
  let lastCompositionEndAt = 0;

  const clearPendingTrailingEnter = () => {
    pendingTrailingEnter = false;
    lastCompositionEndAt = 0;
  };

  const onCompositionStart = () => {
    composing = true;
    clearPendingTrailingEnter();
  };

  const onCompositionEnd = () => {
    composing = false;
    pendingTrailingEnter = true;
    lastCompositionEndAt = nowFn();
  };

  const onKeyUp = () => {
    clearPendingTrailingEnter();
  };

  const onInput = (e?: ImeInputLikeEvent | ReactFormEvent | Event) => {
    if (!pendingTrailingEnter) return;
    if (isCompositionCommitInput(e)) return;
    clearPendingTrailingEnter();
  };

  const isImeKeyEvent = (e: ImeKeyLikeEvent | ReactKeyboardEvent | KeyboardEvent): boolean => {
    const native = ('nativeEvent' in e && e.nativeEvent ? e.nativeEvent : e) as {
      key?: string;
      keyCode?: number;
      isComposing?: boolean;
    };
    const reactKeyCode = 'keyCode' in e && typeof e.keyCode === 'number' ? e.keyCode : 0;
    const nativeKeyCode = typeof native.keyCode === 'number' ? native.keyCode : 0;
    const key = e.key ?? native.key ?? '';

    if (native.isComposing || reactKeyCode === 229 || nativeKeyCode === 229 || native.key === 'Process' || key === 'Process') {
      return true;
    }
    if (composing) {
      return true;
    }

    if (key === 'Enter' && !e.metaKey && !e.ctrlKey && !e.altKey) {
      if (pendingTrailingEnter) {
        const elapsed = nowFn() - lastCompositionEndAt;
        clearPendingTrailingEnter();
        return elapsed >= 0 && elapsed <= IME_TRAILING_ENTER_WINDOW_MS;
      }
      return false;
    }

    if (pendingTrailingEnter) {
      clearPendingTrailingEnter();
    }
    return false;
  };

  return {
    isComposing: () => composing,
    onCompositionStart,
    onCompositionEnd,
    onKeyUp,
    onInput,
    clearPendingTrailingEnter,
    isImeKeyEvent,
  };
}

/**
 * Shared IME composition guard for inputs & textareas across macOS WKWebView and all browsers.
 * Spread `compositionProps` onto `<input>` / `<textarea>`, and check `isImeEnter(e)` before
 * handling Enter / Escape / navigation shortcuts.
 */
export function useImeGuard() {
  const controllerRef = useRef<ImeGuardController | null>(null);
  if (controllerRef.current === null) {
    controllerRef.current = createImeGuardController();
  }
  const ctrl = controllerRef.current;

  const onCompositionStart = useCallback(() => ctrl.onCompositionStart(), [ctrl]);
  const onCompositionEnd = useCallback(() => ctrl.onCompositionEnd(), [ctrl]);
  const onKeyUp = useCallback(() => ctrl.onKeyUp(), [ctrl]);
  const onInput = useCallback((e: ReactFormEvent<HTMLElement>) => ctrl.onInput(e), [ctrl]);

  const compositionProps = useMemo(
    () => ({
      onCompositionStart,
      onCompositionEnd,
      onKeyUp,
      onInput,
    }),
    [onCompositionStart, onCompositionEnd, onKeyUp, onInput],
  );

  const isImeEnter = useCallback(
    (e: ReactKeyboardEvent | KeyboardEvent): boolean => ctrl.isImeKeyEvent(e),
    [ctrl],
  );

  return {
    onCompositionStart,
    onCompositionEnd,
    onKeyUp,
    onInput,
    compositionProps,
    isImeEnter,
  };
}
