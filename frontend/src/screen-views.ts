import { APIError, sessionResponse } from "./api-transport.js";
import { getReportTelemetryEventUrl, type ReportTelemetryEventBody, type TelemetryEventPropertiesScreen } from "./generated/docbank.js";

let memory = { day: "", screens: [] as TelemetryEventPropertiesScreen[] };

export function startScreenReporting(session: string, screen: TelemetryEventPropertiesScreen): () => void {
  let controller: AbortController | undefined;
  let timer: number | undefined;
  let stopped = false;
  const report = (): void => {
    const day = new Date().toISOString().slice(0, 10);
    const claims = memory;
    if (stopped || document.visibilityState !== "visible" || controller || claims.day === day && claims.screens.includes(screen)) return;
    window.clearTimeout(timer);
    let retry = false;
    controller = new AbortController();
    timer = window.setTimeout(() => controller?.abort(), 3000);
    void sessionResponse(getReportTelemetryEventUrl(), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ event: "screen_viewed", properties: { screen, surface: "web" } } satisfies ReportTelemetryEventBody),
      session,
      signal: controller.signal,
    })
      .then(() => {
        if (stopped) return;
        const current = memory;
        if (current.day > day) return;
        memory = { day, screens: [...new Set([...(current.day === day ? current.screens : []), screen])] };
      }).catch((error: unknown) => {
        retry = !(error instanceof APIError) || [502, 503, 504].includes(error.status);
      }).finally(() => {
        window.clearTimeout(timer);
        controller = undefined;
        if (retry && !stopped) timer = window.setTimeout(report, 1000);
      });
  };
  report();
  window.addEventListener("focus", report);
  document.addEventListener("visibilitychange", report);
  return () => {
    stopped = true;
    window.removeEventListener("focus", report);
    document.removeEventListener("visibilitychange", report);
    window.clearTimeout(timer);
    controller?.abort();
  };
}
