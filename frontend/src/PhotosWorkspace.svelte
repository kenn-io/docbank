<script lang="ts">
  import { onMount } from "svelte";
  import { Button, EmptyState, SelectDropdown, Spinner } from "@kenn-io/kit-ui";
  import ImageIcon from "@lucide/svelte/icons/image";
  import type { Photos } from "./photos.svelte.js";
  import { groupPhotos, ROW_HEIGHTS, type Density } from "./photoGrid.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import PhotoGrid from "./PhotoGrid.svelte";
  import SelectionDock from "./SelectionDock.svelte";

  let { photos, cache }: { photos: Photos; cache: PhotoPreviewCache } = $props();
  let grid = $state<{ preservePosition: () => (() => Promise<void>) }>();
  const preserve = () => grid?.preservePosition();
  const groups = $derived(groupPhotos(photos.items, photos.grouping));
  const orderedIDs = $derived(groups.flatMap(group => group.items.map(item => item.asset_id)));
  const densityOptions = [{ value: "compact", label: "Compact" }, { value: "comfortable", label: "Comfortable" }, { value: "large", label: "Large" }];
  const groupingOptions = [{ value: "months", label: "Months" }, { value: "sessions", label: "Capture sessions" }];

  onMount(() => {
    if (!photos.started && !photos.error) void photos.loadMore(preserve);
  });
  function escape(event: KeyboardEvent) {
    if (event.key === "Escape" && !(event.target instanceof Element && event.target.closest("input, select, textarea, [role=dialog]"))) photos.clearSelection();
  }
</script>

<svelte:window onkeydown={escape} />
<main class="photos-workspace" aria-label="Photo library">
  <div class="photo-toolbar">
    <div class="library-title"><h1>Library</h1><span>{photos.total.toLocaleString()} photos · {photos.items.length.toLocaleString()} loaded</span></div>
    <div class="photo-controls">
      <SelectDropdown title="Group photos" value={photos.grouping} options={groupingOptions} onchange={value => photos.grouping = value as "months" | "sessions"} />
      <SelectDropdown title="Grid density" value={photos.density} options={densityOptions} onchange={value => photos.setDensity(value as Density)} />
      <Button size="sm" disabled={photos.loading} onclick={() => void photos.refresh(preserve)}>Refresh previews</Button>
    </div>
  </div>
  {#if photos.error}
    <div class="photo-error" role="alert"><span>{photos.error}</span><Button size="sm" onclick={() => void photos.retry(preserve)}>Retry</Button></div>
  {/if}
  {#if photos.items.length}
    <PhotoGrid bind:this={grid} bind:scrollTop={photos.scrollTop} {groups} targetRowHeight={ROW_HEIGHTS[photos.density]} {cache} selectedIDs={photos.selection.selectedIDs} onselect={(id, event) => photos.select(id, event, orderedIDs)} oncheck={(id, checked, range) => photos.check(id, checked, range, orderedIDs)} onloadmore={() => void photos.loadMore(preserve)} />
  {:else if !photos.loading && !photos.error}
    <EmptyState title="Your photo library is empty" description="Import photos with docbank photos import to browse them here.">
      {#snippet icon()}<ImageIcon size="24" />{/snippet}
    </EmptyState>
  {/if}
  <div class="photo-loading" role="status">{#if photos.loading}<Spinner size={14} />Loading photos…{:else if photos.cursor && !photos.error}<Button size="sm" onclick={() => void photos.loadMore(preserve)}>Load more</Button>{/if}</div>
  <SelectionDock context="photos" selectedCount={photos.selection.selectedIDs.size} visibleDocumentCount={photos.items.length} truncated={photos.items.length < photos.total} onclear={() => photos.clearSelection()} onselectvisible={() => photos.selectLoaded()} />
</main>

<style>
  .photos-workspace { max-height: calc(100dvh - var(--header-height)); display: flex; flex-direction: column; flex: 1; min-height: 0; overflow: hidden; background: var(--bg-surface); }
  .photo-toolbar { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: var(--space-3); padding: var(--space-4) var(--space-5); border-bottom: 1px solid var(--border-default); }
  .library-title h1 { margin: 0 0 4px; font-size: var(--font-size-lg); color: var(--text-primary); }
  .library-title span { font-size: var(--font-size-xs); color: var(--text-muted); }
  .photo-controls { display: flex; flex-wrap: wrap; gap: var(--space-2); }
  .photo-error { display: flex; align-items: center; gap: var(--space-3); padding: var(--space-3) var(--space-5); color: var(--text-primary); background: var(--bg-inset); }
  .photo-loading { height: 38px; flex-shrink: 0; display: flex; gap: var(--space-2); align-items: center; justify-content: center; padding: var(--space-2); color: var(--text-muted); font-size: var(--font-size-sm); }
  @media (max-width: 640px) { .photo-toolbar { padding: var(--space-3); } }
</style>
