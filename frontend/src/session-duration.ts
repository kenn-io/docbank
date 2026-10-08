import { sessionResponse } from "./api-transport.js";
import { getReportTelemetryEventUrl } from "./generated/docbank.js";

const MINUTE = 60_000;
const HIDDEN_SESSION_END = 30 * MINUTE;

function durationBucket(elapsed: number): string {
  if (elapsed < MINUTE) return "under_1m";
  if (elapsed < 5 * MINUTE) return "1_to_5m";
  if (elapsed <= 30 * MINUTE) return "5_to_30m";
  return "over_30m";
}

/**
 * Reports session_ended with the page's visible time when the page hides for good, stays hidden
 * for thirty minutes, or the returned cleanup runs. The cleanup resolves once every report settles.
 */
export function startSessionReporting(session: string): () => Promise<void> {
  let started = document.hidden ? undefined : performance.now();
  let visible = 0;
  let hiddenSince: number | undefined;
  let hiddenTimer: ReturnType<typeof setTimeout> | undefined;
  let pending = Promise.resolve();
  const pause = () => {
    if (started === undefined) return;
    visible += performance.now() - started;
    started = undefined;
  };
  const clearTimer = () => {
    clearTimeout(hiddenTimer);
    hiddenTimer = undefined;
  };
  const end = () => {
    clearTimer();
    hiddenSince = undefined;
    pause();
    const elapsed = visible;
    visible = 0;
    if (elapsed <= 0) return;
    const send = sessionResponse(getReportTelemetryEventUrl(), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        event: "session_ended",
        properties: { surface: "web", duration_bucket: durationBucket(elapsed) },
      }),
      session,
      keepalive: true,
    }).catch(() => undefined);
    pending = Promise.all([pending, send]).then(() => undefined);
  };
  const resume = () => {
    if (hiddenSince !== undefined && Date.now() - hiddenSince >= HIDDEN_SESSION_END) end();
    clearTimer();
    if (!document.hidden) {
      hiddenSince = undefined;
      if (started === undefined) started = performance.now();
    }
  };
  const visibility = () => {
    if (!document.hidden) return resume();
    pause();
    hiddenSince = Date.now();
    clearTimer();
    hiddenTimer = setTimeout(end, HIDDEN_SESSION_END);
  };
  document.addEventListener("visibilitychange", visibility);
  window.addEventListener("pagehide", end);
  window.addEventListener("pageshow", resume);
  return () => {
    end();
    document.removeEventListener("visibilitychange", visibility);
    window.removeEventListener("pagehide", end);
    window.removeEventListener("pageshow", resume);
    return pending;
  };
}
