<script lang="ts">
  import { devicePixelRatio } from "svelte/reactivity/window";
  import { attachPanZoom, type PanZoom } from "@kenn-io/kit-ui";
  import { photoViewGeometry, toPanView, fromPanView, type PhotoView } from "./photoView.js";
  import { photoRotationStyle, observePhotoSize } from "./photoRotation.js";
  let {
    url,
    name,
    rotation = 0,
    unavailable = false,
    onzoom,
    onmove,
    view,
    onview,
    onregion,
    regionMode = false,
  }: {
    url: string;
    name: string;
    rotation?: number;
    unavailable?: boolean;
    onzoom: () => void;
    onmove: (direction: -1 | 1) => void;
    view?: PhotoView;
    onview?: (value: PhotoView) => void;
    onregion?: (value: { x: number; y: number; width: number; height: number }) => void;
    regionMode?: boolean;
  } = $props();
  let width = $state(0);
  let height = $state(0);
  let naturalWidth = $state(0);
  let naturalHeight = $state(0);
  const dpr = $derived(devicePixelRatio.current || 1);
  let zoom: PanZoom | undefined;
  const geometry = $derived(
    width && height && naturalWidth && naturalHeight
      ? photoViewGeometry(naturalWidth, naturalHeight, width, height, dpr, rotation)
      : undefined,
  );
  $effect(() => {
    if (view && geometry && zoom) {
      const pan = toPanView(view, geometry);
      zoom.setLimits({ minScale: Math.min(0.4, pan.scale), maxScale: Math.max(8, pan.scale) });
      zoom.setView(pan);
      if (pan.scale > 1) onzoom();
    }
  });
  let origin: { x: number; y: number } | undefined;
  function point(event: PointerEvent) {
    const rect = viewport!.getBoundingClientRect();
    const pan = zoom!.getView();
    return {
      x: Math.max(
        0,
        Math.min(
          1,
          0.5 +
            (event.clientX - rect.left - rect.width / 2 - pan.offsetX) /
              (geometry!.width * pan.scale),
        ),
      ),
      y: Math.max(
        0,
        Math.min(
          1,
          0.5 +
            (event.clientY - rect.top - rect.height / 2 - pan.offsetY) /
              (geometry!.height * pan.scale),
        ),
      ),
    };
  }
  function startRegion(event: PointerEvent) {
    if (!regionMode || !geometry || !zoom) return;
    event.stopPropagation();
    event.preventDefault();
    origin = point(event);
    viewport!.setPointerCapture(event.pointerId);
  }
  function endRegion(event: PointerEvent) {
    if (!origin || !geometry) return;
    event.stopPropagation();
    event.preventDefault();
    const end = point(event);
    const w = Math.abs(end.x - origin.x),
      h = Math.abs(end.y - origin.y);
    onregion?.({
      x: (end.x + origin.x) / 2,
      y: (end.y + origin.y) / 2,
      width: w < 0.01 ? 0.15 : w,
      height: h < 0.01 ? 0.15 : h,
    });
    origin = undefined;
  }

  let viewport: HTMLElement | undefined;
  function panZoom(element: HTMLElement) {
    viewport = element;
    zoom = attachPanZoom(element, element.querySelector<HTMLElement>(".pan")!, {
      onChange: (value) => {
        if (geometry && onview) onview(fromPanView(value, geometry));
        if (value.scale > 1) onzoom();
      },
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

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="photo-viewport"
  class:region-mode={regionMode}
  onpointerdowncapture={startRegion}
  onpointerupcapture={endRegion}
  {@attach panZoom}
>
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
        onload={(event) => {
          naturalWidth = (event.currentTarget as HTMLImageElement).naturalWidth;
          naturalHeight = (event.currentTarget as HTMLImageElement).naturalHeight;
        }}
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
  .region-mode {
    cursor: crosshair;
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
