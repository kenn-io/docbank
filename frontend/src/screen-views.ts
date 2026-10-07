import { sessionResponse } from "./api-transport.js";
import { getReportTelemetryEventUrl } from "./generated/docbank.js";

export function startScreenReporting(session: string, screen: string): () => void {
  const controllers = new Map<AbortController, number>();
  const report = (): void => {
    if (document.visibilityState !== "visible" || controllers.size) return;
    const controller = new AbortController();
    const timer = window.setTimeout(() => controller.abort(), 3000);
    controllers.set(controller, timer);
    void sessionResponse(getReportTelemetryEventUrl(), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ event: "screen_viewed", properties: { screen, surface: "web" } }),
      session,
      signal: controller.signal,
    }).catch(() => {}).finally(() => {
      window.clearTimeout(timer);
      controllers.delete(controller);
    });
  };
  report();
  window.addEventListener("focus", report);
  document.addEventListener("visibilitychange", report);
  return () => {
    window.removeEventListener("focus", report);
    document.removeEventListener("visibilitychange", report);
    controllers.forEach((timer, controller) => {
      window.clearTimeout(timer);
      controller.abort();
    });
  };
}
