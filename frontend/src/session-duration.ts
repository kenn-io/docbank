import type { BrowserTelemetryReporter } from "@kenn-io/kit-ui/utils/browser-telemetry";
import type { TelemetryEventPropertiesDurationBucket } from "./generated/docbank.js";

const MINUTE = 60_000;
const HIDDEN_SESSION_END = 30 * MINUTE;

function durationBucket(elapsed: number): TelemetryEventPropertiesDurationBucket {
  if (elapsed < MINUTE) return "under_1m";
  if (elapsed < 5 * MINUTE) return "1_to_5m";
  if (elapsed <= 30 * MINUTE) return "5_to_30m";
  return "over_30m";
}

/**
 * Reports session_ended with the page's visible time when the page hides for good, stays hidden
 * for thirty minutes, or stop runs. take consumes the final duration for a revoke request.
 */
export function startSessionReporting(
  reporter: Pick<BrowserTelemetryReporter, "reportVisit" | "onQuietChange">,
) {
  let disabled = false;
  let started = document.hidden ? undefined : performance.now();
  let visible = 0;
  let hiddenSince: number | undefined;
  let hiddenTimer: ReturnType<typeof setTimeout> | undefined;
  const pause = () => {
    if (started === undefined) return;
    visible += performance.now() - started;
    started = undefined;
  };
  const clearTimer = () => {
    clearTimeout(hiddenTimer);
    hiddenTimer = undefined;
  };
  const take = () => {
    clearTimer();
    hiddenSince = undefined;
    pause();
    const elapsed = visible;
    visible = 0;
    return !disabled && elapsed > 0 ? durationBucket(elapsed) : undefined;
  };
  const end = () => {
    const bucket = take();
    if (!bucket) return;
    reporter.reportVisit(
      { event: "session_ended", properties: { surface: "web", duration_bucket: bucket } },
      { keepalive: true },
    );
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
  const listen = () => {
    document.addEventListener("visibilitychange", visibility);
    window.addEventListener("pagehide", end);
    window.addEventListener("pageshow", resume);
  };
  const unlisten = () => {
    document.removeEventListener("visibilitychange", visibility);
    window.removeEventListener("pagehide", end);
    window.removeEventListener("pageshow", resume);
  };
  listen();
  const unsubscribe = reporter.onQuietChange((_quiet, reason) => {
    if (reason === "disabled" || reason === "stopped") {
      disabled = true;
      take();
      unlisten();
    } else if (reason === "enabled" && disabled) {
      disabled = false;
      listen();
      resume();
    }
  });
  const stop = () => {
    end();
    unsubscribe();
    unlisten();
  };
  return { take, stop };
}
