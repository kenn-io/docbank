import { lockPhotoHidden } from "./generated/docbank.js";
import { APIError } from "./api-transport.js";

export class HiddenAccess {
  lockError = $state("");
  private queue = Promise.resolve();

  constructor(readonly session: string, readonly onauthfailure: (cause: unknown) => void) {}

  run<T>(request: () => Promise<T>): Promise<T> {
    const result = this.queue.then(request);
    this.queue = result.then(() => {}, () => {});
    return result;
  }
  settled() { return this.queue; }
  lock({ keepalive = false } = {}) {
    return this.run(async () => {
      try {
        await lockPhotoHidden({}, { session: this.session, signal: AbortSignal.timeout(30_000), keepalive });
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
