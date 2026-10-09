import { afterEach, expect, it, vi } from "vitest";
import { storage } from "./photo-test-fixtures.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";

const instances: PhotoPreviewCache[] = [];
function workspace() { const cache = new PhotoPreviewCache("scoped", vi.fn()); instances.push(cache); return cache; }
afterEach(async () => { await Promise.all(instances.splice(0).map(cache => cache.dispose())); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

it("keeps no-store previews out of browser storage", async () => {
  const stored = storage();
  const fetcher = vi.fn(async () => new Response("hidden-preview", { headers: { "Cache-Control": "private, no-store" } }));
  vi.stubGlobal("fetch", fetcher);
  const cache = workspace();
  expect(await (await cache.get("asset", "generation")).text()).toBe("hidden-preview");
  expect(await (await cache.get("asset", "generation")).text()).toBe("hidden-preview");
  expect(fetcher).toHaveBeenCalledTimes(2);
  const opened = await stored.open.mock.results[0].value;
  expect(opened.put).not.toHaveBeenCalled();
});

it("retains bytes without fetch and cancels abandoned reads", async () => {
  const stored = storage();
  const fetcher = vi.fn(async (_url: string | URL | Request, _init: RequestInit) => new Response("synthetic-jpeg"));
  vi.stubGlobal("fetch", fetcher);
  const cache = workspace();
  const first = cache.get("asset", "generation");
  expect(await (await first).text()).toBe("synthetic-jpeg");
  const opened = await stored.open.mock.results[0].value;
  expect(opened.match).toHaveBeenCalledTimes(1);
  expect(opened.put).toHaveBeenCalledTimes(1);
  expect(await (await cache.get("asset", "generation")).text()).toBe("synthetic-jpeg");
  expect(fetcher).toHaveBeenCalledTimes(1);
  const key = opened.put.mock.calls[0][0];
  expect(key.credentials).toBe("omit");
  expect([...key.headers]).toHaveLength(0);
  await cache.get("asset", "new-generation");
  expect(fetcher).toHaveBeenCalledTimes(2);
  const second = workspace();
  await second.get("asset", "generation");
  expect(fetcher).toHaveBeenCalledTimes(3);
  expect(stored.data.size).toBe(2);
  await cache.dispose();
  expect(stored.data.size).toBe(1);
  await second.get("asset", "generation");
  expect(fetcher).toHaveBeenCalledTimes(3);
  await expect(second.get("never", "generation", AbortSignal.abort())).rejects.toThrow();
  expect(fetcher).toHaveBeenCalledTimes(3);
  let finishOld!: (response: Response) => void;
  let finishFresh!: (response: Response) => void;
  fetcher.mockImplementationOnce(() => new Promise(resolve => finishOld = resolve))
    .mockImplementationOnce(() => new Promise(resolve => finishFresh = resolve));
  const caller = new AbortController();
  const abandoned = second.get("uncached", "generation", caller.signal);
  const rejected = expect(abandoned).rejects.toThrow();
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(4));
  caller.abort();
  expect(fetcher.mock.calls[3][1].signal!.aborted).toBe(true);
  const fresh = second.get("uncached", "generation");
  await rejected;
  finishOld(new Response("canceled-jpeg"));
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(5));
  finishFresh(new Response("fresh-jpeg"));
  expect(await (await fresh).text()).toBe("fresh-jpeg");
  expect(fetcher).toHaveBeenCalledTimes(5);
});

it("routes expired network authentication to the workspace handler", async () => {
  storage();
  const auth = vi.fn();
  const cache = new PhotoPreviewCache("scoped", auth); instances.push(cache);
  vi.stubGlobal("fetch", vi.fn(async () => new Response("{}", { status: 401 })));
  await expect(cache.get("asset", "generation")).rejects.toThrow("HTTP 401");
  expect(auth).toHaveBeenCalledTimes(1);
});

