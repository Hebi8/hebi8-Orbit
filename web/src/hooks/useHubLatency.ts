import { useEffect, useState } from 'react';

const HEALTH_PING_INTERVAL_MS = 15_000;

/**
 * Measures round-trip latency to the connected Orbit Hub (`/api/health`), mirroring
 * Lody LAN fork's `useLanHubLatency`: `undefined` until first probe, `null` when unreachable.
 * Pauses while the browser tab is hidden.
 */
export function useHubLatency(): number | null | undefined {
  const [latency, setLatency] = useState<number | null | undefined>(undefined);

  useEffect(() => {
    let active = true;
    let inflight = false;

    const measure = () => {
      if (inflight || document.visibilityState === 'hidden') return;
      inflight = true;
      const start = performance.now();
      const ctrl = new AbortController();
      const timeout = window.setTimeout(() => ctrl.abort(), 5000);
      fetch('/api/health', { cache: 'no-store', credentials: 'omit', signal: ctrl.signal })
        .then((res) => {
          window.clearTimeout(timeout);
          inflight = false;
          if (!active) return;
          if (!res.ok) {
            setLatency(null);
            return;
          }
          setLatency(Math.max(1, Math.round(performance.now() - start)));
        })
        .catch(() => {
          window.clearTimeout(timeout);
          inflight = false;
          if (active) setLatency(null);
        });
    };

    measure();
    const timer = window.setInterval(measure, HEALTH_PING_INTERVAL_MS);
    const onVis = () => {
      if (document.visibilityState === 'visible') measure();
    };
    document.addEventListener('visibilitychange', onVis);
    return () => {
      active = false;
      window.clearInterval(timer);
      document.removeEventListener('visibilitychange', onVis);
    };
  }, []);

  return latency;
}
