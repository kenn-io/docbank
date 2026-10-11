<script lang="ts">
  import { onMount, tick } from "svelte";
  import { Button, EmptyState, Modal, SelectDropdown, Spinner } from "@kenn-io/kit-ui";
  import ImageIcon from "@lucide/svelte/icons/image";
  import type { Photos, PhotoReturn } from "./photos.svelte.js";
  import { groupPhotos, ROW_HEIGHTS, type Density } from "./photoGrid.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import { isAppShortcutSuppressed } from "./shortcuts.js";
  import PhotoGrid from "./PhotoGrid.svelte";
  import PhotoLoupe from "./PhotoLoupe.svelte";
  import type { PhotoBrowseRow } from "./generated/docbank.js";
  import SelectionDock from "./SelectionDock.svelte";

  let { photos, cache, photoID, onphotochange, ontrashed }: { photos: Photos; cache: PhotoPreviewCache; ontrashed?: () => void; photoID: string; onphotochange: (id: string, mode: "open" | "step" | "close") => void } = $props();
  let trashOpen = $state(false);
  let direct = $state<PhotoBrowseRow>();
  let detailError = $state("");
  let detailLoading = $state(false);
  let reload = $state.raw<{id:string}>();
  let consumedReload: typeof reload;
  let reloadRevision = $state(0);
  let detailController: AbortController | undefined;
  const fromGrid = $derived(photos.viewerReturn?.contextID === photos.contextID);
  const listed = $derived(photos.items.some(item => item.asset_id === photoID));
  const membershipNote = $derived(photoID && fromGrid && !listed && !photos.loading ? "This photo is no longer in the refreshed result." : "");
  let localReturn: PhotoReturn | undefined;
  let openingElement: HTMLElement | undefined;
  let openingID = "";
  let restorePosition: (() => Promise<void>) | undefined;
  $effect(() => {
    const id = photoID;
    if (!id) { direct = undefined; detailError = ""; if (fromGrid) void restoreGrid(); return; }
    const controller = new AbortController();
    detailController = controller;
    const explicit = reload?.id === id && reload !== consumedReload;
    if (!explicit) { direct = undefined; detailError = ""; }
    if (!explicit && fromGrid && listed) { detailLoading = false; return () => controller.abort(); }
    detailLoading = true;
    if (explicit) consumedReload = reload;
    void photos.lookup(id, AbortSignal.any([controller.signal, AbortSignal.timeout(30_000)])).then(photo => { if (!controller.signal.aborted) { const index = photos.items.findIndex(item => item.asset_id === id); if (index >= 0) photos.items[index] = photo; direct = photo; detailError = ""; if (explicit) reloadRevision++; } }).catch(cause => { if (!controller.signal.aborted) detailError = cause instanceof Error ? cause.message : String(cause); }).finally(() => { if (!controller.signal.aborted) detailLoading = false; });
    return () => controller.abort();
  });
  async function restoreGrid() {
    const record = photos.viewerReturn!;
    await tick();
    if (photoID || photos.viewerReturn !== record) return;
    if (record === localReturn) await restorePosition?.();
    await tick();
    const control = record.control === "check" ? "input[type=checkbox]" : record.control === "open" ? ".open-photo" : ".photo-image";
    const target = record === localReturn && openingElement?.isConnected ? openingElement : document.querySelector<HTMLElement>(`[data-asset="${record.assetID}"] ${control}`);
    target?.focus({ preventScroll: true });
    if (!photoID && photos.viewerReturn === record) photos.viewerReturn = undefined;
  }
  export function captureReturn(element: HTMLElement = document.activeElement as HTMLElement) {
    openingElement = element; openingID = element.closest<HTMLElement>("[data-asset]")?.dataset.asset ?? ""; restorePosition = preserve();
    photos.viewerReturn = {contextID:photos.contextID, assetID:openingID, control:element.matches(".open-photo") ? "open" : element.matches("input[type=checkbox]") ? "check" : "image", scrollTop:photos.scrollTop};
    localReturn = photos.viewerReturn;
  }
  function open(id: string, element: HTMLElement) {
    captureReturn(element);
    onphotochange(id, "open");
  }
  async function move(direction: -1 | 1) {
    const controller = detailController;
    if (!fromGrid || !current || !controller) return;
    const id = current.asset_id;
    const index = ordered.findIndex(item => item.asset_id === id);
    if (index < 0) return;
    if (direction === 1 && index === ordered.length - 1 && photos.cursor) await photos.loadMore(preserve);
    if (controller.signal.aborted || photoID !== id) return;
    const currentIndex = ordered.findIndex(item => item.asset_id === id);
    if (currentIndex < 0) return;
    const next = ordered[currentIndex + direction];
    if (next) onphotochange(next.asset_id, "step");
  }
  async function retryNavigation() {
    await photos.retry(preserve);
  }
  function select(id: string, event: MouseEvent) {
    const touch = "pointerType" in event && event.pointerType === "touch";
    if (touch && !event.shiftKey && !event.ctrlKey && !event.metaKey) {
      if (photos.selection.selectedIDs.size) photos.check(id, !photos.selection.selectedIDs.has(id), false, orderedIDs);
      else open(id, event.currentTarget as HTMLElement);
    }
    else photos.select(id, event, orderedIDs);
  }
  let grid = $state<{ preservePosition: () => (() => Promise<void>) }>();
  const preserve = () => grid?.preservePosition();
  const groups = $derived(groupPhotos(photos.items, photos.grouping));
  const ordered = $derived(photos.items);
  const current = $derived(photoID ? (fromGrid ? ordered.find(item => item.asset_id === photoID) ?? direct : direct) : undefined);
  const orderedIDs = $derived(groups.flatMap(group => group.items.map(item => item.asset_id)));
  const densityOptions = [{ value: "compact", label: "Compact" }, { value: "comfortable", label: "Comfortable" }, { value: "large", label: "Large" }];
  const groupingOptions = [{ value: "months", label: "Months" }, { value: "sessions", label: "Capture sessions" }];

  onMount(() => {
    void photos.resume(preserve);
    return () => { photos.cancelPending(); openingElement = undefined; restorePosition = undefined; localReturn = undefined; };
  });
  function relayout(change: () => void) {
    const restore = preserve();
    change();
    void restore?.();
  }
  function escape(event: KeyboardEvent) {
    if (!photoID && event.key === "Escape" && !isAppShortcutSuppressed(event, false, document, ".photo-cell")) photos.clearSelection();
  }
