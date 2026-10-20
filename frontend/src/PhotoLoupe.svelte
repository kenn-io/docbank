<script lang="ts">
  import { tick, untrack } from "svelte";
  import { Button, IconButton, Spinner, trapFocus, Notice } from "@kenn-io/kit-ui";
  import X from "@lucide/svelte/icons/x";
  import ChevronLeft from "@lucide/svelte/icons/chevron-left";
  import ChevronRight from "@lucide/svelte/icons/chevron-right";
  import Info from "@lucide/svelte/icons/info";
  import RotateCcw from "@lucide/svelte/icons/rotate-ccw";
  import type { Photos, PhotoRow } from "./photos.svelte.js";
  import type { PhotoBrowseRow } from "./generated/docbank.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import { isAppShortcutSuppressed } from "./shortcuts.js";
  import { APIError } from "./api-transport.js";
  import { previewEligibility } from "./download.js";
  import PhotoInspector from "./PhotoInspector.svelte";
  import type { PhotoAuthoredEditor } from "./photoAuthored.svelte.js";
  import type { PhotoAuthoredPatch, PhotoKeywordEdit } from "./generated/docbank.js";
  import PhotoCell from "./PhotoCell.svelte";
  import { photoFacts } from "./photoFacts.js";
  import { PhotoPreviewLoader, prefetchPreviews } from "./photoPreview.svelte.js";
  import PhotoLoupeImage from "./PhotoLoupeImage.svelte";

  let {
    metadataOpen = $bindable(false),
    editor,
    actionDisabled = false,
    onedit,
    onundo,
    onresponse,
    photo,
    error,
    note = "",
    photos,
    cache,
    items,
    canLoadMore,
    onmove,
    onchoose,
    onclose,
    onretry,
    onreload,
    reloading = false,
    reloadRevision = 0,
  }: {
    metadataOpen?: boolean;
    editor?: PhotoAuthoredEditor;
    actionDisabled?: boolean;
    onedit?: (patch: PhotoAuthoredPatch, keywords?: PhotoKeywordEdit[]) => void;
    onundo?: () => void;
    onresponse?: (
      receipt?: import("./generated/docbank.js").PhotoAuthoredReceipt,
      tags?: import("./generated/docbank.js").Tag[],
    ) => Promise<void>;
    photo?: PhotoBrowseRow;
    error: string;
    note?: string;
    photos: Photos;
    cache: PhotoPreviewCache;
    items: PhotoRow[];
    canLoadMore: boolean;
    onmove: (direction: -1 | 1) => void;
    onchoose: (id: string) => void;
    onclose: () => void;
    onretry: () => void;
    onreload?: () => void;
    reloading?: boolean;
    reloadRevision?: number;
  } = $props();
  let loader = $state.raw<PhotoPreviewLoader>(untrack(() => new PhotoPreviewLoader(photos, cache)));
  const unavailable = $derived(!loader.retrying && !!(error || loader.failure));
  let busy = $state(false);
  let image = $state<PhotoLoupeImage>();
  let dialog = $state<HTMLDivElement>();
  const identity = $derived(photo ? `${photo.asset_id}:${photo.content_version_id}` : "");
  const index = $derived(items.findIndex((item) => item.asset_id === photo?.asset_id));
  const rail = $derived(index < 0 ? [] : items.slice(Math.max(0, index - 4), index + 5));
  const previous = $derived(index > 0);
  const next = $derived(
    index >= 0 && (index < items.length - 1 || (canLoadMore && !photos.error && !photos.loading)),
  );
  const originalAllowed = $derived.by(() => {
    try {
      return !!photo && previewEligibility(photo.media_type, 0).kind === "image";
    } catch {
      return false;
    }
  });
  const facts = $derived(photoFacts(photo));

  $effect(() => {
    void identity;
    void reloadRevision;
    const selected = untrack(() => photo);
    busy = false;
    const active = new PhotoPreviewLoader(photos, cache, selected);
    loader = active;
    void tick().then(() => {
      if (!active.active.signal.aborted && image?.isZoomed()) active.zoom();
    });
    return () => active.dispose();
  });
  $effect(() =>
    prefetchPreviews(
      photos,
      cache,
      index < 0
        ? []
        : items
            .slice(Math.max(0, index - 2), index + 3)
            .filter((item) => item.asset_id !== photo?.asset_id),
    ),
  );
  function retry() {
    if (reloading) return;
    dialog?.focus({ preventScroll: true });
    void loader.retry();
  }
  async function original() {
    if (!photo || busy) return;
    dialog?.focus({ preventScroll: true });
    const selected = photo,
      display = loader,
      active = display.active;
    busy = true;
    try {
      const objectURL = await photos.original(
        selected,
        AbortSignal.any([active.signal, AbortSignal.timeout(30_000)]),
      );
      if (active.signal.aborted) {
        URL.revokeObjectURL(objectURL);
        return;
      }
      await display.show(objectURL, 3, "original");
    } catch (cause) {
      display.fail({
        rank: 3,
        message: cause instanceof Error ? cause.message : String(cause),
        action:
          cause instanceof APIError && cause.code === "photo_display_changed"
            ? "reload"
            : undefined,
      });
    } finally {
      if (!active.signal.aborted) busy = false;
    }
  }
  function move(direction: -1 | 1) {
    dialog?.focus({ preventScroll: true });
    onmove(direction);
  }
  function key(event: KeyboardEvent) {
    if (
      event.altKey ||
      event.ctrlKey ||
      event.metaKey ||
      isAppShortcutSuppressed(
        event,
        false,
        event.currentTarget as HTMLElement,
        ".loupe-toolbar button, .edge button, .filmstrip button, .loupe-error button",
      )
    )
      return;
    if (event.key === "Escape") {
      event.preventDefault();
      onclose();
    } else if (event.key === "ArrowLeft" && previous) {
      event.preventDefault();
      move(-1);
    } else if (event.key === "ArrowRight" && next) {
      event.preventDefault();
      move(1);
    } else if (event.key.toLowerCase() === "i") {
      event.preventDefault();
      metadataOpen = !metadataOpen;
    }
  }
