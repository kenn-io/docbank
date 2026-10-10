<script lang="ts">
  import { onMount, untrack } from "svelte";
  import { Button, EmptyState, Modal, SelectDropdown, SearchInput, Spinner } from "@kenn-io/kit-ui";
  import ImageIcon from "@lucide/svelte/icons/image";
  import type { Photos } from "./photos.svelte.js";
  import { groupPhotos, ROW_HEIGHTS, type Density } from "./photoGrid.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import { isAppShortcutSuppressed } from "./shortcuts.js";
  import PhotoGrid from "./PhotoGrid.svelte";
  import FacetSidebar from "./FacetSidebar.svelte";
  import SelectionDock from "./SelectionDock.svelte";

  let { photos, cache, title = "Library", ontrashed, onhidden, onactionerror }: { photos: Photos; cache: PhotoPreviewCache; title?: string; ontrashed?: () => void; onhidden?: () => void; onactionerror?: (error: string) => void } = $props();
  let trashOpen = $state(false);
  let grid = $state<{ preservePosition: () => (() => Promise<void>); restoreScrollTop: (top: number) => Promise<void> }>();
  const preserve = () => grid?.preservePosition();
  export function refresh() { return photos.refresh(preserve); }
  async function setHidden(id: string) {
    if (photos.hiding || photos.trashing) return;
    onactionerror?.("");
    await photos.setHidden(id, preserve, onhidden, onactionerror);
  }
  const groups = $derived(groupPhotos(photos.items, photos.query.sort.field === "relevance" ? "relevance" : photos.grouping));
  const filtered = $derived(Boolean(photos.query.text.trim()) || Object.values(photos.query.filters).some(value => Array.isArray(value) ? value.length > 0 : value !== undefined && value !== false && value !== ""));
  const orderedIDs = $derived(groups.flatMap(group => group.items.map(item => item.asset_id)));
  const densityOptions = [{ value: "compact", label: "Compact" }, { value: "comfortable", label: "Comfortable" }, { value: "large", label: "Large" }];
  let search = $state(untrack(() => photos.query.text));
  $effect(() => { search = photos.query.text; });
  const sortOptions = $derived([{ value: "capture_time", label: "Capture date" }, ...(photos.query.text.trim() ? [{ value: "relevance", label: "Relevance" }] : [])]);
  const groupingOptions = [{ value: "months", label: "Months" }, { value: "sessions", label: "Capture sessions" }];

  onMount(() => {
    const top = photos.scrollTop;
    void Promise.resolve(photos.resume(preserve)).then(() => grid?.restoreScrollTop(top));
    return () => photos.cancelPending();
  });
  function relayout(change: () => void) {
    const restore = preserve();
    change();
    void restore?.();
  }
  function escape(event: KeyboardEvent) {
    if (event.key === "Escape" && !isAppShortcutSuppressed(event, false, document, ".photo-cell")) photos.clearSelection();
  }
</script>

