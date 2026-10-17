import searchTypes from "../../internal/telemetry/search_types.json";
import { sessionResponse } from "./api-transport.js";

export function reportAction(session: string, event: string, value = ""): void {
  const search_type = Object.hasOwn(searchTypes, value)
    ? searchTypes[value as keyof typeof searchTypes]
    : searchTypes.default;
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

// The daemon owns daily claims, so failed deliveries can be retried by later gestures.
export function inspectionReporter(event: string, session: () => string): () => void {
  return () => reportAction(session(), event);
}
