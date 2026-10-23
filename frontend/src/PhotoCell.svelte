<script lang="ts">
  import { Checkbox, Menu, MenuTrigger, MenuContent, MenuItem } from "@kenn-io/kit-ui";
  import RotateCcw from "@lucide/svelte/icons/rotate-ccw";
  import ImageIcon from "@lucide/svelte/icons/image";
  import type { PhotoGridRow } from "./photoGrid.js";
  import { previewObjectURL, type PhotoPreviewCache } from "./photoPreviewCache.js";
  import { photoRotationStyle, photoRotationTransform, observePhotoSize } from "./photoRotation.js";

  let {
    photo,
    cache,
    selected,
    onclick,
    oncheck,
    expanded = false,
    onexpand,
    onhidden,
    hidden = false,
    onopen,
    compact = false,
    ondragstart,
    ondragend,
  }: {
    photo: PhotoGridRow;
    cache: PhotoPreviewCache;
    selected: boolean;
    expanded?: boolean;
    onexpand?: () => void;
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
  const quarterTurn = $derived(photo.rotation === 90 || photo.rotation === 270);
  let width = $state(0);
  let height = $state(0);
  let imageButton = $state<HTMLButtonElement>();
  let lastClick = $state<{ time: number; x: number; y: number; opened: boolean } | undefined>();
  let touchClick = false;
  let pointerType: string | undefined;
  let tapCancelled = false;
  let suppressDoubleClick = false;
  let url = $state("");
  let failed = $state(false);
  let errorMessage = $state("");
  let retry = $state(0);
  function openImage(element: HTMLElement): void {
    if (touchClick && !selected) oncheck?.(true, false);
    onopen?.(element);
  }
  function clickImage(event: MouseEvent & { currentTarget: HTMLButtonElement }): void {
    touchClick =
      event.detail > 0 &&
      (pointerType ?? ("pointerType" in event ? event.pointerType : undefined)) === "touch" &&
      !event.ctrlKey &&
      !event.metaKey &&
      !event.shiftKey &&
      !event.altKey;
    pointerType = undefined;
    suppressDoubleClick = touchClick && tapCancelled;
    tapCancelled = false;
    if (!compact && touchClick && event.detail > 1 && !suppressDoubleClick) return;
    if (!compact && touchClick && (event.detail === 1 || suppressDoubleClick)) {
      const time = event.timeStamp;
      const { clientX: x, clientY: y } = event;
      if (
        lastClick &&
        !lastClick.opened &&
        time - lastClick.time <= 350 &&
        Math.hypot(x - lastClick.x, y - lastClick.y) <= 30
      ) {
        lastClick = { time, x, y, opened: true };
        openImage(event.currentTarget);
        return;
      }
      lastClick = { time, x, y, opened: false };
    } else lastClick = undefined;
    onclick(event, touchClick);
  }
  function doubleClickImage(event: MouseEvent & { currentTarget: HTMLButtonElement }): void {
    if (compact || suppressDoubleClick || lastClick?.opened) return;
    lastClick = undefined;
    openImage(event.currentTarget);
  }
  // A click outside this tile or a pointercancel on any tile ends a pending double-tap; the listener exists only while one is pending.
  $effect(() => {
    if (!lastClick) return;
    const reset = (event: Event) => {
      if (!(event.target instanceof Node) || !imageButton?.contains(event.target))
        lastClick = undefined;
    };
    const cancel = () => {
      lastClick = undefined;
      tapCancelled = true;
    };
    document.addEventListener("click", reset, true);
    document.addEventListener("pointercancel", cancel, true);
    return () => {
      document.removeEventListener("click", reset, true);
      document.removeEventListener("pointercancel", cancel, true);
    };
  });
  $effect(() => {
    const slot = photo.previews.grid;
    void retry;
    url = "";
    failed = false;
    errorMessage = "";
    if (slot.state !== "ready" || !slot.generation_id) return;
    return previewObjectURL(
      cache,
      photo.asset_id,
      slot.generation_id,
      (value) => (url = value),
      (cause) => {
        failed = true;
        errorMessage = cause instanceof Error ? cause.message : String(cause);
      },
      retry > 0,
    );
  });
  const placeholder = $derived(
    failed || photo.previews.grid.state === "failed"
      ? "Preview failed"
      : photo.previews.grid.state === "unsupported"
        ? "Preview unsupported"
        : photo.previews.grid.state === "missing"
          ? "Preview pending"
          : "Loading preview",
  );
</script>

<div
  class="photo-cell"
  class:selected
  class:compact
  data-asset={photo.asset_id}
  {@attach quarterTurn
    ? (element) =>
        observePhotoSize(element, (w, h) => {
          width = w;
          height = h;
        })
    : undefined}
>
  <button
    type="button"
    class="photo-image"
    bind:this={imageButton}
    aria-label={`${compact ? "Show" : "Select"} ${photo.name}`}
    aria-pressed={compact ? undefined : selected}
    aria-current={compact && selected ? "true" : undefined}
    onpointerdown={(event) => (pointerType = event.pointerType)}
    onpointercancel={() => {
      lastClick = undefined;
      tapCancelled = true;
      pointerType = undefined;
    }}
    onclick={clickImage}
    ondblclick={doubleClickImage}
    oncontextmenu={(event) => {
      if (onhidden) {
        event.preventDefault();
        menuOpen = true;
      }
    }}
    onkeydown={(event) => {
      if (!compact && event.key === "Enter") {
        event.preventDefault();
        onopen?.(event.currentTarget);
      }
    }}
    draggable={!!ondragstart}
    {ondragstart}
    {ondragend}
  >
    {#if photo.matched_ai_suggestion}<span class="ai-match">Matched AI suggestion</span>{/if}
    {#if url}
      <img
        src={url}
        alt={photo.name}
        style={quarterTurn
          ? photoRotationStyle(photo.rotation ?? 0, width, height)
          : `width:100%;height:100%;transform:${photoRotationTransform(photo.rotation ?? 0)}`}
        draggable="false"
        onerror={() => {
          url = "";
          failed = true;
        }}
      />
    {:else}
      <span
        class="placeholder"
        title={errorMessage}
        class:pending={placeholder === "Preview pending"}
      >
        <ImageIcon size="24" aria-hidden="true" />
        <span class:kit-sr-only={compact}>{placeholder}</span>
      </span>
    {/if}
  </button>
  {#if !compact}
    <div class="photo-check">
      <Checkbox
        checked={selected}
        ariaLabel={`Select photo ${photo.name}`}
        onchange={(checked) => oncheck?.(checked, false)}
      />
    </div>
    {#if onhidden}
      <div class="photo-menu">
        <Menu bind:open={menuOpen} align="end"
          ><MenuTrigger ariaLabel={`Actions for ${photo.name}`}>•••</MenuTrigger><MenuContent
            ariaLabel="Photo actions"
            ><MenuItem onselect={onhidden}>{hidden ? "Unhide" : "Hide"}</MenuItem></MenuContent
          ></Menu
        >
      </div>
    {/if}
    <button
      type="button"
      class="open-photo kit-sr-only"
      aria-label={`Open ${photo.name}`}
      onclick={(event) => onopen?.(event.currentTarget)}>Open</button
    >
  {/if}
  {#if !compact && photo.stack && photo.stack.asset_ids.length > 1}
    <button
      type="button"
      class="stack-count"
      aria-label={`${expanded ? "Collapse" : "Expand"} stack of ${photo.stack.asset_ids.length} photos`}
      onclick={onexpand}>{photo.stack.asset_ids.length}</button
    >
  {/if}
  {#if failed}<button
      type="button"
      class="retry-preview"
      aria-label={compact ? `Retry preview for ${photo.name}` : undefined}
      onclick={() => {
        if (compact) imageButton?.focus({ preventScroll: true });
        retry++;
      }}
      >{#if compact}<RotateCcw size={14} aria-hidden="true" />{:else}Retry preview{/if}</button
    >{/if}
</div>

<style>
  .ai-match {
    position: absolute;
    inset: auto 3rem var(--space-2) var(--space-2);
    z-index: 1;
    background: var(--bg-surface);
    color: var(--text-primary);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    font-size: 0.75rem;
  }
  .stack-count {
    position: absolute;
    bottom: 8px;
    right: 8px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-primary);
    padding: 4px 8px;
  }
  .photo-cell {
    position: relative;
    height: 100%;
    background: var(--bg-inset);
    border-radius: var(--radius-sm);
    overflow: hidden;
  }
  .photo-cell.selected {
    outline: 3px solid var(--accent-blue);
    outline-offset: -3px;
  }
  .photo-image {
    display: block;
    width: 100%;
    height: 100%;
    padding: 0;
    border: 0;
    background: transparent;
    cursor: pointer;
    color: var(--text-muted);
    touch-action: manipulation;
  }
  .photo-image:focus-visible {
    outline: 3px solid var(--accent-blue);
    outline-offset: -3px;
  }
  img {
    position: absolute;
    top: 50%;
    left: 50%;
    display: block;
    object-fit: cover;
  }
  .placeholder {
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: 8px;
    height: 100%;
    font-size: var(--font-size-xs);
  }
  .pending {
    background: var(--bg-surface-hover);
  }
  .photo-menu {
    position: absolute;
    top: 8px;
    right: 8px;
  }
  .photo-check {
    position: absolute;
    top: 8px;
    left: 8px;
    display: flex;
  }
  .open-photo:focus-visible {
    position: absolute;
    bottom: 8px;
    right: 8px;
    width: auto;
    height: auto;
    margin: 0;
    padding: 4px 8px;
    overflow: visible;
    clip-path: none;
    white-space: normal;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-primary);
  }
  .retry-preview {
    position: absolute;
    bottom: 8px;
    left: 8px;
    background: var(--bg-surface);
    color: var(--text-primary);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    padding: 4px 8px;
    font-size: var(--font-size-xs);
  }
  .compact .retry-preview {
    bottom: 2px;
    left: 2px;
    display: flex;
    padding: 3px;
  }
</style>
