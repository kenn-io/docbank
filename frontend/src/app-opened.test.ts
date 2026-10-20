import { afterEach, beforeEach, describe, expect, it, type MockInstance, vi } from "vite-plus/test";
import { startTelemetryReporting } from "./app-opened.js";

const settle = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

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
  let stop: (() => void) | undefined;
  let fetchMock: MockInstance<typeof fetch>;

  beforeEach(() => {
    localStorage.clear();
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-03-04T10:00:00Z"));
    fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async () => respond(202));
  });

  afterEach(() => {
    stop?.();
    stop = undefined;
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  const focus = () => window.dispatchEvent(new Event("focus"));

  it("posts app_opened to the daemon with only the browser session", async () => {
    stop = startTelemetryReporting("secret").stop;
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
    stop = startTelemetryReporting("secret").stop;
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
});

describe("shared browser receipt gate", () => {
  let reporter: ReturnType<typeof startTelemetryReporting>;
  beforeEach(() => {
    vi.useFakeTimers();
    localStorage.clear();
    vi.setSystemTime(new Date("2026-03-04T10:00:00Z"));
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  });
  afterEach(() => {
    reporter.stop();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });
  it("retains disabled receipts across screen changes, visits, and reloads", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(respond(202));
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
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockImplementation(async () => new Response('{"status":"queued"}', { status: 202 }));
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
    await vi.advanceTimersByTimeAsync(999);
    expect(fetchMock).toHaveBeenCalledTimes(3);
    await vi.advanceTimersByTimeAsync(1);
    expect(fetchMock).toHaveBeenCalledTimes(4);
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
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockImplementation(async () => new Response('{"status":"queued"}', { status: 202 }));
    reporter = startTelemetryReporting("secret");
    reporter.reportScreen("browse");
    reporter.reportScreen("help");
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchMock.mock.calls.map(([, init]) => JSON.parse(String(init?.body)).event)).toEqual([
      "app_opened",
    ]);
    visibility = "visible";
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchMock.mock.calls.map(([, init]) => JSON.parse(String(init?.body)))).toEqual([
      { event: "app_opened", properties: { surface: "web" } },
      { event: "screen_viewed", properties: { surface: "web", screen: "help" } },
    ]);
    reporter.stop();
    reporter.reportScreen("search");
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
});
