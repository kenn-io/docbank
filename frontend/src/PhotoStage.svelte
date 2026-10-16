<script lang="ts">
  import { untrack } from "svelte";
  import type { Photos } from "./photos.svelte.js";
  import type { PhotoBrowseRow } from "./generated/docbank.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import type { PhotoView } from "./photoView.js";
  import { loadPhotoPreview, PhotoPreviewDisplay } from "./photoPreview.js";
  import PhotoLoupeImage from "./PhotoLoupeImage.svelte";
  import { Button } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
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
    onreload,
    onunavailable,
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
    onreload?: (row: PhotoBrowseRow) => void;
    onunavailable?: (message: string) => void;
  } = $props();
  let url = $state("");
  let error = $state("");
  let large = false;
  let retrySize = $state<"fit" | "large">();
  let active: PhotoPreviewDisplay;
  let current = $state.raw(untrack(() => photo));
  let stale = false;
  $effect(() => {
    current = photo;
  });
  async function load(
    selected: PhotoBrowseRow,
    size: "grid" | "fit" | "large",
    controller: PhotoPreviewDisplay,
    reload = false,
  ) {
    const level = size === "grid" ? 0 : size === "fit" ? 1 : 2;
    try {
      const result = await loadPhotoPreview(
        photos,
        cache,
        selected,
        size,
        controller.signal,
        reload,
      );
      if (controller.signal.aborted) return;
      if (!result.blob) {
        if (level < controller.rank) return;
        if (size !== "grid") error = "Preview unavailable";
        if (size !== "grid") failed(size);
        return;
      }
      const objectURL = URL.createObjectURL(result.blob);
      await controller.show(objectURL, level, () => {
        url = objectURL;
        error = "";
      });
    } catch (cause) {
      if (!controller.signal.aborted && level >= controller.rank) {
        error = cause instanceof Error ? cause.message : String(cause);
        if (
          cause instanceof APIError &&
          (cause.status === 404 || (cause.status === 409 && cause.code === "photo_display_changed"))
        )
          stale = true;
        if (size !== "grid") failed(size);
      }
    }
  }
  function failed(size: "fit" | "large") {
    if (size === "large") large = false;
    retrySize = size;
  }
  $effect(() => {
    const selected = current;
    const controller = (active = new PhotoPreviewDisplay());
    url = "";
    error = "";
    large = false;
    retrySize = undefined;
    stale = false;
    void load(selected, "grid", controller).then(() => {
      if (!controller.signal.aborted) return load(selected, "fit", controller);
    });
    return () => controller.dispose();
  });
  function zoom(reload = false) {
    if (large || !active) return;
    large = true;
    retrySize = undefined;
    void load(
      untrack(() => current),
      "large",
      active,
      reload,
    );
  }
  async function retry() {
    const size = retrySize;
    if (!size) return;
    retrySize = undefined;
    error = "";
    if (stale) {
      const controller = active;
      try {
        const row = await photos.lookup(current.asset_id, controller.signal);
        if (controller.signal.aborted) return;
        current = row;
        onreload?.(row);
      } catch (cause) {
        if (controller.signal.aborted) return;
        error = cause instanceof Error ? cause.message : String(cause);
        if (cause instanceof APIError && cause.status === 404) {
          url = "";
          onunavailable?.(error);
        } else retrySize = size;
      }
    } else if (size === "large") zoom(true);
    else void load(current, size, active, true);
  }
</script>

<div class="photo-stage">
  <PhotoLoupeImage
    {url}
    name={current.name}
    rotation={current.rotation}
    {view}
    {onview}
    {onmove}
    {onregion}
    {oncancelregion}
    {regionMode}
    onzoom={() => zoom()}
    unavailable={!!error}
  />
  {#if error}<p role="status">
      {error}{#if retrySize}
        <Button size="sm" onclick={() => void retry()}>Retry preview</Button>{/if}
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
