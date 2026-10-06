import { getReadPhotoPreviewUrl, readPhotoPreview } from "./generated/docbank.js";
import { APIError } from "./api-transport.js";

type PendingPreview = { promise: Promise<Blob>; controller: AbortController; callers: number };

export class PhotoPreviewCache {
  private entries = new Map<string, PendingPreview>();
  private controller = new AbortController();
  private name = `docbank-photo-previews-${crypto.randomUUID()}`;
  private cache?: Promise<Cache>;
  private disposed?: Promise<void>;
  private pagehide = (event: PageTransitionEvent) => { if (!event.persisted) void this.dispose(); };

  constructor(private session: string, private onauthfailure: (cause: unknown) => void) {
    window.addEventListener("pagehide", this.pagehide);
  }

  get(assetID: string, generationID: string, caller?: AbortSignal): Promise<Blob> {
    if (caller?.aborted) return Promise.reject(caller.reason);
    const path = getReadPhotoPreviewUrl(assetID, generationID);
    let entry = this.entries.get(path);
    if (!entry) {
      const controller = new AbortController();
      const signal = AbortSignal.any([controller.signal, this.controller.signal, AbortSignal.timeout(30_000)]);
      entry = { controller, callers: 0, promise: this.read(assetID, generationID, path, signal).catch(cause => {
        if (!signal.aborted && cause instanceof APIError && cause.status === 401) this.onauthfailure(cause);
        throw cause;
      }) };
      const pending = entry;
      entry.promise = entry.promise.finally(() => { if (this.entries.get(path) === pending) this.entries.delete(path); });
      this.entries.set(path, entry);
    }
    const pending = entry;
    pending.callers++;
    return new Promise<Blob>((resolve, reject) => {
      let finished = false;
      const finish = () => {
        if (finished) return false;
        finished = true;
        caller?.removeEventListener("abort", abort);
        if (--pending.callers === 0 && this.entries.get(path) === pending) {
          this.entries.delete(path);
          pending.controller.abort();
        }
        return true;
      };
      const abort = () => { if (finish()) reject(caller!.reason); };
      caller?.addEventListener("abort", abort, { once: true });
      pending.promise.then(blob => { if (finish()) resolve(blob); }, cause => { if (finish()) reject(cause); });
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
        const bytes = await response.arrayBuffer();
        signal.throwIfAborted();
        await cache.put(key, new Response(bytes, { headers: { "Content-Type": "image/jpeg" } }));
        signal.throwIfAborted();
        return new Blob([bytes], { type: "image/jpeg" });
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
