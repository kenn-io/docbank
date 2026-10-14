import { lockPhotoHidden } from "./generated/docbank.js";
import { APIError } from "./api-transport.js";

export class HiddenAccess {
  lockError = $state("");
  private queue = Promise.resolve();
  private pending = 0;

  constructor(readonly session: string, readonly onauthfailure: (cause: unknown) => void) {}

  run<T>(request: () => Promise<T>): Promise<T> {
    this.pending++;
    return this.enqueue(async () => { try { return await request(); } finally { this.pending--; } });
  }
  private enqueue<T>(request: () => Promise<T>): Promise<T> {
    const result = this.queue.then(request);
    this.queue = result.then(() => {}, () => {});
    return result;
  }
  settled() { return this.queue; }
  private requestLock(keepalive: boolean) {
    return lockPhotoHidden({}, { session: this.session, signal: AbortSignal.timeout(30_000), keepalive });
  }
  pagehide() {
    // The page may disappear before a pending unlock releases the queue.
    if (this.pending) void this.requestLock(true).catch(() => {});
  }
  lock({ keepalive = false } = {}) {
    return this.enqueue(async () => {
      try {
        await this.requestLock(keepalive);
        this.lockError = "";
        return true;
      } catch (cause) {
        this.lockError = cause instanceof Error ? cause.message : String(cause);
        if (cause instanceof APIError && cause.status === 401) this.onauthfailure(cause);
        return false;
      }
    });
  }
}
