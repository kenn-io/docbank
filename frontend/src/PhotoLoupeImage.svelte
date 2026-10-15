<script lang="ts">
  import { attachPanZoom, type PanZoom } from "@kenn-io/kit-ui";
  import { photoRotationStyle, observePhotoSize } from "./photoRotation.js";
  let {
    url,
    name,
    rotation = 0,
    unavailable = false,
    onzoom,
    onmove,
  }: {
    url: string;
    name: string;
    rotation?: number;
    unavailable?: boolean;
    onzoom: () => void;
    onmove: (direction: -1 | 1) => void;
  } = $props();
  let width = $state(0);
  let height = $state(0);
  let zoom: PanZoom | undefined;
  let viewport: HTMLElement | undefined;
  function panZoom(element: HTMLElement) {
    viewport = element;
    zoom = attachPanZoom(element, element.querySelector<HTMLElement>(".pan")!, {
      onSwipe: (direction) => {
        zoom?.reset();
        onmove(direction);
      },
    });
    const observer = new MutationObserver(() => {
      if (element.dataset.zoomed) onzoom();
    });
    observer.observe(element, { attributes: true, attributeFilter: ["data-zoomed"] });
    return () => {
      observer.disconnect();
      zoom?.destroy();
      zoom = undefined;
      viewport = undefined;
    };
  }
  export function reset() {
    zoom?.reset();
  }
  export function isZoomed() {
    return !!viewport?.dataset.zoomed;
  }
</script>

<div class="photo-viewport" {@attach panZoom}>
  <div
    class="pan"
    {@attach (element) =>
      observePhotoSize(element, (w, h) => {
        width = w;
        height = h;
      })}
  >
    {#if url}<img
        src={url}
        alt={name}
        style={photoRotationStyle(rotation, width, height)}
        draggable="false"
      />{:else}<span>{unavailable ? "No preview" : "Loading preview…"}</span>{/if}
  </div>
</div>

<style>
  .photo-viewport {
    flex: 1;
    min-width: 0;
    min-height: 0;
    overflow: hidden;
    touch-action: none;
  }
  .pan {
    position: relative;
    width: 100%;
    height: 100%;
    display: flex;
    align-items: center;
    justify-content: center;
    transform-origin: center;
    color: var(--text-muted);
  }
  img {
    position: absolute;
    top: 50%;
    left: 50%;
    display: block;
    object-fit: contain;
    user-select: none;
  }
</style>
