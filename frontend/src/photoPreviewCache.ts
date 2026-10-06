import { getReadPhotoPreviewUrl, readPhotoPreview } from "./generated/docbank.js";
import { APIError } from "./api-transport.js";

export class PhotoPreviewCache {
  private controller = new AbortController();
  private name = `docbank-photo-previews-${crypto.randomUUID()}`;
  private cache?: Promise<Cache>;
  private disposed?: Promise<void>;
  private pagehide = (event: PageTransitionEvent) => { if (!event.persisted) void this.dispose(); };

  constructor(private session: string, private onauthfailure: (cause: unknown) => void) {
    window.addEventListener("pagehide", this.pagehide);
  }

  get(assetID: string, generationID: string, caller?: AbortSignal): Promise<Blob> {
    const signal = AbortSignal.any([this.controller.signal, AbortSignal.timeout(30_000), ...(caller ? [caller] : [])]);
    return this.read(assetID, generationID, getReadPhotoPreviewUrl(assetID, generationID), signal).catch(cause => {
      if (!signal.aborted && cause instanceof APIError && cause.status === 401) this.onauthfailure(cause);
      throw cause;
    });
  }

  private async read(assetID: string, generationID: string, path: string, signal: AbortSignal): Promise<Blob> {
    signal.throwIfAborted();
    let onabort: () => void;
    const aborted = new Promise<never>((_, reject) => {
      onabort = () => reject(signal.reason);
      signal.addEventListener("abort", onabort, { once: true });
    });
    const read = async () => {
      let cache: Cache | undefined;
      const key = new Request(new URL(path, location.origin), { credentials: "omit" });
      let cached: Response | undefined;
      try {
        this.cache ??= caches.open(this.name);
        cache = await this.cache;
        signal.throwIfAborted();
        cached = await cache.match(key);
      } catch {
        signal.throwIfAborted();
        this.cache = undefined;
        cache = undefined;
      }
      signal.throwIfAborted();
      if (cached) return cached.blob();
      const response = await readPhotoPreview(assetID, generationID, undefined, { session: this.session, signal });
      signal.throwIfAborted();
      const bytes = await response.arrayBuffer();
      signal.throwIfAborted();
      try {
        // The network response varies by credentials; retained keys contain no credentials.
        await cache?.put(key, new Response(bytes, { headers: { "Content-Type": "image/jpeg" } }));
      } catch { signal.throwIfAborted(); }
      signal.throwIfAborted();
      return new Blob([bytes], { type: "image/jpeg" });
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
