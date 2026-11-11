import { APIError } from "./api-transport.js";
import { offerPreparedDownload } from "./download.js";
import type { BatesExport } from "./generated/docbank.js";
import type { NumberingInputs } from "./redactionNumbering.js";
import { prepareRecipientPackage, releaseRecipientPackage } from "./redactionRecipientPackage.js";

// One controller belongs to the reader, including all its completed reviews.
export class RecipientPackageController {
  phase = $state<"idle" | "building" | "releasing" | "cleanup-failed">("idle");
  allocation = $state("");
  error = $state("");
  private generation = 0;
  private request?: AbortController;
  private owned = false;
  private cleanup?: Promise<void>;

  constructor(
    private session: () => string,
    private authFailure: (cause: unknown) => void,
    private notice: (message: string) => void,
  ) {}

  async start(inputs: NumberingInputs, artifact: BatesExport) {
    if (this.phase !== "idle") return;
    const generation = ++this.generation,
      previous = this.owned;
    this.allocation = artifact.allocation_id;
    this.error = "";
    this.notice("");
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
      this.notice("Download started. Keep this tab open until the download finishes.");
      this.phase = "idle";
    } catch (cause) {
      if (generation !== this.generation) return;
      this.error = cause instanceof Error ? cause.message : String(cause);
      if (
        cause instanceof APIError &&
        (cause.code === "recipient_package_busy" ||
          cause.code === "redaction_busy" ||
          [400, 401, 403, 404, 409, 413, 422].includes(cause.status))
      ) {
        this.owned = previous;
        this.phase = "idle";
        if (cause.status === 401) this.authFailure(cause);
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
    return this.release();
  }

  release(keepalive = false): Promise<void> {
    if (this.cleanup) return this.cleanup;
    if (!this.owned) return Promise.resolve();
    this.phase = "releasing";
    this.cleanup = releaseRecipientPackage(this.session(), AbortSignal.timeout(30_000), keepalive)
      .then(() => {
        this.owned = false;
        this.phase = "idle";
      })
      .catch((cause: unknown) => {
        this.phase = "cleanup-failed";
        this.error = cause instanceof Error ? cause.message : String(cause);
        if (cause instanceof APIError && cause.status === 401) this.authFailure(cause);
      })
      .finally(() => {
        this.cleanup = undefined;
      });
    return this.cleanup;
  }

  stop() {
    this.generation++;
    this.request?.abort();
    return this.release(true);
  }
}
