import { APIError } from "./api.js";
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

export interface PageViewerState {
  identity: string;
  page: number;
  inventory?: PageInventory;
  frame?: PageFrame;
  url?: string;
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
      page_selection_stale: "The selected source changed. Refresh the document selection.",
      page_runtime_unavailable: "Page runtime unavailable. Retained pages remain readable.",
      page_unsupported: "This source has unsupported page geometry or density.",
      page_limit: "This document or page exceeds supported rendering limits.",
      page_image_corrupt: "Invalid page image. Verification failed.",
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
    const inventory = await readPageInventory(this.session, binding, signal);
    if (!this.current(signal)) return;
    if (job) verifyJobInventory(job, inventory);
    const page = this.state.page,
      frame = inventory.inventory.frames[page - 1]?.frame;
    this.publish({ inventory, frame });
    const result = job?.results.find((v) => v.page === page);
    const image = result ?? inventory.inventory.images.find((v) => v.page === page);
    if (!image) {
      this.publish({ status: "missing" });
      return;
    }
    const recipe = inventory.inventory.recipes.find(
      (v) => v.sha256 === image.recipe_sha256,
    )!.recipe;
    const url = await readPageImage(this.session, binding, image, recipe, signal);
    if (!this.current(signal)) {
      URL.revokeObjectURL(url);
      return;
    }
    this.publish({ status: "ready", url, frame });
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
    const signal = this.reset();
    this.publish({ status: "rendering" });
    try {
      const binding = pageBinding(this.source, this.revision),
        dpi = this.source.mimeType === "image/png" ? 0 : 144;
      let job = await requestPageJob(this.session, binding, page, dpi, operation, signal);
      if (!this.current(signal)) return;
      this.publish({ job });
      if (job.id !== operation && pending(job)) {
        this.publish({ status: "external" });
        return;
      }
      // Poll only the operation created by this viewer, for at most its backend
      // ten-minute lifetime. A reused operation is never silently adopted.
      const deadline = Math.min(Date.now() + 600000, Date.parse(job.created_at) + 600000);
      while (pending(job)) {
        if (Date.now() >= deadline)
          throw new Error("Rendering exceeded its time limit. Refresh to check retained pages.");
        await pause(signal);
        job = await readPageJob(
          this.session,
          binding,
          job,
          AbortSignal.any([signal, AbortSignal.timeout(Math.max(1, deadline - Date.now()))]),
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
      this.failure(cause, signal, true);
    }
  }
  async cancel(): Promise<void> {
    const job = this.state.job;
    if (!job || job.id !== this.operations.get(this.state.page) || !pending(job)) return;
    const signal = this.reset();
    this.publish({ status: "rendering" });
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
