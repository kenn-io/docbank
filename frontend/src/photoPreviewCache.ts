import { readPhotoPreview } from "./generated/docbank.js";
import { APIError } from "./api-transport.js";

export class PhotoPreviewCache {
  private entries = new Map<string, Promise<string>>();
  private urls = new Set<string>();
  private controller = new AbortController();

  constructor(private session: string, private onauthfailure: (cause: unknown) => void) {}

  get(assetID: string, generationID: string): Promise<string> {
    const key = `${assetID}:${generationID}`;
    const cached = this.entries.get(key);
    if (cached) return cached;
    const request = readPhotoPreview(assetID, generationID, undefined, { session: this.session, signal: this.controller.signal })
      .then(response => response.blob())
      .then(blob => {
        if (this.controller.signal.aborted) throw new DOMException("Workspace closed", "AbortError");
        const url = URL.createObjectURL(blob);
        this.urls.add(url);
        return url;
      }).catch(cause => {
        this.entries.delete(key);
        if (!this.controller.signal.aborted && cause instanceof APIError && cause.status === 401) this.onauthfailure(cause);
        throw cause;
      });
    this.entries.set(key, request);
    return request;
  }

  dispose() {
    this.controller.abort();
    for (const url of this.urls) URL.revokeObjectURL(url);
    this.urls.clear();
    this.entries.clear();
  }
}
