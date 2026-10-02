import { reportTelemetryEvent } from "./generated/docbank.js";

/** Reports app_opened now and on the first window focus of each later UTC day while a session exists; returns a cleanup. */
export function startAppOpenedReporting(session: () => string): () => void {
  let reportedDay = "";
  const report = (): void => {
    const token = session();
    if (!token) return;
    const day = new Date().toISOString().slice(0, 10);
    if (day === reportedDay) return;
    reportedDay = day;
    void reportTelemetryEvent({ event: "app_opened" }, { session: token }).catch(() => {});
  };
  report();
  window.addEventListener("focus", report);
  return () => window.removeEventListener("focus", report);
}
