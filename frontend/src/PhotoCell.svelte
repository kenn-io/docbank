<script lang="ts">
  import { Checkbox } from "@kenn-io/kit-ui";
  import ImageIcon from "@lucide/svelte/icons/image";
  import type { PhotoBrowseRow } from "./generated/docbank.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";

  let { photo, cache, selected, onclick, oncheck }: {
    photo: PhotoBrowseRow;
    cache: PhotoPreviewCache;
    selected: boolean;
    onclick: (event: MouseEvent) => void;
    oncheck: (checked: boolean, range: boolean) => void;
  } = $props();
  let url = $state("");
  let failed = $state(false);
  let errorMessage = $state("");
  let retry = $state(0);
  $effect(() => {
    const slot = photo.previews.grid;
    void retry;
    url = "";
    failed = false;
    errorMessage = "";
    if (slot.state !== "ready" || !slot.generation_id) return;
    const controller = new AbortController();
    let current = true;
    let objectURL = "";
    void cache.get(photo.asset_id, slot.generation_id, controller.signal).then(blob => {
      if (current) { objectURL = URL.createObjectURL(blob); url = objectURL; }
    }).catch(cause => { if (current) { failed = true; errorMessage = cause instanceof Error ? cause.message : String(cause); } });
    return () => { current = false; controller.abort(); if (objectURL) URL.revokeObjectURL(objectURL); };
  });
  const placeholder = $derived(failed || photo.previews.grid.state === "failed" ? "Preview failed" : photo.previews.grid.state === "unsupported" ? "Preview unsupported" : photo.previews.grid.state === "missing" ? "Preview pending" : "Loading preview");
</script>

<div class="photo-cell" class:selected data-asset={photo.asset_id}>
  <button type="button" class="photo-image" aria-label={`Select ${photo.name}`} aria-pressed={selected} {onclick}>
    {#if url}
      <img src={url} alt={photo.name} onerror={() => { url = ""; failed = true; }} />
    {:else}
      <span class="placeholder" title={errorMessage} class:pending={placeholder === "Preview pending"}>
        <ImageIcon size="24" aria-hidden="true" />
        <span>{placeholder}</span>
      </span>
    {/if}
  </button>
  <div class="photo-check">
    <Checkbox checked={selected} ariaLabel={`Select photo ${photo.name}`} onchange={(checked) => oncheck(checked, false)} />
  </div>
  {#if failed}<button type="button" class="retry-preview" onclick={() => retry++}>Retry preview</button>{/if}
</div>

<style>
  .photo-cell { position: relative; height: 100%; background: var(--bg-inset); border-radius: var(--radius-sm); overflow: hidden; }
  .photo-cell.selected { outline: 3px solid var(--accent-blue); outline-offset: -3px; }
  .photo-image { display: block; width: 100%; height: 100%; padding: 0; border: 0; background: transparent; cursor: pointer; color: var(--text-muted); }
  .photo-image:focus-visible { outline: 3px solid var(--accent-blue); outline-offset: -3px; }
  img { display: block; width: 100%; height: 100%; object-fit: cover; }
  .placeholder { display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 8px; height: 100%; font-size: var(--font-size-xs); }
  .pending { background: var(--bg-surface-hover); }
  .photo-check { position: absolute; top: 8px; left: 8px; display: flex; }
  .retry-preview { position: absolute; bottom: 8px; left: 8px; background: var(--bg-surface); color: var(--text-primary); border: 1px solid var(--border-default); border-radius: var(--radius-sm); padding: 4px 8px; font-size: var(--font-size-xs); }
</style>
