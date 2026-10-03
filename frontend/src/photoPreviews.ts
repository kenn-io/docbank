import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { sessionResponse } from "./api-transport";
import { digestHeaderMatches, readExactBody } from "./download";
import type { PhotoPreviewSlot } from "./generated/docbank";

export interface PhotoPreviewLease {
  url: string;
  release(): void;
}

interface Waiter {
  resolve(lease: PhotoPreviewLease): void;
  reject(reason: unknown): void;
  removeAbort(): void;
}

interface Entry {
  key: string;
  preview: PhotoPreviewSlot;
  controller: AbortController;
  waiters: Set<Waiter>;
  url?: string;
  references: number;
}

const maxPreviewBytes = 32 * 1024 * 1024;
const generationPath = /^\/api\/v1\/photos\/assets\/[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\/previews\/([0-9a-f]{64})$/;

// One instance owns one session's in-flight reads and leased object URLs.
export class PhotoPreviewCache {
  private entries = new Map<string, Entry>();
  private idle = new Map<string, Entry>();
  private queue: Entry[] = [];
  private active = 0;
  private disposed = false;

  constructor(private readonly session: string) {}

  acquire(preview: PhotoPreviewSlot, signal: AbortSignal): Promise<PhotoPreviewLease> {
    try {
      signal.throwIfAborted();
      if (this.disposed) throw new Error("Photo preview cache is disposed.");
      const key = this.validate(preview);
      let entry = this.entries.get(key);
      if (entry && !this.sameIdentity(entry.preview, preview)) throw new Error("Photo preview metadata changed for an immutable URL.");
      if (entry?.url) return Promise.resolve(this.lease(entry));
      if (!entry) {
        entry = { key, preview: { ...preview }, controller: new AbortController(), waiters: new Set(), references: 0 };
        this.entries.set(key, entry);
        this.queue.push(entry);
      }
      const owned = entry;
      const promise = new Promise<PhotoPreviewLease>((resolve, reject) => {
        const waiter: Waiter = { resolve, reject, removeAbort: () => signal.removeEventListener("abort", abort) };
        const abort = () => {
          owned.waiters.delete(waiter);
          waiter.removeAbort();
          reject(signal.reason);
          if (!owned.waiters.size && !owned.url) {
            owned.controller.abort();
            if (this.entries.get(key) === owned) this.entries.delete(key);
          }
        };
        signal.addEventListener("abort", abort, { once: true });
        owned.waiters.add(waiter);
      });
      this.pump();
      return promise;
    } catch (reason) {
      return Promise.reject(reason);
    }
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    for (const entry of this.entries.values()) {
      entry.controller.abort();
      this.rejectWaiters(entry, new DOMException("Photo preview cache disposed.", "AbortError"));
      this.revoke(entry);
    }
    this.entries.clear();
    this.idle.clear();
    this.queue = [];
  }

  private validate(preview: PhotoPreviewSlot): string {
    if (preview.state !== "ready" || !preview.url || !preview.generation_id ||
        !preview.sha256 || !/^[0-9a-f]{64}$/.test(preview.sha256) ||
        !Number.isSafeInteger(preview.size) || (preview.size ?? 0) <= 0 || (preview.size ?? 0) > maxPreviewBytes ||
        preview.media_type !== "image/jpeg" || !Number.isSafeInteger(preview.width) || (preview.width ?? 0) <= 0 ||
        !Number.isSafeInteger(preview.height) || (preview.height ?? 0) <= 0) throw new Error("Photo preview lacks ready generation metadata.");
    const url = new URL(preview.url, location.origin);
    if (url.origin !== location.origin || url.username || url.password || url.search || url.hash || generationPath.exec(url.pathname)?.[1] !== preview.generation_id) throw new Error("Photo preview URL is outside its generation route.");
    return url.href;
  }

  private sameIdentity(left: PhotoPreviewSlot, right: PhotoPreviewSlot): boolean {
    return left.generation_id === right.generation_id && left.sha256 === right.sha256 && left.size === right.size && left.media_type === right.media_type && left.width === right.width && left.height === right.height;
  }

  private lease(entry: Entry): PhotoPreviewLease {
    this.idle.delete(entry.key);
    entry.references++;
    let released = false;
    return { url: entry.url!, release: () => {
      if (released) return;
      released = true;
      entry.references--;
      if (this.disposed || !entry.url || entry.references > 0) return;
      this.idle.set(entry.key, entry);
      while (this.idle.size > 128) {
        const oldest = this.idle.values().next().value!;
        this.idle.delete(oldest.key);
        this.entries.delete(oldest.key);
        this.revoke(oldest);
      }
    } };
  }

  private pump(): void {
    while (!this.disposed && this.active < 4 && this.queue.length) {
      const entry = this.queue.shift()!;
      if (entry.controller.signal.aborted || !entry.waiters.size) continue;
      this.active++;
      void this.load(entry).finally(() => { this.active--; this.pump(); });
    }
  }

  private async load(entry: Entry): Promise<void> {
    const signal = entry.controller.signal;
    try {
      const response = await sessionResponse<Response>(entry.key, { session: this.session, signal, headers: { Accept: "image/jpeg" } });
      signal.throwIfAborted();
      const preview = entry.preview;
      if (response.headers.get("ETag") !== `"${preview.generation_id}"` ||
          response.headers.get("Content-Type") !== preview.media_type ||
          response.headers.get("Content-Length") !== String(preview.size)) throw new Error("Photo preview response disagrees with its generation.");
      const bytes = await readExactBody(response, preview.size!, signal);
      const hash = bytesToHex(sha256(bytes));
      if (hash !== preview.sha256 || !digestHeaderMatches(response.headers, hash)) throw new Error("Photo preview digest disagrees with its generation.");
      signal.throwIfAborted();
      if (this.disposed || !entry.waiters.size) throw new DOMException("Photo preview no longer requested.", "AbortError");
      entry.url = URL.createObjectURL(new Blob([bytes], { type: preview.media_type }));
      for (const waiter of entry.waiters) { waiter.removeAbort(); waiter.resolve(this.lease(entry)); }
      entry.waiters.clear();
    } catch (reason) {
      this.rejectWaiters(entry, reason);
      this.revoke(entry);
      if (this.entries.get(entry.key) === entry) this.entries.delete(entry.key);
    }
  }

  private rejectWaiters(entry: Entry, reason: unknown): void {
    for (const waiter of entry.waiters) { waiter.removeAbort(); waiter.reject(reason); }
    entry.waiters.clear();
  }

  private revoke(entry: Entry): void {
    if (entry.url) { URL.revokeObjectURL(entry.url); entry.url = undefined; }
  }
}
