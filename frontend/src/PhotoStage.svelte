<script lang="ts">
  import { onDestroy, tick, untrack } from "svelte";
  import type { Photos } from "./photos.svelte.js";
  import type { PhotoBrowseRow } from "./generated/docbank.js";
  import { previewSlotsKey, type PhotoPreviewCache } from "./photoPreviewCache.js";
  import type { PhotoView } from "./photoView.js";
  import { PhotoPreviewLoader } from "./photoPreview.svelte.js";
  import PhotoLoupeImage from "./PhotoLoupeImage.svelte";
  import { Button } from "@kenn-io/kit-ui";
  import PhotoFaceCloseups from "./PhotoFaceCloseups.svelte";
  import type { PhotoFaces } from "./photoFaces.svelte.js";
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
    onfocus,
    faces,
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
    onfocus?: () => void;
    faces?: PhotoFaces;
  } = $props();
  let current = $state.raw(untrack(() => photo));
  let loader = $state.raw<PhotoPreviewLoader>(untrack(() => new PhotoPreviewLoader(photos, cache)));
  let reloadRevision = $state(0);
  let image = $state<PhotoLoupeImage>();
  // A row replaced with a newly published preview starts a new load. The loader's own slot
  // updates change the row in place, so they leave the identity alone.
  const identity = $derived.by(() => {
    const row = current;
    return untrack(() =>
      [row.asset_id, row.content_version_id, previewSlotsKey(row.previews)].join(":"),
    );
  });
  $effect(() => {
    current = photo;
  });
  $effect(() => {
    void identity;
    void reloadRevision;
    const active = new PhotoPreviewLoader(
      photos,
      cache,
      untrack(() => current),
      untrack(() => loader),
    );
    loader = active;
    active.restoreZoom(() => image?.isZoomed());
    return () => active.retire();
  });
  onDestroy(() => loader.dispose());
  const failure = $derived(loader.failure ?? (faces ? loader.fitFailure : undefined));
  async function retry() {
    if (failure?.action !== "reload") {
      const active = loader;
      await active.retry(failure?.action);
      if (!active.active.signal.aborted && !active.failure) {
        await tick();
        onfocus?.();
      }
      return;
    }
    const active = loader;
    try {
      const row = await photos.lookup(current.asset_id, active.active.signal);
      if (active.active.signal.aborted) return;
      current = row;
      reloadRevision++;
      onreload?.(row);
      await tick();
      onfocus?.();
    } catch (cause) {
      if (active.active.signal.aborted) return;
      const message = cause instanceof Error ? cause.message : String(cause);
      if (cause instanceof APIError && cause.status === 404) {
        active.url = "";
        onunavailable?.(message);
      }
      active.failure = { rank: active.failure?.rank ?? 1, message, action: "reload" };
    }
  }
</script>

<div class="photo-stage">
  <div class="image">
    <PhotoLoupeImage
      bind:this={image}
      url={loader.url}
      name={current.name}
      rotation={current.rotation}
      {view}
      {onview}
      {onmove}
      {onregion}
      {oncancelregion}
      {regionMode}
      onzoom={() => loader.zoom()}
      unavailable={!!loader.failure && !loader.retrying}
    />
    {#if failure}<p role="status">
        {failure.message}{#if failure.action}
          <Button size="sm" onclick={() => void retry()}>Retry preview</Button>{/if}
      </p>{/if}
  </div>
  {#if faces && current.kind === "photo"}<PhotoFaceCloseups
      photo={current}
      {faces}
      fit={loader.fit}
    />{/if}
</div>

<style>
  .photo-stage {
    display: flex;
    flex: 1;
    min-width: 0;
    min-height: 0;
    flex-direction: column;
  }
  .image {
    display: flex;
    flex: 1;
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
