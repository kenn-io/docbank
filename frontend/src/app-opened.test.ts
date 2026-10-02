import { afterEach, beforeEach, describe, expect, it, type MockInstance, vi } from "vitest";
import { startAppOpenedReporting } from "./app-opened.js";

const settle = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

function accepted(): Response {
  return new Response(JSON.stringify({ status: "disabled" }), {
    status: 202,
    headers: { "Content-Type": "application/json" },
  });
}

describe("app_opened reporting", () => {
  let stop: (() => void) | undefined;
  let fetchMock: MockInstance<typeof fetch>;

  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-03-04T10:00:00Z"));
    fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async () => accepted());
  });

  afterEach(() => {
    stop?.();
    stop = undefined;
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  const focus = () => window.dispatchEvent(new Event("focus"));

  it("posts app_opened once on load with only the browser session", async () => {
    stop = startAppOpenedReporting(() => "secret");
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

  it("ignores focus on the same UTC day", async () => {
    stop = startAppOpenedReporting(() => "secret");
    focus();
    focus();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("posts once on the first focus of the next UTC day", async () => {
    stop = startAppOpenedReporting(() => "secret");
    vi.setSystemTime(new Date("2026-03-05T08:00:00Z"));
    focus();
    focus();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("counts a new day across UTC midnight", async () => {
    vi.setSystemTime(new Date("2026-03-04T23:59:00Z"));
    stop = startAppOpenedReporting(() => "secret");
    vi.setSystemTime(new Date("2026-03-05T00:01:00Z"));
    focus();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("posts nothing without a session and reports once a session exists on a new day", async () => {
    let token = "";
    stop = startAppOpenedReporting(() => token);
    focus();
    await settle();
    expect(fetchMock).not.toHaveBeenCalled();
    vi.setSystemTime(new Date("2026-03-05T08:00:00Z"));
    token = "secret";
    focus();
    focus();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("swallows a rejected problem response and still posts the next day", async () => {
    const unhandled = vi.fn();
    window.addEventListener("unhandledrejection", unhandled);
    fetchMock.mockImplementationOnce(async () => new Response(
      JSON.stringify({ status: 401, title: "Unauthorized" }),
      { status: 401, headers: { "Content-Type": "application/problem+json" } },
    ));
    fetchMock.mockImplementationOnce(async () => {
      throw new TypeError("network down");
    });
    stop = startAppOpenedReporting(() => "secret");
    await settle();
    vi.setSystemTime(new Date("2026-03-05T08:00:00Z"));
    focus();
    await settle();
    vi.setSystemTime(new Date("2026-03-06T08:00:00Z"));
    focus();
    await settle();
    window.removeEventListener("unhandledrejection", unhandled);
    expect(unhandled).not.toHaveBeenCalled();
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it("stops listening after cleanup", async () => {
    stop = startAppOpenedReporting(() => "secret");
    stop();
    stop = undefined;
    vi.setSystemTime(new Date("2026-03-05T08:00:00Z"));
    focus();
    await settle();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