it("displays network bytes when storage fails and retains only healthy writes", async () => {
  const stored = storage();
  stored.open.mockRejectedValueOnce(new Error("Storage disabled"));
  const fetcher = vi.fn(async () => new Response("synthetic-jpeg"));
  vi.stubGlobal("fetch", fetcher);
  const cache = workspace();
  expect(await (await cache.get("asset", "generation")).text()).toBe("synthetic-jpeg");
  expect(await (await cache.get("asset", "generation")).text()).toBe("synthetic-jpeg");
  expect(fetcher).toHaveBeenCalledTimes(2);
  await cache.get("asset", "generation");
  expect(fetcher).toHaveBeenCalledTimes(2);
  const opened = await stored.open.mock.results[1].value;
  opened.match.mockRejectedValueOnce(new Error("Read disabled"));
  expect(await (await cache.get("asset", "match-failure")).text()).toBe("synthetic-jpeg");
  await cache.get("asset", "match-failure");
  const reopened = await stored.open.mock.results[2].value;
  reopened.put.mockRejectedValueOnce(new Error("Quota exceeded"));
  expect(await (await cache.get("asset", "put-failure")).text()).toBe("synthetic-jpeg");
  await cache.get("asset", "put-failure");
  expect(fetcher).toHaveBeenCalledTimes(6);
  await cache.get("asset", "put-failure");
  expect(fetcher).toHaveBeenCalledTimes(6);
  fetcher.mockRejectedValueOnce(new Error("Network failed"));
  await expect(cache.get("asset", "network-failure")).rejects.toThrow("Network failed");
  const broken = new Response("broken-body");
  vi.spyOn(broken, "arrayBuffer").mockRejectedValueOnce(new Error("Body failed"));
  fetcher.mockResolvedValueOnce(broken);
  await expect(cache.get("asset", "body-failure")).rejects.toThrow("Body failed");
  let rejectOpen!: (cause: Error) => void;
  const opening = new Promise<typeof opened>((_resolve, reject) => rejectOpen = reject);
  stored.open.mockReturnValueOnce(opening);
  const unavailable = workspace();
  const caller = new AbortController();
  const rejected = expect(unavailable.get("asset", "canceled", caller.signal)).rejects.toThrow();
  caller.abort(); rejectOpen(new Error("Storage unavailable"));
  await opening.catch(() => {}); await rejected;
  expect(fetcher).toHaveBeenCalledTimes(8);
});

it("deletes its cache when disposal races opening and rejects the late request", async () => {
  const stored = storage();
  const normalOpen = stored.open.getMockImplementation()!;
  let finish: (() => void) | undefined;
  stored.open.mockImplementation(name => new Promise(resolve => { finish = () => void normalOpen(name).then(resolve); }));
  const cache = workspace();
  const request = cache.get("asset", "generation");
  const rejected = expect(request).rejects.toThrow();
  const disposed = cache.dispose();
  finish!();
  await Promise.all([disposed, rejected]);
  expect(stored.data.size).toBe(0);

  stored.open.mockImplementation(async name => {
    const opened = await normalOpen(name);
    opened.put.mockImplementation((key, response) => new Promise(resolve => { finish = () => { void (stored.data.get(name)?.set(key.url, response)); resolve(); }; }));
    return opened;
  });
  vi.stubGlobal("fetch", vi.fn(async () => new Response("synthetic-jpeg")));
  finish = undefined;
  const writing = workspace();
  const write = writing.get("asset", "generation");
  const writeRejected = expect(write).rejects.toThrow();
  await vi.waitFor(() => expect(finish).toBeTypeOf("function"));
  window.dispatchEvent(new PageTransitionEvent("pagehide", { persisted: true }));
  expect(stored.data.size).toBe(1);
  window.dispatchEvent(new PageTransitionEvent("pagehide", { persisted: false }));
  await vi.waitFor(() => expect(stored.data.size).toBe(0));
  finish!();
  await writeRejected;
  expect(stored.data.size).toBe(0);
});

