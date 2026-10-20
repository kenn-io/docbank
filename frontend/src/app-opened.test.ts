import { afterEach, beforeEach, describe, expect, it, type MockInstance, vi } from "vite-plus/test";
import { startTelemetryReporting } from "./app-opened.js";
import { startSessionReporting } from "./session-duration.js";

const settle = () => vi.advanceTimersByTimeAsync(0);

function respond(status: number): Response {
  return new Response(
    JSON.stringify(status === 202 ? { status: "disabled" } : { status, title: "Unauthorized" }),
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

  it("posts app_opened to the daemon with only the browser session", async () => {
    reporter = startTelemetryReporting("secret");
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
  });

  it("treats a problem response as answered without an unhandled rejection", async () => {
    const unhandled = vi.fn();
    window.addEventListener("unhandledrejection", unhandled);
    fetchMock.mockImplementationOnce(async () => respond(401));
    reporter = startTelemetryReporting("secret");
    await settle();
    await settle();
    focus();
    await settle();
    vi.setSystemTime(new Date("2026-03-05T08:00:00Z"));
    focus();
    await settle();
    window.removeEventListener("unhandledrejection", unhandled);
    expect(unhandled).not.toHaveBeenCalled();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
  it("retains disabled receipts across screen changes, visits, and reloads", async () => {
    reporter = startTelemetryReporting("secret");
    await vi.advanceTimersByTimeAsync(0);
    reporter.reportScreen("help");
    reporter.reportVisit({ event: "session_ended", properties: { surface: "web" } });
    reporter.stop();
    reporter = startTelemetryReporting("secret");
    reporter.reportScreen("browse");
    window.dispatchEvent(new Event("focus"));
    await vi.advanceTimersByTimeAsync(60_000);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
  it("bounds screen failures and forwards the deadline and keepalive through transport", async () => {
    fetchMock.mockImplementation(async () => new Response('{"status":"queued"}', { status: 202 }));
    reporter = startTelemetryReporting("secret");
    await vi.advanceTimersByTimeAsync(0);
    reporter.reportVisit(
      { event: "session_ended", properties: { surface: "web" } },
      { keepalive: true },
    );
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchMock.mock.calls[1]?.[1]?.keepalive).toBe(true);
    expect(fetchMock.mock.calls[1]?.[1]?.signal).toBeInstanceOf(AbortSignal);
    fetchMock.mockImplementation(async () => new Response(null, { status: 503 }));
    reporter.reportScreen("help");
    await vi.advanceTimersByTimeAsync(120_000);
    expect(fetchMock).toHaveBeenCalledTimes(8);
    window.dispatchEvent(new Event("focus"));
    reporter.reportScreen("browse");
    await vi.advanceTimersByTimeAsync(120_000);
    expect(fetchMock).toHaveBeenCalledTimes(8);
  });
  it("holds hidden screens and reports only the latest screen on visibility", async () => {
    let visibility: DocumentVisibilityState = "hidden";
    vi.spyOn(document, "visibilityState", "get").mockImplementation(() => visibility);
    fetchMock.mockImplementation(async () => new Response('{"status":"queued"}', { status: 202 }));
    reporter = startTelemetryReporting("secret");
    reporter.reportScreen("browse");
    reporter.reportScreen("help");
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    visibility = "visible";
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(JSON.parse(String(fetchMock.mock.calls[1]?.[1]?.body))).toEqual({
      event: "screen_viewed",
      properties: { surface: "web", screen: "help" },
    });
    reporter.stop();
    reporter.reportScreen("search");
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
  it("discards disabled duration and resumes on a later enabled day", async () => {
    vi.spyOn(document, "hidden", "get").mockReturnValue(false);
    let now = 0;
    vi.spyOn(performance, "now").mockImplementation(() => now);
    reporter = startTelemetryReporting("secret");
    const duration = startSessionReporting(reporter);
    now = 120_000;
    await settle();
    expect(duration.take()).toBeUndefined();
    window.dispatchEvent(new Event("pagehide"));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    fetchMock.mockImplementation(async () => new Response('{"status":"queued"}', { status: 202 }));
    vi.setSystemTime(new Date("2026-03-05T10:00:00Z"));
    focus();
    await settle();
    now += 120_000;
    window.dispatchEvent(new Event("pagehide"));
    await settle();
    expect(JSON.parse(String(fetchMock.mock.calls.at(-1)?.[1]?.body))).toEqual({
      event: "session_ended",
      properties: { surface: "web", duration_bucket: "1_to_5m" },
    });
    duration.stop();
  });
});
