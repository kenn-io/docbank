import { APIError, sessionResponse } from "./api-transport.js";
import type { BrowserSession } from "./browser-session.js";
import { UploadProofError, VerifiedUploadChannel } from "./upload.js";

export type UploadConnectionState =
  | "connecting"
  | "connected"
  | "reconnecting"
  | "failed"
  | "manual"
  | "fatal";

export class UploadReconnect {
  private channel: VerifiedUploadChannel | undefined;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private abort = new AbortController();
  private disposed = false;
  private generation = 0;

  constructor(
    private readonly session: BrowserSession,
    private readonly update: (
      state: UploadConnectionState,
      channel?: VerifiedUploadChannel,
      error?: unknown,
    ) => void,
  ) {}

  start(): void {
    void this.connect(false);
  }

  retry(): void {
    if (this.disposed) return;
    this.update("manual");
    void this.connect(true);
  }

  close(): void {
    this.disposed = true;
    this.generation++;
    this.abort.abort();
    clearTimeout(this.timer);
    this.channel?.close();
  }

  private async connect(reconnecting: boolean): Promise<void> {
    const generation = ++this.generation;
    const delays = reconnecting ? [250, 500, 1000, 2000] : [0];
    for (const delay of delays) {
      if (delay)
        await new Promise<void>((resolve) => {
          const finish = (): void => {
            clearTimeout(this.timer);
            this.abort.signal.removeEventListener("abort", finish);
            resolve();
          };
          this.timer = setTimeout(finish, delay);
          this.abort.signal.addEventListener("abort", finish, { once: true });
        });
      if (this.disposed || generation !== this.generation) return;
      let established = false;
      const channel = new VerifiedUploadChannel(this.session, undefined, () => {
        if (!established || this.channel !== channel || this.disposed) return;
        this.update("reconnecting");
        void this.connect(true);
      });
      this.channel = channel;
      try {
        await channel.connect();
        if (this.disposed || generation !== this.generation) {
          channel.close();
          return;
        }
        established = true;
        this.update("connected", channel);
        return;
      } catch (cause) {
        channel.close();
        if (this.disposed || generation !== this.generation) return;
        if (cause instanceof UploadProofError) {
          this.update("fatal", undefined, cause);
          return;
        }
        try {
          await sessionResponse("/api/v1/processing/local/status", {
            session: this.session.token,
            signal: AbortSignal.any([this.abort.signal, AbortSignal.timeout(5000)]),
          });
        } catch (sessionError) {
          if (this.disposed || generation !== this.generation) return;
          if (
            sessionError instanceof APIError &&
            (sessionError.status === 401 || sessionError.status === 403)
          ) {
            this.update("fatal", undefined, sessionError);
            return;
          }
        }
        if (!reconnecting) {
          this.update("reconnecting");
          void this.connect(true);
          return;
        }
      }
    }
    if (!this.disposed && generation === this.generation) this.update("failed");
  }
}
