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

// dailyAction sends one request per session and UTC day; the daemon keeps the durable claim.
export function dailyAction(event: string, session: () => string): () => void {
  let claimed = "";
  return () => {
    const current = session();
    const claim = `${current}:${new Date().toISOString().slice(0, 10)}`;
    if (claim === claimed) return;
    claimed = claim;
    reportAction(current, event);
  };
}
