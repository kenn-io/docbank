import { sessionResponse } from "./api-transport.js";

export function reportAction(session: string, event: string, value = ""): void {
  const search_type =
    value === "semantic" || value === "similar"
      ? "semantic"
      : value === "hybrid"
        ? "hybrid"
        : "text";
  void sessionResponse("/api/daemon/telemetry/events", {
    session,
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      event,
      properties: { surface: "web", ...(event === "search_run" ? { search_type } : {}) },
    }),
    signal: AbortSignal.timeout(3000),
  }).catch(() => undefined);
}