</script>

<div
  bind:this={dialog}
  class="photo-loupe"
  role="dialog"
  aria-modal="true"
  aria-label={photo ? `Photo viewer, ${photo.name}` : "Photo viewer"}
  tabindex="-1"
  onkeydown={key}
  {@attach trapFocus}
>
  <header class="loupe-toolbar">
    <IconButton ariaLabel="Close photo viewer" onclick={onclose}><X size={18} /></IconButton>
    <div class="caption">
      <strong>{photo?.name ?? "Photo"}</strong>{#if index >= 0}<span
          >{index + 1} of {photos.total.toLocaleString()}</span
        >{/if}
    </div>
    <div class="mobile-navigation">
      <IconButton ariaLabel="Previous photo" disabled={!previous} onclick={() => move(-1)}
        ><ChevronLeft size={18} /></IconButton
      ><IconButton ariaLabel="Next photo" disabled={!next} onclick={() => move(1)}
        ><ChevronRight size={18} /></IconButton
      >
    </div>
    <Button
      size="sm"
      disabled={!originalAllowed || busy || loader.quality === "original"}
      onclick={original}
      >{#if busy}<Spinner size={14} />{/if}View original</Button
    >
    <IconButton ariaLabel="Reset zoom" onclick={() => image?.reset()}
      ><RotateCcw size={18} /></IconButton
    >
    <IconButton
      ariaLabel="Metadata"
      ariaPressed={metadataOpen}
      onclick={() => (metadataOpen = !metadataOpen)}><Info size={18} /></IconButton
    >
  </header>
  {#if (items.length || canLoadMore) && photos.error}<div class="loupe-error" role="alert">
      <span>{photos.error}</span><Button
        size="sm"
        disabled={photos.loading}
        onclick={() => {
          dialog?.focus({ preventScroll: true });
          onretry();
        }}>Retry navigation</Button
      >
    </div>{/if}
  {#if note || error || loader.failure}<div class="loupe-error" role="alert">
      {#if note}<span>{note}</span>{/if}{#if error}<span>{error}</span
        >{/if}{#if loader.failure}<span>{loader.failure.message}</span
        >{/if}{#if note || error || loader.failure?.action === "reload"}<Button
          size="sm"
          disabled={reloading}
          onclick={() => {
            dialog?.focus({ preventScroll: true });
            onreload?.();
          }}>Reload photo</Button
        >{/if}{#if (loader.failure?.action === "fit" || loader.failure?.action === "large") && photo}<Button
          size="sm"
          disabled={loader.retrying || reloading}
          onclick={retry}>Retry preview</Button
        >{/if}
    </div>{/if}
  {#if editor?.error}<Notice tone="error" message={editor.error} />{/if}
  {#if editor?.receiptID}<Notice
      tone="success"
      message={editor.undoState.summary}
      actionLabel="Undo last photo edit"
      onaction={onundo}
    />{/if}
  {#if editor?.notice}<Notice tone={editor.noticeTone} message={editor.notice} />{/if}
  <div class="loupe-body">
    <div class="stage">
      <div class="edge previous">
        <IconButton ariaLabel="Previous photo" disabled={!previous} onclick={() => move(-1)}
          ><ChevronLeft size={24} /></IconButton
        >
      </div>
      {#key identity}<PhotoLoupeImage
          rotation={photo?.rotation ?? 0}
          bind:this={image}
          url={loader.url}
          {unavailable}
          name={photo?.name ?? "Photo"}
          onzoom={() => loader.zoom()}
          onmove={move}
        />{/key}
      <div class="edge next">
        <IconButton ariaLabel="Next photo" disabled={!next} onclick={() => move(1)}
          ><ChevronRight size={24} /></IconButton
        >
      </div>
      {#if loader.quality}<span class="quality" aria-live="polite"
          >{loader.quality === "original"
            ? "Original"
            : `${loader.quality[0]!.toUpperCase()}${loader.quality.slice(1)} preview`}</span
        >{/if}
    </div>
    {#if metadataOpen}<aside class="photo-info" aria-label="Photo details">
        {#if editor && onedit}<PhotoInspector
            {editor}
            {actionDisabled}
            {onedit}
            {onresponse}
          />{/if}
        <h2>Photo details</h2>
        <dl>
          {#each facts as [label, value]}<dt>{label}</dt>
            <dd>{value}</dd>{/each}
        </dl>
      </aside>{/if}
  </div>
  {#if rail.length}<nav class="filmstrip" aria-label="Photos in current result">
      {#each rail as item (item.asset_id)}<div class="thumb">
          <PhotoCell
            compact
            photo={item}
            {cache}
            selected={item.asset_id === photo?.asset_id}
            onclick={() => onchoose(item.asset_id)}
          />
        </div>{/each}
    </nav>{/if}
</div>

<style>
  .photo-loupe {
    position: absolute;
    inset: 0;
    z-index: 10;
    display: flex;
    flex-direction: column;
    background: var(--bg-surface);
    color: var(--text-primary);
    outline: none;
  }
  .loupe-toolbar {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    padding: var(--space-3) var(--space-4);
    border-bottom: 1px solid var(--border-default);
  }
  .caption {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .caption strong {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-size: var(--font-size-sm);
  }
  .caption span,
  .quality {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .loupe-body {
    flex: 1;
    min-height: 0;
    display: flex;
  }
  .stage {
    position: relative;
    flex: 1;
    min-width: 0;
    min-height: 0;
    display: flex;
    background: var(--bg-inset);
  }
  .edge {
    position: absolute;
    top: 50%;
    transform: translateY(-50%);
    z-index: 1;
    background: var(--bg-surface);
    border-radius: var(--radius-sm);
  }
  .previous {
    left: 12px;
  }
  .next {
    right: 12px;
  }
  .quality {
    position: absolute;
    bottom: 12px;
    left: 50%;
    transform: translateX(-50%);
    background: var(--bg-surface);
    padding: 4px 8px;
    border-radius: var(--radius-sm);
  }
  .photo-info {
    width: 320px;
    flex-shrink: 0;
    padding: var(--space-5);
    overflow-y: auto;
    border-left: 1px solid var(--border-default);
  }
  h2 {
    margin-top: 0;
    font-size: var(--font-size-sm);
  }
  dl {
    margin: 0;
    font-size: var(--font-size-sm);
  }
  dt {
    color: var(--text-muted);
    margin-top: 16px;
  }
  dd {
    margin: 4px 0 0;
    overflow-wrap: anywhere;
  }
  .filmstrip {
    display: flex;
    gap: 8px;
    padding: 12px;
    justify-content: center;
    overflow: hidden;
    border-top: 1px solid var(--border-default);
  }
  .thumb {
    flex-shrink: 0;
    width: 72px;
    height: 54px;
  }
  .loupe-error {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 12px;
    padding: 8px 16px;
    font-size: var(--font-size-sm);
    background: var(--bg-inset);
  }
  .mobile-navigation {
    display: none;
  }
  @media (max-width: 760px) {
    .loupe-toolbar {
      flex-wrap: wrap;
      gap: 8px;
      padding: 8px;
    }
    .caption {
      min-width: 100px;
    }
    .mobile-navigation {
      display: flex;
    }
    .edge,
    .filmstrip {
      display: none;
    }
    .loupe-body {
      flex-direction: column;
    }
    .stage {
      min-height: 180px;
    }
    .photo-info {
      width: auto;
      max-height: 35%;
      border-left: 0;
      border-top: 1px solid var(--border-default);
      padding: 12px;
    }
  }
</style>
