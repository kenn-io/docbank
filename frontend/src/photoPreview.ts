import type { PhotoBrowseRow } from "./generated/docbank.js";
import type { Photos } from "./photos.svelte.js";
import type { PhotoPreviewCache } from "./photoPreviewCache.js";

export async function loadPhotoPreview(
  photos: Photos,
  cache: PhotoPreviewCache,
  photo: PhotoBrowseRow,
  size: "grid" | "fit" | "large",
  signal: AbortSignal,
  reload = false,
) {
  const slot = size === "grid" ? photo.previews.grid : await photos.preview(photo, size, signal);
  const blob =
    slot.state === "ready" && slot.generation_id
      ? await cache.get(photo.asset_id, slot.generation_id, signal, reload)
      : undefined;
  return { slot, blob };
}

// One display owns decoded object URLs until its photo changes or the view closes.
export class PhotoPreviewDisplay {
  private controller = new AbortController();
  private urls = new Set<string>();
  rank = -1;
  get signal() {
    return this.controller.signal;
  }
  async show(objectURL: string, level: number, onshow: () => void) {
    if (this.signal.aborted) {
      URL.revokeObjectURL(objectURL);
      return;
    }
    this.urls.add(objectURL);
    try {
      const image = new Image();
      image.src = objectURL;
      await image.decode();
    } catch (cause) {
      if (this.urls.delete(objectURL)) URL.revokeObjectURL(objectURL);
      throw cause;
    }
    if (this.signal.aborted || level < this.rank) return;
    this.rank = level;
    onshow();
  }
  dispose() {
    this.controller.abort();
    for (const url of this.urls) URL.revokeObjectURL(url);
    this.urls.clear();
  }
}
