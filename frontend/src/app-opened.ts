import { startBrowserTelemetry } from "@kenn-io/kit-ui/utils/browser-telemetry";
import { APIError, sessionResponse } from "./api-transport.js";
import { getReportTelemetryEventUrl } from "./generated/docbank.js";

export function startTelemetryReporting(session: string) {
  return startBrowserTelemetry({
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
}
