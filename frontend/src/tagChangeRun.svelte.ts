import { runBatchTagChange, type BatchTagReceipt, type BatchTagRequest } from "./batch-tags.js";

export interface TagChangeRunOptions {
  session: string;
  onauthfailure: (cause: unknown) => void;
  /** Called with the receipt once the daemon confirms the change. */
  onconfirmed: (receipt: BatchTagReceipt) => void;
  onunknown?: () => void;
  /** Shown instead of the error text when no answer arrived. */
  unknownMessage?: string;
  /** Shown instead of the error text when a selected document changed. */
  staleMessage?: string;
}

// One tag change from request to outcome, shared by the dialogs that send it.
export class TagChangeRun {
  pending = $state(false);
  stale = $state(false);
  failure = $state("");
  private alive = true;

  constructor(private readonly options: TagChangeRunOptions) {}

  destroy(): void {
    this.alive = false;
  }

  async submit(request: BatchTagRequest): Promise<void> {
    this.pending = true;
    this.failure = "";
    const outcome = await runBatchTagChange(this.options.session, request);
    if (!this.alive) return;
    this.pending = false;
    if (outcome.kind === "confirmed") {
      this.options.onconfirmed(outcome.receipt);
      return;
    }
    if (outcome.kind === "unknown") this.stale = true;
    if (outcome.kind === "unknown") this.options.onunknown?.();
    if (outcome.kind === "auth") {
      this.options.onauthfailure(outcome.cause);
      return;
    }
    if (outcome.kind === "rejected") this.stale = outcome.stale;
    const text = outcome.cause instanceof Error ? outcome.cause.message : String(outcome.cause);
    this.failure =
      (outcome.kind === "unknown" ? this.options.unknownMessage : undefined) ??
      (outcome.kind === "rejected" && outcome.stale ? this.options.staleMessage : undefined) ??
      text;
  }
}
