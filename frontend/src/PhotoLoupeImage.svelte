<script lang="ts">
  import { attachPanZoom, type PanZoom } from "@kenn-io/kit-ui";
  let { url, name, unavailable = false, onzoom, onmove }: { url: string; name: string; unavailable?: boolean; onzoom: () => void; onmove: (direction: -1 | 1) => void } = $props();
  let zoom: PanZoom | undefined;
  let viewport: HTMLElement | undefined;
  function panZoom(element: HTMLElement) {
    viewport = element;
    zoom = attachPanZoom(element, element.querySelector<HTMLElement>(".pan")!, { onSwipe: direction => { zoom?.reset(); onmove(direction); } });
    const observer = new MutationObserver(() => { if (element.dataset.zoomed) onzoom(); });
    observer.observe(element, { attributes: true, attributeFilter: ["data-zoomed"] });
    return () => { observer.disconnect(); zoom?.destroy(); zoom = undefined; viewport = undefined; };
  }
  export function reset() { zoom?.reset(); }
  export function isZoomed() { return !!viewport?.dataset.zoomed; }
</script>
<div class="photo-viewport" {@attach panZoom}>
  <div class="pan">{#if url}<img src={url} alt={name} draggable="false" />{:else}<span>{unavailable ? "No preview" : "Loading preview…"}</span>{/if}</div>
</div>
<style>
  .photo-viewport { flex: 1; min-width: 0; min-height: 0; overflow: hidden; touch-action: none; }
  .pan { width: 100%; height: 100%; display: flex; align-items: center; justify-content: center; transform-origin: center; color: var(--text-muted); }
  img { display: block; width: 100%; height: 100%; object-fit: contain; user-select: none; }
</style>
