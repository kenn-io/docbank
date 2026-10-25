<script lang="ts">
  import type { PhotoGroup, Row } from "./photoGrid.js";
  import { STICKY_HEADER_HEIGHT, visibleRows } from "./photoGrid.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import PhotoCell from "./PhotoCell.svelte";

  let {
    group,
    layout,
    offset,
    top,
    bottom,
    cache,
    selectedIDs,
    onselect,
    oncheck,
    expandedStacks = new Set<string>(),
    onexpand,
    onhidden,
    hidden = false,
    onopen,
    ondragstart,
    ondragend,
  }: {
    group: PhotoGroup;
    layout: { rows: Row[]; totalHeight: number; intrinsicHeight: number; contentOffset: number };
    offset: number;
    top: number;
    bottom: number;
    cache: PhotoPreviewCache;
    selectedIDs: ReadonlySet<string>;
    onselect: (id: string, event: MouseEvent, touch: boolean) => void;
    expandedStacks?: ReadonlySet<string>;
    onexpand?: (id: string) => void;
    onhidden?: (id: string) => void;
    hidden?: boolean;
    onopen: (id: string, element: HTMLElement) => void;
    oncheck: (id: string, checked: boolean, range: boolean) => void;
    ondragstart?: (id: string, event: DragEvent) => void;
    ondragend?: () => void;
  } = $props();
  const inWindow = $derived(offset <= bottom && offset + layout.intrinsicHeight >= top);
  const rows = $derived(
    inWindow
      ? visibleRows(
          layout.rows,
          top - offset - layout.contentOffset,
          bottom - offset - layout.contentOffset,
        )
      : [],
  );
  const cells = $derived(
    rows.flatMap((row) =>
      row.items.map((cell) => ({
        photo: group.items[cell.index],
        x: cell.x,
        y: row.y,
        width: cell.width,
        height: row.height,
      })),
    ),
  );
</script>

<section
  class="photo-month"
  data-month={group.key}
  style:height={`${layout.intrinsicHeight}px`}
  aria-label={group.label}
>
  {#if inWindow}
    {#if group.key !== "relevance"}<h2 style:height={`${STICKY_HEADER_HEIGHT}px`}>
        {group.label}
      </h2>{/if}
    <div
      class="cells"
      style:top={`${layout.contentOffset}px`}
      style:height={`${layout.totalHeight}px`}
    >
      {#each cells as cell (cell.photo.asset_id)}
        {@const photo = cell.photo}
        <div
          class="cell"
          style:left={`${cell.x}px`}
          style:top={`${cell.y}px`}
          style:width={`${cell.width}px`}
          style:height={`${cell.height}px`}
        >
          <PhotoCell
            {hidden}
            onhidden={() => onhidden?.(photo.asset_id)}
            onexpand={onexpand ? () => onexpand(photo.asset_id) : undefined}
            expanded={!!photo.stack && expandedStacks.has(photo.stack.asset_ids[0]!)}
            {photo}
            {cache}
            selected={selectedIDs.has(photo.asset_id)}
            onclick={(event, touch) => onselect(photo.asset_id, event, touch)}
            oncheck={(checked, range) => oncheck(photo.asset_id, checked, range)}
            onopen={(element) => onopen(photo.asset_id, element)}
            ondragstart={ondragstart ? (event) => ondragstart?.(photo.asset_id, event) : undefined}
            {ondragend}
          />
        </div>
      {/each}
    </div>
  {/if}
</section>

<style>
  .photo-month {
    position: relative;
  }
  h2 {
    position: sticky;
    top: 0;
    z-index: 2;
    background: var(--bg-surface);
    border-bottom: 1px solid var(--border-default);
    margin: 0;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    font-weight: 600;
  }
  .cells {
    position: absolute;
    width: 100%;
  }
  .cell {
    position: absolute;
  }
</style>
