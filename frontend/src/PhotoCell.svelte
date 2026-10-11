<script lang="ts">
  import { Checkbox } from "@kenn-io/kit-ui";
  import RotateCcw from "@lucide/svelte/icons/rotate-ccw";
  import ImageIcon from "@lucide/svelte/icons/image";
  import type { PhotoBrowseRow } from "./generated/docbank.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";

  let { photo, cache, selected, onclick, oncheck, onopen, compact = false }: {
    photo: PhotoBrowseRow;
    cache: PhotoPreviewCache;
    selected: boolean;
    onclick: (event: MouseEvent) => void;
    compact?: boolean;
    onopen?: (element: HTMLElement) => void;
    oncheck?: (checked: boolean, range: boolean) => void;
  } = $props();
  let imageButton = $state<HTMLButtonElement>();
  let touchClick = false;
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
    void cache.get(photo.asset_id, slot.generation_id, controller.signal, retry > 0).then(blob => {
      if (current) { objectURL = URL.createObjectURL(blob); url = objectURL; }
    }).catch(cause => { if (current) { failed = true; errorMessage = cause instanceof Error ? cause.message : String(cause); } });
    return () => { current = false; controller.abort(); if (objectURL) URL.revokeObjectURL(objectURL); };
  });
  const placeholder = $derived(failed || photo.previews.grid.state === "failed" ? "Preview failed" : photo.previews.grid.state === "unsupported" ? "Preview unsupported" : photo.previews.grid.state === "missing" ? "Preview pending" : "Loading preview");
</script>

<div class="photo-cell" class:selected class:compact data-asset={photo.asset_id}>
  <button type="button" class="photo-image" bind:this={imageButton} aria-label={`${compact ? "Show" : "Select"} ${photo.name}`} aria-pressed={compact ? undefined : selected} aria-current={compact && selected ? "true" : undefined} onclick={event => { touchClick = "pointerType" in event && event.pointerType === "touch"; onclick(event); }} ondblclick={event => { if (!compact && !touchClick) onopen?.(event.currentTarget); }} onkeydown={event => { if (!compact && event.key === "Enter") { event.preventDefault(); onopen?.(event.currentTarget); } }}>
    {#if url}
      <img src={url} alt={photo.name} onerror={() => { url = ""; failed = true; }} />
    {:else}
      <span class="placeholder" title={errorMessage} class:pending={placeholder === "Preview pending"}>
        <ImageIcon size="24" aria-hidden="true" />
        <span class:kit-sr-only={compact}>{placeholder}</span>
      </span>
    {/if}
  </button>
  {#if !compact}
  <div class="photo-check">
    <Checkbox checked={selected} ariaLabel={`Select photo ${photo.name}`} onchange={(checked) => oncheck?.(checked, false)} />
  </div>
  <button type="button" class="open-photo" aria-label={`Open ${photo.name}`} onclick={event => onopen?.(event.currentTarget)}>Open</button>
  {/if}
  {#if failed}<button type="button" class="retry-preview" aria-label={compact ? `Retry preview for ${photo.name}` : undefined} onclick={() => { if (compact) imageButton?.focus({preventScroll:true}); retry++; }}>{#if compact}<RotateCcw size={14} aria-hidden="true" />{:else}Retry preview{/if}</button>{/if}
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
  .open-photo { position: absolute; bottom: 8px; right: 8px; padding: 4px 8px; border: 1px solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-surface); color: var(--text-primary); opacity: 0; }
  .photo-cell:hover .open-photo, .photo-cell:focus-within .open-photo { opacity: 1; }
  @media (pointer: coarse) { .open-photo { opacity: 1; } }
  .retry-preview { position: absolute; bottom: 8px; left: 8px; background: var(--bg-surface); color: var(--text-primary); border: 1px solid var(--border-default); border-radius: var(--radius-sm); padding: 4px 8px; font-size: var(--font-size-xs); }
  .compact .retry-preview { bottom: 2px; left: 2px; display: flex; padding: 3px; }
</style>
