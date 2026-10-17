import { lockPhotoHidden, type PhotoHiddenState } from "./generated/docbank.js";
import { APIError } from "./api-transport.js";

export class HiddenAccess {
  lockError = $state("");
  private queue = Promise.resolve();
  private pending = 0;

  constructor(
    readonly session: string,
    readonly onauthfailure: (cause: unknown) => void,
  ) {}

  scope = $state(0);
  deadline = $state(0);
  private observedAt = -1;
  private expiry = "";
  private timer: ReturnType<typeof setTimeout> | undefined;

  acceptState(state: PhotoHiddenState, requestedAt: number) {
    if (requestedAt < this.observedAt) return;
    this.observedAt = requestedAt;
    const deadline = state.expires_in ? requestedAt + state.expires_in * 1000 : 0;
    const next = deadline > Date.now() ? deadline : 0;
    const expiry = next ? (state.expires_at ?? "") : "";
    if (!!next !== !!this.deadline || expiry !== this.expiry) this.scope++;
    this.expiry = expiry;
    this.deadline = next;
    clearTimeout(this.timer);
    if (next) this.timer = setTimeout(() => this.revoke(), next - Date.now());
  }
  private revoke() {
    this.observedAt = Date.now();
    if (this.deadline) this.scope++;
    this.deadline = 0;
    this.expiry = "";
    clearTimeout(this.timer);
  }
  dispose() {
    clearTimeout(this.timer);
  }

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
        this.revoke();
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
