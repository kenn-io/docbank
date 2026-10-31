<script lang="ts">
  import { onDestroy } from "svelte";
  import type { PDFRedactionPage, PDFRedactionBox, PDFRedactionGap } from "./generated/docbank.js";
  import { physicalRectangle, type Point } from "./redactionGeometry.js";
  let {
    page,
    masks,
    gaps,
    proposed = [],
    drawing,
    oncommit,
    ondrag,
  }: {
    page: PDFRedactionPage;
    masks: PDFRedactionBox[];
    gaps: PDFRedactionGap[];
    proposed?: PDFRedactionBox[];
    drawing: boolean;
    oncommit: (box: PDFRedactionBox) => void;
    ondrag: (active: boolean) => void;
  } = $props();
  let canvas: HTMLDivElement;
  let start = $state<Point>();
  let cursor = $state<Point>({ x: 0, y: 0 });
  let pointer: number | undefined;
  const bounds = $derived({ left: 0, top: 0, width: page.width, height: page.height });
  const draft = $derived(
    start ? physicalRectangle(start, cursor, bounds, page.width, page.height) : undefined,
  );
  function cancel() {
    start = undefined;
    ondrag(false);
    if (pointer !== undefined && canvas?.hasPointerCapture(pointer))
      canvas.releasePointerCapture(pointer);
    pointer = undefined;
  }
  function commit() {
    if (draft) oncommit({ page: page.number, frame_sha256: page.frame_sha256, ...draft });
    cancel();
  }
  function point(event: PointerEvent): Point | undefined {
    const r = canvas.getBoundingClientRect();
    if (r.width <= 0 || r.height <= 0) return;
    return {
      x: Math.max(0, Math.min(page.width, ((event.clientX - r.left) * page.width) / r.width)),
      y: Math.max(0, Math.min(page.height, ((event.clientY - r.top) * page.height) / r.height)),
    };
  }
  function down(e: PointerEvent) {
    if (!drawing || e.button !== 0) return;
    const p = point(e);
    if (!p) return;
    e.preventDefault();
    canvas.focus();
    cursor = p;
    start = p;
    pointer = e.pointerId;
    canvas.setPointerCapture(pointer);
    ondrag(true);
  }
  function move(e: PointerEvent) {
    if (pointer !== e.pointerId) return;
    const p = point(e);
    if (p) cursor = p;
  }
  function up(e: PointerEvent) {
    if (pointer !== e.pointerId) return;
    move(e);
    commit();
  }
  function key(e: KeyboardEvent) {
    if (e.key === "Escape" && start) {
      e.preventDefault();
      e.stopPropagation();
      cancel();
      return;
    }
    if (!drawing) return;
    const step = e.shiftKey ? 1000 : 100;
    if (e.key.startsWith("Arrow")) {
      e.preventDefault();
      cursor = {
        x: Math.max(
          0,
          Math.min(
            page.width,
            cursor.x + (e.key === "ArrowRight" ? step : e.key === "ArrowLeft" ? -step : 0),
          ),
        ),
        y: Math.max(
          0,
          Math.min(
            page.height,
            cursor.y + (e.key === "ArrowDown" ? step : e.key === "ArrowUp" ? -step : 0),
          ),
        ),
      };
    } else if (e.key === "Enter") {
      e.preventDefault();
      if (start) commit();
      else {
        start = { ...cursor };
        ondrag(true);
      }
    }
  }
  $effect(() => {
    if (!drawing) cancel();
  });
  onDestroy(cancel);
</script>

<!-- svelte-ignore a11y_no_noninteractive_element_interactions, a11y_no_noninteractive_tabindex (custom keyboard and pointer drawing canvas) -->
<div
  bind:this={canvas}
  class="mask-canvas"
  class:drawing
  role="application"
  tabindex="0"
  aria-label="Mask canvas"
  aria-describedby="mask-keyboard"
  onpointerdown={down}
  onpointermove={move}
  onpointerup={up}
  onpointercancel={cancel}
  onkeydown={key}
>
  <svg viewBox={`0 0 ${page.width} ${page.height}`} preserveAspectRatio="none" aria-hidden="true">
    {#each gaps.filter((g) => g.box.page === page.number) as gap}<rect
        class="gap"
        x={gap.box.x0}
        y={gap.box.y0}
        width={gap.box.x1 - gap.box.x0}
        height={gap.box.y1 - gap.box.y0}
      />{/each}
    {#each masks.filter((b) => b.page === page.number) as b}<rect
        class="mask"
        x={b.x0}
        y={b.y0}
        width={b.x1 - b.x0}
        height={b.y1 - b.y0}
      />{/each}
    {#each proposed.filter((b) => b.page === page.number) as b}<rect
        class="draft"
        x={b.x0}
        y={b.y0}
        width={b.x1 - b.x0}
        height={b.y1 - b.y0}
      />{/each}
    {#if draft}<rect
        class="draft"
        x={draft.x0}
        y={draft.y0}
        width={draft.x1 - draft.x0}
        height={draft.y1 - draft.y0}
      />{/if}
    {#if drawing}<path
        class="cursor"
        d={`M${cursor.x - 300},${cursor.y}h600 M${cursor.x},${cursor.y - 300}v600`}
      />{/if}
  </svg>
</div>
<span class="kit-sr-only" aria-live="polite"
  >Page {page.number}, cursor {(cursor.x / 10000).toFixed(2)}, {(cursor.y / 10000).toFixed(2)} inches.</span
>

<style>
  .mask-canvas {
    position: absolute;
    inset: 0;
    outline-offset: -3px;
  }
  .mask-canvas.drawing {
    cursor: crosshair;
    touch-action: none;
  }
  svg {
    width: 100%;
    height: 100%;
    pointer-events: none;
    position: absolute;
    inset: 0;
  }
  rect,
  path {
    vector-effect: non-scaling-stroke;
    stroke-width: 2;
  }
  .mask {
    /* kit-ui-check-ignore: the mask represents black output ink on the white PDF canvas in either theme. */
    fill: rgb(0 0 0 / 25%);
    /* kit-ui-check-ignore: white document pixels need a dark mask boundary in either theme. */
    stroke: #111;
  }
  .gap {
    fill: color-mix(in srgb, var(--accent-amber) 15%, transparent);
    stroke: var(--accent-amber);
    stroke-dasharray: 4 4;
  }
  .draft {
    fill: color-mix(in srgb, var(--accent-blue) 15%, transparent);
    stroke: var(--accent-blue);
  }
  .cursor {
    stroke: var(--accent-blue);
    fill: none;
  }
</style>
