import { APIError } from "./api-transport.js";
import {
  pageBinding,
  readPageImage,
  readPageInventory,
  readPageJob,
  requestPageJob,
  verifyJobInventory,
  type PageFrame,
  type PageInventory,
  type PageJob,
} from "./pages.js";
import type { SelectedSource } from "./selectedSource.js";

export const pdfPageDPI = 144;
export const pageFailureMessages: Record<string, string> = {
  unavailable: "Page runtime unavailable. Retained pages remain readable.",
  unsupported: "Unsupported page geometry or density.",
  invalid_output: "Invalid page image. Verification failed.",
  stale_source: "The selected source changed. Refresh the document selection.",
  limit: "This document or page exceeds supported rendering limits.",
  interrupted: "Rendering was interrupted.",
  storage: "The rendered page could not be retained.",
};

export interface PageViewerState {
  identity: string;
  page: number;
  inventory?: PageInventory;
  frame?: PageFrame;
  url?: string;
  dpi?: number;
  status:
    | "loading"
    | "ready"
    | "missing"
    | "rendering"
    | "external"
    | "canceled"
    | "failed"
    | "error";
  message?: string;
  job?: PageJob;
  retry?: boolean;
}
function pending(job: PageJob): boolean {
  return job.state === "queued" || job.state === "running";
}
function pause(signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const abort = () => {
      clearTimeout(timer);
      reject(signal.reason);
    };
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", abort);
      resolve();
    }, 500);
    signal.addEventListener("abort", abort, { once: true });
    if (signal.aborted) abort();
  });
}