it("sends at most six network reads and times each read from when it is sent", async () => {
  storage();
  const finishers: ((response: Response) => void)[] = [];
  const fetcher = vi.fn((_url: string | URL | Request, _init: RequestInit) => new Promise<Response>(resolve => finishers.push(resolve)));
  vi.stubGlobal("fetch", fetcher);
  const timeout = vi.spyOn(AbortSignal, "timeout");
  const cache = workspace();
  const reads = Array.from({ length: 6 }, (_, index) => cache.get("asset", `generation-${index}`));
  const caller = new AbortController();
  const canceled = cache.get("asset", "canceled", caller.signal);
  const queued = cache.get("asset", "queued");
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(6));
  await new Promise(resolve => setTimeout(resolve, 0));
  expect(fetcher).toHaveBeenCalledTimes(6);
  expect(timeout).toHaveBeenCalledTimes(6);
  caller.abort();
  await expect(canceled).rejects.toThrow();
  finishers[0](new Response("first"));
  await reads[0];
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(7));
  expect(String(fetcher.mock.calls[6][0])).toContain("queued");
  expect(timeout).toHaveBeenCalledTimes(7);
  for (const finish of finishers.slice(1)) finish(new Response("later"));
  expect(await (await queued).text()).toBe("later");
  await Promise.all(reads);
});

it("reloads cached bytes from the network on request", async () => {
  storage();
  vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(new Response("undecodable")).mockResolvedValueOnce(new Response("repaired")));
  const cache = workspace();
  await cache.get("asset", "generation");
  expect(await (await cache.get("asset", "generation", undefined, true)).text()).toBe("repaired");
  expect(await (await cache.get("asset", "generation")).text()).toBe("repaired");
});

it("removes caches left by closed pages and releases its lock after disposal", async () => {
  const stored = storage();
  for (const name of ["docbank-photo-previews-closed", "docbank-photo-previews-open", "unrelated"]) stored.data.set(name, new Map());
  const request = vi.fn((_name: string, callback: () => Promise<void>) => callback());
  const query = vi.fn(async () => ({ held: [{ name: "docbank-photo-previews-open" }], pending: [] }));
  Object.defineProperty(navigator, "locks", { configurable: true, value: { request, query } });
  try {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const cache = workspace();
    await vi.waitFor(() => expect([...stored.data.keys()]).toEqual(["docbank-photo-previews-open", "unrelated"]));
    let released = false;
    void request.mock.results[0].value.then(() => released = true);
    stored.remove.mockRejectedValueOnce(new Error("Storage broken"));
    await cache.dispose();
    await vi.waitFor(() => expect(released).toBe(true));
    expect(warn).toHaveBeenCalledWith("Could not delete photo preview cache", expect.any(Error));
  } finally {
    Reflect.deleteProperty(navigator, "locks");
  }
});
it("evicts only hidden assets and retains unrelated previews and requests", async () => {
  const stored = storage();
  let finish!: (response: Response) => void;
  let pendingSignal: AbortSignal | undefined;
  const fetcher = vi.fn(async (url: string, options?: RequestInit) => {
    if (url.includes("/pending/")) { pendingSignal = options?.signal ?? undefined; return new Promise<Response>(resolve => finish = resolve); }
    return new Response("synthetic-jpeg");
  });
  vi.stubGlobal("fetch", fetcher);
  const cache = workspace();
  await cache.get("hidden", "generation");
  await cache.get("visible", "generation");
  const pending = cache.get("pending", "generation");
  await vi.waitFor(() => expect(pendingSignal).toBeDefined());
  await cache.evict(["hidden"]);
  expect(pendingSignal?.aborted).toBe(false);
  const entries = [...stored.data.values()][0];
  expect([...entries.keys()].some(key => key.includes("/hidden/"))).toBe(false);
  expect([...entries.keys()].some(key => key.includes("/visible/"))).toBe(true);
  const before = fetcher.mock.calls.length;
  await cache.get("visible", "generation");
  expect(fetcher).toHaveBeenCalledTimes(before);
  finish(new Response("pending-preview"));
  expect(await (await pending).text()).toBe("pending-preview");
});
