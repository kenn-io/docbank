import { APIError } from "./api-transport.js";
import { offerPreparedDownload } from "./download.js";
import type { BatesExport } from "./generated/docbank.js";
import type { NumberingInputs } from "./redactionNumbering.js";
import { prepareRecipientPackage, releaseRecipientPackage } from "./redactionRecipientPackage.js";

// Server details stay in logs; these name what the operator can do next.
function describe(cause: unknown): string {
  if (cause instanceof APIError) {
    switch (cause.code) {
      case "recipient_package_busy":
        return "Another recipient ZIP is being prepared or downloaded. Try again when it finishes.";
      case "redaction_busy":
        return "Another PDF operation is running. Try again when it finishes.";
      case "redaction_review_changed":
        return "This review no longer matches the kept PDF.";
      case "recipient_package_limit":
        return "This PDF is too large for a recipient ZIP, or its labels cannot be imported.";
      case "redaction_integrity":
        return "The kept PDF failed verification. No ZIP was created.";
    }
  }
  // fetch rejects with a TypeError when the connection drops; its text is browser-specific.
  if (cause instanceof TypeError) return "The connection was interrupted. Try again.";
  return cause instanceof Error ? cause.message : String(cause);
}

// One controller belongs to the reader, including all its completed reviews.
export class RecipientPackageController {
  phase = $state<"idle" | "building" | "releasing" | "cleanup-failed">("idle");
  allocation = $state("");
  message = $state("");
  error = $state("");
  private generation = 0;
  private request?: AbortController;
  private owned = false;
  private cleanup?: Promise<void>;

  constructor(
    private session: () => string,
    private authFailure: (cause: unknown) => void,
  ) {}

  async start(inputs: NumberingInputs, artifact: BatesExport) {
    if (this.phase !== "idle") return;
    const generation = ++this.generation,
      previous = this.owned;
    this.allocation = artifact.allocation_id;
    this.error = "";
    this.message = "";
    this.phase = "building";
    this.owned = true;
    const request = new AbortController();
    this.request = request;
    try {
      const result = await prepareRecipientPackage(
        this.session(),
        inputs,
        artifact,
        request.signal,
      );
      if (generation !== this.generation) return;
      offerPreparedDownload(result);
      this.message = "Download started. Keep this review open until it finishes.";
      this.phase = "idle";
    } catch (cause) {
      if (generation !== this.generation) return;
      if (cause instanceof APIError && cause.status === 401) {
        this.owned = previous;
        this.phase = "idle";
        this.authFailure(cause);
        return;
      }
      this.error = describe(cause);
      if (
        cause instanceof APIError &&
        (cause.code === "recipient_package_busy" ||
          cause.code === "redaction_busy" ||
          [400, 403, 404, 409, 413, 422].includes(cause.status))
      ) {
        this.owned = previous;
        this.phase = "idle";
      } else {
        await this.release();
      }
    } finally {
      if (this.request === request) this.request = undefined;
    }
  }

  cancel() {
    this.generation++;
    this.request?.abort();
    this.error = "";
    this.message = "";
    return this.release();
  }

  release(keepalive = false): Promise<void> {
    if (this.cleanup) return this.cleanup;
    if (!this.owned) {
      this.phase = "idle";
      return Promise.resolve();
    }
    this.phase = "releasing";
    // The server answers once creation or transfer has stopped and files are gone.
    this.cleanup = releaseRecipientPackage(this.session(), AbortSignal.timeout(65_000), keepalive)
      .then(() => {
        this.owned = false;
        this.phase = "idle";
      })
      .catch((cause: unknown) => {
        if (cause instanceof APIError && cause.status === 401) {
          this.phase = "idle";
          this.authFailure(cause);
          return;
        }
        this.phase = "cleanup-failed";
        this.error = "The recipient ZIP could not be released. Release it before trying again.";
      })
      .finally(() => {
        this.cleanup = undefined;
      });
    return this.cleanup;
  }

  /** Shows another download's status in the same single line, unless a package is busy. */
  note(allocation: string, message: string): boolean {
    if (this.phase !== "idle") return false;
    this.allocation = allocation;
    this.message = message;
    this.error = "";
    return true;
  }

  stop() {
    this.generation++;
    this.request?.abort();
    return this.release(true);
  }
}
