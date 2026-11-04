<script lang="ts">
  import { tick, untrack } from "svelte";
  import {
    createPhotoChunks,
    chunkAt,
    layoutRow,
    monthAt,
    type PhotoGridRow,
    rowTop,
    seekOffset,
    type PhotoChunk,
    type PhotoGroup,
  } from "./photoGrid.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import type { CaptureDayFacet } from "./photoTimeline.js";
  import PhotoMonthChunk from "./PhotoMonthChunk.svelte";
  import PhotoTimeline from "./PhotoTimeline.svelte";

  let {
    groups,
    loadedItems,
    narrow = false,
    timeline,
    timelineError = "",
    timelineLoading = false,
    timelineDisabled = false,
    timelineHint = "",
    onretrytimeline,
    onjump,
    more = false,
    seek = $bindable(""),
    targetRowHeight,
    loading,
    cache,
    selectedIDs,
    onselect,
    oncheck,
    expandedStacks = new Set<string>(),
    onexpand,
    onhidden,
    hidden = false,
    onopen,
    onloadmore,
    ondragstart,
    ondragend,
    scrollTop = $bindable(0),
  }: {
    groups: PhotoGroup[];
    loadedItems: PhotoGridRow[];
    narrow?: boolean;
    timeline?: CaptureDayFacet;
    timelineError?: string;
    timelineLoading?: boolean;
    timelineDisabled?: boolean;
    timelineHint?: string;
    onretrytimeline?: () => void;
    // Without onjump the grid shows no timeline.
    onjump?: (month: string) => void;
    more?: boolean;
    // The month a timeline choice is scrolling to. It stays set while pages load, and any
    // scroll the grid did not make clears it.
    seek?: string;
    targetRowHeight: number;
    loading: boolean;
    cache: PhotoPreviewCache;
    selectedIDs: ReadonlySet<string>;
    onselect: (id: string, event: MouseEvent, touch: boolean) => void;
    onopen: (id: string, element: HTMLElement) => void;
    oncheck: (id: string, checked: boolean, range: boolean) => void;
    expandedStacks?: ReadonlySet<string>;
    onexpand?: (id: string) => void;
    onhidden?: (id: string) => void;
    hidden?: boolean;
    onloadmore: () => void;
    ondragstart?: (id: string, event: DragEvent) => void;
    ondragend?: () => void;
    scrollTop?: number;
  } = $props();
  let container = $state<HTMLDivElement>();
  let width = $state(800);
  let initialized = $state(false);
  let viewport = $state(960);
  const buildChunks = createPhotoChunks();
  const chunks: PhotoChunk[] = $derived(buildChunks(groups, width, targetRowHeight));
  const totalHeight = $derived(
    chunks.length ? chunks.at(-1)!.offset + chunks.at(-1)!.layout.intrinsicHeight : 0,
  );
  const firstChunk = $derived(chunkAt(chunks, Math.max(0, scrollTop - viewport)));
  const windowChunks = $derived.by(() => {
    const end = chunkAt(chunks, scrollTop + 2 * viewport);
    return chunks.slice(firstChunk, Math.min(chunks.length, end + 1));
  });
  const showTimeline = $derived(
    !!onjump &&
      (!!timelineError ||
        timelineLoading ||
        !!timeline?.values.length ||
        (timeline?.missing ?? 0) > 0),
  );
  // The chosen year stays open until a scroll the grid did not make.
  let chosenYear = $state(untrack(() => seek.slice(0, 4)));
  const current = $derived(monthAt(chunks, scrollTop));
  function choose(month: string) {
    onjump?.(month);
    chosenYear = month.slice(0, 4);
  }
  // Every scroll the grid makes goes through here and records where the browser actually put it,
  // so a clamped position is never mistaken for the user's scroll.
  function place(top: number) {
    if (!container) return;
    container.scrollTop = top;
    scrollTop = container.scrollTop;
  }

  $effect(() => {
    if (!container) return;
    const element = container;
    const grid = element.querySelector<HTMLElement>(".grid")!;
    const savedTop = untrack(() => scrollTop);
    let current = true;
    const measure = () => {
      const style = getComputedStyle(grid);
      const nextWidth = Math.max(
        1,
        (Number.parseFloat(style.width) || grid.clientWidth) -
          Number.parseFloat(style.paddingRight) -
          (Number.parseFloat(style.borderRightWidth) || 0),
      );
      const restore = untrack(() =>
        initialized && nextWidth !== width ? preservePosition() : undefined,
      );
      width = nextWidth;
      viewport = element.clientHeight;
      void restore?.();
    };
    const resize = new ResizeObserver(measure);
    resize.observe(element);
    resize.observe(grid);
    measure();
    void tick().then(() => {
      if (!current) return;
      place(savedTop);
      initialized = true;
    });
    return () => {
      current = false;
      resize.disconnect();
    };
  });
  $effect(() => {
    if (initialized && container && container.scrollTop !== scrollTop)
      container.scrollTop = scrollTop;
  });
  $effect(() => {
    if (!initialized || loading) return;
    if (seek) {
      const offset = seekOffset(chunks, seek, !more, loadedItems);
      // Done once the month can sit at the top of the view, or the list ends.
      const done = !more || (offset !== undefined && offset + viewport <= totalHeight);
      // A loaded month scrolls now, so it still works when paging has failed.
      if (offset !== undefined || done) place(Math.ceil(offset ?? totalHeight));
      if (done) seek = "";
      else untrack(onloadmore);
    } else if (totalHeight < scrollTop + viewport + 800) untrack(onloadmore);
  });

  export function preservePosition() {
    const element = container;
    if (!element) return async () => {};
    const oldTop = element.scrollTop;
    const viewportTop = element.getBoundingClientRect().top;
    const cell = [...element.querySelectorAll<HTMLElement>("[data-asset]")].find((item) => {
      const group = item.closest<HTMLElement>("[data-month]");
      const chunk = windowChunks.find((chunk) => chunk.group.key === group?.dataset.month);
      return item.getBoundingClientRect().bottom > viewportTop + (chunk?.layout.contentOffset ?? 0);
    });
    const id = cell?.dataset.asset;
    const key = cell?.closest<HTMLElement>("[data-month]")?.dataset.month;
    const chunkIndex = firstChunk + windowChunks.findIndex((chunk) => chunk.group.key === key);
    const itemIndex = Number(cell?.closest<HTMLElement>("[data-index]")?.dataset.index ?? -1);
    const pixelOffset = cell ? cell.getBoundingClientRect().top - viewportTop : 0;
    return async () => {
      let newTop = oldTop;
      if (id) {
        const own =
          chunks[chunkIndex]?.group.key === key
            ? chunks[chunkIndex]
            : chunks.find((chunk) => chunk.group.key === key);
        const candidates = own ? [own] : chunks;
        for (const chunk of candidates) {
          const index =
            chunk.group.items[itemIndex]?.asset_id === id
              ? itemIndex
              : chunk.group.items.findIndex((item) => item.asset_id === id);
          if (index < 0) continue;
          const row = layoutRow(chunk, index);
          if (row) newTop = rowTop(chunk, row) - pixelOffset;
          break;
        }
      }
      scrollTop = newTop;
      await tick();
      place(newTop);
      await tick();
    };
  }
