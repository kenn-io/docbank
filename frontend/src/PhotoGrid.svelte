<script lang="ts">
  import { Button } from "@kenn-io/kit-ui";
  import { computeMonthLayout, type PhotoGroup } from "./photoGrid.js";
  import type { PhotoPreviewCache } from "./photoPreviewCache.js";
  import PhotoMonthChunk from "./PhotoMonthChunk.svelte";

  let { groups, targetRowHeight, cache, selectedIDs, onselect, oncheck, onloadmore }: {
    groups: PhotoGroup[];
    targetRowHeight: number;
    cache: PhotoPreviewCache;
    selectedIDs: ReadonlySet<string>;
    onselect: (id: string, event: MouseEvent) => void;
    oncheck: (id: string, checked: boolean, range: boolean) => void;
    onloadmore: () => void;
  } = $props();
  let container = $state<HTMLDivElement>();
  let width = $state(800);
  let scrollTop = $state(0);
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
  const activeLabel = $derived(chunks.find(chunk => chunk.offset + chunk.layout.intrinsicHeight > scrollTop + 1)?.group.label ?? "");
  const years = $derived([...new Set(groups.map(group => group.year).filter(Boolean))]);

  $effect(() => {
    if (!container) return;
    const element = container;
    const resize = new ResizeObserver(() => {
      width = Math.max(1, element.clientWidth - 64);
      viewport = element.clientHeight;
    });
    resize.observe(element);
    width = Math.max(1, element.clientWidth - 64);
    viewport = element.clientHeight;
    return () => resize.disconnect();
  });
  $effect(() => {
    if (totalHeight < scrollTop + viewport + 800) onloadmore();
  });

  function jump(year: string) {
    const chunk = chunks.find(chunk => chunk.group.year === year);
    if (chunk && container) {
      container.scrollTo({ top: Math.ceil(chunk.offset) });
      scrollTop = Math.ceil(chunk.offset);
    }
  }
</script>

<div class="photo-scroll" bind:this={container} onscroll={() => scrollTop = container?.scrollTop ?? 0} data-testid="photo-scroll">
  <div class="sticky-month" aria-live="polite">{activeLabel}</div>
  <nav class="year-scrubber" aria-label="Photo years">
    {#each years as year}<Button size="sm" onclick={() => jump(year)}>{year}</Button>{/each}
  </nav>
  <div class="grid" style:height={`${totalHeight}px`}>
    {#each chunks as chunk (chunk.group.key)}
      <PhotoMonthChunk group={chunk.group} layout={chunk.layout} offset={chunk.offset} top={Math.max(0, scrollTop - viewport)} bottom={scrollTop + 2 * viewport} {cache} {selectedIDs} {onselect} {oncheck} />
    {/each}
  </div>
</div>

<style>
  .photo-scroll { position: relative; flex: 1; min-height: 0; overflow: auto; padding: 0 52px 0 12px; }
  .sticky-month { position: sticky; top: 0; height: 36px; display: flex; align-items: center; background: var(--bg-surface); border-bottom: 1px solid var(--border-default); font-size: var(--font-size-sm); color: var(--text-primary); font-weight: 600; z-index: 2; }
  .year-scrubber { position: sticky; top: 44px; float: right; width: 48px; margin-right: -50px; height: 0; z-index: 3; display: flex; flex-direction: column; align-items: center; gap: 2px; }
  .grid { position: relative; }
</style>
