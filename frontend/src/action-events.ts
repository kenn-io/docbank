import searchTypes from "../../internal/telemetry/search_types.json";
import { sessionResponse } from "./api-transport.js";

export function reportAction(session: string, event: string, value = ""): void {
  void sendAction(session, event, value).catch(() => undefined);
}

async function sendAction(session: string, event: string, value = ""): Promise<void> {
  const search_type = Object.hasOwn(searchTypes, value)
    ? searchTypes[value as keyof typeof searchTypes]
    : searchTypes.default;
  await sessionResponse("/api/daemon/telemetry/events", {
    session,
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      event,
      properties: { surface: "web", ...(event === "search_run" ? { search_type } : {}) },
    }),
    signal: AbortSignal.timeout(3000),
  });
}

// Share successful daily deliveries across live and snapshot viewers.
const inspectionGates = new Map<string, { day: string; pending: boolean }>();

export function inspectionReporter(event: string, session: () => string): () => void {
  return () => {
    const day = new Date().toISOString().slice(0, 10);
    let gate = inspectionGates.get(event);
    if (!gate) {
      gate = { day: "", pending: false };
      inspectionGates.set(event, gate);
    }
    if (gate.pending || gate.day === day) return;
    gate.pending = true;
    void sendAction(session(), event)
      .then(() => {
        gate.day = day;
      })
      .catch(() => undefined)
      .finally(() => {
        gate.pending = false;
      });
  };
}
