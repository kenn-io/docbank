import { afterEach, beforeEach, describe, expect, it, type MockInstance, vi } from "vitest";
import { startAppOpenedReporting } from "./app-opened.js";

const settle = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

function respond(status: number): Response {
  return new Response(JSON.stringify(status === 202 ? { status: "disabled" } : { status, title: "Unauthorized" }), {
    status,
    headers: { "Content-Type": "application/json" },
  });
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
    stop = startAppOpenedReporting("secret");
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] ?? [];
    expect(url).toBe("/api/daemon/telemetry/events");
    expect(init?.method).toBe("POST");
    expect(init?.body).toBe(JSON.stringify({ event: "app_opened" }));
    const headers = new Headers(init?.headers);
    expect(headers.get("X-Docbank-Web-Session")).toBe("secret");
    expect(headers.get("X-Api-Key")).toBeNull();
  });

  it("treats a problem response as answered without an unhandled rejection", async () => {
    const unhandled = vi.fn();
    window.addEventListener("unhandledrejection", unhandled);
    fetchMock.mockImplementationOnce(async () => respond(401));
    stop = startAppOpenedReporting("secret");
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
