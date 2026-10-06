import { getReadPhotoPreviewUrl, readPhotoPreview } from "./generated/docbank.js";
import { APIError } from "./api-transport.js";

export class PhotoPreviewCache {
  private entries = new Map<string, Promise<Blob>>();
  private controller = new AbortController();
  private name = `docbank-photo-previews-${crypto.randomUUID()}`;
  private cache?: Promise<Cache>;
  private disposed?: Promise<void>;
  private pagehide = () => { void this.dispose(); };

  constructor(private session: string, private onauthfailure: (cause: unknown) => void) {
    window.addEventListener("pagehide", this.pagehide);
  }

  get(assetID: string, generationID: string): Promise<Blob> {
    const path = getReadPhotoPreviewUrl(assetID, generationID);
    const pending = this.entries.get(path);
    if (pending) return pending;
    const signal = AbortSignal.any([this.controller.signal, AbortSignal.timeout(30_000)]);
    const request = this.read(assetID, generationID, path, signal).catch(cause => {
      if (!this.controller.signal.aborted && cause instanceof APIError && cause.status === 401) this.onauthfailure(cause);
      throw cause;
    }).finally(() => this.entries.delete(path));
    this.entries.set(path, request);
    return request;
  }

  private async read(assetID: string, generationID: string, path: string, signal: AbortSignal): Promise<Blob> {
    signal.throwIfAborted();
    let onabort: () => void;
    const aborted = new Promise<never>((_, reject) => {
      onabort = () => reject(signal.reason);
      signal.addEventListener("abort", onabort, { once: true });
    });
    const read = async () => {
      let cache: Cache;
      const key = new Request(new URL(path, location.origin), { credentials: "omit" });
      let cached: Response | undefined;
      try {
        this.cache ??= caches.open(this.name);
        cache = await this.cache;
        signal.throwIfAborted();
        cached = await cache.match(key);
      } catch (cause) {
        if (signal.aborted) throw signal.reason;
        this.cache = undefined;
        throw new Error("Preview storage is unavailable. Retry preview.", { cause });
      }
      signal.throwIfAborted();
      if (cached) return cached.blob();
      const response = await readPhotoPreview(assetID, generationID, undefined, { session: this.session, signal });
      signal.throwIfAborted();
      try {
        // The network response varies by credentials; retained keys contain no credentials.
        await cache.put(key, new Response(await response.arrayBuffer(), { headers: { "Content-Type": "image/jpeg" } }));
        signal.throwIfAborted();
        const stored = await cache.match(key);
        if (!stored) throw new Error("Preview was not retained");
        return stored.blob();
      } catch (cause) {
        if (signal.aborted) throw signal.reason;
        throw new Error("Preview storage is unavailable. Retry preview.", { cause });
      }
    };
    return Promise.race([read(), aborted]).finally(() => signal.removeEventListener("abort", onabort));
  }

  dispose(): Promise<void> {
    if (this.disposed) return this.disposed;
    this.controller.abort();
    window.removeEventListener("pagehide", this.pagehide);
    const opening = this.cache;
    this.disposed = Promise.resolve(opening).catch(() => undefined).then(async () => {
      if (typeof caches !== "undefined") await caches.delete(this.name);
    }).catch(() => undefined);
    return this.disposed;
  }
}