<svelte:window onkeydown={escape} />
<main class="photos-workspace" aria-label="Photo library">
  <div class="photo-toolbar browser-toolbar">
    <div class="library-title"><h1>{title}</h1><span>{photos.total.toLocaleString()} photos · {photos.items.length.toLocaleString()} loaded</span></div>
    <div class="toolbar-actions">
      <div class="photo-options">
        <SelectDropdown title="Group photos" disabled={photos.query.sort.field !== "capture_time"} value={photos.grouping} options={groupingOptions} onchange={value => relayout(() => photos.grouping = value as "months" | "sessions")} />
        <SelectDropdown title="Grid density" value={photos.density} options={densityOptions} onchange={value => relayout(() => photos.setDensity(value as Density))} />
      </div>
      <Button size="sm" disabled={photos.loading || photos.hiding || photos.trashing} onclick={() => void photos.refresh(preserve)}>Refresh previews</Button>
    </div>
  </div>
  {#if photos.actionError && !onactionerror}<p role="alert">{photos.actionError}</p>{/if}
  <form class="photo-search" onsubmit={event => { event.preventDefault(); void photos.setQuery({ ...photos.query, text: search, syntax: "simple", sort: { field: search.trim() ? "relevance" : "capture_time", direction: "desc" } }); }}>
    <SearchInput disabled={photos.loading || photos.hiding || photos.trashing} ariaLabel="Search photos" placeholder="Search photos" bind:value={search} />
    <Button type="submit" size="sm" disabled={photos.loading || photos.hiding || photos.trashing}>Search</Button>
    <SelectDropdown disabled={photos.loading || photos.hiding || photos.trashing} title="Sort photos" value={photos.query.sort.field} options={sortOptions} onchange={value => void photos.setQuery({ ...photos.query, sort: { field: value as "capture_time" | "relevance", direction: "desc" } })} />
    {#if filtered}
      <Button type="button" size="sm" disabled={photos.loading || photos.hiding || photos.trashing} onclick={() => { search = ""; void photos.setQuery({ ...photos.query, text: "", filters: {}, sort: { field: "capture_time", direction: "desc" } }); }}>Clear filters</Button>
    {/if}
  </form>
  {#if photos.query.sort.field === "relevance" && photos.total > photos.items.length}
    <p class="photo-search-limit">Showing the best 250 of {photos.total.toLocaleString()} matches. Refine the search or sort by capture date to see all.</p>
  {/if}
  {#if photos.error}
    <div class="photo-error" role="alert"><span>{photos.error}</span><Button size="sm" onclick={() => void photos.retry(preserve)}>Retry</Button></div>
  {/if}
  <div class="photo-browser">
  <div class="photo-facets">
    {#if photos.facetsLoading}<div class="photo-loading" role="status"><Spinner size={14} />Loading counts…</div>{/if}
    {#if photos.facetsError}<div class="photo-error" role="alert"><span>{photos.facetsError}</span>{#if photos.facetsRetryable}<Button size="sm" onclick={() => void photos.retryFacets()}>Retry counts</Button>{/if}</div>{/if}
    <FacetSidebar disabled={photos.loading || photos.hiding || photos.trashing} facets={photos.facets} query={photos.query} unit="photos" onchange={value => void photos.setQuery(value)} />
  </div>
  <div class="photo-results">
  {#if photos.items.length}
    <PhotoGrid bind:this={grid} bind:scrollTop={photos.scrollTop} {groups} targetRowHeight={ROW_HEIGHTS[photos.density]} loading={photos.loading} {cache} hidden={photos.hidden} onhidden={id => void setHidden(id)} selectedIDs={photos.selection.selectedIDs} onselect={(id, event) => photos.select(id, event, orderedIDs)} oncheck={(id, checked, range) => photos.check(id, checked, range, orderedIDs)} onloadmore={() => void photos.loadMore(preserve)} />
  {:else if !photos.loading && !photos.error}
    <EmptyState title={filtered ? "No matching photos" : photos.hidden ? "No hidden photos" : "Your photo library is empty"} description={filtered ? "Clear a filter or try another search." : photos.hidden ? "Choose Hide from a photo's actions menu in Library." : "Import photos with docbank photos import to browse them here."}>
      {#snippet icon()}<ImageIcon size="24" />{/snippet}
    </EmptyState>
  {/if}
  <div class="photo-loading" role="status">{#if photos.loading}<Spinner size={14} />Loading photos…{:else if photos.cursor && !photos.error}<Button size="sm" onclick={() => void photos.loadMore(preserve)}>Load more</Button>{/if}</div>
  </div></div>
  <SelectionDock context="photos" selectedCount={photos.selection.selectedIDs.size} visibleDocumentCount={photos.items.length} onclear={() => photos.clearSelection()} onselectvisible={() => photos.selectLoaded()} ontrash={() => { photos.trashError = ""; trashOpen = true; }} trashDisabled={photos.trashing || photos.hiding || photos.loading} />
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
  .photos-workspace { max-height: calc(100dvh - var(--header-height)); display: flex; flex-direction: column; flex: 1; min-height: 0; overflow: hidden; background: var(--bg-surface); }
  .photo-search { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-2); padding: var(--space-3) var(--space-5); border-bottom: 1px solid var(--border-default); }
  .photo-search-limit { margin: 0; padding: var(--space-3) var(--space-5); color: var(--text-muted); font-size: var(--font-size-sm); }
  .photo-browser { display: flex; flex: 1; min-height: 0; overflow: hidden; }
  .photo-facets { display: flex; flex-direction: column; width: 230px; flex-shrink: 0; overflow: auto; }
  .photo-facets :global(.facets) { flex: 1; }
  .photo-results { display: flex; flex-direction: column; flex: 1; min-width: 0; min-height: 0; }
  @media (max-width: 640px) { .photo-browser { flex-direction: column; } .photo-facets { width: auto; max-height: 180px; } }
  .photo-toolbar { border-bottom: 1px solid var(--border-default); }
  .library-title h1 { margin: 0 0 4px; font-size: var(--font-size-lg); color: var(--text-primary); }
  .library-title span { font-size: var(--font-size-xs); color: var(--text-muted); }
  .photo-options { display: flex; flex-wrap: wrap; gap: var(--space-2); }
  .photo-error { display: flex; align-items: center; gap: var(--space-3); padding: var(--space-3) var(--space-5); color: var(--text-primary); background: var(--bg-inset); }
  .photo-loading { height: 38px; flex-shrink: 0; display: flex; gap: var(--space-2); align-items: center; justify-content: center; padding: var(--space-2); color: var(--text-muted); font-size: var(--font-size-sm); }
</style>
