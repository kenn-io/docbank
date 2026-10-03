import { startAppOpenedReporting as startKitAppOpened } from "@kenn-io/kit-ui/utils/app-opened";
import { APIError, sessionResponse } from "./api-transport.js";
import { getReportTelemetryEventUrl } from "./generated/docbank.js";

/** Reports app_opened to the daemon with the browser session through kit-ui's daily gate; returns a cleanup. */
export function startAppOpenedReporting(session: string): () => void {
  return startKitAppOpened({
    route: getReportTelemetryEventUrl(),
    surface: "web",
    post: (route, { event }) =>
      sessionResponse(route, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ event }),
        session,
      }).then(
        (response) => ({ status: response.status }),
        (cause: unknown) => (cause instanceof APIError ? { status: cause.status } : Promise.reject(cause)),
      ),
  });
}
