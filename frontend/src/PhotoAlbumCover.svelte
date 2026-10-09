<script lang="ts">
  import ImageIcon from "@lucide/svelte/icons/image";
  import type { PhotoAlbumItem } from "./photoAlbums.svelte.js";
  import { previewObjectURL, type PhotoPreviewCache } from "./photoPreviewCache.js";
  let { album, cache }: { album: PhotoAlbumItem; cache: PhotoPreviewCache } = $props();
  let url = $state("");
  let failed = $state(false);
  $effect(() => {
    const asset = album.effective_cover_asset_id;
    const generation = album.cover_generation_id;
    url = ""; failed = false;
    if (!asset || !generation) return;
    return previewObjectURL(cache, asset, generation, value => url = value, () => failed = true);
  });
</script>

<div class="album-cover">
  {#if url}<img src={url} alt="" onerror={() => { url = ""; failed = true; }} />
  {:else}<ImageIcon size="28" /><span>{failed || !album.cover_known ? "Cover unavailable" : album.cover_generation_id ? "Loading cover…" : "No cover yet"}</span>{/if}
</div>

<style>
  .album-cover { aspect-ratio: 1; background: var(--bg-inset); display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 8px; color: var(--text-muted); font-size: var(--font-size-xs); overflow: hidden; }
  img { width: 100%; height: 100%; object-fit: cover; }
</style>
