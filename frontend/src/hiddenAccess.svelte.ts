import { lockPhotoHidden, setupPhotoHidden } from "./generated/docbank.js";
import { APIError } from "./api-transport.js";

export function passcodeValidation(value: string, empty: string) {
  return !value
    ? empty
    : new TextEncoder().encode(value).length > 1024
      ? "Use 1 to 1,024 bytes."
      : "";
}
type SetupResult = "ready" | "saved" | "exists" | "cancelled";

export class HiddenAccess {
  configured = $state<boolean>();
  lockError = $state("");
  setupPrompt = $state<{ done: (result: SetupResult) => void; signal: AbortSignal }>();
  private setupAbort?: AbortController;
  private setupSaving = false;
  private queue = Promise.resolve();
  private pending = 0;

  constructor(
    readonly session: string,
    readonly onauthfailure: (cause: unknown) => void,
  ) {}

  async ensureSetup(lifetime: AbortSignal): Promise<SetupResult> {
    if (lifetime.aborted) return "cancelled";
    if (this.configured !== false) return "ready";
    if (this.setupAbort) return "cancelled";
    const controller = (this.setupAbort = new AbortController());
    const signal = AbortSignal.any([lifetime, controller.signal]);
    try {
      return await new Promise<SetupResult>((resolve) => {
        const done = (result: SetupResult) => {
          if (this.setupPrompt?.done !== done) return;
          signal.removeEventListener("abort", abort);
          this.setupPrompt = undefined;
          resolve(result);
        };
        const abort = () => {
          if (!this.setupSaving) done("cancelled");
        };
        this.setupPrompt = { done, signal };
        signal.addEventListener("abort", abort, { once: true });
      });
    } finally {
      this.setupAbort = undefined;
    }
  }
  cancelSetup() {
    this.setupAbort?.abort();
  }
  setupPasscode(passcode: string) {
    return setupPhotoHidden(
      { passcode },
      { session: this.session, signal: AbortSignal.timeout(30_000) },
    );
  }
  async createPasscode(passcode: string) {
    this.setupSaving = true;
    try {
      const result = await this.run(() => this.setupPasscode(passcode));
      this.configured = true;
      return result;
    } catch (cause) {
      if (cause instanceof APIError && cause.status === 401) this.onauthfailure(cause);
      throw cause;
    } finally {
      this.setupSaving = false;
    }
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
