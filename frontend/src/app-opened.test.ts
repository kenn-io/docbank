import { afterEach, beforeEach, describe, expect, it, type MockInstance, vi } from "vite-plus/test";
import { startTelemetryReporting } from "./app-opened.js";

const settle = () => vi.advanceTimersByTimeAsync(0);

function respond(status: number): Response {
  return new Response(
    JSON.stringify(
      status === 202
        ? { status: "disabled" }
        : { status, title: status === 401 ? "Unauthorized" : "Service Unavailable" },
    ),
    {
      status,
      headers: { "Content-Type": "application/json" },
    },
  );
}

describe("app_opened reporting", () => {
  let reporter: ReturnType<typeof startTelemetryReporting>;
  let fetchMock: MockInstance<typeof fetch>;

  beforeEach(() => {
    localStorage.clear();
    vi.useFakeTimers();
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
    vi.setSystemTime(new Date("2026-03-04T10:00:00Z"));
    fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async () => respond(202));
  });

  afterEach(() => {
    reporter?.stop();
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  const focus = () => window.dispatchEvent(new Event("focus"));

  it("authenticates reports and holds hidden screens until visible", async () => {
    let visibility: DocumentVisibilityState = "hidden";
    vi.spyOn(document, "visibilityState", "get").mockImplementation(() => visibility);
    fetchMock.mockImplementation(async () => new Response('{"status":"queued"}', { status: 202 }));
    reporter = startTelemetryReporting("secret");
    reporter.reportScreen("browse");
    reporter.reportScreen("help");
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] ?? [];
    expect(url).toBe("/api/daemon/telemetry/events");
    expect(init?.method).toBe("POST");
    expect(init?.body).toBe(
      JSON.stringify({ event: "app_opened", properties: { surface: "web" } }),
    );
    const headers = new Headers(init?.headers);
    expect(headers.get("X-Docbank-Web-Session")).toBe("secret");
    visibility = "visible";
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(JSON.parse(String(fetchMock.mock.calls[1]?.[1]?.body))).toEqual({
      event: "screen_viewed",
      properties: { surface: "web", screen: "help" },
    });
    reporter.reportVisit(
      { event: "session_ended", properties: { surface: "web", duration_bucket: "1_to_5m" } },
      { keepalive: true },
    );
    await settle();
    expect(fetchMock.mock.calls.at(-1)?.[1]?.keepalive).toBe(true);
    expect(fetchMock.mock.calls.at(-1)?.[1]?.signal).toBeInstanceOf(AbortSignal);
  });

  it("maps unauthorized responses so the gate stops same-day retries", async () => {
    fetchMock.mockImplementation(async () => respond(401));
    reporter = startTelemetryReporting("secret");
    await vi.advanceTimersByTimeAsync(60_000);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("restores visibility work and the latest hidden screen after an unavailable reply", async () => {
    const add = vi.spyOn(document, "addEventListener");
    const remove = vi.spyOn(document, "removeEventListener");
    fetchMock.mockImplementation(async () => respond(503));
    reporter = startTelemetryReporting("secret");
    reporter.reportScreen("browse");
    await vi.advanceTimersByTimeAsync(60_000);
    const listener = add.mock.calls.find(([name]) => name === "visibilitychange")?.[1];
    expect(listener).toBeDefined();
    expect(remove).toHaveBeenCalledWith("visibilitychange", listener);
    fetchMock.mockClear();
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
    reporter.reportScreen("help");
    await settle();
    expect(fetchMock).not.toHaveBeenCalled();
    fetchMock.mockImplementation(async () => new Response('{"status":"queued"}', { status: 202 }));
    vi.setSystemTime(new Date("2026-03-05T10:00:00Z"));
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
    focus();
    await settle();
    expect(add.mock.calls.filter(([name]) => name === "visibilitychange")).toHaveLength(2);
    expect(fetchMock.mock.calls.map(([, init]) => JSON.parse(String(init?.body)))).toEqual([
      { event: "app_opened", properties: { surface: "web" } },
      { event: "screen_viewed", properties: { surface: "web", screen: "help" } },
    ]);
  });
});
