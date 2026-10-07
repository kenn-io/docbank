<script lang="ts">
  import type { PhotoGroup, Row } from "./photoGrid.js";
  import { HEADER_HEIGHT, visibleRows } from "./photoGrid.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import PhotoCell from "./PhotoCell.svelte";

  let { group, layout, offset, top, bottom, cache, selectedIDs, onselect, oncheck }: {
    group: PhotoGroup;
    layout: { rows: Row[]; totalHeight: number; intrinsicHeight: number };
    offset: number;
    top: number;
    bottom: number;
    cache: PhotoPreviewCache;
    selectedIDs: ReadonlySet<string>;
    onselect: (id: string, event: MouseEvent) => void;
    oncheck: (id: string, checked: boolean, range: boolean) => void;
  } = $props();
  const inWindow = $derived(offset <= bottom && offset + layout.intrinsicHeight >= top);
  const rows = $derived(inWindow ? visibleRows(layout.rows, top - offset - HEADER_HEIGHT, bottom - offset - HEADER_HEIGHT) : []);
  const cells = $derived(rows.flatMap(row => row.items.map(cell => ({ photo: group.items[cell.index], x: cell.x, y: row.y, width: cell.width, height: row.height }))));
</script>

<section class="photo-month" data-month={group.key} style:height={`${layout.intrinsicHeight}px`} aria-label={group.label}>
  {#if inWindow}
    <h2>{group.label}<span>{group.items.length} loaded</span></h2>
    <div class="cells" style:height={`${layout.totalHeight}px`}>
      {#each cells as cell (cell.photo.asset_id)}
        {@const photo = cell.photo}
        <div class="cell" style:left={`${cell.x}px`} style:top={`${cell.y}px`} style:width={`${cell.width}px`} style:height={`${cell.height}px`}>
          <PhotoCell {photo} {cache} selected={selectedIDs.has(photo.asset_id)} onclick={event => onselect(photo.asset_id, event)} oncheck={(checked, range) => oncheck(photo.asset_id, checked, range)} />
        </div>
      {/each}
    </div>
  {/if}
</section>

<style>
  .photo-month { position: relative; }
  h2 { position: sticky; top: 0; z-index: 2; background: var(--bg-surface); border-bottom: 1px solid var(--border-default); height: 44px; margin: 0 0 12px; display: flex; align-items: center; justify-content: space-between; gap: 8px; color: var(--text-primary); font-size: var(--font-size-sm); font-weight: 600; }
  h2 span { color: var(--text-muted); font-size: var(--font-size-xs); font-weight: 400; }
  .cells { position: relative; }
  .cell { position: absolute; }
</style>
