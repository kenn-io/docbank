import { afterEach, expect, it, vi } from "vitest";
import { startScreenReporting } from "./screen-views.js";

afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers(); });

it("reports canonical names through the authenticated route and retries focus after failure", async () => {
  vi.useFakeTimers();
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  const fetchMock = vi.spyOn(globalThis, "fetch").mockRejectedValueOnce(new Error("offline"))
    .mockImplementation(() => new Promise(() => {}));
  const stop = startScreenReporting("synthetic-session", "browse");
  await vi.advanceTimersByTimeAsync(0);
  window.dispatchEvent(new Event("focus"));
  await vi.advanceTimersByTimeAsync(0);
  expect(fetchMock).toHaveBeenCalledTimes(2);
  const [url, init] = fetchMock.mock.calls[1]!;
  expect(url).toBe("/api/daemon/telemetry/events");
  expect(JSON.parse(init!.body as string)).toEqual({ event: "screen_viewed", properties: { screen: "browse", surface: "web" } });
  expect(new Headers(init!.headers).get("X-Docbank-Web-Session")).toBe("synthetic-session");
  stop();
  expect(init!.signal!.aborted).toBe(true);
  expect(vi.getTimerCount()).toBe(0);
  window.dispatchEvent(new Event("focus"));
  expect(fetchMock).toHaveBeenCalledTimes(2);
});
