import { lockPhotoHidden } from "./generated/docbank.js";
import { APIError } from "./api-transport.js";

export class HiddenAccess {
  lockError = $state("");
  private queue = Promise.resolve();
  private pending = 0;

  constructor(
    readonly session: string,
    readonly onauthfailure: (cause: unknown) => void,
  ) {}

  run<T>(request: () => Promise<T>): Promise<T> {
    this.pending++;
    return this.enqueue(async () => {
      try {
        return await request();
      } finally {
        this.pending--;
      }
    });
  }
  private enqueue<T>(request: () => Promise<T>): Promise<T> {
    const result = this.queue.then(request);
    this.queue = result.then(
      () => {},
      () => {},
    );
    return result;
  }
  settled() {
    return this.queue;
  }
  private requestLock() {
    return lockPhotoHidden(
      {},
      { session: this.session, signal: AbortSignal.timeout(30_000), keepalive: true },
    );
  }
  pagehide() {
    // The page may disappear before a pending unlock releases the queue.
    if (this.pending) void this.requestLock().catch(() => {});
  }
  lock() {
    return this.enqueue(async () => {
      try {
        await this.requestLock();
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
