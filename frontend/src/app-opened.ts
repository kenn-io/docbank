import { startBrowserTelemetry } from "@kenn-io/kit-ui/utils/browser-telemetry";
import { APIError, sessionResponse } from "./api-transport.js";
import { getReportTelemetryEventUrl } from "./generated/docbank.js";

export function startTelemetryReporting(session: string) {
  const reporter = startBrowserTelemetry({
    route: getReportTelemetryEventUrl(),
    surface: "web",
    storageKey: "docbank.web.telemetry.v1",
    post: (route, event, options) =>
      sessionResponse(route, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(event),
        session,
        keepalive: options?.keepalive,
        signal: AbortSignal.timeout(3000),
      }).catch((cause: unknown) => {
        if (cause instanceof APIError) return { status: cause.status, data: undefined };
        throw cause;
      }),
  });
  let latestScreen: string | undefined;
  const reportVisibleScreen = () => {
    if (latestScreen !== undefined && document.visibilityState === "visible")
      reporter.reportScreen(latestScreen);
  };
  document.addEventListener("visibilitychange", reportVisibleScreen);
  return {
    ...reporter,
    reportScreen(screen: string) {
      latestScreen = screen;
      reportVisibleScreen();
    },
    stop() {
      document.removeEventListener("visibilitychange", reportVisibleScreen);
      reporter.stop();
    },
  };
}
