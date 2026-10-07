import { afterEach, expect, it, vi } from "vitest";
import { startScreenReporting } from "./screen-views.js";

afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers(); });

it("reports canonical names through the authenticated route and retries focus after failure", async () => {
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  const fetchMock = vi.spyOn(globalThis, "fetch").mockRejectedValueOnce(new Error("offline"))
    .mockResolvedValue(new Response('{"status":"queued"}', { status: 202 }));
  const stop = startScreenReporting("synthetic-session", "browse");
  await new Promise((resolve) => setTimeout(resolve, 0));
  window.dispatchEvent(new Event("focus"));
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(fetchMock).toHaveBeenCalledTimes(2);
  const [url, init] = fetchMock.mock.calls[1]!;
  expect(url).toBe("/api/daemon/telemetry/events");
  expect(JSON.parse(init!.body as string)).toEqual({ event: "screen_viewed", properties: { screen: "browse", surface: "web" } });
  expect(new Headers(init!.headers).get("X-Docbank-Web-Session")).toBe("synthetic-session");
  stop();
  window.dispatchEvent(new Event("focus"));
  expect(fetchMock).toHaveBeenCalledTimes(2);
});

it("aborts pending requests when the screen closes", () => {
  vi.useFakeTimers();
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(() => new Promise(() => {}));
  const stop = startScreenReporting("synthetic-session", "help");
  const signal = fetchMock.mock.calls[0]![1]!.signal!;
  stop();
  expect(signal.aborted).toBe(true);
  expect(vi.getTimerCount()).toBe(0);
});
