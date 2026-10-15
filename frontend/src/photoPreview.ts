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
