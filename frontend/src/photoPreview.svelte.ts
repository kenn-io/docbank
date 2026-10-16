import { untrack } from "svelte";
import { APIError } from "./api-transport.js";
import type { PhotoRow } from "./photos.svelte.js";
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

export class PhotoPreviewLoader {
  url = $state("");
  quality = $state("");
  failure = $state<{ rank: number; message: string; action?: "reload" | "fit" | "large" }>();
  retrying = $state(false);
  private largeStarted = false;
  readonly active = new PhotoPreviewDisplay();
  private timer?: ReturnType<typeof setTimeout>;
  constructor(
    private photos: Photos,
    private cache: PhotoPreviewCache,
    readonly photo?: PhotoBrowseRow,
  ) {
    if (!photo) return;
    void this.load("grid");
    this.timer = setTimeout(() => void this.load("fit"), 150);
  }
  dispose() {
    clearTimeout(this.timer);
    this.active.dispose();
  }
  show(blobURL: string, level: number, label: string) {
    return this.active.show(blobURL, level, () => {
      this.url = blobURL;
      this.quality = label;
      if (this.failure && this.failure.rank <= level) this.failure = undefined;
    });
  }
  fail(result: NonNullable<PhotoPreviewLoader["failure"]>) {
    untrack(() => {
      if (
        !this.active.signal.aborted &&
        result.rank >= this.active.rank &&
        result.rank >= (this.failure?.rank ?? -1)
      )
        this.failure = result;
    });
  }
  private async load(size: "grid" | "fit" | "large", reload = false) {
    const selected = this.photo!,
      active = this.active;
    if (size === "large") this.largeStarted = true;
    const signal = AbortSignal.any([active.signal, AbortSignal.timeout(30_000)]);
    const level = size === "grid" ? 0 : size === "fit" ? 1 : 2;
    try {
      const result = await loadPhotoPreview(
        this.photos,
        this.cache,
        selected,
        size,
        signal,
        reload,
      );
      const slot = result.slot;
      if (slot.state !== "ready" || !slot.generation_id) {
        if (slot.state !== "missing")
          this.fail({
            rank: level,
            message:
              slot.state === "unsupported"
                ? "Preview unsupported. Download this photo from Documents."
                : "Preview unavailable for this version. Download this photo from Documents.",
          });
        return;
      }
      const objectURL = URL.createObjectURL(result.blob!);
      await this.show(objectURL, level, size);
    } catch (cause) {
      const reload =
        cause instanceof APIError &&
        (cause.code === "photo_display_changed" || cause.status === 404);
      this.fail({
        rank: level,
        message: cause instanceof Error ? cause.message : String(cause),
        action: reload ? "reload" : size === "grid" ? undefined : size,
      });
      if (!active.signal.aborted && size === "large") this.largeStarted = false;
    }
  }
  zoom() {
    if (!this.photo || this.largeStarted || this.quality === "original") return;
    void this.load("large");
  }
  async retry() {
    const action = this.failure?.action;
    if (this.retrying || !this.photo || (action !== "fit" && action !== "large")) return;
    this.retrying = true;
    try {
      await this.load(action, true);
    } finally {
      if (!this.active.signal.aborted) this.retrying = false;
    }
  }
}

export function prefetchPreviews(photos: Photos, cache: PhotoPreviewCache, rows: PhotoRow[]) {
  const active = new AbortController();
  const timer = setTimeout(async () => {
    for (const row of rows) {
      if (active.signal.aborted) break;
      const signal = AbortSignal.any([active.signal, AbortSignal.timeout(30_000)]);
      try {
        await loadPhotoPreview(photos, cache, await photos.detail(row, signal), "fit", signal);
      } catch {}
    }
  }, 150);
  return () => {
    clearTimeout(timer);
    active.abort();
  };
}
