import { afterEach, beforeEach, expect, it, vi } from "vitest";

let startScreenReporting: typeof import("./screen-views.js").startScreenReporting;
beforeEach(async () => {
  vi.resetModules();
  ({ startScreenReporting } = await import("./screen-views.js"));
});

afterEach(() => { localStorage.clear(); vi.restoreAllMocks(); vi.useRealTimers(); });

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

it("records answered screens across focus, visibility and remounts while allowing a new screen", async () => {
  vi.useFakeTimers();
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(null, { status: 202 }));
  const stop = startScreenReporting("synthetic-session", "browse");
  await vi.advanceTimersByTimeAsync(0);
  window.dispatchEvent(new Event("focus"));
  document.dispatchEvent(new Event("visibilitychange"));
  expect(fetchMock).toHaveBeenCalledTimes(1);
  stop();
  const stopAgain = startScreenReporting("synthetic-session", "browse");
  expect(fetchMock).toHaveBeenCalledTimes(1);
  stopAgain();
  const stopSearch = startScreenReporting("synthetic-session", "search");
  await vi.advanceTimersByTimeAsync(0);
  expect(fetchMock).toHaveBeenCalledTimes(2);
  stopSearch();
});

it.each([400, 500, 502, 503, 504])("retries focus after a %s response", async (status) => {
  vi.useFakeTimers();
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(null, { status }));
  const stop = startScreenReporting("synthetic-session", "browse");
  await vi.advanceTimersByTimeAsync(0);
  window.dispatchEvent(new Event("focus"));
  await vi.advanceTimersByTimeAsync(0);
  expect(fetchMock).toHaveBeenCalledTimes(2);
  stop();
});

it.each(["getItem", "setItem"] as const)("remembers screens in memory when localStorage.%s fails", async (method) => {
  vi.useFakeTimers();
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  vi.spyOn(Storage.prototype, method).mockImplementation(() => { throw new Error("storage blocked"); });
  const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(null, { status: 202 }));
  const stop = startScreenReporting("synthetic-session", "browse");
  await vi.advanceTimersByTimeAsync(0);
  stop();
  const stopAgain = startScreenReporting("synthetic-session", "browse");
  window.dispatchEvent(new Event("focus"));
  expect(fetchMock).toHaveBeenCalledTimes(1);
  stopAgain();
});
