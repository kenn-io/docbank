<script module lang="ts">
  let lastClick: { image: HTMLElement; time: number; opened: boolean } | undefined;
  if (typeof document !== "undefined") document.addEventListener("click", event => {
    if (lastClick && (!(event.target instanceof Node) || !lastClick.image.contains(event.target))) lastClick = undefined;
  }, true);
</script>

<script lang="ts">
  import { Checkbox, Menu, MenuTrigger, MenuContent, MenuItem } from "@kenn-io/kit-ui";
  import RotateCcw from "@lucide/svelte/icons/rotate-ccw";
  import ImageIcon from "@lucide/svelte/icons/image";
  import type { PhotoGridRow } from "./photoGrid.js";
  import { previewObjectURL, type PhotoPreviewCache } from "./photoPreviewCache.js";

  let { photo, cache, selected, onclick, oncheck, onhidden, hidden = false, onopen, compact = false, ondragstart, ondragend }: {
    photo: PhotoGridRow;
    cache: PhotoPreviewCache;
    selected: boolean;
    onhidden?: () => void;
    hidden?: boolean;
    onclick: (event: MouseEvent, touch: boolean) => void;
    compact?: boolean;
    onopen?: (element: HTMLElement) => void;
    oncheck?: (checked: boolean, range: boolean) => void;
    ondragstart?: (event: DragEvent) => void;
    ondragend?: () => void;
  } = $props();
  let menuOpen = $state(false);
  let imageButton = $state<HTMLButtonElement>();
  let touchClick = false;
  let pointerType: string | undefined;
  let url = $state("");
  let failed = $state(false);
  let errorMessage = $state("");
  let retry = $state(0);
  function openImage(element: HTMLElement): void {
    if (touchClick && !selected) oncheck?.(true, false);
    onopen?.(element);
  }
  function clickImage(event: MouseEvent & { currentTarget: HTMLButtonElement }): void {
    touchClick = event.detail > 0 && (pointerType ?? ("pointerType" in event ? event.pointerType : undefined)) === "touch" && !event.ctrlKey && !event.metaKey && !event.shiftKey && !event.altKey;
    pointerType = undefined;
    if (!touchClick) lastClick = undefined;
    if (!compact && touchClick && event.detail > 1) return;
    if (!compact && touchClick && event.detail === 1) {
      const time = event.timeStamp;
      if (lastClick?.image === event.currentTarget && !lastClick.opened && time - lastClick.time <= 350) {
        lastClick = { image: event.currentTarget, time, opened: true };
        openImage(event.currentTarget);
        return;
      }
      lastClick = { image: event.currentTarget, time, opened: false };
    } else lastClick = undefined;
    onclick(event, touchClick);
  }
  function doubleClickImage(event: MouseEvent & { currentTarget: HTMLButtonElement }): void {
    if (compact || lastClick?.image === event.currentTarget && lastClick.opened) return;
    lastClick = undefined;
    openImage(event.currentTarget);
  }
  $effect(() => {
    const slot = photo.previews.grid;
    void retry;
    url = "";
    failed = false;
    errorMessage = "";
    if (slot.state !== "ready" || !slot.generation_id) return;
    return previewObjectURL(cache, photo.asset_id, slot.generation_id, value => url = value, cause => { failed = true; errorMessage = cause instanceof Error ? cause.message : String(cause); }, retry > 0);
  });
  const placeholder = $derived(failed || photo.previews.grid.state === "failed" ? "Preview failed" : photo.previews.grid.state === "unsupported" ? "Preview unsupported" : photo.previews.grid.state === "missing" ? "Preview pending" : "Loading preview");
</script>

<div class="photo-cell" class:selected class:compact data-asset={photo.asset_id}>
  <button type="button" class="photo-image" bind:this={imageButton} aria-label={`${compact ? "Show" : "Select"} ${photo.name}`} aria-pressed={compact ? undefined : selected} aria-current={compact && selected ? "true" : undefined} onpointerdown={event => pointerType = event.pointerType} onclick={clickImage} ondblclick={doubleClickImage} oncontextmenu={event => { if (onhidden) { event.preventDefault(); menuOpen = true; } }} onkeydown={event => { if (!compact && event.key === "Enter") { event.preventDefault(); onopen?.(event.currentTarget); } }} draggable={!!ondragstart} {ondragstart} {ondragend}>
    {#if url}
      <img src={url} alt={photo.name} draggable="false" onerror={() => { url = ""; failed = true; }} />
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
  {#if onhidden}
    <div class="photo-menu"><Menu bind:open={menuOpen} align="end"><MenuTrigger ariaLabel={`Actions for ${photo.name}`}>•••</MenuTrigger><MenuContent ariaLabel="Photo actions"><MenuItem onselect={onhidden}>{hidden ? "Unhide" : "Hide"}</MenuItem></MenuContent></Menu></div>
  {/if}
  <button type="button" class="open-photo kit-sr-only" aria-label={`Open ${photo.name}`} onclick={event => onopen?.(event.currentTarget)}>Open</button>
  {/if}
  {#if failed}<button type="button" class="retry-preview" aria-label={compact ? `Retry preview for ${photo.name}` : undefined} onclick={() => { if (compact) imageButton?.focus({preventScroll:true}); retry++; }}>{#if compact}<RotateCcw size={14} aria-hidden="true" />{:else}Retry preview{/if}</button>{/if}
</div>

<style>
  .photo-cell { position: relative; height: 100%; background: var(--bg-inset); border-radius: var(--radius-sm); overflow: hidden; }
  .photo-cell.selected { outline: 3px solid var(--accent-blue); outline-offset: -3px; }
  .photo-image { display: block; width: 100%; height: 100%; padding: 0; border: 0; background: transparent; cursor: pointer; color: var(--text-muted); touch-action: manipulation; }
  .photo-image:focus-visible { outline: 3px solid var(--accent-blue); outline-offset: -3px; }
  img { display: block; width: 100%; height: 100%; object-fit: cover; }
  .placeholder { display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 8px; height: 100%; font-size: var(--font-size-xs); }
  .pending { background: var(--bg-surface-hover); }
  .photo-menu { position: absolute; top: 8px; right: 8px; }
  .photo-check { position: absolute; top: 8px; left: 8px; display: flex; }
  .open-photo:focus-visible { position: absolute; bottom: 8px; right: 8px; width: auto; height: auto; margin: 0; padding: 4px 8px; overflow: visible; clip-path: none; white-space: normal; border: 1px solid var(--border-default); border-radius: var(--radius-sm); background: var(--bg-surface); color: var(--text-primary); }
  .retry-preview { position: absolute; bottom: 8px; left: 8px; background: var(--bg-surface); color: var(--text-primary); border: 1px solid var(--border-default); border-radius: var(--radius-sm); padding: 4px 8px; font-size: var(--font-size-xs); }
  .compact .retry-preview { bottom: 2px; left: 2px; display: flex; padding: 3px; }
</style>