</script>

<svelte:window onkeydown={escape} />
<main class="photos-workspace" aria-label="Photo library">
  <div class="library-grid" inert={!!photoID}>
  <div class="photo-toolbar browser-toolbar">
    <div class="library-title"><h1>Library</h1><span>{photos.total.toLocaleString()} photos · {photos.items.length.toLocaleString()} loaded</span></div>
    <div class="toolbar-actions">
      <div class="photo-options">
        <SelectDropdown title="Group photos" value={photos.grouping} options={groupingOptions} onchange={value => relayout(() => photos.grouping = value as "months" | "sessions")} />
        <SelectDropdown title="Grid density" value={photos.density} options={densityOptions} onchange={value => relayout(() => photos.setDensity(value as Density))} />
      </div>
      <Button size="sm" disabled={photos.loading} onclick={() => void photos.refresh(preserve)}>Refresh previews</Button>
    </div>
  </div>
  {#if photos.error}
    <div class="photo-error" role="alert"><span>{photos.error}</span><Button size="sm" onclick={() => void photos.retry(preserve)}>Retry</Button></div>
  {/if}
  {#if photos.items.length}
    <PhotoGrid bind:this={grid} bind:scrollTop={photos.scrollTop} {groups} targetRowHeight={ROW_HEIGHTS[photos.density]} loading={photos.loading || !!photoID} {cache} selectedIDs={photos.selection.selectedIDs} onselect={select} onopen={open} oncheck={(id, checked, range) => photos.check(id, checked, range, orderedIDs)} onloadmore={() => void photos.loadMore(preserve)} />
  {:else if !photos.loading && !photos.error}
    <EmptyState title="Your photo library is empty" description="Import photos with docbank photos import to browse them here.">
      {#snippet icon()}<ImageIcon size="24" />{/snippet}
    </EmptyState>
  {/if}
  <div class="photo-loading" role="status">{#if photos.loading}<Spinner size={14} />Loading photos…{:else if photos.cursor && !photos.error}<Button size="sm" onclick={() => void photos.loadMore(preserve)}>Load more</Button>{/if}</div>
  {#if !photoID}<SelectionDock context="photos" selectedCount={photos.selection.selectedIDs.size} visibleDocumentCount={photos.items.length} onclear={() => photos.clearSelection()} onselectvisible={() => photos.selectLoaded()} ontrash={() => { photos.trashError = ""; trashOpen = true; }} trashDisabled={photos.trashing || photos.loading} />{/if}
  </div>
  {#if photoID}<PhotoLoupe photo={current} error={detailError} note={membershipNote} reloading={detailLoading} {reloadRevision} onreload={() => reload = {id:photoID}} onretry={retryNavigation} {photos} {cache} items={fromGrid ? ordered : []} canLoadMore={fromGrid && !!photos.cursor} onmove={move} onchoose={id => onphotochange(id, "step")} onclose={() => onphotochange("", "close")} />{/if}
</main>

{#if trashOpen}
  <Modal title="Move selected photos to trash?" tone="danger" ariaLabel="Move selected photos to trash" onclose={() => { if (!photos.trashing) trashOpen = false; }} closeOnOverlayClick={!photos.trashing}>
    <p>Move {photos.selection.selectedIDs.size} selected photo{photos.selection.selectedIDs.size === 1 ? "" : "s"} and every RAW, image, video, and sidecar member to recoverable trash. Stored files and album membership stay intact.</p>
    {#if photos.trashError}<p role="alert">{photos.trashError} Failed photos remain selected for retry.</p>{/if}
    {#snippet footer()}
      <Button disabled={photos.trashing} onclick={() => trashOpen = false}>Keep in Docbank</Button>
      <Button tone="danger" disabled={photos.trashing || photos.selection.selectedIDs.size === 0} onclick={async () => { if (await photos.trashSelected(preserve, ontrashed)) trashOpen = false; }}>{photos.trashing ? "Moving…" : "Move to trash"}</Button>
    {/snippet}
  </Modal>
{/if}

<style>
  .library-grid { display: flex; flex-direction: column; flex: 1; min-height: 0; }
  .photos-workspace { position: relative; max-height: calc(100dvh - var(--header-height)); display: flex; flex-direction: column; flex: 1; min-height: 0; overflow: hidden; background: var(--bg-surface); }
  .photo-toolbar { border-bottom: 1px solid var(--border-default); }
  .library-title h1 { margin: 0 0 4px; font-size: var(--font-size-lg); color: var(--text-primary); }
  .library-title span { font-size: var(--font-size-xs); color: var(--text-muted); }
  .photo-options { display: flex; flex-wrap: wrap; gap: var(--space-2); }
  .photo-error { display: flex; align-items: center; gap: var(--space-3); padding: var(--space-3) var(--space-5); color: var(--text-primary); background: var(--bg-inset); }
  .photo-loading { height: 38px; flex-shrink: 0; display: flex; gap: var(--space-2); align-items: center; justify-content: center; padding: var(--space-2); color: var(--text-muted); font-size: var(--font-size-sm); }
</style>
