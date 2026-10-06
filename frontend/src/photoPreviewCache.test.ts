import { afterEach, expect, it, vi } from "vitest";
import { PhotoPreviewCache } from "./photoPreviewCache.js";

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); });

function urls() {
  Object.defineProperty(URL, "createObjectURL", { configurable: true, value: vi.fn().mockReturnValue("blob:synthetic-preview") });
  Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: vi.fn() });
}

it("shares pending work and retains seen previews through unmounts until disposal", async () => {
  urls();
  const fetcher = vi.fn().mockImplementation(() => Promise.resolve(new Response("synthetic-jpeg", { headers: { "Content-Type": "image/jpeg" } })));
  vi.stubGlobal("fetch", fetcher);
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  const first = cache.get("asset", "generation");
  expect(cache.get("asset", "generation")).toBe(first);
  expect(await first).toBe("blob:synthetic-preview");
  expect(await cache.get("asset", "generation")).toBe("blob:synthetic-preview");
  expect(fetcher).toHaveBeenCalledTimes(1);
  expect(fetcher.mock.calls[0][1].headers.get("X-Docbank-Web-Session")).toBe("scoped");
  await cache.get("asset", "new-generation");
  expect(fetcher).toHaveBeenCalledTimes(2);
  cache.dispose();
  expect(URL.revokeObjectURL).toHaveBeenCalledWith("blob:synthetic-preview");
});

it("retries failures and reports expired authentication", async () => {
  urls();
  const auth = vi.fn();
  vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(new Response("{}", { status: 401 })).mockResolvedValueOnce(new Response("synthetic-jpeg")));
  const cache = new PhotoPreviewCache("scoped", auth);
  await expect(cache.get("asset", "generation")).rejects.toThrow("HTTP 401");
  expect(auth).toHaveBeenCalledTimes(1);
  expect(await cache.get("asset", "generation")).toBe("blob:synthetic-preview");
  cache.dispose();
});

it("aborts requests on disposal and prevents late blobs from creating URLs", async () => {
  urls();
  let finish!: (value: Response) => void;
  const fetcher = vi.fn(() => new Promise<Response>(resolve => finish = resolve));
  vi.stubGlobal("fetch", fetcher);
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  const request = cache.get("asset", "generation");
  const rejected = expect(request).rejects.toThrow("Workspace closed");
  cache.dispose();
  expect((fetcher.mock.calls[0] as unknown as [string, RequestInit])[1].signal?.aborted).toBe(true);
  finish(new Response("late-jpeg"));
  await rejected;
  expect(URL.createObjectURL).not.toHaveBeenCalled();
});
