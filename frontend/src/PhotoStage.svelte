<script lang="ts">
  import { untrack } from "svelte";
  import type { Photos } from "./photos.svelte.js";
  import type { PhotoBrowseRow } from "./generated/docbank.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import type { PhotoView } from "./photoView.js";
  import { PhotoPreviewLoader } from "./photoPreview.svelte.js";
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
  let current = $state.raw(untrack(() => photo));
  let loader = $state.raw<PhotoPreviewLoader>(untrack(() => new PhotoPreviewLoader(photos, cache)));
  let reloadRevision = $state(0);
  const identity = $derived(`${current.asset_id}:${current.content_version_id}`);
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
    );
    loader = active;
    return () => active.dispose();
  });
  async function retry() {
    if (loader.failure?.action !== "reload") return loader.retry();
    const active = loader;
    try {
      const row = await photos.lookup(current.asset_id, active.active.signal);
      if (active.active.signal.aborted) return;
      current = row;
      reloadRevision++;
      onreload?.(row);
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
  <PhotoLoupeImage
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
  {#if loader.failure}<p role="status">
      {loader.failure.message}{#if loader.failure.action}
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
