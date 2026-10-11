import { getReadPhotoPreviewUrl, readPhotoPreview } from "./generated/docbank.js";
import { APIError } from "./api-transport.js";

const cachePrefix = "docbank-photo-previews-";
const maxFetches = 6;

export class PhotoPreviewCache {
  private controller = new AbortController();
  private name = `${cachePrefix}${crypto.randomUUID()}`;
  private cache?: Promise<Cache>;
  private disposed?: Promise<void>;
  private releaseLock?: () => void;
  private fetching = 0;
  private waiting: (() => void)[] = [];
  private pagehide = (event: PageTransitionEvent) => { if (!event.persisted) void this.dispose(); };

  constructor(private session: string, private onauthfailure: (cause: unknown) => void, private hidden = false) {
    window.addEventListener("pagehide", this.pagehide);
    void this.sweep().catch(cause => console.warn("Could not remove abandoned photo preview caches", cause));
  }

  // Each live cache holds a Web Lock with its name, so unlocked caches belong to closed pages.
  private async sweep() {
    if (typeof navigator.locks === "undefined" || typeof caches === "undefined") return;
    void navigator.locks.request(this.name, () => this.disposed ?? new Promise<void>(resolve => this.releaseLock = resolve));
    const { held = [], pending = [] } = await navigator.locks.query();
    const live = new Set([...held, ...pending].map(lock => lock.name));
    for (const name of await caches.keys()) {
      if (name.startsWith(cachePrefix) && name !== this.name && !live.has(name)) await caches.delete(name);
    }
  }

  get(assetID: string, generationID: string, caller?: AbortSignal, reload = false): Promise<Blob> {
    const signal = AbortSignal.any([this.controller.signal, ...(caller ? [caller] : [])]);
    return this.read(assetID, generationID, getReadPhotoPreviewUrl(assetID, generationID), signal, reload).catch(cause => {
      if (!signal.aborted && cause instanceof APIError && (cause.status === 401 || this.hidden && cause.status === 403)) this.onauthfailure(cause);
      throw cause;
    });
  }

  private async read(assetID: string, generationID: string, path: string, signal: AbortSignal, reload: boolean): Promise<Blob> {
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
        if (!reload) cached = await cache.match(key);
      } catch {
        signal.throwIfAborted();
        this.cache = undefined;
        cache = undefined;
      }
      signal.throwIfAborted();
      if (cached) return cached.blob();
      await this.acquire(signal);
      let bytes: ArrayBuffer;
      let noStore = false;
      try {
        const fetchSignal = AbortSignal.any([signal, AbortSignal.timeout(30_000)]);
        const response = await readPhotoPreview(assetID, generationID, undefined, { session: this.session, signal: fetchSignal });
        noStore = /(?:^|,)\s*no-store\s*(?:,|$)/i.test(response.headers.get("Cache-Control") ?? "");
        bytes = await response.arrayBuffer();
      } finally { this.release(); }
      signal.throwIfAborted();
      try {
        // The network response varies by credentials; retained keys contain no credentials.
        if (!noStore) {
          await cache?.put(key, new Response(bytes, { headers: { "Content-Type": "image/jpeg" } }));
        }
      } catch { signal.throwIfAborted(); }
      signal.throwIfAborted();
      return new Blob([bytes], { type: "image/jpeg" });
    };
    return Promise.race([read(), aborted]).finally(() => signal.removeEventListener("abort", onabort));
  }

  private async acquire(signal: AbortSignal) {
    signal.throwIfAborted();
    if (this.fetching < maxFetches) { this.fetching++; return; }
    await new Promise<void>((resolve, reject) => {
      const grant = () => { signal.removeEventListener("abort", cancel); resolve(); };
      const cancel = () => { this.waiting.splice(this.waiting.indexOf(grant), 1); reject(signal.reason); };
      this.waiting.push(grant);
      signal.addEventListener("abort", cancel, { once: true });
    });
  }

  private release() {
    const next = this.waiting.shift();
    if (next) next();
    else this.fetching--;
  }

  dispose(): Promise<void> {
    if (this.disposed) return this.disposed;
    this.controller.abort();
    window.removeEventListener("pagehide", this.pagehide);
    const opening = this.cache;
    this.disposed = Promise.resolve(opening).catch(() => undefined).then(async () => {
      if (typeof caches !== "undefined") await caches.delete(this.name);
    }).catch(cause => console.warn("Could not delete photo preview cache", cause)).finally(() => this.releaseLock?.());
    return this.disposed;
  }
}
