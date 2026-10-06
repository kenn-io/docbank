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
</script>

<section class="photo-month" data-month={group.key} style:height={`${layout.intrinsicHeight}px`} aria-label={group.label}>
  {#if inWindow}
    <h2>{group.label}<span>{group.items.length} photos</span></h2>
    <div class="cells" style:height={`${layout.totalHeight}px`}>
      {#each rows as row (row.y)}
        {#each row.items as cell (cell.index)}
          {@const photo = group.items[cell.index]}
          <div class="cell" style:left={`${cell.x}px`} style:top={`${row.y}px`} style:width={`${cell.width}px`} style:height={`${row.height}px`}>
            <PhotoCell {photo} {cache} selected={selectedIDs.has(photo.asset_id)} onclick={event => onselect(photo.asset_id, event)} oncheck={(checked, range) => oncheck(photo.asset_id, checked, range)} />
          </div>
        {/each}
      {/each}
    </div>
  {/if}
</section>

<style>
  .photo-month { position: relative; }
  h2 { height: 44px; margin: 0; display: flex; align-items: center; justify-content: space-between; gap: 8px; color: var(--text-primary); font-size: var(--font-size-sm); font-weight: 600; }
  h2 span { color: var(--text-muted); font-size: var(--font-size-xs); font-weight: 400; }
  .cells { position: relative; }
  .cell { position: absolute; }
</style>
