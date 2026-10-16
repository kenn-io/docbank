<script lang="ts">
  import { untrack } from "svelte";
  import type { Photos } from "./photos.svelte.js";
  import type { PhotoBrowseRow } from "./generated/docbank.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import type { PhotoView } from "./photoView.js";
  import { loadPhotoPreview, PhotoPreviewDisplay } from "./photoPreview.js";
  import PhotoLoupeImage from "./PhotoLoupeImage.svelte";
  import { Button } from "@kenn-io/kit-ui";
  let {
    photo,
    photos,
    cache,
    view,
    onview,
    onmove = () => {},
    onregion,
    oncancelregion,
    regionMode = false,
  }: {
    photo: PhotoBrowseRow;
    photos: Photos;
    cache: PhotoPreviewCache;
    view: PhotoView;
    onview?: (value: PhotoView) => void;
    onmove?: (direction: -1 | 1) => void;
    onregion?: (value: { x: number; y: number; width: number; height: number }) => void;
    oncancelregion?: () => void;
    regionMode?: boolean;
  } = $props();
  let url = $state("");
  let error = $state("");
  let large = false;
  let retryable = $state(false);
  let active: PhotoPreviewDisplay;
  async function load(
    selected: PhotoBrowseRow,
    size: "grid" | "fit" | "large",
    controller: PhotoPreviewDisplay,
  ) {
    const level = size === "grid" ? 0 : size === "fit" ? 1 : 2;
    try {
      const result = await loadPhotoPreview(photos, cache, selected, size, controller.signal);
      if (controller.signal.aborted) return;
      if (!result.blob) {
        if (size !== "grid") error = "Preview unavailable";
        if (size === "large") failLarge();
        return;
      }
      const objectURL = URL.createObjectURL(result.blob);
      await controller.show(objectURL, level, () => {
        url = objectURL;
        error = "";
      });
    } catch (cause) {
      if (!controller.signal.aborted) {
        error = cause instanceof Error ? cause.message : String(cause);
        if (size === "large") failLarge();
      }
    }
  }
  // A failed large request must not latch: the fitted image stays and the photographer can retry.
  function failLarge() {
    large = false;
    retryable = true;
  }
  $effect(() => {
    const selected = photo;
    const controller = (active = new PhotoPreviewDisplay());
    url = "";
    error = "";
    large = false;
    retryable = false;
    void load(selected, "grid", controller).then(() => {
      if (!controller.signal.aborted) return load(selected, "fit", controller);
    });
    return () => controller.dispose();
  });
  function zoom() {
    if (large || !active) return;
    large = true;
    retryable = false;
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
    {oncancelregion}
    {regionMode}
    onzoom={zoom}
    unavailable={!!error}
  />
  {#if error}<p role="status">
      {error}{#if retryable}
        <Button
          size="sm"
          onclick={() => {
            error = "";
            zoom();
          }}>Retry preview</Button
        >{/if}
    </p>{/if}
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
    display: flex;
    align-items: center;
    gap: 8px;
    position: absolute;
    bottom: 0;
    padding: 8px;
    background: var(--bg-surface);
  }
</style>
