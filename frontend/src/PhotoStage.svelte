<script lang="ts">
  import { untrack } from "svelte";
  import type { Photos } from "./photos.svelte.js";
  import type { PhotoBrowseRow } from "./generated/docbank.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import type { PhotoView } from "./photoView.js";
  import { loadPhotoPreview } from "./photoPreview.js";
  import PhotoLoupeImage from "./PhotoLoupeImage.svelte";
  let {
    photo,
    photos,
    cache,
    view,
    onview,
    onmove = () => {},
    onregion,
    regionMode = false,
  }: {
    photo: PhotoBrowseRow;
    photos: Photos;
    cache: PhotoPreviewCache;
    view: PhotoView;
    onview?: (value: PhotoView) => void;
    onmove?: (direction: -1 | 1) => void;
    onregion?: (value: { x: number; y: number; width: number; height: number }) => void;
    regionMode?: boolean;
  } = $props();
  let url = $state("");
  let error = $state("");
  let large = false;
  let rank = -1;
  let active: AbortController;
  let owned: string[] = [];
  async function load(
    selected: PhotoBrowseRow,
    size: "grid" | "fit" | "large",
    controller: AbortController,
  ) {
    const level = size === "grid" ? 0 : size === "fit" ? 1 : 2;
    try {
      const result = await loadPhotoPreview(photos, cache, selected, size, controller.signal);
      if (controller.signal.aborted) return;
      if (!result.blob) {
        if (size !== "grid") error = "Preview unavailable";
        return;
      }
      const objectURL = URL.createObjectURL(result.blob);
      owned.push(objectURL);
      const image = new Image();
      image.src = objectURL;
      await image.decode();
      if (!controller.signal.aborted && level >= rank) {
        rank = level;
        url = objectURL;
        error = "";
      }
    } catch (cause) {
      if (!controller.signal.aborted)
        error = cause instanceof Error ? cause.message : String(cause);
    }
  }
  $effect(() => {
    const selected = photo;
    const controller = (active = new AbortController());
    const urls: string[] = (owned = []);
    url = "";
    error = "";
    large = false;
    rank = -1;
    void load(selected, "grid", controller).then(() => {
      if (!controller.signal.aborted) return load(selected, "fit", controller);
    });
    return () => {
      controller.abort();
      for (const value of urls) URL.revokeObjectURL(value);
    };
  });
  function zoom() {
    if (large || !active) return;
    large = true;
    void load(
      untrack(() => photo),
      "large",
      active,
    );
  }
</script>

<div class="photo-stage">
  <PhotoLoupeImage
    {url}
    name={photo.name}
    rotation={photo.rotation}
    {view}
    {onview}
    {onmove}
    {onregion}
    {regionMode}
    onzoom={zoom}
    unavailable={!!error}
  />
  {#if error}<p role="status">{error}</p>{/if}
</div>

<style>
  .photo-stage {
    display: flex;
    flex: 1;
    min-width: 0;
    min-height: 0;
    position: relative;
  }
  p {
    position: absolute;
    bottom: 0;
    padding: 8px;
    background: var(--bg-surface);
  }
</style>