// One mounted selection owns its requests, operation UUIDs and blob URL. A page
// change retires that request generation before starting any asynchronous work.
export class PageViewerSession {
  private controller = new AbortController();
  private operations = new Map<number, string>();
  private disposed = false;
  private state: PageViewerState;
  constructor(
    private session: string,
    private source: SelectedSource,
    private revision: number,
    identity: string,
    private changed: (state: PageViewerState) => void,
    private authFailure: (cause: unknown) => void,
  ) {
    this.state = { identity, page: 1, status: "loading" };
  }
  private publish(next: Partial<PageViewerState>): void {
    if (!this.disposed) {
      if (next.status === "failed" || next.status === "canceled") this.operations.delete(this.state.page);
      this.state = { ...this.state, ...next };
      this.changed(this.state);
    }
  }
  private reset(): AbortSignal {
    this.controller.abort();
    this.controller = new AbortController();
    if (this.state.url) URL.revokeObjectURL(this.state.url);
    this.publish({
      url: undefined,
      dpi: undefined,
      frame: undefined,
      job: undefined,
      message: undefined,
      retry: false,
      status: "loading",
    });
    return this.controller.signal;
  }
  private current(signal: AbortSignal): boolean {
    return !this.disposed && !signal.aborted;
  }
  private failure(cause: unknown, signal: AbortSignal, retry = false): void {
    if (!this.current(signal)) return;
    if (cause instanceof APIError && cause.status === 401) this.authFailure(cause);
    const messages: Record<string, string> = {
      page_selection_stale: pageFailureMessages.stale_source!,
      page_runtime_unavailable: pageFailureMessages.unavailable!,
      page_unsupported: pageFailureMessages.unsupported!,
      page_limit: pageFailureMessages.limit!,
      page_image_corrupt: pageFailureMessages.invalid_output!,
      page_image_unavailable: "The retained page image is unavailable.",
    };
    this.publish({
      status: "error",
      message:
        cause instanceof APIError
          ? (messages[cause.code] ?? cause.message)
          : cause instanceof Error
            ? cause.message
            : String(cause),
      retry,
    });
  }
  async load(page = this.state.page): Promise<void> {
    if (
      this.disposed ||
      !Number.isInteger(page) ||
      page < 1 ||
      page > (this.state.inventory?.inventory.page_count || 1)
    )
      return;
    const signal = this.reset();
    this.publish({ page });
    try {
      await this.loadImage(signal);
    } catch (cause) {
      this.failure(cause, signal);
    }
  }
  private async loadImage(signal: AbortSignal, job?: PageJob): Promise<void> {
    const binding = pageBinding(this.source, this.revision);
    // Recheck authority on navigation and pick up pages retained by other requests.
    const inventory = await readPageInventory(this.session, binding, signal);
    if (!this.current(signal)) return;
    if (job) verifyJobInventory(job, inventory);
    const page = this.state.page,
      frame = inventory.inventory.frames[page - 1]?.frame;
    this.publish({ inventory, frame });
    const result = job?.results.find((v) => v.page === page);
    const recipes = new Map(inventory.inventory.recipes.map(v => [v.sha256, v.recipe]));
    const dpi = frame?.input_units === "pixel" ? frame.pixels_per_metre_x! * 127 / 5000 : pdfPageDPI;
    // Prefer the standard recipe; retain the highest available DPI as a fallback.
    const images = inventory.inventory.images.filter(v => v.page === page)
      .toSorted((a, b) => recipes.get(b.recipe_sha256)!.dpi - recipes.get(a.recipe_sha256)!.dpi);
    const image = result ?? images.find(v => recipes.get(v.recipe_sha256)!.dpi === dpi) ?? images[0];
    if (!image) {
      this.publish({ status: "missing" });
      return;
    }
    const recipe = recipes.get(image.recipe_sha256)!;
    const url = await readPageImage(this.session, binding, image, recipe, signal);
    if (!this.current(signal)) {
      URL.revokeObjectURL(url);
      return;
    }
    this.publish({ status: "ready", url, frame, dpi: recipe.dpi });
  }
  async render(): Promise<void> {
    if (
      this.disposed ||
      this.state.status === "rendering" ||
      !this.state.inventory?.runtime_available
    )
      return;
    const page = this.state.page;
    let operation = this.operations.get(page);
    if (!operation) {
      operation = crypto.randomUUID();
      this.operations.set(page, operation);
    }
    const previousJob = this.state.job;
    const signal = this.reset();
    this.publish({ status: "rendering", job: previousJob?.id === operation ? previousJob : undefined });
    try {
      const binding = pageBinding(this.source, this.revision),
        dpi = this.source.mimeType === "image/png" ? 0 : pdfPageDPI;
      let job = await requestPageJob(this.session, binding, page, dpi, operation, signal);
      if (!this.current(signal)) return;
      this.publish({ job });
      if (job.id !== operation && pending(job)) {
        this.publish({ status: "external" });
        return;
      }
      // Bound each explicit polling attempt locally. Server timestamps include
      // queue time and may come from a different clock.
      const deadline = performance.now() + 600000;
      while (pending(job)) {
        if (performance.now() >= deadline)
          throw new Error("Rendering exceeded its time limit. Refresh to check retained pages.");
        await pause(signal);
        job = await readPageJob(
          this.session,
          binding,
          job,
          AbortSignal.any([signal, AbortSignal.timeout(Math.max(1, Math.ceil(deadline - performance.now())))]),
        );
        if (!this.current(signal)) return;
        this.publish({ job });
      }
      if (job.state === "completed") await this.loadImage(signal, job);
      else
        this.publish({
          status: job.state === "canceled" ? "canceled" : "failed",
          message: job.failure_code,
        });
    } catch (cause) {
      // Keep the operation UUID: a lost response or local timeout does not mean
      // the daemon stopped the job. Only transport failures offer direct retry.
      this.failure(cause, signal, cause instanceof TypeError && this.state.job?.state !== "completed");
    }
  }
  async cancel(): Promise<void> {
    const job = this.state.job;
    if (!job || job.id !== this.operations.get(this.state.page) || !pending(job)) return;
    const signal = this.reset();
    this.publish({ status: "rendering", job });
    try {
      const canceled = await readPageJob(
        this.session,
        pageBinding(this.source, this.revision),
        job,
        signal,
        true,
      );
      if (!this.current(signal)) return;
      if (canceled.state === "completed") await this.loadImage(signal, canceled);
      else
        this.publish({
          job: canceled,
          status: canceled.state === "canceled" ? "canceled" : "failed",
          message: canceled.failure_code,
        });
    } catch (cause) {
      this.failure(cause, signal);
    }
  }
  dispose(): void {
    this.disposed = true;
    this.controller.abort();
    if (this.state.url) URL.revokeObjectURL(this.state.url);
    this.operations.clear();
  }
}
