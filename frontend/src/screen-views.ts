import { APIError, sessionResponse } from "./api-transport.js";
import { getReportTelemetryEventUrl } from "./generated/docbank.js";

const storageKey = "docbank.screen-views";
let memory = { day: "", screens: [] as string[] };

function screenClaims(): typeof memory {
  try {
    const stored = JSON.parse(localStorage.getItem(storageKey) ?? "null") as typeof memory | null;
    if (stored && typeof stored.day === "string" && Array.isArray(stored.screens)) {
      if (stored.day > memory.day) memory = stored;
      else if (stored.day === memory.day) memory.screens = [...new Set([...memory.screens, ...stored.screens])];
    }
  } catch {}
  return memory;
}

export function startScreenReporting(session: string, screen: string): () => void {
  let controller: AbortController | undefined;
  let timer: number | undefined;
  let stopped = false;
  const report = (): void => {
    const day = new Date().toISOString().slice(0, 10);
    const claims = screenClaims();
    if (document.visibilityState !== "visible" || controller || claims.day === day && claims.screens.includes(screen)) return;
    controller = new AbortController();
    timer = window.setTimeout(() => controller?.abort(), 3000);
    void sessionResponse(getReportTelemetryEventUrl(), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ event: "screen_viewed", properties: { screen, surface: "web" } }),
      session,
      signal: controller.signal,
    }).then(() => true, (error: unknown) => error instanceof APIError && ![502, 503, 504].includes(error.status))
      .then((answered) => {
        if (!answered || stopped) return;
        const current = screenClaims();
        if (current.day > day) return;
        memory = { day, screens: [...new Set([...(current.day === day ? current.screens : []), screen])] };
        try { localStorage.setItem(storageKey, JSON.stringify(memory)); } catch {}
      }).finally(() => {
        window.clearTimeout(timer);
        controller = undefined;
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
