<script lang="ts">
  import { SplitResizeHandle } from "@kenn-io/kit-ui";
  import { clampPanelWidth, savePanelWidth } from "./panelWidth.js";

  interface Props {
    /** Names the panel to assistive technology and keys its saved width. */
    name: string;
    label: string;
    /** The width set by dragging, or undefined for the panel's default. */
    width?: number;
    min: number;
    max: number;
  }

  let { name, label, width = $bindable(), min, max }: Props = $props();

  // The panel is the element right after the handle, so dragging left widens it.
  let root = $state<HTMLElement>();
  let size = $state(0);
  let start = 0;

  function measure(): void {
    const panel = root?.nextElementSibling;
    size = panel ? panel.getBoundingClientRect().width : 0;
  }

  $effect(() => {
    void width;
    measure();
  });
  const limit = () => Math.min(max, Math.floor(window.innerWidth / 2));

  function change(next: number | undefined): void {
    width = next;
    savePanelWidth(name, next);
  }
</script>

<svelte:window onresize={measure} />

<div class="panel-resizer" bind:this={root}>
  <SplitResizeHandle
    ariaLabel={label}
    ariaValueMin={min}
    ariaValueMax={Math.max(min, limit())}
    ariaValueNow={Math.round(size)}
    onResizeStart={() => {
      measure();
      start = size;
    }}
    onResize={(event) => (width = clampPanelWidth(start - event.delta, min, limit()))}
    onResizeEnd={() => change(width)}
    onActivate={() => change(undefined)}
  />
</div>

<style>
  .panel-resizer {
    display: flex;
    align-self: stretch;
  }

  .panel-resizer :global(.kit-split-resize-handle) {
    align-self: stretch;
  }
</style>
