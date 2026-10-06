import { Blob } from "node:buffer";
import { afterEach, expect, it, vi } from "vitest";
import { PhotoPreviewCache } from "./photoPreviewCache.js";

function storage() {
  vi.stubGlobal("Blob", Blob);
  const data = new Map<string, Map<string, Response>>();
  const open = vi.fn(async (name: string) => {
    const entries = data.get(name) ?? new Map<string, Response>();
    data.set(name, entries);
    return {
      match: vi.fn(async (key: Request) => entries.get(key.url)?.clone()),
      put: vi.fn(async (key: Request, response: Response) => { entries.set(key.url, response.clone()); }),
    };
  });
  const remove = vi.fn(async (name: string) => data.delete(name));
  vi.stubGlobal("caches", { open, delete: remove });
  return { data, open, remove };
}
const instances: PhotoPreviewCache[] = [];
function workspace() { const cache = new PhotoPreviewCache("scoped", vi.fn()); instances.push(cache); return cache; }
afterEach(async () => { await Promise.all(instances.splice(0).map(cache => cache.dispose())); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

it("shares pending reads, retains bytes without fetch, and releases settled entries", async () => {
  const stored = storage();
  const fetcher = vi.fn(async (_url: string | URL | Request, _init: RequestInit) => new Response("synthetic-jpeg"));
  vi.stubGlobal("fetch", fetcher);
  const cache = workspace();
  const first = cache.get("asset", "generation");
  const caller = new AbortController();
  const canceled = expect(cache.get("asset", "generation", caller.signal)).rejects.toThrow();
  caller.abort();
  await canceled;
  expect(await (await first).text()).toBe("synthetic-jpeg");
  const opened = await stored.open.mock.results[0].value;
  expect(opened.match).toHaveBeenCalledTimes(1);
  expect(opened.put).toHaveBeenCalledTimes(1);
  expect(await (await cache.get("asset", "generation")).text()).toBe("synthetic-jpeg");
  expect(fetcher).toHaveBeenCalledTimes(1);
  expect((cache as unknown as { entries: Map<string, unknown> }).entries.size).toBe(0);
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
  const left = new AbortController(); const right = new AbortController();
  const abandoned = second.get("uncached", "generation", left.signal);
  const shared = second.get("uncached", "generation", right.signal);
  const rejected = Promise.all([expect(abandoned).rejects.toThrow(), expect(shared).rejects.toThrow()]);
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(4));
  left.abort();
  expect(fetcher.mock.calls[3][1].signal!.aborted).toBe(false);
  right.abort();
  expect(fetcher.mock.calls[3][1].signal!.aborted).toBe(true);
  const fresh = second.get("uncached", "generation");
  await rejected;
  finishOld(new Response("canceled-jpeg"));
  await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(5));
  const joined = second.get("uncached", "generation");
  finishFresh(new Response("fresh-jpeg"));
  expect(await (await fresh).text()).toBe("fresh-jpeg");
  expect(await (await joined).text()).toBe("fresh-jpeg");
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

it("reports storage failure and retries rather than retaining an in-memory substitute", async () => {
  const stored = storage();
  const normalOpen = stored.open.getMockImplementation()!;
  stored.open.mockRejectedValueOnce(new Error("Storage disabled"));
  const fetcher = vi.fn(async () => new Response("synthetic-jpeg"));
  vi.stubGlobal("fetch", fetcher);
  const cache = workspace();
  await expect(cache.get("asset", "generation")).rejects.toThrow("Preview storage is unavailable");
  expect(fetcher).not.toHaveBeenCalled();
  stored.open.mockImplementation(normalOpen);
  await cache.get("asset", "generation");
  expect(fetcher).toHaveBeenCalledTimes(1);
  const opened = await stored.open.mock.results[1].value;
  opened.put.mockRejectedValueOnce(new Error("Quota exceeded"));
  await expect(cache.get("asset", "other-generation")).rejects.toThrow("Preview storage is unavailable");
  await cache.get("asset", "other-generation");
  expect(fetcher).toHaveBeenCalledTimes(3);
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
