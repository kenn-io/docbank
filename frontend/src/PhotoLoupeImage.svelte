<script lang="ts">
  import { devicePixelRatio } from "svelte/reactivity/window";
  import { attachPanZoom, type PanZoom } from "@kenn-io/kit-ui";
  import { photoViewGeometry, toPanView, fromPanView, type PhotoView } from "./photoView.js";
  import { photoRotationStyle, observePhotoSize, rotatePhotoBox } from "./photoRotation.js";
  let {
    url,
    name,
    rotation = 0,
    unavailable = false,
    onready,
    onzoom,
    onmove,
    view,
    onview,
    onregion,
    oncancelregion,
    onconfirmregion,
    minimumRegionDrag = false,
    regionMode = false,
    regionLabel,
    faces = [],
  }: {
    url: string;
    name: string;
    rotation?: number;
    unavailable?: boolean;
    onready?: (ready: boolean) => void;
    onzoom: () => void;
    onmove: (direction: -1 | 1) => void;
    view?: PhotoView;
    onview?: (value: PhotoView) => void;
    onregion?: (value: { x: number; y: number; width: number; height: number }) => void;
    oncancelregion?: () => void;
    onconfirmregion?: () => void;
    minimumRegionDrag?: boolean;
    regionMode?: boolean;
    regionLabel?: string;
    faces?: { id: string; label: string; box: import("./photoRotation.js").NormalizedBox }[];
  } = $props();
  let width = $state(0);
  let height = $state(0);
  let naturalWidth = $state(0);
  let naturalHeight = $state(0);
  let decodedURL = $state("");
  const ready = $derived(!!url && decodedURL === url);
  $effect(() => onready?.(ready));
  const dpr = $derived(devicePixelRatio.current || 1);
  let zoom: PanZoom | undefined;
  let panScale = $state(1);
  const geometry = $derived(
    (!minimumRegionDrag || ready) && width && height && naturalWidth && naturalHeight
      ? photoViewGeometry(naturalWidth, naturalHeight, width, height, dpr, rotation)
      : undefined,
  );
  let keyboardPoint = $state({ x: 0.5, y: 0.5 });
  const keyboardRegion = $derived.by(() => {
    if (!regionMode || !geometry || !view) return;
    const scale = view.actual ? geometry.actualScale : view.scale;
    return {
      left: width / 2 + (keyboardPoint.x - view.x) * geometry.width * scale,
      top: height / 2 + (keyboardPoint.y - view.y) * geometry.height * scale,
      width: geometry.width * scale * 0.15,
      height: geometry.height * scale * 0.15,
    };
  });
  $effect(() => {
    if (view && geometry && zoom) {
      const pan = toPanView(view, geometry);
      zoom.setLimits({ minScale: Math.min(0.4, pan.scale), maxScale: Math.max(8, pan.scale) });
      zoom.setView(pan);
      panScale = pan.scale;
    }
  });
  let origin: { x: number; y: number; clientX: number; clientY: number } | undefined;
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
    origin = { ...point(event), clientX: event.clientX, clientY: event.clientY };
    viewport!.setPointerCapture(event.pointerId);
  }
  function endRegion(event: PointerEvent) {
    if (!regionMode) {
      origin = undefined;
      return;
    }
    if (!origin || !geometry) return;
    event.stopPropagation();
    event.preventDefault();
    const end = point(event);
    const w = Math.abs(end.x - origin.x),
      h = Math.abs(end.y - origin.y);
    if (
      minimumRegionDrag &&
      (Math.hypot(event.clientX - origin.clientX, event.clientY - origin.clientY) < 6 || !w || !h)
    ) {
      origin = undefined;
      return;
    }
    onregion?.({
      x: (end.x + origin.x) / 2,
      y: (end.y + origin.y) / 2,
      width: minimumRegionDrag ? w : w < 0.01 ? 0.15 : w,
      height: minimumRegionDrag ? h : h < 0.01 ? 0.15 : h,
    });
    origin = undefined;
  }
  function regionKey(event: KeyboardEvent) {
    if (!regionMode) return;
    const step = event.shiftKey ? 0.01 : 0.05;
    let point: { x: number; y: number } | undefined;
    if (event.key === "ArrowLeft")
      point = { ...keyboardPoint, x: Math.max(0.075, keyboardPoint.x - step) };
    else if (event.key === "ArrowRight")
      point = { ...keyboardPoint, x: Math.min(0.925, keyboardPoint.x + step) };
    else if (event.key === "ArrowUp")
      point = { ...keyboardPoint, y: Math.max(0.075, keyboardPoint.y - step) };
    else if (event.key === "ArrowDown")
      point = { ...keyboardPoint, y: Math.min(0.925, keyboardPoint.y + step) };
    if (point) {
      event.preventDefault();
      event.stopPropagation();
      keyboardPoint = point;
    } else if (event.key === "Enter" || event.key === " ") {
      event.stopPropagation();
    } else if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      origin = undefined;
      oncancelregion?.();
    }
  }
  function confirmRegion(event: MouseEvent) {
    event.preventDefault();
    event.stopPropagation();
    if (!regionMode || (minimumRegionDrag && !ready)) return;
    if (onconfirmregion) onconfirmregion();
    else onregion?.({ ...keyboardPoint, width: 0.15, height: 0.15 });
  }

  let viewport: HTMLElement | undefined;
  function panZoom(element: HTMLElement) {
    viewport = element;
    zoom = attachPanZoom(element, element.querySelector<HTMLElement>(".pan")!, {
      onChange: (value) => {
        panScale = value.scale;
        if (geometry && onview) onview(fromPanView(value, geometry));
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
  {#if regionMode && (!minimumRegionDrag || ready)}<button
      type="button"
      class="region-keyboard"
      aria-label={regionLabel ?? `Position closeup on ${name}`}
      aria-keyshortcuts="ArrowLeft ArrowRight ArrowUp ArrowDown Enter Escape"
      onkeydown={regionKey}
      onclick={confirmRegion}
    ></button>{/if}
  {#if keyboardRegion}<span
      class="keyboard-region"
      aria-hidden="true"
      style:left={`${keyboardRegion.left}px`}
      style:top={`${keyboardRegion.top}px`}
      style:width={`${keyboardRegion.width}px`}
      style:height={`${keyboardRegion.height}px`}
    ></span>{/if}
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
        onload={async (event) => {
          const image = event.currentTarget as HTMLImageElement;
          const loaded = url;
          const decoded = await image.decode().then(
            () => true,
            () => false,
          );
          if (!decoded || loaded !== url) return;
          ({ naturalWidth, naturalHeight } = image);
          if (naturalWidth && naturalHeight) decodedURL = loaded;
        }}
        onerror={() => (decodedURL = "")}
      />{:else}<span>{unavailable ? "No preview" : "Loading preview…"}</span>{/if}
    {#if url && geometry}
      <div
        class="face-layer"
        style:width={`${geometry.width}px`}
        style:height={`${geometry.height}px`}
      >
        {#each faces as face (face.id)}
          {@const box = rotatePhotoBox(face.box, rotation)}
          <div
            class="face-box"
            style:left={`${box.x * 100}%`}
            style:top={`${box.y * 100}%`}
            style:width={`${box.width * 100}%`}
            style:height={`${box.height * 100}%`}
          >
            <span style:transform={`scale(${1 / panScale})`}>{face.label}</span>
          </div>
        {/each}
      </div>
    {/if}
  </div>
</div>

<style>
  .face-layer {
    position: absolute;
    top: 50%;
    left: 50%;
    transform: translate(-50%, -50%);
    pointer-events: none;
  }
  .face-box {
    position: absolute;
    border: 2px solid var(--border-focus, #ffcc00);
    box-sizing: border-box;
  }
  .face-box span {
    transform-origin: bottom left;
    position: absolute;
    bottom: 100%;
    left: -2px;
    padding: 2px 4px;
    background: var(--bg-surface);
    color: var(--text-primary);
    font-size: var(--font-size-xs);
    white-space: nowrap;
  }

  .photo-viewport {
    position: relative;
    flex: 1;
    min-width: 0;
    min-height: 0;
    overflow: hidden;
    touch-action: none;
  }
  .photo-viewport:focus-within {
    outline: 2px solid var(--border-focus, #ffcc00);
    outline-offset: -2px;
  }
  .region-keyboard {
    position: absolute;
    z-index: 2;
    inset: 0;
    padding: 0;
    border: 0;
    background: transparent;
    cursor: crosshair;
    pointer-events: none;
  }
  .region-mode {
    cursor: crosshair;
  }
  .keyboard-region {
    position: absolute;
    z-index: 1;
    border: 2px solid var(--border-focus, #ffcc00);
    background: color-mix(in srgb, var(--border-focus, #ffcc00) 12%, transparent);
    transform: translate(-50%, -50%);
    pointer-events: none;
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