</script>

{#snippet dates(compact: boolean)}
  <PhotoTimeline
    facet={timeline}
    loading={timelineLoading}
    error={timelineError}
    chosen={chosenYear}
    {current}
    {compact}
    disabled={timelineDisabled}
    hint={timelineHint}
    onselect={choose}
    onretry={() => onretrytimeline?.()}
  />
{/snippet}

{#if showTimeline && narrow}{@render dates(true)}{/if}
<div
  class="photo-scroll"
  class:with-years={showTimeline && !narrow}
  bind:this={container}
  onscroll={() => {
    if (initialized && container && container.scrollTop !== scrollTop) {
      scrollTop = container.scrollTop;
      seek = "";
      chosenYear = "";
    }
  }}
  data-testid="photo-scroll"
>
  {#if showTimeline && !narrow}
    <div class="year-scrubber" style:--timeline-height={`calc(${viewport}px - 2 * var(--space-5))`}>
      {@render dates(false)}
    </div>
  {/if}
  <div class="grid" style:height={`${totalHeight}px`}>
    {#if initialized}
      <div style:position="relative" style:top={`${windowChunks[0]?.offset ?? 0}px`}>
        {#each windowChunks as chunk (chunk.group.key)}
          <PhotoMonthChunk
            group={chunk.group}
            layout={chunk.layout}
            offset={chunk.offset}
            top={Math.max(0, scrollTop - viewport)}
            bottom={scrollTop + 2 * viewport}
            {cache}
            {selectedIDs}
            {onselect}
            {oncheck}
            {onopen}
            {expandedStacks}
            {onexpand}
            {onhidden}
            {hidden}
            {ondragstart}
            {ondragend}
          />
        {/each}
      </div>
    {/if}
  </div>
</div>

<style>
  .photo-scroll {
    position: relative;
    flex: 1;
    min-height: 0;
    overflow: auto;
    overflow-anchor: none;
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    column-gap: var(--space-5);
    align-content: start;
    padding: 0 var(--space-5);
  }
  .with-years {
    grid-template-columns: minmax(0, 1fr) max-content;
  }
  .year-scrubber {
    position: sticky;
    top: var(--space-5);
    grid-column: 2;
    grid-row: 1;
    height: 0;
    z-index: 3;
  }
  .grid {
    position: relative;
    grid-column: 1;
    grid-row: 1;
  }
</style>
