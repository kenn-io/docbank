<script lang="ts">
  import { tick, untrack } from "svelte";
  import { Button } from "@kenn-io/kit-ui";
  import { computeMonthLayout, HEADER_HEIGHT, type PhotoGroup } from "./photoGrid.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import PhotoMonthChunk from "./PhotoMonthChunk.svelte";

  let { groups, targetRowHeight, cache, selectedIDs, onselect, oncheck, onloadmore, scrollTop = $bindable(0) }: {
    groups: PhotoGroup[];
    targetRowHeight: number;
    cache: PhotoPreviewCache;
    selectedIDs: ReadonlySet<string>;
    onselect: (id: string, event: MouseEvent) => void;
    oncheck: (id: string, checked: boolean, range: boolean) => void;
    onloadmore: () => void;
    scrollTop?: number;
  } = $props();
  let container = $state<HTMLDivElement>();
  let width = $state(800);
  let initialized = $state(false);
  let viewport = $state(960);
  const chunks = $derived.by(() => {
    let offset = 0;
    return groups.map(group => {
      const layout = computeMonthLayout(group.items, width, targetRowHeight);
      const chunk = { group, layout, offset };
      offset += layout.intrinsicHeight;
      return chunk;
    });
  });
  const totalHeight = $derived(chunks.reduce((sum, chunk) => sum + chunk.layout.intrinsicHeight, 0));
  const years = $derived([...new Set(groups.map(group => group.year).filter(Boolean))]);

  $effect(() => {
    if (!container) return;
    const element = container;
    const savedTop = untrack(() => scrollTop);
    let current = true;
    const resize = new ResizeObserver(() => {
      width = Math.max(1, element.querySelector<HTMLElement>(".grid")!.clientWidth);
      viewport = element.clientHeight;
    });
    resize.observe(element);
    width = Math.max(1, element.querySelector<HTMLElement>(".grid")!.clientWidth);
    viewport = element.clientHeight;
    void tick().then(() => {
      if (!current) return;
      element.scrollTop = savedTop;
      scrollTop = element.scrollTop;
      initialized = true;
    });
    return () => { current = false; resize.disconnect(); };
  });
  $effect(() => {
    if (initialized && totalHeight < scrollTop + viewport + 800) untrack(onloadmore);
  });

  function jump(year: string) {
    const chunk = chunks.find(chunk => chunk.group.year === year);
    if (chunk && container) {
      container.scrollTo({ top: Math.ceil(chunk.offset) });
      scrollTop = Math.ceil(chunk.offset);
    }
  }

  export function preservePosition() {
    const element = container;
    if (!element) return async () => {};
    const oldTop = element.scrollTop;
    const viewportTop = element.getBoundingClientRect().top;
    const cell = [...element.querySelectorAll<HTMLElement>("[data-asset]")].find(item => item.getBoundingClientRect().bottom > viewportTop + HEADER_HEIGHT);
    const id = cell?.dataset.asset;
    const pixelOffset = cell ? cell.getBoundingClientRect().top - viewportTop : 0;
    return async () => {
      let newTop = oldTop;
      if (id) {
        for (const chunk of chunks) {
          const index = chunk.group.items.findIndex(item => item.asset_id === id);
          if (index < 0) continue;
          const row = chunk.layout.rows.find(row => row.items.some(item => item.index === index));
          if (row) newTop = chunk.offset + HEADER_HEIGHT + row.y - pixelOffset;
          break;
        }
      }
      scrollTop = newTop;
      await tick();
      element.scrollTop = newTop;
      scrollTop = element.scrollTop;
      await tick();
    };
  }
</script>

<div class="photo-scroll" bind:this={container} onscroll={() => { if (initialized) scrollTop = container?.scrollTop ?? 0; }} data-testid="photo-scroll">
  <nav class="year-scrubber" aria-label="Photo years">
    {#each years as year}<Button size="sm" onclick={() => jump(year)}>{year}</Button>{/each}
  </nav>
  <div class="grid" style:height={`${totalHeight}px`}>
    {#if initialized}
    {#each chunks as chunk (chunk.group.key)}
      <PhotoMonthChunk group={chunk.group} layout={chunk.layout} offset={chunk.offset} top={Math.max(0, scrollTop - viewport)} bottom={scrollTop + 2 * viewport} {cache} {selectedIDs} {onselect} {oncheck} />
    {/each}
    {/if}
  </div>
</div>

<style>
  .photo-scroll { position: relative; flex: 1; min-height: 0; overflow: auto; overflow-anchor: none; display: grid; grid-template-columns: minmax(0, 1fr) 64px; column-gap: 8px; align-content: start; padding: 0 8px 0 12px; }
  .year-scrubber { position: sticky; top: 7px; grid-column: 2; grid-row: 1; height: 0; z-index: 3; display: flex; flex-direction: column; align-items: center; gap: 2px; }
  .grid { position: relative; grid-column: 1; grid-row: 1; border-right: 1px solid var(--border-default); }
</style>
